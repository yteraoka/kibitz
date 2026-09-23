package azuredevops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/textdiff"
)

// Limits on how much work one diff is allowed to be.
const (
	// changePageSize is the largest page Azure DevOps serves for a list of
	// changes. Asking for the maximum is what keeps a wide pull request to
	// one round trip instead of twenty.
	changePageSize = 2000
	// changePages bounds paging, so a pull request that somehow never
	// finishes cannot keep the worker busy forever.
	changePages = 10
	// blobWorkers is how many blobs are fetched at once. Each file costs up
	// to two requests, so a pull request touching fifty files is a hundred;
	// doing them one at a time would make the diff the slowest part of a
	// review.
	blobWorkers = 8
	// maxBlobBytes caps what one diff will download. Past it the remaining
	// files are reported without a patch, which is what the other platforms
	// do with a file they decline to diff.
	maxBlobBytes = 8 << 20
)

// Diff implements [forge.Client].
//
// This is the expensive one, and the reason is the platform rather than the
// implementation. Azure DevOps has no endpoint that returns a patch: the
// changes it lists carry a path, a change type and the git object ids of the
// two versions, and nothing else. The patch kibitz needs — for the prompt and
// for checking that a finding lands on a line that actually changed — is
// therefore computed here, from the two blobs.
//
// The cost is up to two requests per changed file, which is why the file
// count and the total bytes are both capped.
func (c *Client) Diff(ctx context.Context, ref forge.PRRef) (*forge.Diff, error) {
	iteration, err := c.latestIteration(ctx, ref)
	if err != nil {
		return nil, err
	}
	if iteration == nil {
		// A pull request always has at least one iteration. None means the
		// pull request is not there, or is not readable with this token.
		return nil, fmt.Errorf("azuredevops: %s has no iterations", ref)
	}

	changes, truncated, err := c.iterationChanges(ctx, ref, iteration.ID)
	if err != nil {
		return nil, err
	}
	return c.buildDiff(ctx, ref, changes, truncated, iteration.CommonRefCommit.CommitID)
}

// Compare implements [forge.Client].
//
// It answers with what changed between two commits, which is how a second
// review looks only at what was pushed since the first one. A commit that is
// gone — the ordinary result of a force push — comes back as
// [forge.ErrNoCompare] rather than as a failure, and the caller reviews the
// whole diff instead.
func (c *Client) Compare(ctx context.Context, ref forge.PRRef, base, head string) (*forge.Diff, error) {
	if base == "" || head == "" {
		return nil, forge.ErrNoCompare
	}
	if base == head {
		return &forge.Diff{}, nil
	}

	changes, truncated, err := c.commitChanges(ctx, ref, base, head)
	if err != nil {
		return nil, err
	}
	return c.buildDiff(ctx, ref, changes, truncated, base)
}

// pullRequestIteration is one push to a pull request. The last one is the
// current state of it.
type pullRequestIteration struct {
	ID              int       `json:"id"`
	CommonRefCommit commitRef `json:"commonRefCommit"`
	SourceRefCommit commitRef `json:"sourceRefCommit"`
	TargetRefCommit commitRef `json:"targetRefCommit"`
}

// latestIteration returns the most recent iteration of a pull request.
func (c *Client) latestIteration(ctx context.Context, ref forge.PRRef) (*pullRequestIteration, error) {
	var body struct {
		Value []pullRequestIteration `json:"value"`
	}
	if err := c.do(ctx, http.MethodGet, c.prPath(ref, "/iterations"), nil, nil, &body); err != nil {
		return nil, err
	}
	if len(body.Value) == 0 {
		return nil, nil
	}
	// Iterations come back oldest first and are numbered from one.
	latest := body.Value[len(body.Value)-1]
	for i := range body.Value {
		if body.Value[i].ID > latest.ID {
			latest = body.Value[i]
		}
	}
	return &latest, nil
}

