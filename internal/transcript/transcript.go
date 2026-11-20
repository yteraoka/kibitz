// Package transcript keeps a record of what the agent did for one job — the
// prompt it was given and every event its engine reported — so that a person
// reading a comment kibitz wrote can find out how it got there.
//
// A comment carries only the record's id, as a trailer. The record itself is
// in a bucket, and kibitz-server renders it (see [Handler]). The comment is
// the wrong place for it: a run is easily megabytes of tool output, and most
// of it is the repository's own code read back.
package transcript

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Version is the shape of [Record]. A viewer that meets a newer one renders
// what it understands rather than refusing.
const Version = 1

// TrailerKey starts the line a comment names its record on. It is written
// like a git trailer so that it reads as metadata rather than as part of the
// review, and so that something scanning comments can find it.
const TrailerKey = "Kibitz-Transcript"

// MaxEventBytes bounds what one record keeps of the engines' events. A run
// that read a large file through a tool reports the file back, and keeping
// every byte of every such run would make the bucket the most expensive part
// of a review. Past the limit events are counted rather than kept.
const MaxEventBytes = 16 << 20

// Record is everything kept about one job.
type Record struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	// EventID is the normalized event the job ran for, which is what the
	// worker's logs are searched by.
	EventID    string `json:"event_id,omitempty"`
	Platform   string `json:"platform,omitempty"`
	Repository string `json:"repository,omitempty"`
	// Ref names the pull request or issue, as "owner/repo#12".
	Ref string `json:"ref,omitempty"`
	// URL is the pull request's or issue's page, for a way back.
	URL string `json:"url,omitempty"`
	// Kind is why the job ran: a push, a command, a question.
	Kind string `json:"kind,omitempty"`
	// Runs are the engine runs the job made, in order. A review that needed
	// a triage pass first, or a retry, has more than one.
	Runs []Run `json:"runs"`
}

// Run is one invocation of the engine.
type Run struct {
	Mode      string        `json:"mode"`
	Engine    string        `json:"engine,omitempty"`
	Model     string        `json:"model,omitempty"`
	StartedAt time.Time     `json:"started_at"`
	Duration  time.Duration `json:"duration_ns"`
	// Prompt is what the agent was asked, as kibitz built it.
	Prompt string `json:"prompt,omitempty"`
	// Events are what the engine reported, as it reported them. They are
	// kept raw rather than normalized, because the shape is the engine's and
	// changes from release to release: a record that keeps what was said can
	// still be read after kibitz's idea of it has moved on.
	Events []json.RawMessage `json:"events,omitempty"`
	// OmittedEvents counts events not kept because the record was full.
	OmittedEvents int `json:"omitted_events,omitempty"`
	// Output is what the run produced: the summary, or the reply.
	Output string `json:"output,omitempty"`
	// Error is why the run failed, when it did.
	Error string `json:"error,omitempty"`

	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	ToolCalls    int `json:"tool_calls,omitempty"`
}

// NewID returns an id for a record.
//
// It is 128 random bits because the id is the link: anybody who can read the
// comment can open the record, and an id that could be guessed or counted
// would open everybody else's too.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform kibitz runs on, and a
		// predictable id is worse than no record.
		panic("transcript: reading random bytes: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

var idPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidID reports whether id is one [NewID] could have returned. It is what
// stands between a URL path and an object name.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Recorder collects one job's runs. It is safe for concurrent use.
type Recorder struct {
	mu     sync.Mutex
	record Record
	size   int
}

// NewRecorder starts a record.
func NewRecorder(r Record) *Recorder {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	r.Version = Version
	return &Recorder{record: r}
}

// ID is the record's id.
func (r *Recorder) ID() string {
	if r == nil {
		return ""
	}
	return r.record.ID
}

// Add appends a run, dropping its events past [MaxEventBytes] for the record
// as a whole. A nil recorder ignores it, so callers need not check.
func (r *Recorder) Add(run Run) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	r.size += len(run.Prompt) + len(run.Output)
	kept := make([]json.RawMessage, 0, len(run.Events))
	for _, ev := range run.Events {
		if r.size+len(ev) > MaxEventBytes {
			run.OmittedEvents++
			continue
		}
		r.size += len(ev)
		kept = append(kept, ev)
	}
	run.Events = kept
	r.record.Runs = append(r.record.Runs, run)
}

// Empty reports whether nothing has been recorded.
func (r *Recorder) Empty() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.record.Runs) == 0
}

// Snapshot returns a copy of the record so far.
func (r *Recorder) Snapshot() *Record {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.record
	out.Runs = append([]Run(nil), r.record.Runs...)
	return &out
}

type contextKey struct{}

// NewContext returns a context that carries rec.
func NewContext(ctx context.Context, rec *Recorder) context.Context {
	return context.WithValue(ctx, contextKey{}, rec)
}

// FromContext returns the recorder ctx carries, or nil.
func FromContext(ctx context.Context) *Recorder {
	rec, _ := ctx.Value(contextKey{}).(*Recorder)
	return rec
}

// SplitLines turns an engine's JSONL stdout into events. A line that is not
// JSON is kept as a JSON string, because a CLI's stray warning is often the
// part of a transcript that explains it.
func SplitLines(stdout []byte) []json.RawMessage {
	var out []json.RawMessage
	start := 0
	for i := 0; i <= len(stdout); i++ {
		if i < len(stdout) && stdout[i] != '\n' {
			continue
		}
		line := trimSpace(stdout[start:i])
		start = i + 1
		if len(line) == 0 {
			continue
		}
		if json.Valid(line) {
			out = append(out, json.RawMessage(append([]byte(nil), line...)))
			continue
		}
		quoted, err := json.Marshal(string(line))
		if err != nil {
			continue
		}
		out = append(out, quoted)
	}
	return out
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\r') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// RoutePattern is where kibitz-server serves records.
const RoutePattern = "GET /transcripts/{id}"

// URL is where a record is served under baseURL, or "" without one.
func URL(baseURL, id string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" || id == "" {
		return ""
	}
	return baseURL + "/transcripts/" + id
}

// Trailer is the line a comment names its record on. With a base URL the id
// is a link to the server's page; without one it is still the key an
// operator looks the object up by.
func Trailer(baseURL, id string) string {
	if id == "" {
		return ""
	}
	if u := URL(baseURL, id); u != "" {
		return fmt.Sprintf("%s: [%s](%s)", TrailerKey, id, u)
	}
	return fmt.Sprintf("%s: `%s`", TrailerKey, id)
}

var trailerPattern = regexp.MustCompile(`(?m)^(?:<sub>)?` + TrailerKey + `: \[?` + "`?" + `([0-9a-f]{32})`)

// ParseTrailer finds the record a comment names, or "".
func ParseTrailer(body string) string {
	m := trailerPattern.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}
