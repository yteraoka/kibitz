package transcript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storage "google.golang.org/api/storage/v1"
)

// ErrNotFound reports that no record has the id asked for.
var ErrNotFound = errors.New("transcript: no such record")

// maxRecordBytes bounds what is read back. A record is written under
// [MaxEventBytes] of events plus its prompts, so anything far larger is not
// one kibitz wrote.
const maxRecordBytes = 64 << 20

// Store is where records live. The worker writes them and the server reads
// them; neither does the other's half.
type Store interface {
	Put(ctx context.Context, rec *Record) error
	Get(ctx context.Context, id string) (*Record, error)
}

// OpenStore resolves a location into a store: "gs://bucket/prefix" for Cloud
// Storage, anything else a directory on disk, which is how the same code runs
// under docker compose and in tests.
func OpenStore(ctx context.Context, location string, opts ...option.ClientOption) (Store, error) {
	location = strings.TrimSpace(location)
	if rest, ok := strings.CutPrefix(location, "gs://"); ok {
		bucket, prefix, _ := strings.Cut(rest, "/")
		if bucket == "" {
			return nil, fmt.Errorf("transcript: %q names no bucket", location)
		}
		svc, err := storage.NewService(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("transcript: connecting to Cloud Storage: %w", err)
		}
		return &gcsStore{svc: svc, bucket: bucket, prefix: strings.Trim(prefix, "/")}, nil
	}
	if location == "" {
		return nil, errors.New("transcript: no location given")
	}
	return &DirStore{Root: location}, nil
}

func objectName(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("transcript: %q is not a record id", id)
	}
	return id + ".json", nil
}

func decode(r io.Reader) (*Record, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRecordBytes+1))
	if err != nil {
		return nil, fmt.Errorf("transcript: reading: %w", err)
	}
	if len(data) > maxRecordBytes {
		return nil, errors.New("transcript: record is too large")
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("transcript: decoding: %w", err)
	}
	return &rec, nil
}

// DirStore keeps records as files in a directory.
type DirStore struct{ Root string }

// Put implements [Store].
func (d *DirStore) Put(_ context.Context, rec *Record) error {
	name, err := objectName(rec.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("transcript: encoding: %w", err)
	}
	if err := os.MkdirAll(d.Root, 0o750); err != nil {
		return fmt.Errorf("transcript: %w", err)
	}
	if err := os.WriteFile(filepath.Join(d.Root, name), data, 0o600); err != nil {
		return fmt.Errorf("transcript: writing %s: %w", name, err)
	}
	return nil
}

// Get implements [Store].
func (d *DirStore) Get(_ context.Context, id string) (*Record, error) {
	name, err := objectName(id)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	f, err := os.Open(filepath.Join(d.Root, name)) //nolint:gosec // the name is a checked id
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("transcript: reading %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	return decode(f)
}

// gcsStore keeps records in Cloud Storage, through the JSON API client the
// module already depends on.
type gcsStore struct {
	svc    *storage.Service
	bucket string
	prefix string
}

func (g *gcsStore) object(name string) string {
	if g.prefix == "" {
		return name
	}
	return g.prefix + "/" + name
}

func (g *gcsStore) Put(ctx context.Context, rec *Record) error {
	name, err := objectName(rec.ID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("transcript: encoding: %w", err)
	}
	object := &storage.Object{
		Name:        g.object(name),
		ContentType: "application/json",
		Metadata: map[string]string{
			"repository": rec.Repository,
			"ref":        rec.Ref,
			"event_id":   rec.EventID,
		},
	}
	_, err = g.svc.Objects.Insert(g.bucket, object).
		Media(bytes.NewReader(data), googleapi.ContentType("application/json")).
		Context(ctx).
		Do()
	if err != nil {
		return fmt.Errorf("transcript: uploading gs://%s/%s: %w", g.bucket, g.object(name), err)
	}
	return nil
}

func (g *gcsStore) Get(ctx context.Context, id string) (*Record, error) {
	name, err := objectName(id)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	resp, err := g.svc.Objects.Get(g.bucket, g.object(name)).Context(ctx).Download()
	if err != nil {
		var apiErr *googleapi.Error
		if errors.As(err, &apiErr) && apiErr.Code == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("transcript: downloading gs://%s/%s: %w", g.bucket, g.object(name), err)
	}
	defer func() { _ = resp.Body.Close() }()
	return decode(resp.Body)
}
