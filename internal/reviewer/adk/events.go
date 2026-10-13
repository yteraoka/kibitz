package adk

import (
	"slices"
	"strings"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

// transcript is what a run's events add up to.
//
// The other engines reconstruct this from a CLI's event stream, whose shape
// changes from release to release. Here the events are ADK's own types, so
// there is nothing to parse and nothing to guess: a tool's name is the name
// it was registered with, and the token counts are the provider's.
type transcript struct {
	usage reviewer.Usage
	tools reviewer.ToolUse
	// text is the model's last reply, without its thoughts.
	text string
	// modelCalls counts responses from the model.
	modelCalls int
	// finishReason is the last one the model gave.
	finishReason genai.FinishReason
	// errorCode and errorMessage are set when the last response was an
	// error rather than content.
	errorCode    string
	errorMessage string
}

func (t *transcript) record(ev *session.Event) {
	if ev == nil || ev.Partial {
		// Partial events repeat what the final one carries. Streaming is
		// off, so none should arrive, but counting one would double the
		// tokens.
		return
	}
	if ev.UsageMetadata != nil {
		t.modelCalls++
		t.usage = t.usage.Add(usageOf(ev.UsageMetadata))
	}
	if ev.ErrorCode != "" || ev.ErrorMessage != "" {
		t.errorCode, t.errorMessage = ev.ErrorCode, ev.ErrorMessage
	}
	if ev.FinishReason != "" {
		t.finishReason = ev.FinishReason
	}
	if ev.Content == nil {
		return
	}

	var text strings.Builder
	for _, part := range ev.Content.Parts {
		switch {
		case part == nil:
		case part.FunctionCall != nil:
			t.recordCall(part.FunctionCall)
		case part.FunctionResponse != nil:
			t.recordResponse(part.FunctionResponse)
		case part.Text != "" && !part.Thought:
			text.WriteString(part.Text)
		}
	}
	if ev.Content.Role == genai.RoleModel && text.Len() > 0 {
		t.text = text.String()
	}
}

// recordCall notes what a call asked for. Calls are counted when they come
// back, as the other engines count only finished ones, but the arguments are
// on the call.
func (t *transcript) recordCall(call *genai.FunctionCall) {
	switch {
	case reviewer.ToolMatches(call.Name, reviewer.GetDocTool):
		if path, _ := call.Args["path"].(string); path != "" && !slices.Contains(t.tools.Documents, path) {
			t.tools.Documents = append(t.tools.Documents, path)
		}
	case reviewer.ToolMatches(call.Name, reviewer.SearchDocsTool):
		t.tools.Searches++
	}
}

// recordResponse counts a finished call. ADK reports a tool that failed — an
// error from the tool, a tool that does not exist, an MCP result marked as an
// error — as a response with an "error" key, which is what makes it a failure
// here.
func (t *transcript) recordResponse(resp *genai.FunctionResponse) {
	if t.tools.Calls == nil {
		t.tools.Calls = map[string]int{}
	}
	t.tools.Calls[resp.Name]++
	if _, failed := resp.Response["error"]; failed {
		if t.tools.Failed == nil {
			t.tools.Failed = map[string]int{}
		}
		t.tools.Failed[resp.Name]++
	}
}

// usageOf translates one response's counts.
//
// Gemini's prompt count includes what the cache served, while kibitz's input
// count excludes it (the two are billed apart), so the cached part is taken
// out. Tool-use prompt tokens are input the model read too. Gemini reports no
// cache writes: its implicit cache is not billed for them.
//
// Thoughts are not part of the candidate count, so nothing is taken out of
// it: Gemini's total is prompt + candidates + tool-use prompt + thoughts.
// This is the opposite of pi, whose output count includes the reasoning.
func usageOf(u *genai.GenerateContentResponseUsageMetadata) reviewer.Usage {
	cached := int(u.CachedContentTokenCount)
	return reviewer.Usage{
		InputTokens:     max(int(u.PromptTokenCount)-cached, 0) + int(u.ToolUsePromptTokenCount),
		CacheReadTokens: cached,
		OutputTokens:    int(u.CandidatesTokenCount),
		ReasoningTokens: int(u.ThoughtsTokenCount),
	}
}
