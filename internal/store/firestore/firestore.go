// Package firestore implements [store.Store] on Cloud Firestore, which is the
// state store kibitz runs on in production.
//
// Every write that has to be atomic goes through a transaction: taking a lock
// and claiming a delivery are races between workers by definition, and doing
// them with a read followed by a write would let two workers review the same
// pull request.
package firestore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gcp "cloud.google.com/go/firestore"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yteraoka/kibitz/internal/store"
)

// DefaultCollection holds every key kibitz stores.
const DefaultCollection = "kibitz"

// Config configures the store.
type Config struct {
	ProjectID string
	// Database is the Firestore database id. Empty means "(default)".
	Database string
	// Collection holds the documents. Empty means [DefaultCollection].
	Collection string
}

// Store is a Firestore-backed [store.Store].
type Store struct {
	client     *gcp.Client
	collection string
	// now is replaceable in tests.
	now func() time.Time
}

// document is one stored key. ExpiresAt is also what a Firestore TTL policy
// should be configured on, so that expired documents are actually deleted
// rather than just ignored on read (see deploy/terraform).
type document struct {
	Value     []byte    `firestore:"value,omitempty"`
	Count     int64     `firestore:"count,omitempty"`
	Owner     string    `firestore:"owner,omitempty"`
	ExpiresAt time.Time `firestore:"expiresAt,omitempty"`
	UpdatedAt time.Time `firestore:"updatedAt"`
}

func (d document) expired(now time.Time) bool {
	return !d.ExpiresAt.IsZero() && now.After(d.ExpiresAt)
}

// New connects to Firestore. It honours FIRESTORE_EMULATOR_HOST.
func New(ctx context.Context, cfg Config, opts ...option.ClientOption) (*Store, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("firestore: project id is required")
	}
	database := cfg.Database
	if database == "" {
		database = "(default)"
	}
	collection := cfg.Collection
	if collection == "" {
		collection = DefaultCollection
	}

	client, err := gcp.NewClientWithDatabase(ctx, cfg.ProjectID, database, opts...)
	if err != nil {
		return nil, fmt.Errorf("firestore: connecting: %w", err)
	}
	return &Store{client: client, collection: collection, now: time.Now}, nil
}

// MarkProcessed implements [store.Store].
func (s *Store) MarkProcessed(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	doc := s.doc(key)
	first := false

	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *gcp.Transaction) error {
		existing, err := readDoc(tx, doc)
		if err != nil {
			return err
		}
		now := s.now()
		if existing != nil && !existing.expired(now) {
			first = false
			return nil
		}
		first = true
		return tx.Set(doc, document{
			Value:     []byte("1"),
			ExpiresAt: expiry(now, ttl),
			UpdatedAt: now,
		})
	})
	if err != nil {
		return false, fmt.Errorf("firestore: marking %s: %w", key, err)
	}
	return first, nil
}

// AcquireLock implements [store.Store].
func (s *Store) AcquireLock(ctx context.Context, key string, ttl time.Duration) (store.Lease, error) {
	doc := s.doc(key)
	owner := newOwner()

	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *gcp.Transaction) error {
		existing, err := readDoc(tx, doc)
		if err != nil {
			return err
		}
		now := s.now()
		if existing != nil && !existing.expired(now) {
			return store.ErrLocked
		}
		return tx.Set(doc, document{
			Owner:     owner,
			ExpiresAt: expiry(now, ttl),
			UpdatedAt: now,
		})
	})
	switch {
	case errors.Is(err, store.ErrLocked):
		return nil, store.ErrLocked
	case err != nil:
		return nil, fmt.Errorf("firestore: locking %s: %w", key, err)
	}
	return &lease{store: s, key: key, owner: owner}, nil
}

// Get implements [store.Store].
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	snapshot, err := s.doc(key).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("firestore: reading %s: %w", key, err)
	}

	var d document
	if err := snapshot.DataTo(&d); err != nil {
		return nil, fmt.Errorf("firestore: decoding %s: %w", key, err)
	}
	if d.expired(s.now()) {
		return nil, store.ErrNotFound
	}
	return d.Value, nil
}

