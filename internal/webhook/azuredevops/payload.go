package azuredevops

import (
	"net/url"
	"strings"
	"time"
)

// Event IDs, as Azure DevOps calls them. They arrive in the body rather than
// a header, which is the first thing that makes this platform different.
const (
	EventPRCreated   = "git.pullrequest.created"
	EventPRUpdated   = "git.pullrequest.updated"
	EventPRMerged    = "git.pullrequest.merged"
	EventPRCommented = "ms.vss-code.git-pullrequest-comment-event"
)

// payload is the envelope every service hook delivery shares.
//
// Only the fields kibitz reads are decoded. The delivery also carries
// message/detailedMessage in three renderings, which are sentences meant for
// a chat window ("Jamal Hartnett marked the pull request as completed") and
// are deliberately not parsed: they are prose, they are localized, and
// deciding what happened from them would be guessing.
type payload struct {
	ID          string `json:"id"`
	EventType   string `json:"eventType"`
	PublisherID string `json:"publisherId"`
	CreatedDate string `json:"createdDate"`

	Resource           resource `json:"resource"`
	ResourceContainers struct {
		Collection container `json:"collection"`
		Account    container `json:"account"`
		Project    container `json:"project"`
	} `json:"resourceContainers"`
}

type container struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseUrl"`
}

// resource is the union of what the pull request events and the comment event
// carry. The comment event nests the pull request under "pullRequest"; the
// others put its fields at the top level, so both shapes are decoded into one
// struct and [resource.pr] picks whichever arrived.
type resource struct {
	pullRequest

	// Comment and PullRequest are how the comment event nests the same data.
	Comment     *comment     `json:"comment"`
	PullRequest *pullRequest `json:"pullRequest"`
}

// pr returns the pull request this delivery is about, from whichever shape it
// arrived in.
func (r *resource) pr() *pullRequest {
	if r.PullRequest != nil {
		return r.PullRequest
	}
	if r.PullRequestID == 0 {
		return nil
	}
	return &r.pullRequest
}

type pullRequest struct {
	PullRequestID int      `json:"pullRequestId"`
	Status        string   `json:"status"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	SourceRefName string   `json:"sourceRefName"`
	TargetRefName string   `json:"targetRefName"`
	MergeStatus   string   `json:"mergeStatus"`
	ClosedDate    string   `json:"closedDate"`
	URL           string   `json:"url"`
	IsDraft       *bool    `json:"isDraft"`
	CreatedBy     identity `json:"createdBy"`
	Repository    repo     `json:"repository"`

	LastMergeSourceCommit commitRef `json:"lastMergeSourceCommit"`
	LastMergeTargetCommit commitRef `json:"lastMergeTargetCommit"`

	// ForkSource is set when the pull request comes from a fork. The worker
	// re-reads the pull request from the API before reviewing it, so this is a
	// hint for the policy rather than the last word.
	ForkSource *struct {
		Repository repo `json:"repository"`
	} `json:"forkSource"`
}

type commitRef struct {
	CommitID string `json:"commitId"`
}

type repo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	RemoteURL     string `json:"remoteUrl"`
	DefaultBranch string `json:"defaultBranch"`
	Project       struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		URL        string `json:"url"`
		Visibility string `json:"visibility"`
	} `json:"project"`
}

type identity struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	UniqueName  string `json:"uniqueName"`
}

// login is the name to attribute an action to. uniqueName is the sign-in
// address and is stable; displayName is what a person chose to be called and
// can be anything, so it is only the fallback.
func (i identity) login() string {
	if v := strings.TrimSpace(i.UniqueName); v != "" {
		return v
	}
	return strings.TrimSpace(i.DisplayName)
}

type comment struct {
	ID              int      `json:"id"`
	ParentCommentID int      `json:"parentCommentId"`
	Author          identity `json:"author"`
	Content         string   `json:"content"`
	PublishedDate   string   `json:"publishedDate"`
	LastUpdatedDate string   `json:"lastUpdatedDate"`
	// CommentType separates what a person wrote ("text") from what the
	// service wrote ("system"): a vote, a branch update, a policy result.
	CommentType string `json:"commentType"`
	Links       struct {
		Threads struct {
			Href string `json:"href"`
		} `json:"threads"`
		Self struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"_links"`
}

// edited reports whether this delivery is about a comment that already
// existed. Azure DevOps sends the same event for a new comment and for an
// edit, and answering an edit would mean answering the same question twice.
func (c *comment) edited() bool {
	published, err1 := time.Parse(time.RFC3339, c.PublishedDate)
	updated, err2 := time.Parse(time.RFC3339, c.LastUpdatedDate)
	if err1 != nil || err2 != nil {
		return false
	}
	return updated.After(published)
}

// threadID is the discussion the comment belongs to, which is what a reply
// has to be posted into.
//
// It is read out of the thread link rather than taken from a field, because
// the payload has no field for it: _links.threads.href ends in
// ".../pullRequests/1/threads/5".
func (c *comment) threadID() string {
	href := c.Links.Threads.Href
	if href == "" {
		href = c.Links.Self.Href
	}
	if href == "" {
		return ""
	}
	if u, err := url.Parse(href); err == nil {
		href = u.Path
	}

	parts := strings.Split(strings.Trim(href, "/"), "/")
	for i := len(parts) - 1; i > 0; i-- {
		if strings.EqualFold(parts[i-1], "threads") {
			return parts[i]
		}
	}
	return ""
}

// branchOf turns "refs/heads/mytopic" into "mytopic". A ref that is not a
// branch is left as it is rather than mangled.
func branchOf(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}
