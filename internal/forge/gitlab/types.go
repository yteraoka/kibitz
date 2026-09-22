package gitlab

import (
	"strconv"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// The structs below cover only the fields kibitz reads.

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

func (u apiUser) normalize() event.Actor {
	a := event.Actor{Login: u.Username}
	if u.ID != 0 {
		a.ID = strconv.FormatInt(u.ID, 10)
	}
	a.IsBot = strings.HasSuffix(u.Username, "_bot") || strings.HasPrefix(u.Username, "project_")
	return a
}

// diffRefs are the three commits a diff note has to be anchored to. GitLab
// populates them asynchronously, so they can be empty on a merge request that
// was created a moment ago.
type diffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

func (d diffRefs) complete() bool {
	return d.BaseSHA != "" && d.HeadSHA != "" && d.StartSHA != ""
}

type apiMergeRequest struct {
	ID           int64    `json:"id"`
	IID          int      `json:"iid"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	State        string   `json:"state"`
	Draft        bool     `json:"draft"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	WebURL       string   `json:"web_url"`
	SHA          string   `json:"sha"`
	Author       apiUser  `json:"author"`
	DiffRefs     diffRefs `json:"diff_refs"`

	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	ChangesCount    string `json:"changes_count"`
}

func (m apiMergeRequest) normalize() *event.PullRequest {
	pr := &event.PullRequest{
		Number:      m.IID,
		Title:       m.Title,
		Description: m.Description,
		State:       state(m.State),
		Draft:       m.Draft,
		Merged:      m.State == "merged",
		URL:         m.WebURL,
		Author:      m.Author.normalize(),
		IsFork:      m.SourceProjectID != 0 && m.TargetProjectID != 0 && m.SourceProjectID != m.TargetProjectID,
		Source:      event.Ref{Branch: m.SourceBranch, SHA: firstNonEmpty(m.DiffRefs.HeadSHA, m.SHA)},
		Target:      event.Ref{Branch: m.TargetBranch},
	}
	if m.ID != 0 {
		pr.ID = strconv.FormatInt(m.ID, 10)
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(m.ChangesCount, "+")); err == nil {
		pr.ChangedFiles = n
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

// apiDiff is one changed file, as both the merge request diffs endpoint and
// the repository compare endpoint return it.
type apiDiff struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	Diff        string `json:"diff"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	// TooLarge and Collapsed mean the patch is not in this response. The file
	// still changed, so it is reported with an empty patch rather than
	// dropped.
	TooLarge  bool `json:"too_large"`
	Collapsed bool `json:"collapsed"`
}

func (d apiDiff) normalize() forge.File {
	f := forge.File{
		Path:   d.NewPath,
		Status: fileStatus(d),
		Patch:  d.Diff,
	}
	if d.RenamedFile && d.OldPath != d.NewPath {
		f.PreviousPath = d.OldPath
	}
	if d.DeletedFile {
		f.Path = firstNonEmpty(d.OldPath, d.NewPath)
	}
	// GitLab does not count the lines, and the diff is right there.
	f.Additions, f.Deletions = countChanges(d.Diff)
	return f
}

func fileStatus(d apiDiff) forge.FileStatus {
	switch {
	case d.NewFile:
		return forge.FileAdded
	case d.DeletedFile:
		return forge.FileRemoved
	case d.RenamedFile:
		return forge.FileRenamed
	default:
		return forge.FileModified
	}
}

// countChanges counts the added and removed lines of a unified diff. The
// "+++" and "---" headers are not changes to any line.
func countChanges(patch string) (additions, deletions int) {
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

// apiNote is a comment. GitLab calls both a standalone comment and a reply a
// note, and groups them into discussions.
type apiNote struct {
	ID           int64   `json:"id"`
	Body         string  `json:"body"`
	Author       apiUser `json:"author"`
	System       bool    `json:"system"`
	Type         string  `json:"type"`
	Position     *apiPos `json:"position"`
	DiscussionID string  `json:"discussion_id"`
}

type apiPos struct {
	NewPath string `json:"new_path"`
	OldPath string `json:"old_path"`
	NewLine int    `json:"new_line"`
	OldLine int    `json:"old_line"`
}

func (n apiNote) normalize(discussionID string) forge.Comment {
	c := forge.Comment{
		ID:       strconv.FormatInt(n.ID, 10),
		ThreadID: firstNonEmpty(n.DiscussionID, discussionID),
		Body:     n.Body,
		Author:   n.Author.normalize(),
	}
	if n.Position != nil {
		c.Path = firstNonEmpty(n.Position.NewPath, n.Position.OldPath)
		c.Line = firstNonZero(n.Position.NewLine, n.Position.OldLine)
	}
	if c.ThreadID == "" {
		c.ThreadID = c.ID
	}
	return c
}

type apiDiscussion struct {
	ID    string    `json:"id"`
	Notes []apiNote `json:"notes"`
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}
