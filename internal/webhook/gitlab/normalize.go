package gitlab

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
)

// Event names GitLab sends in the X-Gitlab-Event header.
const (
	EventMergeRequest = "Merge Request Hook"
	EventNote         = "Note Hook"
)

// Normalize implements [webhook.Handler].
//
// It returns (nil, nil) for deliveries that are well formed but not
// interesting: a ping, an approval, a comment on an issue, a note GitLab wrote
// itself.
func (h *Handler) Normalize(r *http.Request, body []byte) (*event.ReviewEvent, error) {
	switch r.Header.Get(HeaderEvent) {
	case EventMergeRequest:
		return h.normalizeMergeRequest(r, body)
	case EventNote:
		return h.normalizeNote(r, body)
	default:
		return nil, nil
	}
}

func (h *Handler) normalizeMergeRequest(r *http.Request, body []byte) (*event.ReviewEvent, error) {
	var p mergeRequestPayload
	if err := unmarshal(body, &p); err != nil {
		return nil, err
	}

	kind, ok := mergeRequestKind(p.ObjectAttributes, p.Changes)
	if !ok {
		return nil, nil
	}

	ev := h.newEvent(r, p.ObjectKind, p.ObjectAttributes.Action, p.Project, p.User)
	ev.Kind = kind
	ev.PullRequest = p.ObjectAttributes.normalize()
	ev.OccurredAt = firstNonZero(p.ObjectAttributes.UpdatedAt.Time, p.ObjectAttributes.CreatedAt.Time, h.now())
	return ev, ev.Validate()
}

// mergeRequestKind maps GitLab's action onto what the worker does.
//
// "update" is the awkward one: GitLab uses it for a push, an edited
// description, a label, a reviewer and a draft being marked ready alike. The
// action alone cannot tell them apart, so the changed attributes decide.
func mergeRequestKind(mr mergeRequest, ch changes) (event.Kind, bool) {
	switch mr.Action {
	case "open", "reopen":
		return event.KindPROpened, true
	case "merge":
		return event.KindPRMerged, true
	case "close":
		return event.KindPRClosed, true
	case "update":
		switch {
		case mr.OldRev != "":
			// New commits on the source branch: GitHub's "synchronize".
			return event.KindPRUpdated, true
		case ch.Draft != nil && ch.Draft.Previous && !ch.Draft.Current:
			return event.KindPRReadyForReview, true
		case ch.Reviewers != nil && len(ch.Reviewers.Current) > len(ch.Reviewers.Previous):
			return event.KindPRReviewRequested, true
		default:
			// An edited title, a label, a milestone. Nothing to review.
			return "", false
		}
	default:
		// approval, approved, unapproval, unapproved and whatever GitLab adds
		// next.
		return "", false
	}
}

func (h *Handler) normalizeNote(r *http.Request, body []byte) (*event.ReviewEvent, error) {
	var p notePayload
	if err := unmarshal(body, &p); err != nil {
		return nil, err
	}

	// The same event fires for issues, commits and snippets.
	if p.ObjectAttributes.NoteableType != "MergeRequest" {
		return nil, nil
	}
	// A note GitLab wrote itself ("changed the description") is not somebody
	// asking for anything.
	if p.ObjectAttributes.System {
		return nil, nil
	}
	if action := p.ObjectAttributes.Action; action != "" && action != "create" {
		return nil, nil
	}

	ev := h.newEvent(r, p.ObjectKind, p.ObjectAttributes.Action, p.Project, p.User)
	ev.Kind = event.KindCommentCreated
	ev.PullRequest = p.MergeRequest.normalize()
	ev.Comment = &event.Comment{
		ID:     strconv.FormatInt(p.ObjectAttributes.ID, 10),
		Body:   p.ObjectAttributes.Note,
		Author: p.User.normalize(),
		URL:    p.ObjectAttributes.URL,
		// GitLab calls a thread a discussion, and every note belongs to one.
		ThreadID: p.ObjectAttributes.DiscussionID,
	}
	if pos := p.ObjectAttributes.Position; pos != nil {
		ev.Comment.Path = firstNonEmpty(pos.NewPath, pos.OldPath)
		ev.Comment.Line = firstNonZeroInt(pos.NewLine, pos.OldLine)
	}
	if p.ObjectAttributes.InReplyToID != 0 {
		ev.Comment.InReplyTo = strconv.FormatInt(p.ObjectAttributes.InReplyToID, 10)
	}
	if ev.Comment.ThreadID == "" {
		ev.Comment.ThreadID = ev.Comment.ID
	}
	ev.OccurredAt = firstNonZero(p.ObjectAttributes.CreatedAt.Time, h.now())
	return ev, ev.Validate()
}

// newEvent fills in what every GitLab delivery has in common.
func (h *Handler) newEvent(r *http.Request, kind, action string, p project, u user) *event.ReviewEvent {
	delivery := deliveryID(r)

	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		// The id is derived from the delivery, so a redelivery of the same
		// webhook carries the same event id.
		ID: string(event.PlatformGitLab) + ":" + delivery,
		Source: event.Source{
			Platform:    event.PlatformGitLab,
			InstanceURL: instanceURL(r, p.WebURL),
			DeliveryID:  delivery,
			EventName:   eventName(kind, action),
		},
		Repository: p.normalize(),
		Actor:      u.normalize(),
	}
}

// eventName records GitLab's own vocabulary ("merge_request.update"), which is
// what the project's webhook settings page shows.
func eventName(kind, action string) string {
	if action == "" {
		return kind
	}
	return kind + "." + action
}

func unmarshal(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return &webhook.ErrMalformedPayload{Platform: event.PlatformGitLab, Err: err}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZeroInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// firstNonZero returns the first timestamp that was actually set.
func firstNonZero(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