// iterationChanges lists what a pull request changed, following the paging
// Azure DevOps describes in the response rather than guessing at it.
func (c *Client) iterationChanges(ctx context.Context, ref forge.PRRef, iteration int) ([]gitChange, bool, error) {
	var out []gitChange
	skip := 0

	for page := 0; page < changePages; page++ {
		query := url.Values{}
		query.Set("$top", strconv.Itoa(changePageSize))
		if skip > 0 {
			query.Set("$skip", strconv.Itoa(skip))
		}

		var body struct {
			ChangeEntries []gitChange `json:"changeEntries"`
			NextSkip      int         `json:"nextSkip"`
		}
		path := c.prPath(ref, fmt.Sprintf("/iterations/%d/changes", iteration))
		if err := c.do(ctx, http.MethodGet, path, query, nil, &body); err != nil {
			return nil, false, err
		}

		out = append(out, body.ChangeEntries...)
		if body.NextSkip <= 0 || len(body.ChangeEntries) == 0 {
			return out, false, nil
		}
		skip = body.NextSkip
	}
	return out, true, nil
}

// commitChanges lists what changed between two commits.
func (c *Client) commitChanges(ctx context.Context, ref forge.PRRef, base, head string) ([]gitChange, bool, error) {
	var out []gitChange
	skip := 0

	for page := 0; page < changePages; page++ {
		query := url.Values{}
		query.Set("baseVersion", base)
		query.Set("baseVersionType", "commit")
		query.Set("targetVersion", head)
		query.Set("targetVersionType", "commit")
		// Between two commits of a pull request, what is wanted is what the
		// second added, not what the branches have drifted apart by.
		query.Set("diffCommonCommit", "true")
		query.Set("$top", strconv.Itoa(changePageSize))
		if skip > 0 {
			query.Set("$skip", strconv.Itoa(skip))
		}

		var body struct {
			Changes            []gitChange `json:"changes"`
			AllChangesIncluded bool        `json:"allChangesIncluded"`
		}
		if err := c.do(ctx, http.MethodGet, c.repoPath(ref)+"/diffs/commits", query, nil, &body); err != nil {
			if notFound(err) {
				return nil, false, fmt.Errorf("%w: %s...%s", forge.ErrNoCompare, base, head)
			}
			return nil, false, err
		}

		out = append(out, body.Changes...)
		if body.AllChangesIncluded || len(body.Changes) == 0 {
			return out, false, nil
		}
		skip += len(body.Changes)
	}
	return out, true, nil
}

// buildDiff turns a list of changes into a diff with patches in it.
func (c *Client) buildDiff(ctx context.Context, ref forge.PRRef, changes []gitChange, truncated bool, base string) (*forge.Diff, error) {
	diff := &forge.Diff{Truncated: truncated}

	files := make([]forge.File, 0, len(changes))
	keep := make([]gitChange, 0, len(changes))
	for _, ch := range changes {
		if ch.Item.IsFolder || ch.Item.isTree() {
			// A directory is not a file, and Azure DevOps lists the ones a
			// change created.
			continue
		}
		path := strings.TrimPrefix(ch.Item.Path, "/")
		if path == "" {
			continue
		}
		if len(files) >= c.maxFiles {
			diff.Truncated = true
			break
		}
		files = append(files, forge.File{
			Path:         path,
			PreviousPath: strings.TrimPrefix(ch.OriginalPath, "/"),
			Status:       ch.status(),
		})
		keep = append(keep, ch)
	}

	incomplete, err := c.fillPatches(ctx, ref, files, keep, base)
	if err != nil {
		return nil, err
	}
	if incomplete {
		diff.Truncated = true
	}
	diff.Files = files
	return diff, nil
}

