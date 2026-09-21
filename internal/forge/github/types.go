package github

import (
	"strconv"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// Only the fields kibitz reads are decoded; GitHub's payloads are large and
// keep growing.

type user struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

func (u user) normalize() event.Actor {
	a := event.Actor{Login: u.Login, IsBot: u.Type == "Bot"}
	if u.ID != 0 {
		a.ID = strconv.FormatInt(u.ID, 10)
	}
	return a
}

type prRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type pullRequest struct {
	ID           int64  `json:"id"`
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Body         string `json:"body"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	Merged       bool   `json:"merged"`
	HTMLURL      string `json:"html_url"`
	User         user   `json:"user"`
	Head         prRef  `json:"head"`
	Base         prRef  `json:"base"`
	ChangedFiles int    `json:"changed_files"`
}

func (p pullRequest) normalize(ref forge.PRRef) *event.PullRequest {
	pr := &event.PullRequest{
		Number:       p.Number,
		Title:        p.Title,
		Description:  p.Body,
		State:        p.State,
		Draft:        p.Draft,
		Merged:       p.Merged,
		URL:          p.HTMLURL,
		Author:       p.User.normalize(),
		Source:       event.Ref{Branch: p.Head.Ref, SHA: p.Head.SHA},
		Target:       event.Ref{Branch: p.Base.Ref, SHA: p.Base.SHA},
		ChangedFiles: p.ChangedFiles,
	}
	if p.ID != 0 {
		pr.ID = strconv.FormatInt(p.ID, 10)
	}
	if p.Head.Repo != nil {
		pr.Source.RepoFullName = p.Head.Repo.FullName
		pr.IsFork = p.Head.Repo.FullName != ref.FullName()
	}
	if p.Base.Repo != nil {
		pr.Target.RepoFullName = p.Base.Repo.FullName
	}
	return pr
}

type changedFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	Patch            string `json:"patch"`
}

func (f changedFile) normalize() forge.File {
	return forge.File{
		Path:         f.Filename,
		PreviousPath: f.PreviousFilename,
		Status:       forge.FileStatus(f.Status),
		Additions:    f.Additions,
		Deletions:    f.Deletions,
		Patch:        f.Patch,
	}
}

type issueComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User user   `json:"user"`
}

func (c issueComment) normalize() forge.Comment {
	return forge.Comment{
		ID:     strconv.FormatInt(c.ID, 10),
		Body:   c.Body,
		Author: c.User.normalize(),
	}
}

type reviewComment struct {
	ID          int64  `json:"id"`
	Body        string `json:"body"`
	User        user   `json:"user"`
	Path        string `json:"path"`
	Line        int    `json:"line"`
	InReplyToID int64  `json:"in_reply_to_id"`
}

func (c reviewComment) normalize() forge.Comment {
	cm := forge.Comment{
		ID:     strconv.FormatInt(c.ID, 10),
		Body:   c.Body,
		Author: c.User.normalize(),
		Path:   c.Path,
		Line:   c.Line,
	}
	cm.ThreadID = cm.ID
	if c.InReplyToID != 0 {
		cm.ThreadID = strconv.FormatInt(c.InReplyToID, 10)
	}
	return cm
}

type reviewCommentRequest struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"start_line,omitempty"`
	Side      string `json:"side,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

type createReviewRequest struct {
	Body     string                 `json:"body"`
	Event    string                 `json:"event"`
	CommitID string                 `json:"commit_id,omitempty"`
	Comments []reviewCommentRequest `json:"comments,omitempty"`
}
