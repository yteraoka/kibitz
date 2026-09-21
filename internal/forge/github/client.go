// Package github implements the forge client for GitHub and GitHub Enterprise
// Server. Authentication is always a GitHub App installation token.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

const (
	apiVersion    = "2022-11-28"
	defaultAPIURL = "https://api.github.com"
	// pageSize is the maximum GitHub allows; fewer round trips on a large
	// pull request means less rate limit spent.
	pageSize = 100
	// maxPages bounds paging so a pathological pull request cannot keep the
	// job fetching until its deadline.
	maxPages = 20
)

// Config configures the client.
type Config struct {
	AppID          int64
	InstallationID int64
	PrivateKey     string
	// BaseURL is the API root. Empty means github.com; a GitHub Enterprise
	// Server uses https://HOSTNAME/api/v3.
	BaseURL string
	// MaxFiles bounds how many changed files are fetched. Zero means 300.
	MaxFiles int
}

// Client talks to the GitHub REST API.
type Client struct {
	auth     *appAuth
	http     *http.Client
	baseURL  string
	maxFiles int
}

// Option customizes a client.
type Option func(*Client)

// WithHTTPClient replaces the HTTP client, which is how tests point the client
// at an httptest server.
func WithHTTPClient(c *http.Client) Option {
	return func(cl *Client) {
		cl.http = c
		cl.auth.httpClient = c
	}
}

// WithClock replaces the clock used for token expiry.
func WithClock(now func() time.Time) Option {
	return func(cl *Client) { cl.auth.now = now }
}

// New builds a client.
func New(cfg Config, opts ...Option) (*Client, error) {
	if cfg.AppID == 0 {
		return nil, errors.New("github: app id is required")
	}
	if cfg.InstallationID == 0 {
		return nil, errors.New("github: installation id is required")
	}
	key, err := parsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}

	baseURL := strings.TrimSuffix(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultAPIURL
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	c := &Client{
		auth: &appAuth{
			appID:          cfg.AppID,
			installationID: cfg.InstallationID,
			key:            key,
			baseURL:        baseURL,
			httpClient:     httpClient,
			now:            time.Now,
		},
		http:     httpClient,
		baseURL:  baseURL,
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

// Platform implements [forge.Client].
func (c *Client) Platform() event.Platform { return event.PlatformGitHub }

// APIError is a non-2xx response from GitHub.
type APIError struct {
	StatusCode int
	Message    string
	// RetryAfter is set when GitHub asked the caller to back off.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("github: %d %s (retry after %s)", e.StatusCode, e.Message, e.RetryAfter)
	}
	return fmt.Sprintf("github: %d %s", e.StatusCode, e.Message)
}

// Temporary reports whether retrying the same request might succeed. Rate
// limiting and server errors are worth another attempt; a 404 is not.
func (e *APIError) Temporary() bool {
	switch {
	case e.StatusCode == http.StatusTooManyRequests:
		return true
	case e.StatusCode == http.StatusForbidden && e.RetryAfter > 0:
		// A secondary rate limit arrives as a 403 with Retry-After.
		return true
	case e.StatusCode >= 500:
		return true
	default:
		return false
	}
}

func errorFromResponse(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))

	var payload struct {
		Message string `json:"message"`
	}
	message := strings.TrimSpace(string(body))
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		message = payload.Message
	}

	apiErr := &APIError{StatusCode: resp.StatusCode, Message: message}
	if after := resp.Header.Get("Retry-After"); after != "" {
		if seconds, err := strconv.Atoi(after); err == nil {
			apiErr.RetryAfter = time.Duration(seconds) * time.Second
		}
	}
	// A rate limit that resets at a known time arrives without Retry-After.
	if apiErr.RetryAfter == 0 && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Until(time.Unix(reset, 0)); wait > 0 {
				apiErr.RetryAfter = wait
			}
		}
	}
	return apiErr
}

// do performs an authenticated request and decodes a JSON response into out.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	token, err := c.auth.installationToken(ctx)
	if err != nil {
		return err
	}

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
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
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

func repoPath(ref forge.PRRef, suffix string) string {
	return fmt.Sprintf("/repos/%s/%s%s", ref.Owner, ref.Repo, suffix)
}
