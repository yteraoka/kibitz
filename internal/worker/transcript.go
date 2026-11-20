package worker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/transcript"
)

// startTranscript gives the job a record to collect its engine runs in, when
// the deployment keeps transcripts at all.
func (j *ReviewJob) startTranscript(ctx context.Context, ev *event.ReviewEvent) context.Context {
	if j.Transcripts == nil {
		return ctx
	}
	rec := transcript.Record{
		EventID:    ev.ID,
		Platform:   string(ev.Source.Platform),
		Repository: ev.Repository.FullName,
		Kind:       string(ev.Kind),
	}
	switch {
	case ev.PullRequest != nil:
		rec.Ref = forge.RefOf(ev).String()
		rec.URL = ev.PullRequest.URL
	case ev.Issue != nil:
		if ref, ok := forge.IssueRefOf(ev); ok {
			rec.Ref = ref.String()
		}
		rec.URL = ev.Issue.URL
	}
	return transcript.NewContext(ctx, transcript.NewRecorder(rec))
}

// runEngine runs the engine and records the run in the job's transcript.
// Every engine run a job makes goes through here, so that the record has the
// triage pass and the retry as well as the run whose output was posted.
func (j *ReviewJob) runEngine(ctx context.Context, engine reviewer.Engine, engineName string, req reviewer.Request) (*reviewer.Result, error) {
	rec := transcript.FromContext(ctx)
	if rec == nil {
		return engine.Run(ctx, req)
	}

	started := time.Now()
	result, err := engine.Run(ctx, req)
	run := transcript.Run{
		Mode:      string(req.Mode),
		Engine:    engineName,
		Model:     req.Model,
		StartedAt: started.UTC(),
		Duration:  time.Since(started),
		// Built again rather than handed back by the engine: every engine
		// builds it with this one function, from this one request.
		Prompt: reviewer.BuildPrompt(req),
		Events: reviewer.EventsOf(err),
	}
	if err != nil {
		run.Error = err.Error()
	}
	if result != nil {
		run.Events = result.Events
		run.InputTokens = result.Usage.Input()
		run.OutputTokens = result.Usage.Output()
		run.ToolCalls = result.Tools.Total()
		switch {
		case result.Reply != "":
			run.Output = result.Reply
		case result.Summary != "":
			run.Output = result.Summary
		case result.Triage != nil:
			run.Output = result.Triage.Notes
		}
	}
	rec.Add(run)
	return result, err
}

// saveTranscript writes the job's record and returns its id, or "" when there
// is nothing to save or it could not be saved. A transcript that failed to
// save costs the link, never the comment it would have been linked from.
func (j *ReviewJob) saveTranscript(ctx context.Context) string {
	rec := transcript.FromContext(ctx)
	if j.Transcripts == nil || rec.Empty() {
		return ""
	}
	snapshot := rec.Snapshot()
	if err := j.Transcripts.Put(context.WithoutCancel(ctx), snapshot); err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not save the transcript",
			slog.String("transcript_id", snapshot.ID),
			slog.String("error", err.Error()),
		)
		return ""
	}
	j.Logger.LogAttrs(ctx, slog.LevelInfo, "saved the transcript",
		slog.String("transcript_id", snapshot.ID),
		slog.Int("runs", len(snapshot.Runs)),
	)
	return snapshot.ID
}

// transcriptTrailer saves the job's record and returns the line that names
// it, ready to append to a comment: empty when there is none.
func (j *ReviewJob) transcriptTrailer(ctx context.Context) string {
	return j.trailerFor(j.saveTranscript(ctx))
}

func (j *ReviewJob) trailerFor(id string) string {
	if id == "" {
		return ""
	}
	return "\n" + transcript.Trailer(j.TranscriptBaseURL, id) + "\n"
}

// transcriptFailure carries the id of a failed job's transcript to the
// failure notice, which is written by the guard after the job has returned.
type transcriptFailure struct {
	err error
	id  string
}

func (e *transcriptFailure) Error() string { return e.err.Error() }
func (e *transcriptFailure) Unwrap() error { return e.err }

// withTranscript saves the transcript of a job that failed and attaches its
// id to the error. The run that failed is the one somebody will want to read.
func (j *ReviewJob) withTranscript(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if _, retry := RetryDelay(err); retry {
		// Stepping aside for another worker ran nothing worth keeping.
		return err
	}
	id := j.saveTranscript(ctx)
	if id == "" {
		return err
	}
	return &transcriptFailure{err: err, id: id}
}

// transcriptOf returns the transcript id a failure carries.
func transcriptOf(err error) string {
	var t *transcriptFailure
	if errors.As(err, &t) {
		return t.id
	}
	return ""
}
