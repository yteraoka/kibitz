package gitlab

import (
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
)

// The structs below cover only the fields kibitz reads. GitLab payloads are
// large and grow over time; decoding a subset keeps normalization stable when
// fields kibitz does not use are added or changed.

// gitlabTime parses the two shapes GitLab uses in one payload: "2026-01-16
// 05:56:22 UTC" for record timestamps and RFC3339 for commit timestamps.
type gitlabTime struct{ time.Time }

func (t *gitlabTime) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700"} {
		if parsed, err := time.Parse(layout, s); err == nil {
			t.Time = parsed
			return nil
		}
	}
	// A timestamp kibitz cannot read is not worth failing a delivery over:
	// the caller falls back to the time of receipt.
	return nil
}

type user struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
}

func (u user) normalize() event.Actor {
	a := event.Actor{Login: u.Username}
	if u.ID != 0 {
		a.ID = strconv.FormatInt(u.ID, 10)
	}
	// GitLab names its own service accounts, and a bot's username ends in the
	// same suffix a human's never does.
	a.IsBot = strings.HasSuffix(u.Username, "_bot") || strings.HasPrefix(u.Username, "project_")
	return a
}

type project struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	GitHTTPURL        string `json:"git_http_url"`
	DefaultBranch     string `json:"default_branch"`
	VisibilityLevel   int    `json:"visibility_level"`
}

func (p project) normalize() event.Repository {
	repo := event.Repository{
		Name:          p.Name,
		FullName:      p.PathWithNamespace,
		CloneURL:      p.GitHTTPURL,
		DefaultBranch: p.DefaultBranch,
		Visibility:    visibility(p.VisibilityLevel),
	}
	if p.ID != 0 {
		repo.ID = strconv.FormatInt(p.ID, 10)
	}
	// GitLab's namespace can be nested ("group/subgroup/project"), and what
	// the API needs is the whole path before the last segment.
	if idx := strings.LastIndex(p.PathWithNamespace, "/"); idx > 0 {
		repo.Owner = p.PathWithNamespace[:idx]
		if repo.Name == "" {
			repo.Name = p.PathWithNamespace[idx+1:]
		}
	}
	return repo
}

// visibility translates GitLab's numeric levels.
func visibility(level int) string {
	switch level {
	case 0:
		return "private"
	case 10:
		return "internal"
	case 20:
		return "public"
	default:
		return ""
	}
}

type commit struct {
	ID string `json:"id"`
}

// mergeRequest is the object_attributes of a merge request event, and also
// the merge_request member of a note event. The two overlap in everything
// kibitz reads.
type mergeRequest struct {
	ID              int64      `json:"id"`
	IID             int        `json:"iid"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	State           string     `json:"state"`
	Draft           bool       `json:"draft"`
	WorkInProgress  bool       `json:"work_in_progress"`
	SourceBranch    string     `json:"source_branch"`
	TargetBranch    string     `json:"target_branch"`
	SourceProjectID int64      `json:"source_project_id"`
	TargetProjectID int64      `json:"target_project_id"`
	AuthorID        int64      `json:"author_id"`
	URL             string     `json:"url"`
	Action          string     `json:"action"`
	OldRev          string     `json:"oldrev"`
	LastCommit      commit     `json:"last_commit"`
	CreatedAt       gitlabTime `json:"created_at"`
	UpdatedAt       gitlabTime `json:"updated_at"`
	Source          project    `json:"source"`
	Target          project    `json:"target"`
}

func (m mergeRequest) normalize() *event.PullRequest {
	pr := &event.PullRequest{
		Number:      m.IID,
		Title:       m.Title,
		Description: m.Description,
		State:       state(m.State),
		Draft:       m.Draft || m.WorkInProgress,
		Merged:      m.State == "merged",
		URL:         m.URL,
		// A merge request opened from a fork lives in a different project,
		// and runs in the reduced mode (docs/security.md).
		IsFork: m.SourceProjectID != 0 && m.TargetProjectID != 0 && m.SourceProjectID != m.TargetProjectID,
		Source: event.Ref{Branch: m.SourceBranch, SHA: m.LastCommit.ID, RepoFullName: m.Source.PathWithNamespace},
		Target: event.Ref{Branch: m.TargetBranch, RepoFullName: m.Target.PathWithNamespace},
	}
	if m.ID != 0 {
		pr.ID = strconv.FormatInt(m.ID, 10)
	}
	// The payload names the author by id only. The worker fetches the merge
	// request before reviewing it, which is where the name comes from.
	if m.AuthorID != 0 {
		pr.Author = event.Actor{ID: strconv.FormatInt(m.AuthorID, 10)}
	}
	return pr
}

// state translates GitLab's vocabulary into the one every consumer uses.
func state(s string) string {
	switch s {
	case "opened", "locked":
		return "open"
	case "closed":
		return "closed"
	case "merged":
		return "merged"
	default:
		return s
	}
}

// changes is the set of attributes an update touched. It is how a plain edit
// is told apart from a draft being marked ready or a reviewer being added.
type changes struct {
	Draft     *change[bool]   `json:"draft"`
	Reviewers *change[[]any]  `json:"reviewers"`
	Title     *change[string] `json:"title"`
}

type change[T any] struct {
	Previous T `json:"previous"`
	Current  T `json:"current"`
}

type mergeRequestPayload struct {
	ObjectKind       string       `json:"object_kind"`
	EventType        string       `json:"event_type"`
	User             user         `json:"user"`
	Project          project      `json:"project"`
	ObjectAttributes mergeRequest `json:"object_attributes"`
	Changes          changes      `json:"changes"`
}

// position is where a comment sits in the diff. It is absent on a comment made
// on the merge request itself.
type position struct {
	NewPath string `json:"new_path"`
	OldPath string `json:"old_path"`
	NewLine int    `json:"new_line"`
	OldLine int    `json:"old_line"`
	HeadSHA string `json:"head_sha"`
}

type note struct {
	ID   int64  `json:"id"`
	Note string `json:"note"`
	// The tags match GitLab's own spelling of the field names.
	NoteableType string     `json:"noteable_type"` //nolint:misspell // GitLab's own spelling
	NoteableID   int64      `json:"noteable_id"`   //nolint:misspell // GitLab's own spelling
	Action       string     `json:"action"`
	System       bool       `json:"system"`
	URL          string     `json:"url"`
	DiscussionID string     `json:"discussion_id"`
	CreatedAt    gitlabTime `json:"created_at"`
	Position     *position  `json:"position"`
	InReplyToID  int64      `json:"in_reply_to_id"`
}

type notePayload struct {
	ObjectKind       string       `json:"object_kind"`
	EventType        string       `json:"event_type"`
	User             user         `json:"user"`
	Project          project      `json:"project"`
	ObjectAttributes note         `json:"object_attributes"`
	MergeRequest     mergeRequest `json:"merge_request"`
}
