package transcript_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/transcript"
)

func TestNewIDIsValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := transcript.NewID()
		if !transcript.ValidID(id) {
			t.Fatalf("NewID() = %q, which ValidID rejects", id)
		}
		if seen[id] {
			t.Fatalf("NewID() repeated %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "../etc/passwd", strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 33)} {
		if transcript.ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func TestSplitLinesKeepsJSONAndQuotesTheRest(t *testing.T) {
	got := transcript.SplitLines([]byte("{\"type\":\"a\"}\n\nwarning: something\r\n  [1,2]  \n{broken"))
	want := []string{`{"type":"a"}`, `"warning: something"`, `[1,2]`, `"{broken"`}
	if len(got) != len(want) {
		t.Fatalf("SplitLines = %q, want %q", got, want)
	}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Errorf("event %d = %s, want %s", i, got[i], want[i])
		}
	}
}

func TestTrailerLinksWhenThereIsABaseURL(t *testing.T) {
	id := transcript.NewID()
	got := transcript.Trailer("https://kibitz.example/", id)
	want := "Kibitz-Transcript: [" + id + "](https://kibitz.example/transcripts/" + id + ")"
	if got != want {
		t.Errorf("Trailer = %q, want %q", got, want)
	}
	if plain := transcript.Trailer("", id); plain != "Kibitz-Transcript: `"+id+"`" {
		t.Errorf("Trailer without a base URL = %q", plain)
	}
	for _, body := range []string{"review\n\n---\n" + got + "\n", "answer\n\n<sub>" + transcript.Trailer("", id) + "</sub>"} {
		if parsed := transcript.ParseTrailer(body); parsed != id {
			t.Errorf("ParseTrailer(%q) = %q, want %q", body, parsed, id)
		}
	}
	if transcript.ParseTrailer("no trailer here") != "" {
		t.Error("ParseTrailer found a trailer in a body without one")
	}
}

// One run that read a huge file must not make every record that size.
func TestRecorderStopsKeepingEventsPastTheLimit(t *testing.T) {
	rec := transcript.NewRecorder(transcript.Record{})
	big := json.RawMessage(`"` + strings.Repeat("x", transcript.MaxEventBytes/2) + `"`)
	rec.Add(transcript.Run{Mode: "review", Events: []json.RawMessage{big, big, big}})

	got := rec.Snapshot()
	if len(got.Runs) != 1 {
		t.Fatalf("%d runs, want 1", len(got.Runs))
	}
	if kept, omitted := len(got.Runs[0].Events), got.Runs[0].OmittedEvents; kept != 1 || omitted != 2 {
		t.Errorf("kept %d events and omitted %d, want 1 and 2", kept, omitted)
	}
	if got.Version != transcript.Version || !transcript.ValidID(got.ID) || got.CreatedAt.IsZero() {
		t.Errorf("the record was not started properly: %+v", got)
	}
}

func TestNilRecorderIsHarmless(t *testing.T) {
	var rec *transcript.Recorder
	rec.Add(transcript.Run{})
	if !rec.Empty() || rec.ID() != "" || rec.Snapshot() != nil {
		t.Error("a nil recorder is not empty")
	}
	if transcript.FromContext(context.Background()) != nil {
		t.Error("a context without a recorder returned one")
	}
}

