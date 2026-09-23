package azuredevops

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/webhook"
)

// Normalize implements [webhook.Handler].
//
// It returns (nil, nil) for deliveries that are well formed but not
// interesting: an event kibitz does not act on, a comment the service wrote
// itself, a comment that was edited rather than written.
func (h *Handler) Normalize(_ *http.Request, body []byte) (*event.ReviewEvent, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, &webhook.ErrMalformedPayload{Platform: event.PlatformAzureDevOps, Err: err}
	}

	switch p.EventType {
	case EventPRCreated, EventPRUpdated, EventPRMerged:
		return h.normalizePullRequest(&p)
	case EventPRCommented:
		return h.normalizeComment(&p)
	default:
		return nil, nil
	}
}

func (h *Handler) normalizePullRequest(p *payload) (*event.ReviewEvent, error) {
	pr := p.Resource.pr()
	if pr == nil {
		return nil, nil
	}

	kind, ok := pullRequestKind(p.EventType, pr)
	if !ok {
		return nil, nil
	}

	ev := newEvent(p, pr, pr.CreatedBy)
	ev.Kind = kind
	ev.PullRequest = pr.normalize()
	ev.OccurredAt = firstNonZero(parseTime(p.CreatedDate), h.now())
	return ev, ev.Validate()
}

// pullRequestKind maps an Azure DevOps event onto what the worker does.
//
// git.pullrequest.updated is this platform's difficult one, and it is harder
// than GitLab's "update": a push, a reviewer being added, a vote and the pull
// request being completed all arrive as the same event, and unlike GitLab
// there is nothing in the payload that says which it was. The subscription
// has a notificationType filter, but that is a setting on the hook, not a
// field on the delivery.
//
// So the status decides what can be decided, and everything still ambiguous
// becomes pr.updated. That is safe rather than lossy: the worker does not
// review a commit it has already reviewed, so a vote on an unchanged head
// costs a delivery and a lookup, not a review. Guessing the other way —
// dropping updates that might be pushes — would lose reviews.
func pullRequestKind(eventType string, pr *pullRequest) (event.Kind, bool) {
	switch strings.ToLower(pr.Status) {
	case "completed":
		return event.KindPRMerged, true
	case "abandoned":
		return event.KindPRClosed, true
	}

	switch eventType {
	case EventPRCreated:
		return event.KindPROpened, true
	case EventPRMerged:
		// A merge was attempted, not necessarily performed: the status above
		// is what says it completed. Reaching here means it did not, which is
		// a conflict or a failed policy — nothing to review.
		return "", false
	default:
		return event.KindPRUpdated, true
	}
}

func (h *Handler) normalizeComment(p *payload) (*event.ReviewEvent, error) {
	c := p.Resource.Comment
	pr := p.Resource.pr()
	if c == nil || pr == nil {
		return nil, nil
	}

	// A comment the service wrote itself — a vote, a branch update, a policy
	// result — is not somebody asking for anything.
	if t := strings.ToLower(strings.TrimSpace(c.CommentType)); t != "" && t != "text" {
		return nil, nil
	}
	// Azure DevOps sends the same event for a new comment and for an edit of
	// one. Answering an edit would answer the same question twice.
	if c.edited() {
		return nil, nil
	}

	ev := newEvent(p, pr, c.Author)
	ev.Kind = event.KindCommentCreated
	ev.PullRequest = pr.normalize()
	ev.Comment = &event.Comment{
		ID:       strconv.Itoa(c.ID),
		Body:     c.Content,
		Author:   ev.Actor,
		ThreadID: c.threadID(),
	}
	if c.ParentCommentID != 0 {
		ev.Comment.InReplyTo = strconv.Itoa(c.ParentCommentID)
	}
	if ev.Comment.ThreadID == "" {
		ev.Comment.ThreadID = ev.Comment.ID
	}
	// The delivery carries no file or line: Azure DevOps keeps the position on
	// the thread, and the comment event does not include its thread. The
	// worker reads the thread from the API when it needs the position, which
	// is what docs/event-schema.md means by the position being thin here.
	ev.OccurredAt = firstNonZero(parseTime(c.PublishedDate), parseTime(p.CreatedDate), h.now())
	return ev, ev.Validate()
}

// newEvent fills in what every Azure DevOps delivery has in common.
func newEvent(p *payload, pr *pullRequest, actor identity) *event.ReviewEvent {
	instance := instanceURL(p, pr)

	return &event.ReviewEvent{
		SchemaVersion: event.SchemaVersion,
		// The id is derived from the delivery, so a redelivery carries the
		// same event id.
		ID: string(event.PlatformAzureDevOps) + ":" + p.ID,
		Source: event.Source{
			Platform:    event.PlatformAzureDevOps,
			InstanceURL: instance,
			DeliveryID:  p.ID,
			EventName:   p.EventType,
		},
		Repository: pr.Repository.normalize(organizationOf(instance, pr)),
		Actor:      actor.normalize(),
	}
}

