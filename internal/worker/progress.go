package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/policy"
)

// A review takes minutes, and until now nothing on the pull request said one
// was happening: the first sign of kibitz was the finished review, and the
// only sign of a failure was a comment an hour later. So a review now says
// when it starts, in the same comment it will finish in.
//
// That comment is the summary. It is found by its marker and replaced, which
// is what the summary already did, so the pull request gains no comment it
// did not have: it reads "reviewing" while the review runs, becomes the
// summary when it is done, and becomes the failure if kibitz gives up. A
// separate status comment would have been one more comment on every pull
// request, to say something the summary can say in place.

// isReview reports whether an event asks for a review, as opposed to an
// answer, a command that is not a review, or anything else. Only a review
// announces itself: an answer is a reply in a thread, which cannot be edited
// into its final form, and it is acknowledged when it arrives instead.
func isReview(ev *event.ReviewEvent) bool {
	switch ev.Kind {
	case event.KindPROpened, event.KindPRUpdated, event.KindPRReadyForReview, event.KindPRReviewRequested:
		return true
	case event.KindCommand:
		return ev.Command != nil && ev.Command.Name == policy.CommandReview
	default:
		return false
	}
}

type attemptKey struct{}

// withAttempt records which delivery of the message this is, so that a review
// being retried can say so.
func withAttempt(ctx context.Context, deliveries int) context.Context {
	return context.WithValue(ctx, attemptKey{}, deliveries)
}

func attemptOf(ctx context.Context) int {
	n, _ := ctx.Value(attemptKey{}).(int)
	return n
}

// scope is what a starting review is about to look at.
type scope struct {
	head  string
	since string
	files int
}

// announce says on the pull request that a review has started.
//
// It is called once every reason not to review has been ruled out -- closed,
// draft, turned off, out of budget, already reviewed, nothing changed -- and
// before anything expensive. Earlier would announce reviews that then quietly
// do not happen; later would leave the pull request silent through the part
// that takes the time.
//
// It does not count towards the posting limit. The limit exists to stop a
// loop filling a thread, and this is always the same comment, edited in
// place; it cannot fill anything.
//
// Failing to say so is not a reason to fail the review, which is what the
// person is waiting for.
func (j *ReviewJob) announce(ctx context.Context, client forge.Client, ref forge.PRRef, s scope) {
	if err := client.UpsertSummary(ctx, ref, SummaryMarker, j.startedBody(ctx, s)); err != nil {
		j.Logger.LogAttrs(ctx, slog.LevelWarn, "could not say that the review started",
			slog.String("ref", ref.String()),
			slog.String("error", err.Error()),
		)
	}
}

func (j *ReviewJob) startedBody(ctx context.Context, s scope) string {
	var b strings.Builder
	b.WriteString("## 🔄 レビュー中\n\n")
	if s.head != "" {
		fmt.Fprintf(&b, "`%s` をレビューしています", shortSHA(s.head))
	} else {
		b.WriteString("レビューしています")
	}
	if s.since != "" {
		fmt.Fprintf(&b, " (前回レビューした `%s` からの変更)", shortSHA(s.since))
	}
	b.WriteString("。\n\n")
	fmt.Fprintf(&b, "- 対象ファイル: %d 件\n", s.files)
	if n := attemptOf(ctx); n > 1 {
		// Said because a retry is the one case where somebody may already
		// have been waiting a while, and deserves to know why.
		fmt.Fprintf(&b, "- 再試行: %d 回目 (前回の試行は失敗しました)\n", n)
	}
	b.WriteString("\n完了すると、このコメントがレビュー結果に置き換わります。\n")
	return b.String()
}

// failedBody is what the summary comment becomes when kibitz gives up on a
// review. Without it the comment would say "reviewing" forever.
func (j *ReviewJob) failedBody(ev *event.ReviewEvent, cause error) string {
	var b strings.Builder
	b.WriteString("## ❌ レビューに失敗しました\n\n")
	b.WriteString("```\n")
	b.WriteString(oneLine(cause.Error(), 500))
	b.WriteString("\n```\n\n")
	fmt.Fprintf(&b, "`%s review` で再実行できます。\n", j.mention())
	fmt.Fprintf(&b, "\n<sub>運用者向け: ワーカーのログを `event_id=%s` で検索すると詳細が見られます。</sub>\n", ev.ID)
	return b.String()
}