func TestDirStoreRoundTrips(t *testing.T) {
	ctx := context.Background()
	store, err := transcript.OpenStore(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := sampleRecord()
	if err := store.Put(ctx, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Ref != want.Ref || len(got.Runs) != 1 || len(got.Runs[0].Events) != len(want.Runs[0].Events) {
		t.Errorf("Get = %+v, want %+v", got, want)
	}

	if _, err := store.Get(ctx, transcript.NewID()); !errors.Is(err, transcript.ErrNotFound) {
		t.Errorf("Get of a missing record = %v, want ErrNotFound", err)
	}
	if _, err := store.Get(ctx, "../../etc/passwd"); !errors.Is(err, transcript.ErrNotFound) {
		t.Errorf("Get of a path = %v, want ErrNotFound", err)
	}
	if err := store.Put(ctx, &transcript.Record{ID: "../x"}); err == nil {
		t.Error("Put accepted an id that is a path")
	}
}

func sampleRecord() *transcript.Record {
	rec := transcript.NewRecorder(transcript.Record{
		EventID:    "github:d1",
		Platform:   "github",
		Repository: "yteraoka/kibitz",
		Ref:        "yteraoka/kibitz#42",
		URL:        "https://github.com/yteraoka/kibitz/pull/42",
		Kind:       "pr_opened",
	})
	rec.Add(transcript.Run{
		Mode:      "review",
		Engine:    "opencode",
		Model:     "google-vertex/gemini",
		StartedAt: time.Now(),
		Duration:  3 * time.Second,
		Prompt:    "review <this>",
		Output:    "looks fine",
		Events: []json.RawMessage{
			json.RawMessage(`{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"completed","title":"queue.go"}}}`),
			json.RawMessage(`{"type":"text","part":{"type":"text","text":"<script>alert(1)</script>"}}`),
			json.RawMessage(`{"type":"tool_execution_end","toolName":"bash","isError":true}`),
			json.RawMessage(`{"author":"kibitz","content":{"role":"model","parts":[{"functionCall":{"name":"get_doc"}},{"text":"adk says hi"}]}}`),
			json.RawMessage(`"a stray warning"`),
		},
	})
	return rec.Snapshot()
}

func serve(t *testing.T, h *transcript.Handler, path string, auth func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(transcript.RoutePattern, h)
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != nil {
		auth(req)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func TestHandlerRendersARecord(t *testing.T) {
	ctx := context.Background()
	store := &transcript.DirStore{Root: t.TempDir()}
	rec := sampleRecord()
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	h := &transcript.Handler{Store: store}

	w := serve(t, h, "/transcripts/"+rec.ID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body:\n%s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, want := range []string{"yteraoka/kibitz#42", "review / opencode / google-vertex/gemini", "read completed queue.go", "bash (error)", "functionCall get_doc", "adk says hi", "a stray warning", "review &lt;this&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
	// What the agent read is hostile: it is shown, never run.
	if strings.Contains(body, "<script>alert(1)") {
		t.Error("the page carries the agent's text unescaped")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Error("the page would leak its own address as a referrer")
	}

	w = serve(t, h, "/transcripts/"+rec.ID+"?format=json", nil)
	var got transcript.Record
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.ID != rec.ID {
		t.Errorf("the JSON download = %v, %s", err, w.Body)
	}
}

func TestHandlerRefusesUnknownAndMalformedIDs(t *testing.T) {
	h := &transcript.Handler{Store: &transcript.DirStore{Root: t.TempDir()}}
	for _, path := range []string{"/transcripts/" + transcript.NewID(), "/transcripts/not-an-id", "/transcripts/..%2f..%2fetc%2fpasswd"} {
		if w := serve(t, h, path, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, w.Code)
		}
	}
}

func TestHandlerAsksForATokenWhenConfigured(t *testing.T) {
	ctx := context.Background()
	store := &transcript.DirStore{Root: t.TempDir()}
	rec := sampleRecord()
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	h := &transcript.Handler{Store: store, Tokens: []string{"old", "new"}}
	path := "/transcripts/" + rec.ID

	if w := serve(t, h, path, nil); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("without a token: %d, %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}
	if w := serve(t, h, path, func(r *http.Request) { r.SetBasicAuth("me", "wrong") }); w.Code != http.StatusUnauthorized {
		t.Errorf("with a wrong token: %d", w.Code)
	}
	for _, token := range h.Tokens {
		if w := serve(t, h, path, func(r *http.Request) { r.SetBasicAuth("", token) }); w.Code != http.StatusOK {
			t.Errorf("with token %q: %d", token, w.Code)
		}
	}
}
