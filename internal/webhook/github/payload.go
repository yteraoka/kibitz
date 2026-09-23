package github

import (
	"strconv"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
)

// The structs below cover only the fields kibitz reads. GitHub payloads are
// large and grow over time; decoding a subset keeps normalization stable when
// fields kibitz does not use are added or changed.

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

// actor names who wrote the comment, and which app did it on their behalf
// when GitHub says so. Only issue comments carry that; a review comment does
// not, which is why the app is recorded rather than relied on alone.
func (c comment) actor() event.Actor {
	a := c.User.normalize()
	if c.PerformedVia != nil && c.PerformedVia.ID != 0 {
		a.AppID = strconv.FormatInt(c.PerformedVia.ID, 10)
		a.AppSlug = c.PerformedVia.Slug
	}
	return a
}

type repository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         user   `json:"owner"`
	CloneURL      string `json:"clone_url"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
}

func (r repository) normalize() event.Repository {
	repo := event.Repository{
		Owner:         r.Owner.Login,
		Name:          r.Name,
		FullName:      r.FullName,
		CloneURL:      r.CloneURL,
		DefaultBranch: r.DefaultBranch,
		Visibility:    "public",
	}
	if r.ID != 0 {
		repo.ID = strconv.FormatInt(r.ID, 10)
	}
	if r.Private {
		repo.Visibility = "private"
	}
	return repo
}

type prRef struct {
	Ref  string      `json:"ref"`
	SHA  string      `json:"sha"`
	Repo *repository `json:"repo"`
}

func (r prRef) normalize() event.Ref {
	ref := event.Ref{Branch: r.Ref, SHA: r.SHA}
	if r.Repo != nil {
		ref.RepoFullName = r.Repo.FullName
	}
	return ref
}

type pullRequest struct {
	ID           int64     `json:"id"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	State        string    `json:"state"`
	Draft        bool      `json:"draft"`
	Merged       bool      `json:"merged"`
	HTMLURL      string    `json:"html_url"`
	User         user      `json:"user"`
	Head         prRef     `json:"head"`
	Base         prRef     `json:"base"`
	ChangedFiles int       `json:"changed_files"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (p pullRequest) normalize(repo repository) *event.PullRequest {
	pr := &event.PullRequest{
		Number:       p.Number,
		Title:        p.Title,
		Description:  p.Body,
		State:        p.State,
		Draft:        p.Draft,
		Merged:       p.Merged,
		URL:          p.HTMLURL,
		Author:       p.User.normalize(),
		Source:       p.Head.normalize(),
		Target:       p.Base.normalize(),
		ChangedFiles: p.ChangedFiles,
	}
	if p.ID != 0 {
		pr.ID = strconv.FormatInt(p.ID, 10)
	}
	// A pull request whose head lives in another repository comes from a fork,
	// which the worker treats as untrusted input.
	if p.Head.Repo != nil && repo.FullName != "" && p.Head.Repo.FullName != repo.FullName {
		pr.IsFork = true
	}
	return pr
}

// issue is the shape a pull request takes in issue_comment payloads. It
// carries no head or base, so the worker resolves those from the API.
type issue struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Draft   bool   `json:"draft"`
	HTMLURL string `json:"html_url"`
	User    user   `json:"user"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	PullRequest *struct {
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
}

// normalizeIssue turns the payload into an issue rather than into the pull
// request it is not. GitHub sends the same shape for both and only the
// pull_request member tells them apart.
func (i issue) normalizeIssue() *event.Issue {
	out := &event.Issue{
		Number:      i.Number,
		Title:       i.Title,
		Description: i.Body,
		State:       i.State,
		URL:         i.HTMLURL,
		Author:      i.User.normalize(),
	}
	if i.ID != 0 {
		out.ID = strconv.FormatInt(i.ID, 10)
	}
	for _, label := range i.Labels {
		if label.Name != "" {
			out.Labels = append(out.Labels, label.Name)
		}
	}
	return out
}

func (i issue) normalize() *event.PullRequest {
	pr := &event.PullRequest{
		Number:      i.Number,
		Title:       i.Title,
		Description: i.Body,
		State:       i.State,
		Draft:       i.Draft,
		URL:         i.HTMLURL,
		Author:      i.User.normalize(),
	}
	// The id in an issue payload identifies the issue, not the pull request,
	// so it is deliberately left unset rather than filled with something the
	// pull request API would not recognize.
	return pr
}

// githubApp is the app that performed an action, which GitHub reports on an
// issue comment. It is the only self-identifying thing in a webhook payload:
// the id is stable across renames, so kibitz can recognize its own comments
// from the delivery alone, with no credentials and no API call.
type githubApp struct {
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
}

type comment struct {
	ID           int64      `json:"id"`
	Body         string     `json:"body"`
	User         user       `json:"user"`
	PerformedVia *githubApp `json:"performed_via_github_app"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    time.Time  `json:"created_at"`
	Path         string     `json:"path"`
	Line         int        `json:"line"`
	OriginalLine int        `json:"original_line"`
	InReplyToID  int64      `json:"in_reply_to_id"`
}

type pullRequestPayload struct {
	Action      string      `json:"action"`
	Number      int         `json:"number"`
	PullRequest pullRequest `json:"pull_request"`
	Repository  repository  `json:"repository"`
	Sender      user        `json:"sender"`
}

type issueCommentPayload struct {
	Action     string     `json:"action"`
	Issue      issue      `json:"issue"`
	Comment    comment    `json:"comment"`
	Repository repository `json:"repository"`
	Sender     user       `json:"sender"`
}

type reviewCommentPayload struct {
	Action      string      `json:"action"`
	Comment     comment     `json:"comment"`
	PullRequest pullRequest `json:"pull_request"`
	Repository  repository  `json:"repository"`
	Sender      user        `json:"sender"`
}
