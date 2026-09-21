// Package memory is an in-process [store.Store] for development and tests.
// Nothing survives a restart, which is exactly why it is not for production:
// a worker that forgets what it has done reviews everything twice.
package memory

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/yteraoka/kibitz/internal/store"
)

// Store is an in-memory implementation.
type Store struct {
	// Now is the clock, replaceable in tests so expiry can be exercised
	// without sleeping.
	Now func() time.Time

	mu      sync.Mutex
	entries map[string]entry
	nextID  int64
}

type entry struct {
	value   []byte
	expires time.Time
	// owner is set on lock entries.
	owner string
}

func (e entry) expired(now time.Time) bool {
	return !e.expires.IsZero() && now.After(e.expires)
}

// New creates an empty store.
func New() *Store {
	return &Store{Now: time.Now, entries: make(map[string]entry)}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// lookup returns a live entry, treating an expired one as absent. The caller
// must hold the lock.
func (s *Store) lookup(key string) (entry, bool) {
	e, ok := s.entries[key]
	if !ok || e.expired(s.now()) {
		delete(s.entries, key)
		return entry{}, false
	}
	return e, true
}

// MarkProcessed implements [store.Store].
func (s *Store) MarkProcessed(_ context.Context, key string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.lookup(key); ok {
		return false, nil
	}
	s.entries[key] = entry{value: []byte("1"), expires: s.expiry(ttl)}
	return true, nil
}

// AcquireLock implements [store.Store].
func (s *Store) AcquireLock(_ context.Context, key string, ttl time.Duration) (store.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.lookup(key); ok {
		return nil, store.ErrLocked
	}

	s.nextID++
	owner := strconv.FormatInt(s.nextID, 10)
	s.entries[key] = entry{expires: s.expiry(ttl), owner: owner}
	return &lease{store: s, key: key, owner: owner}, nil
}

// Get implements [store.Store].
func (s *Store) Get(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.lookup(key)
	if !ok {
		return nil, store.ErrNotFound
	}
	return append([]byte(nil), e.value...), nil
}

// Put implements [store.Store].
func (s *Store) Put(_ context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.entries[key] = entry{value: append([]byte(nil), value...), expires: s.expiry(ttl)}
	return nil
}

// Delete implements [store.Store].
func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.entries, key)
	return nil
}

// Incr implements [store.Store].
func (s *Store) Incr(_ context.Context, key string, delta int64, ttl time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var current int64
	e, ok := s.lookup(key)
	if ok {
		current, _ = strconv.ParseInt(string(e.value), 10, 64)
	} else {
		e.expires = s.expiry(ttl)
	}
	current += delta
	s.entries[key] = entry{value: []byte(strconv.FormatInt(current, 10)), expires: e.expires}
	return current, nil
}

// Close implements [store.Store].
func (s *Store) Close() error { return nil }

func (s *Store) expiry(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return s.now().Add(ttl)
}

// Len reports how many live entries the store holds, for tests.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := 0
	for key := range s.entries {
		if _, ok := s.lookup(key); ok {
			n++
		}
	}
	return n
}

type lease struct {
	store *Store
	key   string
	owner string
}

func (l *lease) Extend(_ context.Context, ttl time.Duration) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()

	e, ok := l.store.lookup(l.key)
	if !ok || e.owner != l.owner {
		return store.ErrLeaseLost
	}
	e.expires = l.store.expiry(ttl)
	l.store.entries[l.key] = e
	return nil
}

func (l *lease) Release(_ context.Context) error {
	l.store.mu.Lock()
	defer l.store.mu.Unlock()

	e, ok := l.store.lookup(l.key)
	if !ok || e.owner != l.owner {
		return store.ErrLeaseLost
	}
	delete(l.store.entries, l.key)
	return nil
}
