// Package azuredevops talks to Azure DevOps's REST API on behalf of kibitz.
// It serves Azure DevOps Services and Azure DevOps Server alike; the instance
// is configured, because unlike a webhook there is nothing in a request to
// derive it from.
//
// Reading the pull request back from the API matters more here than anywhere
// else. Azure DevOps does not sign its service hooks (see
// internal/webhook/azuredevops), so the delivery is a claim about what
// happened and this package is what establishes the fact.
//
// # What is missing
//
// This does not implement [forge.Client] yet: Diff and Compare are absent, so
// nothing wires it into the worker. That is not an oversight but an open
// question, and the reason is worth stating where somebody will find it.
//
// Azure DevOps's REST API does not return a diff. A pull request iteration's
// changes, and the commit diffs endpoint, both answer with GitChange, whose
// fields are changeType, item, sourceServerItem and originalPath — a list of
// files, with no patch, no hunks and no line counts. That is Microsoft's own
// published specification (vsts-rest-api-specs, git/7.1), not an omission in
// the reading.
//
// kibitz needs patches for two things: the prompt carries the diff, and
// reviewer.NewPositions checks findings against it. Without them every
// finding is dropped as out of diff.
//
// The two ways out are a decision rather than a detail. One is to fetch both
// blobs for each changed file — the change carries objectId and
// originalObjectId — and compute the diff here, which keeps the platform
// boundary intact and costs up to two requests per file. The other is to
// produce the diff from the checkout the worker already makes, which is exact
// and free but moves the checkout ahead of the diff for every platform and
// hands the client something it does not have today.
package azuredevops

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// apiVersion is pinned rather than left to the service's default. Azure
// DevOps changes response shapes between versions, and an unpinned request
// gets whichever one the instance prefers today.
const apiVersion = "7.1"

// Config configures a client.
type Config struct {
	// OrganizationURL is the account root: "https://dev.azure.com/{org}" for
	// the hosted service, or "https://{server}/{collection}" for Azure DevOps
	// Server. The legacy "https://{org}.visualstudio.com" form works too.
	OrganizationURL string
	// Token authenticates every call. A personal access token is sent as the
	// password of HTTP basic authentication with an empty user, which is how
	// Azure DevOps takes one; an Entra ID access token is sent as a bearer
	// instead. Which one it is is decided by TokenIsBearer.
	Token string
	// TokenIsBearer sends the token as "Authorization: Bearer" rather than as
	// basic auth. Entra ID service principal tokens need this; a PAT does
	// not.
	TokenIsBearer bool
}

// Client implements [forge.Client] for Azure DevOps.
type Client struct {
	http    *http.Client
	baseURL string
	token   string
	bearer  bool
}

// Option customizes a client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client, which is how tests point a client
// at a stub server.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) { cl.http = c }
}

// New builds a client.
func New(cfg Config, opts ...Option) (*Client, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("azuredevops: a token is required")
	}
	base := strings.TrimSuffix(strings.TrimSpace(cfg.OrganizationURL), "/")
	if base == "" {
		return nil, errors.New("azuredevops: an organization url is required")
	}

	c := &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: base,
		token:   cfg.Token,
		bearer:  cfg.TokenIsBearer,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Platform implements [forge.Client].
func (c *Client) Platform() event.Platform { return event.PlatformAzureDevOps }

// repoPath addresses a repository. Azure DevOps nests one level deeper than
// the other two platforms, so the project is part of the address and not an
// optional extra: two projects in one organization may each hold a repository
// of the same name.
func (c *Client) repoPath(ref forge.PRRef) string {
	project := ref.Project
	if project == "" {
		// A reference built without one still addresses something, because
		// Azure DevOps accepts a repository id in place of project/name. It
		// is the caller's problem if the name is ambiguous.
		return "/_apis/git/repositories/" + url.PathEscape(ref.Repo)
	}
	return "/" + url.PathEscape(project) + "/_apis/git/repositories/" + url.PathEscape(ref.Repo)
}

