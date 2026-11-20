package transcript

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxShownEventBytes bounds how much of one event the page prints. The JSON
// download has all of it; a page that tries to show a whole file read back
// through a tool is a page nobody can scroll.
const maxShownEventBytes = 256 << 10

// Handler serves records to people. It is mounted on kibitz-server at
// "GET /transcripts/{id}".
//
// The id is a capability: 128 random bits, handed out only in comments on
// the pull request the record is about, so whoever can read the comment can
// read the record. Where that is not enough — a public repository whose
// reviews read an internal tool through MCP, say — Tokens puts a password in
// front of it as well.
type Handler struct {
	Store  Store
	Logger *slog.Logger
	// Tokens, when any are set, are the passwords HTTP basic authentication
	// accepts. The user name is ignored. More than one allows a rotation.
	Tokens []string
}

// ServeHTTP implements [http.Handler].
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	// Everything on the page came from an agent reading somebody's code, so
	// it is treated as hostile: no script, nothing loaded from anywhere, not
	// framed, not indexed, not cached, and no referrer to leak the id.
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Robots-Tag", "noindex, nofollow")
	header.Set("Cache-Control", "private, no-store")

	if !h.authorized(r) {
		header.Set("WWW-Authenticate", `Basic realm="kibitz transcripts", charset="UTF-8"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	id, asJSON := strings.CutSuffix(r.PathValue("id"), ".json")
	asJSON = asJSON || r.URL.Query().Get("format") == "json"
	if !ValidID(id) {
		http.NotFound(w, r)
		return
	}

	rec, err := h.Store.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if h.Logger != nil {
			h.Logger.LogAttrs(r.Context(), slog.LevelError, "reading a transcript",
				slog.String("transcript_id", id),
				slog.String("error", err.Error()),
			)
		}
		http.Error(w, "the transcript could not be read", http.StatusBadGateway)
		return
	}

	if asJSON {
		header.Set("Content-Type", "application/json")
		header.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kibitz-transcript-%s.json"`, id))
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rec)
		return
	}

	header.Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Execute(w, viewOf(rec)); err != nil && h.Logger != nil {
		h.Logger.LogAttrs(r.Context(), slog.LevelWarn, "rendering a transcript",
			slog.String("transcript_id", id),
			slog.String("error", err.Error()),
		)
	}
}

func (h *Handler) authorized(r *http.Request) bool {
	if len(h.Tokens) == 0 {
		return true
	}
	_, password, ok := r.BasicAuth()
	if !ok {
		return false
	}
	matched := 0
	for _, token := range h.Tokens {
		matched |= subtle.ConstantTimeCompare([]byte(password), []byte(token))
	}
	return matched == 1
}

type recordView struct {
	*Record
	Runs []runView
}

type runView struct {
	Run
	Index    int
	Duration string
	Events   []eventView
}

type eventView struct {
	Index int
	Label string
	// Detail is what the event was about: a tool's name and status, usually.
	Detail string
	// Text is the prose the event carried, shown without opening it.
	Text string
	JSON string
}

func viewOf(rec *Record) recordView {
	v := recordView{Record: rec}
	for i, run := range rec.Runs {
		rv := runView{Run: run, Index: i + 1, Duration: run.Duration.Round(time.Second).String()}
		for j, raw := range run.Events {
			ev := describe(raw)
			ev.Index = j + 1
			rv.Events = append(rv.Events, ev)
		}
		v.Runs = append(v.Runs, rv)
	}
	return v
}

// describe reads enough of an event to label it. It knows the shapes the
// three engines emit — opencode's {"type","part":{…}}, pi's
// {"type","toolName","message"} and ADK's {"author","content":{"parts"}} —
// and labels anything else by its type alone. Nothing here can fail: an event
// it does not understand is still shown whole.
func describe(raw json.RawMessage) eventView {
	ev := eventView{JSON: pretty(raw)}

	var s string
	if json.Unmarshal(raw, &s) == nil {
		ev.Label = "output"
		ev.Text = s
		return ev
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		ev.Label = "event"
		return ev
	}

	ev.Label = str(m, "type")
	if part, ok := m["part"].(map[string]any); ok {
		// opencode.
		if ev.Label == "" {
			ev.Label = str(part, "type")
		}
		if tool := str(part, "tool"); tool != "" {
			state, _ := part["state"].(map[string]any)
			ev.Detail = strings.TrimSpace(tool + " " + str(state, "status") + " " + str(state, "title"))
		}
		if reason := str(part, "reason"); reason != "" && ev.Detail == "" {
			ev.Detail = reason
		}
		ev.Text = str(part, "text")
	}
	if tool := str(m, "toolName"); tool != "" {
		// pi.
		ev.Detail = tool
		if b, ok := m["isError"].(bool); ok && b {
			ev.Detail += " (error)"
		}
	}
	if msg, ok := m["message"].(map[string]any); ok {
		ev.Detail = strings.TrimSpace(ev.Detail + " " + str(msg, "role"))
		ev.Text = textOf(msg["content"])
	}
	if content, ok := m["content"].(map[string]any); ok {
		// ADK.
		if ev.Label == "" {
			ev.Label = str(m, "author")
		}
		var names []string
		if parts, ok := content["parts"].([]any); ok {
			for _, p := range parts {
				part, _ := p.(map[string]any)
				for _, key := range []string{"functionCall", "functionResponse"} {
					if call, ok := part[key].(map[string]any); ok {
						names = append(names, key+" "+str(call, "name"))
					}
				}
			}
		}
		ev.Detail = strings.Join(names, ", ")
		ev.Text = textOf(content["parts"])
	}
	if e := str(m, "errorMessage", "error_message"); e != "" {
		ev.Detail = strings.TrimSpace(ev.Detail + " error: " + e)
	}
	if ev.Label == "" {
		ev.Label = "event"
	}
	return ev
}

