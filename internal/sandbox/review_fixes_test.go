package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/api/option"

	"github.com/yteraoka/kibitz/internal/sandbox"
)

// refusingStore takes the small objects and turns the workspace away before
// reading a byte of it -- a size limit, a quota, a policy on large objects.
//
// It has to accept the request first. A store that refused everything would
// fail Verify on request.json and never reach the upload this is about, and
// the test would pass while exercising nothing; the first version of it did
// exactly that.
type refusingStore struct {
	err     error
	offered []string
}

func (s *refusingStore) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, sandbox.ErrNotFound
}

func (s *refusingStore) Put(_ context.Context, name string, r io.Reader) error {
	s.offered = append(s.offered, name)
	if name == sandbox.WorkspaceObject {
		return s.err
	}
	_, err := io.Copy(io.Discard, r)
	return err
}

// When the store refuses the upload, that refusal is the error. Closing the
// pipe to unblock the packer makes the packer fail too, with "closed pipe",
// and reporting that instead would send whoever reads the log looking at the
// local tree for a problem that is in the bucket's permissions.
func TestAnUploadRefusedByTheStoreReportsTheStore(t *testing.T) {
	denied := errors.New("googleapi: Error 413: the object exceeds the bucket's size limit")
	store := &refusingStore{err: denied}
	job := &sandbox.Job{
		Location: "gs://bucket/runs",
		Executor: &fakeExecutor{finished: true},
		Stores: func(context.Context, string) (sandbox.Store, error) {
			return store, nil
		},
	}

	_, err := job.Verify(context.Background(), tree(t), &sandbox.Request{Commands: [][]string{{"go", "vet"}}})
	// Proof the test reached the code it is about.
	if len(store.offered) != 2 || store.offered[1] != sandbox.WorkspaceObject {
		t.Fatalf("the store was offered %v; the workspace upload was never attempted", store.offered)
	}
	if err == nil {
		t.Fatal("Verify returned nil for an upload the store refused")
	}
	if !errors.Is(err, denied) {
		t.Errorf("error = %v, want the store's own error", err)
	}
	if strings.Contains(err.Error(), "closed pipe") {
		t.Errorf("error = %v, which blames the pipe the upload's own failure closed", err)
	}
}

// fakeRunAPI is enough of the Cloud Run Admin API to see what an execution is
// asked for.
type fakeRunAPI struct {
	mu   sync.Mutex
	runs []map[string]any
}

func (f *fakeRunAPI) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, ":run") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding the request: %v", err)
		}
		f.mu.Lock()
		f.runs = append(f.runs, body)
		f.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "projects/p/locations/l/operations/op1",
			"done": true,
			"response": map[string]any{
				"name":           "projects/p/locations/l/jobs/runner/executions/e1",
				"completionTime": time.Now().UTC().Format(time.RFC3339),
				"succeededCount": 1,
			},
		})
	})
}

// The task has to outlive the commands. The runner downloads the tree,
// unpacks it, runs the commands under the request's timeout, and then uploads
// the result; a task given only the commands' budget is killed while the last
// of them is still running, and the result that would have said "timed out"
// is never written. The worker then sees a failed task and retries a
// verification that would fail the same way every time.
func TestTheTaskIsGivenMoreTimeThanTheCommands(t *testing.T) {
	api := &fakeRunAPI{}
	server := httptest.NewServer(api.handler(t))
	t.Cleanup(server.Close)

	executor, err := sandbox.NewCloudRun(context.Background(), sandbox.CloudRunConfig{
		Job: "projects/p/locations/l/jobs/runner",
	},
		option.WithEndpoint(server.URL+"/"),
		option.WithHTTPClient(server.Client()),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("NewCloudRun: %v", err)
	}

	const commands = 15 * time.Minute
	finished, err := executor.Execute(context.Background(),
		map[string]string{sandbox.RunnerLocationEnv: "gs://bucket/runs/x"}, commands)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !finished {
		t.Error("finished = false for an execution the API reported complete")
	}

	if len(api.runs) != 1 {
		t.Fatalf("want one execution, got %d", len(api.runs))
	}
	overrides, _ := api.runs[0]["overrides"].(map[string]any)
	raw, _ := overrides["timeout"].(string)
	task, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("overrides.timeout = %q: %v", raw, err)
	}
	if task <= commands {
		t.Errorf("the task may run %s and the commands %s; the runner is killed before it can report a timeout",
			task, commands)
	}
}
