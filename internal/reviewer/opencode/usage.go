package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/yteraoka/kibitz/internal/reviewer"
)

// recoverUsage counts the steps the event stream started and never finished.
//
// opencode 2's `run --format json` stops relaying events once the session
// goes idle, and the last step's step_finish -- the event that carries its
// tokens -- usually arrives after that and is dropped
// (https://github.com/anomalyco/opencode/issues/53207). A run that answers in
// one step would report nothing at all, and the budget would take it as free.
//
// The session's own record has every step, by the assistant message the
// stream named in step_start, so those are read back from
// `opencode session export` and nothing else is: the session may hold earlier
// runs' steps, which were counted when they ran.
//
// Failing to read them is logged and otherwise ignored. A review that worked
// is not thrown away over its token count.
func (r *Runner) recoverUsage(ctx context.Context, configPath string, req reviewer.Request, env []string, t *transcript) {
	if len(t.unfinished) == 0 || t.sessionID == "" {
		return
	}
	usage, found, err := r.exportUsage(ctx, configPath, req, env, t.sessionID, t.unfinished)
	if err != nil {
		attrs := []slog.Attr{
			slog.String("session_id", t.sessionID),
			slog.Int("unfinished_steps", len(t.unfinished)),
			slog.String("error", err.Error()),
		}
		if req.Event != nil {
			attrs = append([]slog.Attr{slog.String("event_id", req.Event.ID)}, attrs...)
		}
		r.logger.LogAttrs(ctx, slog.LevelWarn, "the usage of the steps the agent did not report is unknown", attrs...)
		return
	}
	t.usage = t.usage.Add(usage)
	t.steps += found
}

// exportUsage reads the tokens of the given assistant messages from the
// session's export, and how many of them it found.
func (r *Runner) exportUsage(ctx context.Context, configPath string, req reviewer.Request, env []string, sessionID string, messages []string) (reviewer.Usage, int, error) {
	cmd := r.command(ctx, configPath, req, env, []string{"session", "export", "--standalone", sessionID})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return reviewer.Usage{}, 0, fmt.Errorf("opencode session export: %w: %s", err, tail(stderr.String(), 500))
	}
	return usageOfMessages(stdout, messages)
}

// usageOfMessages sums the tokens of the given assistant messages in a
// session export:
//
//	{"info": {…}, "messages": [
//	  {"id": "msg_…", "type": "assistant", "finish": "stop",
//	   "tokens": {"input": 1, "output": 1, "reasoning": 0,
//	              "cache": {"read": 0, "write": 0}}}, …]}
func usageOfMessages(export []byte, messages []string) (reviewer.Usage, int, error) {
	var session struct {
		Messages []struct {
			ID     string `json:"id"`
			Type   string `json:"type"`
			Tokens *struct {
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
				Cache     struct {
					Read  int `json:"read"`
					Write int `json:"write"`
				} `json:"cache"`
			} `json:"tokens"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(export, &session); err != nil {
		return reviewer.Usage{}, 0, fmt.Errorf("reading the session export: %w", err)
	}

	var usage reviewer.Usage
	found := 0
	for _, m := range session.Messages {
		if m.Type != "assistant" || m.Tokens == nil || !slices.Contains(messages, m.ID) {
			continue
		}
		found++
		usage = usage.Add(reviewer.Usage{
			InputTokens:      m.Tokens.Input,
			OutputTokens:     m.Tokens.Output,
			ReasoningTokens:  m.Tokens.Reasoning,
			CacheReadTokens:  m.Tokens.Cache.Read,
			CacheWriteTokens: m.Tokens.Cache.Write,
		})
	}
	if found == 0 {
		return reviewer.Usage{}, 0, errors.New("the export has none of the steps")
	}
	return usage, found, nil
}