// fillPatches fetches both versions of every file and diffs them.
//
// The work is done in parallel because it is almost entirely waiting: a pull
// request touching fifty files is a hundred requests, and doing them in
// sequence would make fetching the diff take longer than reviewing it.
func (c *Client) fillPatches(ctx context.Context, ref forge.PRRef, files []forge.File, changes []gitChange, base string) (incomplete bool, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu         sync.Mutex
		budget     = maxBlobBytes
		failures   []error
		overBudget bool
	)

	// spend reserves part of the download budget. A diff that would pull
	// down more than the budget stops fetching and reports the rest of the
	// files without a patch, rather than downloading a repository.
	spend := func(n int) bool {
		mu.Lock()
		defer mu.Unlock()
		if budget <= 0 {
			return false
		}
		budget -= n
		return true
	}

	work := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < blobWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				before, after, err := c.contents(ctx, ref, changes[idx], base, spend)
				mu.Lock()
				switch {
				case errors.Is(err, errBudgetSpent):
					// Not a failure: the file is reported as changed with no
					// patch, exactly as a platform reports one it declined to
					// diff. The caller is told the diff is incomplete.
					overBudget = true
				case err != nil:
					failures = append(failures, fmt.Errorf("%s: %w", files[idx].Path, err))
				default:
					applyResult(&files[idx], textdiff.Unified(before, after, textdiff.Options{}))
				}
				mu.Unlock()
			}
		}()
	}

	for i := range files {
		select {
		case work <- i:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()

	if len(failures) > 0 {
		return false, fmt.Errorf("azuredevops: reading %d of %d changed files failed: %w",
			len(failures), len(files), errors.Join(failures...))
	}
	return overBudget, nil
}

// applyResult copies a computed diff onto the file it describes.
//
// A file with no patch is not an error. Binary files, files past the size
// worth diffing and files the download budget did not reach all come out
// this way, and the reviewer already treats a file without a patch as one
// with no commentable lines.
func applyResult(f *forge.File, r textdiff.Result) {
	f.Patch = r.Patch
	f.Additions = r.Additions
	f.Deletions = r.Deletions
}

// contents fetches both versions of one changed file.
//
// Either side may be absent: a file that was added has no previous version,
// and one that was deleted has no current one.
func (c *Client) contents(ctx context.Context, ref forge.PRRef, ch gitChange, base string, spend func(int) bool) (before, after []byte, err error) {
	newID, oldID := ch.Item.ObjectID, ch.Item.OriginalObjectID
	if ch.deleted() {
		// There is no version after a deletion, and the object the record
		// names is therefore the one before it. Taking it as both sides
		// would make the file read as unchanged; taking it as the previous
		// version is what it is, and saves looking the same blob up again.
		if oldID == "" {
			oldID = newID
		}
		newID = ""
	}
	if ch.added() {
		oldID = ""
	}

	// An edit whose change record does not name the previous blob still has
	// one; it has to be looked up by path at the commit the pull request
	// started from. Without this the file would come out as wholly new,
	// which would show every line as added and let a finding land anywhere
	// in it.
	if oldID == "" && !ch.added() && base != "" {
		previous := ch.OriginalPath
		if previous == "" {
			previous = ch.Item.Path
		}
		oldID, err = c.objectAt(ctx, ref, previous, base)
		if err != nil {
			return nil, nil, err
		}
	}

	if before, err = c.blob(ctx, ref, oldID, spend); err != nil {
		return nil, nil, err
	}
	if after, err = c.blob(ctx, ref, newID, spend); err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

// blob reads one git object, or nothing when there is no object to read.
func (c *Client) blob(ctx context.Context, ref forge.PRRef, objectID string, spend func(int) bool) ([]byte, error) {
	if objectID == "" {
		return nil, nil
	}
	if !spend(0) {
		return nil, errBudgetSpent
	}

	query := url.Values{}
	query.Set("$format", "octetstream")
	data, err := c.raw(ctx, http.MethodGet, c.repoPath(ref)+"/blobs/"+url.PathEscape(objectID), query, nil)
	if err != nil {
		if notFound(err) {
			// A blob the service has forgotten is not a reason to fail the
			// whole diff.
			return nil, nil
		}
		return nil, err
	}
	spend(len(data))
	return data, nil
}

// errBudgetSpent stops a diff from downloading a whole repository.
var errBudgetSpent = errors.New("azuredevops: the diff download budget is spent")

// objectAt resolves the git object id of a path at one commit.
func (c *Client) objectAt(ctx context.Context, ref forge.PRRef, path, commit string) (string, error) {
	query := url.Values{}
	query.Set("path", apiPath(path))
	query.Set("versionDescriptor.version", commit)
	query.Set("versionDescriptor.versionType", "commit")

	var item gitItem
	if err := c.do(ctx, http.MethodGet, c.repoPath(ref)+"/items", query, nil, &item); err != nil {
		if notFound(err) {
			// The path did not exist at that commit, so there is no previous
			// version and the file is new as far as this diff is concerned.
			return "", nil
		}
		return "", err
	}
	return item.ObjectID, nil
}
