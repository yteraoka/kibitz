package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What a failed run left behind, and where.
//
// opencode reports a failure in three places and says the useful part in only
// one of them. Reproduced against the version the worker image pins:
//
//	stdout  {"type":"error","error":{"name":"UnknownError","data":{
//	          "message":"Unexpected server error. Check server logs for details.",
//	          "ref":"err_bf6ece47"}}}
//	log     level=ERROR message=failed ref=err_bf6ece47
//	          error="ProviderModelNotFoundError: Model not found: nosuch/x."
//	stderr  Error: Session not found            (printed for a person)
//
// The event on stdout names the failure and points at the log; the log says
// what the failure was. Without --print-logs the log goes to a file inside
// the container and stderr is empty, which is how a production failure came
// to be reported as "opencode: exit status 1:" with nothing after the colon.
// Everything in this file exists to put the three back together.

// maxStderr bounds what is kept of stderr. It keeps the end, because that is
// where a failure is logged.
const maxStderr = 32 << 10

// genericMessage is what opencode says on stdout for every server-side
// failure. It is not worth repeating when the log has the real one.
const genericMessage = "Unexpected server error. Check server logs for details."

// Diagnosis is everything known about one run of the agent, gathered so that
// a failure can be explained without reproducing it.
type Diagnosis struct {
	// Status is how the process ended: "exit status 1", "signal: killed".
	Status   string
	ExitCode int
	TimedOut bool
	Duration time.Duration

	// ErrorName, ErrorMessage and ErrorRef come from the error event on
	// stdout. The ref is what ties it to the log.
	ErrorName    string
	ErrorMessage string
	ErrorRef     string
	// Cause is the log's own account of the error the event referred to, or
	// failing that, of the last error the log recorded.
	Cause string

	// Notices are the lines opencode prints for a person rather than as a
	// log: "Error: Session not found", or that an agent definition was
	// missing and the default one is running instead.
	Notices []string
	// Problems are the warnings and errors from its log.
	Problems []string

	// SessionID is the session the run was in. A run that broke off can be
	// resumed in it.
	SessionID string
	// Finish is how the model ended its last step, as opencode recorded it:
	// "stop", "tool-calls", "unknown". "unknown" is the one to look for; see
	// [Diagnosis.EndedOnModelTurn].
	Finish string

	// Events counts the event stream by type. It is a count and not the
	// stream itself: the stream carries the code the agent read and the text
	// it wrote, which belong to the repository and not in a log.
	Events map[string]int
	// Stderr is the end of what was written to stderr.
	Stderr string
}

// Summary is one line saying what went wrong, for the error and for the
// comment on the pull request. It prefers the log's cause, then the event,
// then whatever opencode printed for a person.
func (d *Diagnosis) Summary() string {
	parts := make([]string, 0, 3)
	switch {
	case d.TimedOut:
		parts = append(parts, fmt.Sprintf("timed out after %s", d.Duration.Round(time.Second)))
	case d.Cause != "":
		parts = append(parts, d.Cause)
	case d.ErrorName != "" && d.ErrorMessage != "" && d.ErrorMessage != genericMessage:
		parts = append(parts, d.ErrorName+": "+d.ErrorMessage)
	case d.ErrorName != "":
		parts = append(parts, d.ErrorName)
	}
	// Notices go in even when there is a cause: "Session not found" is how a
	// run that failed only because its session expired is recognized, and a
	// cause from the log must not hide it.
	parts = append(parts, d.Notices...)
	if len(parts) == 0 && len(d.Problems) > 0 {
		parts = append(parts, d.Problems[len(d.Problems)-1])
	}
	if len(parts) == 0 {
		parts = append(parts, "the agent exited without saying why")
	}

	summary := strings.Join(parts, "; ")
	var tags []string
	if d.Status != "" && !d.TimedOut {
		tags = append(tags, d.Status)
	}
	if d.ErrorRef != "" {
		tags = append(tags, "ref "+d.ErrorRef)
	}
	if len(tags) > 0 {
		summary += " (" + strings.Join(tags, ", ") + ")"
	}
	return summary
}

// Attrs renders the diagnosis for a log record.
func (d *Diagnosis) Attrs() []slog.Attr {
	attrs := []slog.Attr{
		slog.String("status", d.Status),
		slog.Int("exit_code", d.ExitCode),
		slog.Bool("timed_out", d.TimedOut),
		slog.Duration("duration", d.Duration),
	}
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	add("error_name", d.ErrorName)
	add("error_message", d.ErrorMessage)
	add("error_ref", d.ErrorRef)
	add("cause", d.Cause)
	add("finish", d.Finish)
	if len(d.Notices) > 0 {
		attrs = append(attrs, slog.Any("notices", d.Notices))
	}
	if len(d.Problems) > 0 {
		attrs = append(attrs, slog.Any("problems", d.Problems))
	}
	if len(d.Events) > 0 {
		attrs = append(attrs, slog.Any("events", d.Events))
	}
	add("stderr", d.Stderr)
	return attrs
}

// modelTurn is how Gemini refuses a request whose history ends with its own
// turn.
const modelTurn = "ending with a model turn"

// EndedOnModelTurn reports the one failure that is opencode's own doing and
// that resuming the session gets past.
//
// Gemini ends some turns with a reason the AI SDK does not know -- OTHER,
// FINISH_REASON_UNSPECIFIED, UNEXPECTED_TOOL_CALL, TOO_MANY_TOOL_CALLS --
// and opencode records every such turn as "unknown", which it treats like a
// tool call: it asks again. Nothing was added to the conversation in
// between, so the request it sends ends with the model's own turn, and
// Vertex AI refuses it. Reproduced against the pinned version with a server
// answering as Gemini does; STOP, MAX_TOKENS, SAFETY and
// MALFORMED_FUNCTION_CALL do not do it.
//
// The session is intact up to the turn that broke off. A new message from
// the user is all the request lacked.
func (d *Diagnosis) EndedOnModelTurn() bool {
	return strings.Contains(strings.ToLower(d.ErrorMessage+"\n"+d.Cause), modelTurn)
}

