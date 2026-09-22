// Package forge abstracts the hosting platform. Reading a pull request and
// posting a review are the only things kibitz needs from GitHub, GitLab or
// Azure DevOps, and they are the only place their differences live.
//
// See docs/architecture.md.
package forge

import (
	"context"
	"errors"
	"fmt"

	"github.com/yteraoka/kibitz/internal/event"
)

// ErrNotSupported is returned by operations a platform does not offer.
var ErrNotSupported = errors.New("forge: operation is not supported on this platform")

// ErrInvalidPosition reports that a comment could not be anchored where it was
// asked to go. Forges reject the whole review over one bad position, so the
// caller falls back to posting the findings as text rather than losing them.
var ErrInvalidPosition = errors.New("forge: a comment position was rejected")

// PRRef identifies one pull request.
type PRRef struct {
	Platform event.Platform
	Owner    string
	Repo     string
	// Project is the Azure DevOps project; empty elsewhere.
	Project string
	Number  int
}

// FullName renders the "owner/name" form used in logs and state keys.
func (r PRRef) FullName() string { return r.Owner + "/" + r.Repo }

// String renders a reference for humans.
func (r PRRef) String() string { return fmt.Sprintf("%s#%d", r.FullName(), r.Number) }

// RefOf derives a reference from a normalized event.
func RefOf(ev *event.ReviewEvent) PRRef {
	ref := PRRef{
		Platform: ev.Source.Platform,
		Owner:    ev.Repository.Owner,
		Repo:     ev.Repository.Name,
		Project:  ev.Repository.Project,
	}
	if ev.PullRequest != nil {
		ref.Number = ev.PullRequest.Number
	}
	return ref
}

// HeadRef is the git ref that points at a pull request's head. Each platform
// publishes pull request heads under its own namespace, and none of them is
// reachable by branch name when the change comes from a fork.
func HeadRef(platform event.Platform, number int) string {
	switch platform {
	case event.PlatformGitLab:
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	case event.PlatformAzureDevOps:
		return fmt.Sprintf("refs/pull/%d/merge", number)
	default:
		return fmt.Sprintf("refs/pull/%d/head", number)
	}
}

// FileStatus describes what happened to a file in a pull request.
type FileStatus string

// File statuses.
const (
	FileAdded    FileStatus = "added"
	FileModified FileStatus = "modified"
	FileRemoved  FileStatus = "removed"
	FileRenamed  FileStatus = "renamed"
)

// File is one changed file.
type File struct {
	Path         string
	PreviousPath string
	Status       FileStatus
	Additions    int
	Deletions    int
	// Patch is the unified diff hunk set. It is empty for binary files and for
	// files the platform considers too large to diff.
	Patch string
}

// Diff is the set of changes in a pull request.
type Diff struct {
	Files []File
	// Truncated is set when the platform did not return every file.
	Truncated bool
}

// Lines counts the changed lines across all files.
func (d *Diff) Lines() int {
	n := 0
	for _, f := range d.Files {
		n += f.Additions + f.Deletions
	}
	return n
}

// Comment is an existing comment on a pull request.
type Comment struct {
	ID       string
	ThreadID string
	Body     string
	Author   event.Actor
	Path     string
	Line     int
}

// ErrFileNotFound reports that a repository has no such file. It is not a
// failure: a repository without a .kibitz.yaml is a repository that runs on
// the deployment's defaults.
var ErrFileNotFound = errors.New("forge: the file does not exist")

// ErrNoCompare reports that two commits could not be compared, usually
// because one of them no longer exists. It is not a failure: reviewing the
// whole diff instead is correct, only more expensive.
var ErrNoCompare = errors.New("forge: the commits could not be compared")

// InlineComment is one finding to post against a line of the diff.
type InlineComment struct {
	Path string
	Line int
	// EndLine makes the comment span a range. Zero means a single line.
	EndLine int
	Body    string
	// Suggestion, when set, is rendered as the platform's suggested change.
	Suggestion string
}

// Review is a batch of findings posted as one review.
type Review struct {
	Summary  string
	Comments []InlineComment
	// CommitSHA anchors the review to the commit that was examined.
	CommitSHA string
}

// CloneCredential authenticates a git clone. The token is short lived and must
// not be written to disk or into a remote URL.
type CloneCredential struct {
	Username string
	Token    string
	// AuthHeader is the value for git's http.extraHeader, which keeps the
	// credential out of the remote URL and therefore out of git's own error
	// messages and any log that captures them.
	AuthHeader string
}

// Client reads a pull request and posts back to it.
type Client interface {
	// Platform identifies the forge this client talks to.
	Platform() event.Platform

	// PullRequest fetches the current state of a pull request. Events carry a
	// snapshot, which is stale by the time a job runs and, for Azure DevOps,
	// not trustworthy in the first place.
	PullRequest(ctx context.Context, ref PRRef) (*event.PullRequest, error)

	// Diff returns the changed files.
	Diff(ctx context.Context, ref PRRef) (*Diff, error)

	// Compare returns what changed between two commits, which is how a second
	// review of the same pull request looks only at what is new. It returns
	// [ErrNoCompare] when the platform cannot answer — after a force push the
	// older commit may be gone — and the caller falls back to the whole diff.
	Compare(ctx context.Context, ref PRRef, base, head string) (*Diff, error)

	// Comments returns the existing comments, which is how kibitz avoids
	// repeating a finding someone has already made.
	Comments(ctx context.Context, ref PRRef) ([]Comment, error)

	// CreateReview posts findings. An empty comment list posts the summary
	// only.
	CreateReview(ctx context.Context, ref PRRef, r Review) error

	// UpsertSummary posts or updates the single comment identified by marker,
	// so that repeated reviews of one pull request replace their summary
	// instead of stacking up.
	UpsertSummary(ctx context.Context, ref PRRef, marker, body string) error

	// ReplyToThread answers an existing discussion.
	ReplyToThread(ctx context.Context, ref PRRef, threadID, body string) error

	// ReadFile returns the contents of a file on the repository's default
	// branch. It returns [ErrFileNotFound] when there is no such file, which
	// is the ordinary case for a repository that keeps no kibitz settings.
	//
	// The default branch, and not the pull request, is the point: settings
	// read from the branch under review would let anyone who can open a pull
	// request decide how it gets reviewed (docs/security.md).
	ReadFile(ctx context.Context, ref PRRef, path string) ([]byte, error)

	// CloneAuth returns a short-lived credential for fetching the code.
	CloneAuth(ctx context.Context, ref PRRef) (CloneCredential, error)
}