// prPath addresses one pull request, or something under it.
func (c *Client) prPath(ref forge.PRRef, suffix string) string {
	return fmt.Sprintf("%s/pullRequests/%d%s", c.repoPath(ref), ref.Number, suffix)
}

// APIError is a non-2xx response from Azure DevOps.
type APIError struct {
	StatusCode int
	Message    string
	// TypeKey is Azure DevOps's own name for the failure
	// ("GitItemNotFoundException"), which is the only thing that separates
	// "no such file" from "no such repository" on a 404.
	TypeKey string
}

func (e *APIError) Error() string {
	switch {
	case e.Message != "" && e.TypeKey != "":
		return fmt.Sprintf("azuredevops: http %d: %s (%s)", e.StatusCode, e.Message, e.TypeKey)
	case e.Message != "":
		return fmt.Sprintf("azuredevops: http %d: %s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("azuredevops: http %d", e.StatusCode)
	}
}

// NotFound reports whether the call failed because the thing was not there.
func (e *APIError) NotFound() bool { return e.StatusCode == http.StatusNotFound }

// do makes one API call and decodes the response into out, which may be nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	raw, err := c.raw(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding %s %s: %w", method, path, err)
	}
	return nil
}

// raw makes one API call and returns the response body, which is how a blob
// is read: its content is not JSON.
func (c *Client) raw(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", apiVersion)

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path+"?"+query.Encode(), reader)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: %w", method, path, errorFrom(resp.StatusCode, data))
	}
	// A credential Azure DevOps does not accept is not answered with a 401.
	// It is answered with 203 and the HTML of a sign-in page, which is a
	// success as far as the status line is concerned. Left alone it surfaces
	// as "invalid character '<'", which sends whoever reads it looking in the
	// wrong place entirely.
	if resp.StatusCode == http.StatusNonAuthoritativeInfo {
		return nil, fmt.Errorf("%s %s: %w", method, path, &APIError{
			StatusCode: resp.StatusCode,
			Message:    "the response is a sign-in page, which means the token was not accepted",
		})
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading %s %s: %w", method, path, readErr)
	}
	return data, nil
}

// authorize adds the credential.
//
// A personal access token goes in as the password of basic authentication
// with an empty user, which is the form Azure DevOps documents and the reason
// a PAT looks like a password rather than a bearer token. An Entra ID token
// is a bearer and would be rejected in the other position.
func (c *Client) authorize(req *http.Request) {
	if c.bearer {
		req.Header.Set("Authorization", "Bearer "+c.token)
		return
	}
	req.Header.Set("Authorization", "Basic "+basic(c.token))
}

// basic encodes a personal access token the way Azure DevOps takes one: as
// the password of HTTP basic authentication, with no user.
func basic(token string) string {
	return base64.StdEncoding.EncodeToString([]byte(":" + token))
}

// errorFrom turns an Azure DevOps error body into something worth reading.
//
// A failure that is not JSON at all is the interesting case here: an
// unauthenticated request to Azure DevOps is answered with a sign-in page,
// so a wrong token produces HTML and a 203 rather than a 401. Saying so is
// the difference between "the token is wrong" and an afternoon.
func errorFrom(status int, data []byte) error {
	apiErr := &APIError{StatusCode: status}
	if len(data) == 0 {
		return apiErr
	}

	var body struct {
		Message string `json:"message"`
		TypeKey string `json:"typeKey"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		if looksLikeSignIn(data) {
			apiErr.Message = "the response is a sign-in page, which means the token was not accepted"
			return apiErr
		}
		apiErr.Message = truncate(strings.TrimSpace(string(data)), 200)
		return apiErr
	}
	apiErr.Message, apiErr.TypeKey = body.Message, body.TypeKey
	return apiErr
}

// looksLikeSignIn reports whether a body is the HTML Azure DevOps serves
// instead of an error when it wants a human to log in.
func looksLikeSignIn(data []byte) bool {
	head := strings.ToLower(string(data[:min(len(data), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "sign in")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
