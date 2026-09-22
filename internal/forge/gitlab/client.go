// Package gitlab talks to GitLab's REST API on behalf of kibitz. It serves
// gitlab.com and self-managed instances alike; the instance is configured,
// because unlike a webhook there is nothing in a request to derive it from.
package gitlab

import (
	"bytes"
	"context"
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

const (
	defaultBaseURL = "https://gitlab.com/api/v4"
	pageSize       = 100
	// maxPages bounds a listing. A merge request with more changed files than
	// this is not one a review is going to help with anyway.
	maxPages = 10
)

// Config configures a client.
type Config struct {
	// BaseURL is the API root. Empty means gitlab.com; a self-managed
	// instance is given as "https://gitlab.example.com" with or without the
	// /api/v4 suffix.
	BaseURL string
	// Token is a personal, group or project access token with api scope.
	// GitLab has no equivalent of a GitHub App installation token, so this is
	// a long-lived credential and is treated as one (docs/security.md).
	Token string
	// MaxFiles caps how many changed files are fetched. Zero means 300.
	MaxFiles int
}

// Client implements [forge.Client] for GitLab.
type Client struct {
	http     *http.Client
	baseURL  string
	token    string
	maxFiles int
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
		return nil, errors.New("gitlab: a token is required")
	}

	c := &Client{
		http:     &http.Client{Timeout: 30 * time.Second},
		baseURL:  normalizeBaseURL(cfg.BaseURL),
		token:    cfg.Token,
		maxFiles: cfg.MaxFiles,
	}
	if c.maxFiles <= 0 {
		c.maxFiles = 300
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// normalizeBaseURL accepts what an operator is likely to paste: the instance
// root, or the API root, with or without a trailing slash.
func normalizeBaseURL(raw string) string {
	raw = strings.TrimSuffix(strings.TrimSpace(raw), "/")
	if raw == "" {
		return defaultBaseURL
	}
	if strings.HasSuffix(raw, "/api/v4") {
		return raw
	}
	return raw + "/api/v4"
}

// Platform implements [forge.Client].
func (c *Client) Platform() event.Platform { return event.PlatformGitLab }

// projectPath is how GitLab names a project in a URL: the full path, encoded
// as one segment. A group can be nested, so the slashes inside it are part of
// the name rather than of the path.
func projectPath(ref forge.PRRef) string {
	full := ref.Repo
	if ref.Owner != "" {
		full = ref.Owner + "/" + ref.Repo
	}
	return "/projects/" + url.PathEscape(full)
}

// mergeRequestPath addresses one merge request by its internal id, which is
// the number people see.
func mergeRequestPath(ref forge.PRRef, suffix string) string {
	return fmt.Sprintf("%s/merge_requests/%d%s", projectPath(ref), ref.Number, suffix)
}

// APIError is a non-2xx response from GitLab.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("gitlab: http %d", e.StatusCode)
	}
	return fmt.Sprintf("gitlab: http %d: %s", e.StatusCode, e.Message)
}

// do makes one API call and decodes the response into out, which may be nil.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %w", method, path, errorFromResponse(resp))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s %s: %w", method, path, err)
	}
	return nil
}

// errorFromResponse turns GitLab's error body into something worth reading.
// It reports either "message" or "error" depending on the endpoint, so both
// are tried.
func errorFromResponse(resp *http.Response) error {
	apiErr := &APIError{StatusCode: resp.StatusCode}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil || len(data) == 0 {
		return apiErr
	}

	var body struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		apiErr.Message = strings.TrimSpace(string(data))
		return apiErr
	}
	switch {
	case body.Error != "":
		apiErr.Message = body.Error
	case body.Message != nil:
		apiErr.Message = fmt.Sprint(body.Message)
	}
	return apiErr
}
