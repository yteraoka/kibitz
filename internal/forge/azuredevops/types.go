package azuredevops

import (
	"strconv"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// The shapes below are the parts of Azure DevOps's Git REST API that kibitz
// reads, taken from Microsoft's published OpenAPI specification
// (vsts-rest-api-specs, git/7.1) rather than from memory.

type pullRequest struct {
	PullRequestID int      `json:"pullRequestId"`
	Status        string   `json:"status"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	SourceRefName string   `json:"sourceRefName"`
	TargetRefName string   `json:"targetRefName"`
	MergeStatus   string   `json:"mergeStatus"`
	IsDraft       bool     `json:"isDraft"`
	URL           string   `json:"url"`
	CreatedBy     identity `json:"createdBy"`
	Repository    struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		RemoteURL     string `json:"remoteUrl"`
		DefaultBranch string `json:"defaultBranch"`
		Project       struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	} `json:"repository"`
	LastMergeSourceCommit commitRef `json:"lastMergeSourceCommit"`
	LastMergeTargetCommit commitRef `json:"lastMergeTargetCommit"`
	// ForkSource is set when the source branch lives in a fork.
	ForkSource *struct {
		Repository struct {
			Name    string `json:"name"`
			Project struct {
				Name string `json:"name"`
			} `json:"project"`
		} `json:"repository"`
	} `json:"forkSource"`
}

type commitRef struct {
	CommitID string `json:"commitId"`
}

type identity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}

// login prefers the sign-in address, which is stable, over the display name,
// which is whatever somebody chose to be called.
func (i identity) login() string {
	if v := strings.TrimSpace(i.UniqueName); v != "" {
		return v
	}
	return strings.TrimSpace(i.DisplayName)
}

func (i identity) normalize() event.Actor {
	return event.Actor{ID: i.ID, Login: i.login()}
}

// normalize turns an API pull request into the one shape the worker knows.
func (p *pullRequest) normalize() *event.PullRequest {
	out := &event.PullRequest{
		ID:          strconv.Itoa(p.PullRequestID),
		Number:      p.PullRequestID,
		Title:       p.Title,
		Description: p.Description,
		State:       strings.ToLower(p.Status),
		Draft:       p.IsDraft,
		Merged:      strings.EqualFold(p.Status, "completed"),
		URL:         p.URL,
		Author:      p.CreatedBy.normalize(),
		Source: event.Ref{
			Branch: branchOf(p.SourceRefName),
			SHA:    p.LastMergeSourceCommit.CommitID,
		},
		Target: event.Ref{
			Branch: branchOf(p.TargetRefName),
			SHA:    p.LastMergeTargetCommit.CommitID,
		},
	}
	if p.ForkSource != nil {
		out.IsFork = true
		out.Source.RepoFullName = forkName(p)
	}
	return out
}

func forkName(p *pullRequest) string {
	fork := p.ForkSource.Repository
	if fork.Project.Name != "" {
		return fork.Project.Name + "/" + fork.Name
	}
	return fork.Name
}

// commentThread is a discussion on a pull request. Azure DevOps has no
// standalone comment: every one belongs to a thread, and a thread is what
// carries the file and line.
type commentThread struct {
	ID            int             `json:"id"`
	Status        string          `json:"status,omitempty"`
	IsDeleted     bool            `json:"isDeleted,omitempty"`
	Comments      []threadComment `json:"comments"`
	ThreadContext *threadContext  `json:"threadContext,omitempty"`
}

type threadComment struct {
	ID              int      `json:"id,omitempty"`
	ParentCommentID int      `json:"parentCommentId,omitempty"`
	Author          identity `json:"author,omitempty"`
	Content         string   `json:"content"`
	CommentType     string   `json:"commentType,omitempty"`
	IsDeleted       bool     `json:"isDeleted,omitempty"`
}

// threadContext positions a thread in a file. Azure DevOps counts lines from
// 1 and character offsets from 0, and distinguishes the left file (before the
// change) from the right (after it). kibitz comments on the right.
type threadContext struct {
	FilePath       string    `json:"filePath"`
	RightFileStart *position `json:"rightFileStart,omitempty"`
	RightFileEnd   *position `json:"rightFileEnd,omitempty"`
	LeftFileStart  *position `json:"leftFileStart,omitempty"`
	LeftFileEnd    *position `json:"leftFileEnd,omitempty"`
}

type position struct {
	Line   int `json:"line"`
	Offset int `json:"offset"`
}

// normalize turns a thread into the comments the worker deduplicates against.
//
// The thread's file and line are copied onto every comment in it, because
// that is where kibitz keeps them and Azure DevOps keeps them one level up.
func (t *commentThread) normalize() []forge.Comment {
	if t.IsDeleted {
		return nil
	}

	path, line := "", 0
	if t.ThreadContext != nil {
		path = strings.TrimPrefix(t.ThreadContext.FilePath, "/")
		if p := firstPosition(t.ThreadContext.RightFileStart, t.ThreadContext.LeftFileStart); p != nil {
			line = p.Line
		}
	}

	out := make([]forge.Comment, 0, len(t.Comments))
	for _, c := range t.Comments {
		// A comment the service wrote itself is not part of the conversation:
		// repeating a finding somebody made is the thing this list prevents,
		// and "kibitz voted" is not a finding.
		if c.IsDeleted || !isText(c.CommentType) {
			continue
		}
		out = append(out, forge.Comment{
			ID:       strconv.Itoa(c.ID),
			ThreadID: strconv.Itoa(t.ID),
			Body:     c.Content,
			Author:   c.Author.normalize(),
			Path:     path,
			Line:     line,
		})
	}
	return out
}

// isText reports whether a comment was written by a person. An empty type is
// treated as text: a thread kibitz created does not set one.
func isText(commentType string) bool {
	t := strings.ToLower(strings.TrimSpace(commentType))
	return t == "" || t == "text" || t == "codechange"
}

func firstPosition(positions ...*position) *position {
	for _, p := range positions {
		if p != nil && p.Line > 0 {
			return p
		}
	}
	return nil
}

// branchOf turns "refs/heads/topic" into "topic". A ref that is not a branch
// is left as it is rather than mangled.
func branchOf(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// apiPath makes a repository path absolute the way Azure DevOps wants it:
// rooted at the repository, with a leading slash.
func apiPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

// gitChange is one file touched by a pull request or by a range of commits.
//
// Note what is not here. Azure DevOps answers with the path, the kind of
// change and the git object ids of the two versions — no patch, no hunks, no
// line counts. That absence is why internal/textdiff exists.
type gitChange struct {
	ChangeType string  `json:"changeType"`
	Item       gitItem `json:"item"`
	// OriginalPath is set when the file was renamed, and names where it was
	// before.
	OriginalPath string `json:"originalPath"`
}

type gitItem struct {
	Path string `json:"path"`
	// ObjectID is the blob as the change leaves it, OriginalObjectID the blob
	// as it was. Both are git object ids, and both are how the content is
	// fetched.
	ObjectID         string `json:"objectId"`
	OriginalObjectID string `json:"originalObjectId"`
	GitObjectType    string `json:"gitObjectType"`
	IsFolder         bool   `json:"isFolder"`
	CommitID         string `json:"commitId"`
}

// isTree reports whether the item is a directory rather than a file. Azure
// DevOps lists the directories a change created alongside the files.
func (i gitItem) isTree() bool {
	return strings.EqualFold(strings.TrimSpace(i.GitObjectType), "tree")
}

// changeKinds splits a change type into its parts.
//
// Azure DevOps combines them: a file that was moved and modified in one
// commit comes back as "edit, rename". Matching on the whole string would
// miss that, and matching on a substring would read "undelete" as a delete
// and "sourceRename" as a rename of this file rather than of another.
func changeKinds(changeType string) map[string]bool {
	kinds := make(map[string]bool)
	for _, part := range strings.Split(changeType, ",") {
		if k := strings.ToLower(strings.TrimSpace(part)); k != "" {
			kinds[k] = true
		}
	}
	return kinds
}

func (c gitChange) added() bool {
	kinds := changeKinds(c.ChangeType)
	return kinds["add"] || kinds["undelete"] || kinds["branch"]
}

func (c gitChange) deleted() bool {
	return changeKinds(c.ChangeType)["delete"] && !changeKinds(c.ChangeType)["undelete"]
}

// status maps a change onto the four states kibitz knows.
//
// The order is what makes the combinations come out right: a file that was
// deleted is deleted whatever else happened to it, and one that was renamed
// and edited is reported as renamed, because the rename is the part the
// other three statuses cannot express.
func (c gitChange) status() forge.FileStatus {
	kinds := changeKinds(c.ChangeType)
	switch {
	case kinds["delete"] && !kinds["undelete"]:
		return forge.FileRemoved
	case kinds["rename"] || kinds["sourcerename"] || kinds["targetrename"]:
		return forge.FileRenamed
	case kinds["add"] || kinds["undelete"] || kinds["branch"]:
		return forge.FileAdded
	default:
		return forge.FileModified
	}
}