// textOf collects the text blocks of a message's content, which every engine
// writes as a list of {"type":"text","text":…} or {"text":…} parts. Thoughts
// are left to the JSON.
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := part["thought"].(bool); t {
				continue
			}
			if typ := str(part, "type"); typ != "" && typ != "text" {
				continue
			}
			if text := str(part, "text"); text != "" {
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

func str(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func pretty(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		b.Reset()
		b.Write(raw)
	}
	if b.Len() > maxShownEventBytes {
		return b.String()[:maxShownEventBytes] + "\n… (truncated; the JSON download has all of it)"
	}
	return b.String()
}

var page = template.Must(template.New("transcript").Parse(`<!doctype html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>kibitz transcript {{.ID}}</title>
<style>
:root { color-scheme: light dark; --fg:#1f2328; --muted:#656d76; --bg:#ffffff; --panel:#f6f8fa; --line:#d0d7de; --err:#cf222e; }
@media (prefers-color-scheme: dark) { :root { --fg:#e6edf3; --muted:#8d96a0; --bg:#0d1117; --panel:#161b22; --line:#30363d; --err:#f85149; } }
body { margin:0 auto; max-width:1100px; padding:16px; font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif; color:var(--fg); background:var(--bg); }
h1 { font-size:20px; margin:0 0 8px; } h2 { font-size:16px; margin:24px 0 8px; border-bottom:1px solid var(--line); padding-bottom:4px; }
dl { display:grid; grid-template-columns:max-content 1fr; gap:2px 12px; margin:0; } dt { color:var(--muted); } dd { margin:0; overflow-wrap:anywhere; }
details { border:1px solid var(--line); border-radius:6px; margin:6px 0; background:var(--panel); }
summary { cursor:pointer; padding:4px 8px; } summary .detail { color:var(--muted); }
pre { margin:0; padding:8px; overflow-x:auto; white-space:pre-wrap; word-break:break-word; font:12px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace; }
.text { white-space:pre-wrap; padding:4px 8px 8px; border-top:1px solid var(--line); }
.error { color:var(--err); } .muted { color:var(--muted); } a { color:inherit; }
</style>
</head>
<body>
<h1>kibitz transcript</h1>
<dl>
<dt>ID</dt><dd><code>{{.ID}}</code> (<a href="?format=json">JSON</a>)</dd>
{{with .Ref}}<dt>対象</dt><dd>{{if $.URL}}<a href="{{$.URL}}" rel="noreferrer noopener">{{.}}</a>{{else}}{{.}}{{end}}</dd>{{end}}
{{with .Platform}}<dt>プラットフォーム</dt><dd>{{.}}</dd>{{end}}
{{with .Kind}}<dt>契機</dt><dd>{{.}}</dd>{{end}}
{{with .EventID}}<dt>event_id</dt><dd><code>{{.}}</code></dd>{{end}}
<dt>作成</dt><dd>{{.CreatedAt.Format "2006-01-02 15:04:05 MST"}}</dd>
</dl>
{{range .Runs}}
<h2>{{.Index}}. {{.Mode}}{{with .Engine}} / {{.}}{{end}}{{with .Model}} / {{.}}{{end}}</h2>
<dl>
<dt>開始</dt><dd>{{.StartedAt.Format "15:04:05"}} (所要 {{.Duration}})</dd>
<dt>トークン</dt><dd>入力 {{.InputTokens}} / 出力 {{.OutputTokens}} / ツール呼び出し {{.ToolCalls}}</dd>
{{with .Error}}<dt>エラー</dt><dd class="error">{{.}}</dd>{{end}}
</dl>
{{with .Prompt}}<details><summary>プロンプト</summary><pre>{{.}}</pre></details>{{end}}
{{with .Output}}<details open><summary>出力</summary><div class="text">{{.}}</div></details>{{end}}
<p class="muted">イベント {{len .Events}} 件{{with .OmittedEvents}} (容量の上限により {{.}} 件を省略){{end}}</p>
{{range .Events}}<details><summary>#{{.Index}} {{.Label}}{{with .Detail}} <span class="detail">{{.}}</span>{{end}}</summary>{{with .Text}}<div class="text">{{.}}</div>{{end}}<pre>{{.JSON}}</pre></details>
{{end}}
{{else}}<p class="muted">記録された実行はありません。</p>
{{end}}
</body>
</html>
`))
