package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The write half of a workspace: what implement mode does after the agent has
// edited files. It is kept here rather than in the worker because it is git,
// and because the credential that can push is already in this package and
// should stay in one place.

// ErrNoChanges reports that the working tree is the same as the commit it was
// checked out at. It is not a failure: an agent that decided nothing needed
// changing is an outcome worth reporting rather than an error to retry.
var ErrNoChanges = errors.New("workspace: nothing was changed")

// Commit is who and what a commit is made of.
type Commit struct {
	Message string
	// Name and Email are the author and committer. They are configured rather
	// than taken from git's own defaults, because a worker's container has
	// none and a commit without them fails.
	Name  string
	Email string
}

// Changes stages everything and returns the paths a commit would contain.
//
// Staging is what makes the list exact: "what git would commit" and "what the
// agent appears to have touched" are two different questions, and the caller is
// about to check this list against what may be edited. Nothing is published by
// staging, and the workspace is thrown away either way, so a list that turns
// out to contain a path the caller refuses costs nothing.
//
// Renames are reported as a delete and an add rather than as a pair, because
// every path in the result has to be checked and a rename that was allowed to
// travel as one entry would have half of it checked.
func (w *Workspace) Changes(ctx context.Context) ([]string, error) {
	if err := w.runner.run(ctx, "add", "--all"); err != nil {
		return nil, err
	}
	out, err := w.runner.output(ctx, "diff", "--cached", "--name-only", "--no-renames", "-z")
	if err != nil {
		return nil, err
	}

	// -z means git does not quote or escape anything, so the bytes between the
	// separators are the paths as they are on disk.
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// Discard removes untracked files under the given paths, leaving anything the
// repository tracks alone.
//
// It exists for one job: taking kibitz's own scaffolding back out of the
// checkout before implement mode asks git what the agent changed. Removing the
// directory outright would be wrong, because a repository may well track files
// in it -- .kibitz/guidelines.md is one kibitz itself reads -- and deleting
// those would read as a change the agent made.
func (w *Workspace) Discard(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"clean", "--force", "-d", "--"}, paths...)
	return w.runner.run(ctx, args...)
}

// Record commits the staged changes and returns the new commit.
//
// It commits onto the detached head the workspace was checked out at, so there
// is no local branch to name and nothing to keep in step with the remote one.
// The branch is decided when it is pushed.
func (w *Workspace) Record(ctx context.Context, c Commit) (string, error) {
	if strings.TrimSpace(c.Message) == "" {
		return "", errors.New("workspace: a commit needs a message")
	}
	if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Email) == "" {
		return "", errors.New("workspace: a commit needs an author name and email")
	}

	staged, err := w.runner.output(ctx, "diff", "--cached", "--name-only")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(staged) == "" {
		return "", ErrNoChanges
	}

	// The identity is given on the command line rather than written into the
	// checkout's config, so that it applies to this one invocation and there is
	// no file to get out of step with it.
	err = w.runner.run(ctx,
		"-c", "user.name="+c.Name,
		"-c", "user.email="+c.Email,
		"commit", "--quiet",
		// Nothing here should be signed, and a global signing config would
		// otherwise stop the commit dead in a container with no key.
		"--no-gpg-sign",
		"--message", c.Message,
	)
	if err != nil {
		return "", err
	}
	return w.runner.output(ctx, "rev-parse", "HEAD")
}

// BranchExists reports whether the remote already has this branch.
//
// It is asked before pushing, and the answer decides whether kibitz pushes at
// all: a branch somebody else created, or one an earlier attempt left behind,
// is not something to overwrite. Nothing in implement mode force pushes.
func (w *Workspace) BranchExists(ctx context.Context, branch string) (bool, error) {
	if err := checkBranch(branch); err != nil {
		return false, err
	}

	// --exit-code separates "no such ref" from "could not ask", which matters
	// here: treating an authentication failure as "the branch is free" would
	// mean pushing over something.
	_, err := w.runner.output(ctx, "ls-remote", "--exit-code", "--heads", "origin", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 2 {
		return false, nil
	}
	return false, err
}

// Push publishes the current commit as a branch.
//
// The push is not forced and never will be. An implement run writes to a
// branch of the repository it was installed on, and the one thing it must not
// be able to do there is destroy work: a rejected push is a question for a
// person, not something to insist through.
func (w *Workspace) Push(ctx context.Context, branch string) error {
	if err := checkBranch(branch); err != nil {
		return err
	}
	return w.runner.run(ctx, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch)
}

// checkBranch refuses a name git would read as something other than a branch.
//
// The name is built by kibitz from a configured prefix and an issue number, so
// this is not where untrusted input arrives — but the prefix is configuration,
// and a prefix with a leading dash would turn the ref into an option.
func checkBranch(branch string) error {
	switch {
	case strings.TrimSpace(branch) == "":
		return errors.New("workspace: no branch name given")
	case branch != strings.TrimSpace(branch):
		return fmt.Errorf("workspace: branch %q has surrounding whitespace", branch)
	case strings.HasPrefix(branch, "-"), strings.HasPrefix(branch, "/"), strings.HasSuffix(branch, "/"):
		return fmt.Errorf("workspace: %q is not a usable branch name", branch)
	case strings.Contains(branch, ".."), strings.Contains(branch, "//"):
		return fmt.Errorf("workspace: %q is not a usable branch name", branch)
	}
	for _, r := range branch {
		switch {
		case r <= ' ' || r == 0x7f:
			return fmt.Errorf("workspace: branch %q contains a control character", branch)
		case strings.ContainsRune("~^:?*[\\", r):
			return fmt.Errorf("workspace: branch %q contains %q, which git does not allow", branch, r)
		}
	}
	return nil
}
