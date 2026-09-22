// Package jobcontext is what one review job knows about its pull request,
// written to a file by the worker and read by kibitz-mcp.
//
// A file rather than an API call, and facts rather than a credential: the
// agent's tools are served by a process that holds nothing worth stealing. It
// cannot reach the forge, so nothing it is persuaded to do can either.
//
// Everything in here was fetched by the worker with its own token before the
// agent started, which is also what makes the tools cheap: they read a local
// file.
package jobcontext

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yteraoka/kibitz/internal/event"
	"github.com/yteraoka/kibitz/internal/forge"
)

// SchemaVersion is the shape of the file. The reader refuses a version it does
// not know rather than guessing at fields.
const SchemaVersion = 1

// Name is the file the worker writes inside the job directory.
const Name = "context.json"

// Context is one job's view of its pull request.
type Context struct {
	SchemaVersion int    `json:"schema_version"`
	Platform      string `json:"platform"`
	Repository    string `json:"repository"`
	PullRequest   PR     `json:"pull_request"`
	// Files is the whole pull request, not the part the prompt carries. A
	// review that was triaged down to ten files can still look at the
	// eleventh when something in it is worth a look.
	Files []File `json:"files"`
	// Truncated reports that the forge did not return every file.
	Truncated bool `json:"truncated,omitempty"`
	// Comments are the review comments already on the pull request.
	Comments []Comment `json:"comments,omitempty"`
	// Reviewed lists the paths the prompt carried, so a tool can say which
	// files were already put in front of the model.
	Reviewed []string `json:"reviewed,omitempty"`
}

// PR is the pull request itself.
type PR struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Author      string `json:"author,omitempty"`
	State       string `json:"state,omitempty"`
	Draft       bool   `json:"draft,omitempty"`
	BaseBranch  string `json:"base_branch,omitempty"`
	HeadBranch  string `json:"head_branch,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	URL         string `json:"url,omitempty"`
	// IsFork marks a pull request opened from a fork, which runs in a reduced
	// mode (docs/security.md).
	IsFork bool `json:"is_fork,omitempty"`
}

// File is one changed file.
type File struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Status       string `json:"status"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	// Patch is the unified diff. It is empty for a binary file and for one the
	// forge considered too large.
	Patch string `json:"patch,omitempty"`
}

// Comment is an existing comment.
type Comment struct {
	Author string `json:"author,omitempty"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Body   string `json:"body"`
}

// Build assembles the context from what the worker already fetched.
func Build(ev *event.ReviewEvent, pr *event.PullRequest, all *forge.Diff, reviewed *forge.Diff, comments []forge.Comment) *Context {
	ctx := &Context{SchemaVersion: SchemaVersion}
	if ev != nil {
		ctx.Platform = string(ev.Source.Platform)
		ctx.Repository = ev.Repository.FullName
	}
	if pr != nil {
		ctx.PullRequest = PR{
			Number:      pr.Number,
			Title:       pr.Title,
			Description: pr.Description,
			Author:      pr.Author.Login,
			State:       pr.State,
			Draft:       pr.Draft,
			BaseBranch:  pr.Target.Branch,
			HeadBranch:  pr.Source.Branch,
			HeadSHA:     pr.Source.SHA,
			URL:         pr.URL,
			IsFork:      pr.IsFork,
		}
	}
	if all != nil {
		ctx.Truncated = all.Truncated
		for _, f := range all.Files {
			ctx.Files = append(ctx.Files, File{
				Path:         f.Path,
				PreviousPath: f.PreviousPath,
				Status:       string(f.Status),
				Additions:    f.Additions,
				Deletions:    f.Deletions,
				Patch:        f.Patch,
			})
		}
	}
	if reviewed != nil {
		for _, f := range reviewed.Files {
			ctx.Reviewed = append(ctx.Reviewed, f.Path)
		}
	}
	for _, c := range comments {
		ctx.Comments = append(ctx.Comments, Comment{
			Author: c.Author.Login,
			Path:   c.Path,
			Line:   c.Line,
			Body:   c.Body,
		})
	}
	return ctx
}

// Write saves the context beside the job's other files.
func (c *Context) Write(dir string) (string, error) {
	path := filepath.Join(dir, Name)
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("jobcontext: encoding: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("jobcontext: %w", err)
	}
	// Readable by this user only: it holds the pull request's contents, which
	// are not public in a private repository.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", fmt.Errorf("jobcontext: writing %s: %w", path, err)
	}
	return path, nil
}

// Load reads a context file.
func Load(path string) (*Context, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is given by the operator, not by a pull request
	if err != nil {
		return nil, fmt.Errorf("jobcontext: %w", err)
	}
	var ctx Context
	if err := json.Unmarshal(data, &ctx); err != nil {
		return nil, fmt.Errorf("jobcontext: %s is not a context file: %w", path, err)
	}
	if ctx.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("jobcontext: %s is schema version %d; this build reads %d",
			path, ctx.SchemaVersion, SchemaVersion)
	}
	return &ctx, nil
}

// File returns the changed file at path, or false.
func (c *Context) File(path string) (File, bool) {
	path = strings.TrimPrefix(strings.TrimSpace(path), "./")
	for _, f := range c.Files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

// WasReviewed reports whether a path was in the diff the prompt carried.
func (c *Context) WasReviewed(path string) bool {
	for _, p := range c.Reviewed {
		if p == path {
			return true
		}
	}
	return false
}