// RunError is a run of the agent that failed. Its message is the summary; the
// rest is in Diagnosis, for the log.
type RunError struct {
	Diagnosis *Diagnosis
	// stdout is what the run wrote before it failed. It is kept so the
	// tokens it spent are still counted when the run is resumed.
	stdout []byte
}

func (e *RunError) Error() string { return "opencode: " + e.Diagnosis.Summary() }

// diagnose puts together what a run left behind.
func diagnose(stdout, stderr []byte, runErr error, timedOut bool, duration time.Duration) *Diagnosis {
	d := &Diagnosis{TimedOut: timedOut, Duration: duration, ExitCode: -1}

	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		d.Status = exitErr.String()
		d.ExitCode = exitErr.ExitCode()
	case runErr != nil:
		// The process never ran: a missing binary, a bad working directory.
		d.Status = runErr.Error()
	default:
		d.Status = "exit status 0"
		d.ExitCode = 0
	}

	d.readEvents(stdout)
	d.readStderr(stderr)
	return d
}

// readEvents counts the event stream and picks out its error.
func (d *Diagnosis) readEvents(stdout []byte) {
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      *struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"part"`
			Error *struct {
				Name string `json:"name"`
				Data struct {
					Message string `json:"message"`
					Ref     string `json:"ref"`
				} `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &ev) != nil || ev.Type == "" {
			continue
		}
		if d.Events == nil {
			d.Events = map[string]int{}
		}
		d.Events[ev.Type]++
		if ev.SessionID != "" {
			d.SessionID = ev.SessionID
		}
		if ev.Part != nil && ev.Part.Type == "step-finish" && ev.Part.Reason != "" {
			d.Finish = ev.Part.Reason
		}
		if ev.Type == "error" && ev.Error != nil {
			// The last one wins: it is the one that ended the run.
			d.ErrorName = ev.Error.Name
			d.ErrorMessage = ev.Error.Data.Message
			d.ErrorRef = ev.Error.Data.Ref
		}
	}
}

// ansi matches terminal colour codes, which opencode puts around the lines it
// prints for a person.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// readStderr separates opencode's log from what it printed for a person, and
// finds the cause the error event referred to.
func (d *Diagnosis) readStderr(stderr []byte) {
	text := strings.TrimSpace(ansi.ReplaceAllString(string(stderr), ""))
	if len(text) > maxStderr {
		text = "…" + text[len(text)-maxStderr:]
	}
	d.Stderr = text

	var lastError string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields, ok := parseLogLine(line)
		if !ok {
			// Not a log line: something printed for a person, which is rare
			// and always worth keeping.
			d.Notices = append(d.Notices, strings.TrimSpace(strings.TrimPrefix(line, "!")))
			continue
		}

		level := fields["level"]
		if level != "ERROR" && level != "WARN" {
			continue
		}
		problem := fields["message"]
		if detail := firstNonEmpty(fields["error"], firstLine(fields["cause"])); detail != "" {
			problem += ": " + detail
		}
		d.Problems = append(d.Problems, level+" "+problem)

		if level != "ERROR" {
			continue
		}
		if msg := fields["error"]; msg != "" {
			lastError = msg
			if d.ErrorRef != "" && fields["ref"] == d.ErrorRef {
				d.Cause = msg
			}
		}
	}
	// Without a ref to match, the last error the log recorded is the best
	// guess at what ended the run. With one that did not match, it is a
	// guess about something else, so it is left alone.
	if d.Cause == "" && d.ErrorRef == "" {
		d.Cause = lastError
	}
	if len(d.Problems) > 20 {
		d.Problems = append([]string{fmt.Sprintf("(%d earlier problems left out)", len(d.Problems)-20)},
			d.Problems[len(d.Problems)-20:]...)
	}
}

// parseLogLine reads one line of opencode's log:
//
//	timestamp=2026-09-27T08:23:28.047Z level=ERROR run=d671 message=failed ref=err_e68d error="Provider…"
//
// Values are bare or double-quoted with backslash escapes. A line that does
// not start with a timestamp is not a log line.
func parseLogLine(line string) (map[string]string, bool) {
	if !strings.HasPrefix(line, "timestamp=") {
		return nil, false
	}
	fields := map[string]string{}
	for i := 0; i < len(line); {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq < 0 {
			break
		}
		key := line[i : i+eq]
		i += eq + 1

		if i < len(line) && line[i] == '"' {
			end := i + 1
			for end < len(line) && line[end] != '"' {
				if line[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(line) {
				end = len(line) - 1
			}
			quoted := line[i : end+1]
			value, err := strconv.Unquote(quoted)
			if err != nil {
				value = strings.Trim(quoted, `"`)
			}
			fields[key] = value
			i = end + 1
			continue
		}
		end := strings.IndexByte(line[i:], ' ')
		if end < 0 {
			end = len(line) - i
		}
		fields[key] = line[i : i+end]
		i += end
	}
	return fields, fields["level"] != ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// tailBuffer keeps the last max bytes written to it. The end of stderr is
// where a failure is logged, and a verbose run must not grow the worker's
// memory without bound to keep the beginning.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte { return t.buf }
