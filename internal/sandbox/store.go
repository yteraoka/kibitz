package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storage "google.golang.org/api/storage/v1"
)

// The names of the three objects one run uses. They live under one prefix so
// that an execution is one directory, and so that cleaning up after a run is
// deleting one thing.
const (
	WorkspaceObject = "workspace.tar.gz"
	RequestObject   = "request.json"
	ResultObject    = "result.json"
)

// Store is where a run's three objects live. The worker writes the first two,
// the runner reads them and writes the third.
//
// It is an interface for one reason: the runner has to be testable, and a
// verification that can only be tested against a bucket is one nobody runs.
type Store interface {
	Get(ctx context.Context, name string) (io.ReadCloser, error)
	Put(ctx context.Context, name string, r io.Reader) error
}

// OpenStore resolves a location into a store.
//
// A "gs://bucket/prefix" location is Cloud Storage, which is how the worker
// and the Cloud Run job pass a tree between them. Anything else is a directory
// on disk, which is how the same code runs under docker compose and in tests.
func OpenStore(ctx context.Context, location string) (Store, error) {
	if bucket, prefix, ok := parseGCS(location); ok {
		return newGCS(ctx, bucket, prefix)
	}
	if strings.TrimSpace(location) == "" {
		return nil, fmt.Errorf("sandbox: no location given")
	}
	return &dirStore{root: location}, nil
}

func parseGCS(location string) (bucket, prefix string, ok bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(location), "gs://")
	if !ok {
		return "", "", false
	}
	bucket, prefix, _ = strings.Cut(rest, "/")
	if bucket == "" {
		return "", "", false
	}
	return bucket, strings.Trim(prefix, "/"), true
}

// dirStore keeps the objects in a directory.
type dirStore struct{ root string }

func (d *dirStore) path(name string) (string, error) {
	// The names are constants in this package, but this is the one place a
	// name becomes a path, so it is checked here rather than trusted.
	if name == "" || strings.ContainsRune(name, filepath.Separator) || strings.Contains(name, "..") {
		return "", fmt.Errorf("sandbox: %q is not an object name", name)
	}
	return filepath.Join(d.root, name), nil
}

func (d *dirStore) Get(_ context.Context, name string) (io.ReadCloser, error) {
	path, err := d.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // checked by path
	if err != nil {
		return nil, fmt.Errorf("sandbox: reading %s: %w", name, err)
	}
	return f, nil
}

func (d *dirStore) Put(_ context.Context, name string, r io.Reader) error {
	path, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	f, err := os.Create(path) //nolint:gosec // checked by path
	if err != nil {
		return fmt.Errorf("sandbox: writing %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("sandbox: writing %s: %w", name, err)
	}
	return nil
}

// gcsStore keeps the objects in Cloud Storage.
//
// The JSON API client is used rather than cloud.google.com/go/storage because
// it is already a dependency of this module. One bucket is all the runner's
// service account can reach, and that is the whole of its access.
type gcsStore struct {
	svc    *storage.Service
	bucket string
	prefix string
}

func newGCS(ctx context.Context, bucket, prefix string, opts ...option.ClientOption) (*gcsStore, error) {
	svc, err := storage.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("sandbox: connecting to Cloud Storage: %w", err)
	}
	return &gcsStore{svc: svc, bucket: bucket, prefix: prefix}, nil
}

func (g *gcsStore) object(name string) string {
	if g.prefix == "" {
		return name
	}
	return g.prefix + "/" + name
}

func (g *gcsStore) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	resp, err := g.svc.Objects.Get(g.bucket, g.object(name)).Context(ctx).Download()
	if err != nil {
		return nil, fmt.Errorf("sandbox: downloading gs://%s/%s: %w", g.bucket, g.object(name), err)
	}
	return resp.Body, nil
}

func (g *gcsStore) Put(ctx context.Context, name string, r io.Reader) error {
	object := &storage.Object{Name: g.object(name)}
	// Resumable uploads are what make a workspace of any size work; the
	// chunk size is the client's default.
	_, err := g.svc.Objects.Insert(g.bucket, object).
		Media(r, googleapi.ContentType("application/octet-stream")).
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("sandbox: uploading gs://%s/%s: %w", g.bucket, g.object(name), err)
	}
	return nil
}