// Put implements [store.Store].
func (s *Store) Put(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	now := s.now()
	_, err := s.doc(key).Set(ctx, document{
		Value:     value,
		ExpiresAt: expiry(now, ttl),
		UpdatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("firestore: writing %s: %w", key, err)
	}
	return nil
}

// Delete implements [store.Store].
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.doc(key).Delete(ctx); err != nil {
		return fmt.Errorf("firestore: deleting %s: %w", key, err)
	}
	return nil
}

// Incr implements [store.Store].
func (s *Store) Incr(ctx context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	doc := s.doc(key)
	var total int64

	err := s.client.RunTransaction(ctx, func(_ context.Context, tx *gcp.Transaction) error {
		existing, err := readDoc(tx, doc)
		if err != nil {
			return err
		}
		now := s.now()

		total = delta
		expires := expiry(now, ttl)
		if existing != nil && !existing.expired(now) {
			total = existing.Count + delta
			// The window keeps its original expiry: a counter that renewed its
			// own deadline on every increment would never reset.
			expires = existing.ExpiresAt
		}
		return tx.Set(doc, document{Count: total, ExpiresAt: expires, UpdatedAt: now})
	})
	if err != nil {
		return 0, fmt.Errorf("firestore: incrementing %s: %w", key, err)
	}
	return total, nil
}

// Close implements [store.Store].
func (s *Store) Close() error { return s.client.Close() }

func (s *Store) doc(key string) *gcp.DocumentRef {
	return s.client.Collection(s.collection).Doc(docID(key))
}

// docID makes a key safe as a Firestore document id, which may not contain a
// slash. The mapping stays readable so that a dump of the collection can still
// be understood.
func docID(key string) string {
	return strings.ReplaceAll(key, "/", "~")
}

// readDoc returns nil when the document does not exist, so callers can treat
// "absent" and "expired" the same way.
func readDoc(tx *gcp.Transaction, doc *gcp.DocumentRef) (*document, error) {
	snapshot, err := tx.Get(doc)
	if status.Code(err) == codes.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var d document
	if err := snapshot.DataTo(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func expiry(now time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return now.Add(ttl)
}

type lease struct {
	store *Store
	key   string
	owner string
}

// Extend pushes the expiry out, but only while this lease still owns the lock.
func (l *lease) Extend(ctx context.Context, ttl time.Duration) error {
	doc := l.store.doc(l.key)

	err := l.store.client.RunTransaction(ctx, func(_ context.Context, tx *gcp.Transaction) error {
		existing, err := readDoc(tx, doc)
		if err != nil {
			return err
		}
		now := l.store.now()
		if existing == nil || existing.Owner != l.owner || existing.expired(now) {
			return store.ErrLeaseLost
		}
		return tx.Set(doc, document{
			Owner:     l.owner,
			ExpiresAt: expiry(now, ttl),
			UpdatedAt: now,
		})
	})
	if errors.Is(err, store.ErrLeaseLost) {
		return store.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("firestore: extending the lease on %s: %w", l.key, err)
	}
	return nil
}

// Release deletes the lock, but only if this lease still owns it: deleting
// someone else's lock would let a third worker in.
func (l *lease) Release(ctx context.Context) error {
	doc := l.store.doc(l.key)

	err := l.store.client.RunTransaction(ctx, func(_ context.Context, tx *gcp.Transaction) error {
		existing, err := readDoc(tx, doc)
		if err != nil {
			return err
		}
		if existing == nil || existing.Owner != l.owner {
			return store.ErrLeaseLost
		}
		return tx.Delete(doc)
	})
	if errors.Is(err, store.ErrLeaseLost) {
		return store.ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("firestore: releasing the lease on %s: %w", l.key, err)
	}
	return nil
}