func (i identity) normalize() event.Actor {
	return event.Actor{ID: i.ID, Login: i.login()}
}

func (r repo) normalize(organization string) event.Repository {
	return event.Repository{
		ID:            r.ID,
		Owner:         organization,
		Project:       r.Project.Name,
		Name:          r.Name,
		FullName:      fullName(organization, r.Project.Name, r.Name),
		CloneURL:      r.RemoteURL,
		DefaultBranch: branchOf(r.DefaultBranch),
		Visibility:    r.Project.Visibility,
	}
}

// fullName is what the allow list is matched against and what keys a pull
// request in the queue and the state store.
//
// Azure DevOps nests one level deeper than the other two platforms:
// organization, then project, then repository. Two projects in one
// organization may each hold a repository called "api", so leaving the
// project out would make them the same pull request as far as the ordering
// key and the lock are concerned — two reviews racing on one change.
func fullName(organization, project, repository string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{organization, project, repository} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "/")
}

func (pr *pullRequest) normalize() *event.PullRequest {
	out := &event.PullRequest{
		ID:          strconv.Itoa(pr.PullRequestID),
		Number:      pr.PullRequestID,
		Title:       pr.Title,
		Description: pr.Description,
		State:       strings.ToLower(pr.Status),
		Merged:      strings.EqualFold(pr.Status, "completed"),
		URL:         pr.URL,
		Author:      pr.CreatedBy.normalize(),
		Source: event.Ref{
			Branch: branchOf(pr.SourceRefName),
			SHA:    pr.LastMergeSourceCommit.CommitID,
		},
		Target: event.Ref{
			Branch: branchOf(pr.TargetRefName),
			SHA:    pr.LastMergeTargetCommit.CommitID,
		},
	}
	if pr.IsDraft != nil {
		out.Draft = *pr.IsDraft
	}
	if pr.ForkSource != nil {
		out.IsFork = true
		out.Source.RepoFullName = pr.ForkSource.Repository.Name
	}
	return out
}

// instanceURL is where the delivery came from.
//
// resourceContainers carries a baseUrl, which is the direct answer — except
// that the comment event's containers carry only ids, with no baseUrl at all.
// That asymmetry is in Microsoft's own published samples, so the repository's
// URLs are the fallback rather than an afterthought.
func instanceURL(p *payload, pr *pullRequest) string {
	for _, base := range []string{
		p.ResourceContainers.Collection.BaseURL,
		p.ResourceContainers.Account.BaseURL,
		p.ResourceContainers.Project.BaseURL,
	} {
		if base = strings.TrimSpace(base); base != "" {
			return strings.TrimSuffix(base, "/")
		}
	}

	for _, raw := range []string{pr.Repository.RemoteURL, pr.Repository.URL, pr.URL} {
		if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host + organizationPath(u)
		}
	}
	return ""
}

// organizationOf names the account a repository belongs to, which is what the
// other platforms call the owner.
//
// On dev.azure.com and on Azure DevOps Server it is the first path segment —
// the organization, or the collection on premises. On the legacy
// {org}.visualstudio.com hosts it is the subdomain, and there is no such
// segment.
func organizationOf(instance string, pr *pullRequest) string {
	for _, raw := range []string{instance, pr.Repository.RemoteURL, pr.Repository.URL, pr.URL} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" {
			continue
		}
		if org := legacyOrganization(u.Host); org != "" {
			return org
		}
		if seg := firstSegment(u.Path); seg != "" {
			return seg
		}
	}
	return ""
}

// legacyOrganization returns the organization of a "{org}.visualstudio.com"
// host, and "" for anything else.
func legacyOrganization(host string) string {
	host, _, _ = strings.Cut(host, ":")
	const suffix = ".visualstudio.com"
	if !strings.HasSuffix(strings.ToLower(host), suffix) {
		return ""
	}
	org := host[:len(host)-len(suffix)]
	if strings.Contains(org, ".") {
		// Not a plain "{org}.visualstudio.com".
		return ""
	}
	return org
}

// organizationPath is the leading path segment of an instance, which on
// dev.azure.com and on premises is part of the address rather than of the
// repository.
func organizationPath(u *url.URL) string {
	if legacyOrganization(u.Host) != "" {
		return ""
	}
	if seg := firstSegment(u.Path); seg != "" {
		return "/" + seg
	}
	return ""
}

// firstSegment returns the first path segment, unless it is one of Azure
// DevOps's own route prefixes — those mean the path started at the host and
// there is no organization in it.
func firstSegment(path string) string {
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "_") {
			return ""
		}
		return seg
	}
	return ""
}

func parseTime(value string) time.Time {
	if value = strings.TrimSpace(value); value == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstNonZero(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
