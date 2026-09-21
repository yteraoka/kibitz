// Package event defines the normalized event that travels on the queue. It is
// the only schema the worker understands: platform-specific vocabulary is
// translated away by the webhook handlers, so GitHub pull requests, GitLab
// merge requests and Azure DevOps pull requests all arrive here in one shape.
//
// See docs/event-schema.md.
package event

import (
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the current major version of [ReviewEvent]. A consumer that
// receives a version it does not know must not retry the message forever; it
// sends it straight to the dead letter queue.
const SchemaVersion = 1

// Platform identifies the forge an event came from.
type Platform string

// Supported platforms.
const (
	PlatformGitHub      Platform = "github"
	PlatformGitLab      Platform = "gitlab"
	PlatformAzureDevOps Platform = "azure_devops"
)

// Kind is what happened, in terms the worker acts on.
type Kind string

// Event kinds. See the table in docs/event-schema.md for what each one makes
// the worker do.
const (
	KindPROpened          Kind = "pr.opened"
	KindPRUpdated         Kind = "pr.updated"
	KindPRReadyForReview  Kind = "pr.ready_for_review"
	KindPRReviewRequested Kind = "pr.review_requested"
	KindPRClosed          Kind = "pr.closed"
	KindPRMerged          Kind = "pr.merged"
	KindCommentCreated    Kind = "comment.created"
	KindCommand           Kind = "command"
)

// Known reports whether k is a kind this version understands.
func (k Kind) Known() bool {
	switch k {
	case KindPROpened, KindPRUpdated, KindPRReadyForReview, KindPRReviewRequested,
		KindPRClosed, KindPRMerged, KindCommentCreated, KindCommand:
		return true
	default:
		return false
	}
}

// Source describes where the event came from.
type Source struct {
	Platform Platform `json:"platform"`
	// InstanceURL distinguishes github.com from a GitHub Enterprise Server, or
	// gitlab.com from a self-managed install.
	InstanceURL string `json:"instance_url,omitempty"`
	// DeliveryID is the forge's own id for this delivery. It is the key used
	// to suppress duplicate processing.
	DeliveryID string `json:"delivery_id"`
	// EventName is the platform-specific name, kept for debugging.
	EventName string `json:"event_name,omitempty"`
}

// Actor is a user or bot account.
type Actor struct {
	ID    string `json:"id,omitempty"`
	Login string `json:"login"`
	IsBot bool   `json:"is_bot,omitempty"`
	// AppID identifies the forge app that acted, when the payload says so.
	// It is how kibitz recognizes its own writing without being told what it
	// is called: an app id cannot be renamed, and the account name is derived
	// from a slug that can.
	AppID string `json:"app_id,omitempty"`
	// AppSlug is that app's short name, which is where the "<slug>[bot]"
	// account name comes from. It is carried so the worker's own check has
	// something to match on, and so the name can be logged once instead of
	// configured.
	AppSlug string `json:"app_slug,omitempty"`
}

// Repository identifies the repository the event concerns.
type Repository struct {
	ID       string `json:"id,omitempty"`
	Owner    string `json:"owner"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url,omitempty"`
	// Project is the Azure DevOps project, empty elsewhere.
	Project       string `json:"project,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Visibility    string `json:"visibility,omitempty"`
}

// Ref is one side of a pull request.
type Ref struct {
	Branch       string `json:"branch"`
	SHA          string `json:"sha"`
	RepoFullName string `json:"repo_full_name,omitempty"`
}

// PullRequest is a pull request, merge request or Azure DevOps pull request.
type PullRequest struct {
	ID          string `json:"id,omitempty"`
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	State       string `json:"state,omitempty"`
	Draft       bool   `json:"draft,omitempty"`
	Merged      bool   `json:"merged,omitempty"`
	URL         string `json:"url,omitempty"`
	Author      Actor  `json:"author"`
	Source      Ref    `json:"source"`
	Target      Ref    `json:"target"`
	// IsFork marks a pull request opened from a fork. Those run in a reduced
	// mode: no secrets, no third-party MCP servers (see docs/security.md).
	IsFork       bool `json:"is_fork,omitempty"`
	ChangedFiles int  `json:"changed_files,omitempty"`
}

// Comment is a comment on a pull request, either on the conversation or on a
// line of the diff.
type Comment struct {
	ID        string `json:"id"`
	ThreadID  string `json:"thread_id,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	Body      string `json:"body"`
	Author    Actor  `json:"author"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	URL       string `json:"url,omitempty"`
}

// Command is an explicit instruction addressed to kibitz in a comment.
type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
}

// PayloadRef points at the raw webhook payload in the blob store, when it was
// too large to travel inside the message (the claim-check pattern).
type PayloadRef struct {
	URI    string `json:"uri"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// Trace carries distributed tracing context from the webhook to the worker.
type Trace struct {
	TraceParent string `json:"traceparent,omitempty"`
	TraceState  string `json:"tracestate,omitempty"`
}

// ReviewEvent is the normalized event published to the queue.
type ReviewEvent struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	OccurredAt    time.Time    `json:"occurred_at"`
	Source        Source       `json:"source"`
	Kind          Kind         `json:"kind"`
	Repository    Repository   `json:"repository"`
	PullRequest   *PullRequest `json:"pull_request,omitempty"`
	Comment       *Comment     `json:"comment,omitempty"`
	Command       *Command     `json:"command,omitempty"`
	Actor         Actor        `json:"actor"`
	PayloadRef    *PayloadRef  `json:"payload_ref,omitempty"`
	Trace         *Trace       `json:"trace,omitempty"`
}

// Key identifies the pull request an event belongs to. It is used as the
// ordering key on Pub/Sub, the message group on SQS and the lock key in the
// state store, so that two events for the same pull request never run at once.
func (e *ReviewEvent) Key() string {
	if e == nil {
		return ""
	}
	if e.PullRequest == nil {
		return fmt.Sprintf("%s/%s", e.Source.Platform, e.Repository.FullName)
	}
	return fmt.Sprintf("%s/%s/%d", e.Source.Platform, e.Repository.FullName, e.PullRequest.Number)
}

// Validate reports whether the event carries the fields every consumer relies
// on. Handlers call it so that a normalization bug is caught at the source
// rather than in the worker.
func (e *ReviewEvent) Validate() error {
	var problems []string

	switch {
	case e == nil:
		return fmt.Errorf("event is nil")
	case e.SchemaVersion != SchemaVersion:
		problems = append(problems, fmt.Sprintf("schema_version is %d, want %d", e.SchemaVersion, SchemaVersion))
	}
	if e.ID == "" {
		problems = append(problems, "id is empty")
	}
	if e.Source.Platform == "" {
		problems = append(problems, "source.platform is empty")
	}
	if e.Source.DeliveryID == "" {
		problems = append(problems, "source.delivery_id is empty")
	}
	if !e.Kind.Known() {
		problems = append(problems, fmt.Sprintf("kind %q is unknown", e.Kind))
	}
	if e.Repository.FullName == "" {
		problems = append(problems, "repository.full_name is empty")
	}
	if e.OccurredAt.IsZero() {
		problems = append(problems, "occurred_at is zero")
	}

	// Every kind except the repository-level ones needs a pull request, and a
	// comment kind needs the comment itself.
	if e.PullRequest == nil {
		problems = append(problems, "pull_request is missing")
	} else if e.PullRequest.Number <= 0 {
		problems = append(problems, "pull_request.number is not positive")
	}
	if (e.Kind == KindCommentCreated || e.Kind == KindCommand) && e.Comment == nil {
		problems = append(problems, fmt.Sprintf("comment is missing for kind %q", e.Kind))
	}
	if e.Kind == KindCommand && (e.Command == nil || e.Command.Name == "") {
		problems = append(problems, "command is missing for kind command")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid review event: %s", strings.Join(problems, "; "))
	}
	return nil
}
