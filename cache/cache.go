// Package cache defines a small byte store with expiry, so a client can keep
// its cached results in memory, or in a shared or remote store a caller
// supplies.
package cache

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Store holds byte values with an expiry. Implementations must be safe for
// concurrent use. Callers treat errors as a miss, never as fatal.
type Store interface {
	// Get returns the value for key. ok is false if there is none or it has
	// expired.
	Get(ctx context.Context, key string) (val []byte, ok bool, err error)
	// Set stores val under key for ttl. A ttl <= 0 stores nothing and removes
	// any value already held under key.
	Set(ctx context.Context, key string, val []byte, ttl time.Duration) error
}

// sweepEvery is the least time between Memory sweeps of expired entries.
const sweepEvery = time.Minute

// Memory is a Store kept in process memory. It is safe for concurrent use.
//
// Expired entries read as a miss at once, and Get removes the one it finds. Set
// removes the rest, sweeping the whole map at most once per minute, so the cost of a
// sweep is spread over many writes.
type Memory struct {
	now func() time.Time

	mu        sync.Mutex
	items     map[string]memItem
	lastSweep time.Time
}

type memItem struct {
	val     []byte
	expires time.Time
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return newMemory(time.Now)
}

func newMemory(now func() time.Time) *Memory {
	return &Memory{now: now, items: map[string]memItem{}, lastSweep: now()}
}

// Get returns a copy of the value for key; an expired value is a miss.
func (m *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.items[key]
	if !ok {
		return nil, false, nil
	}
	if !m.now().Before(it.expires) {
		delete(m.items, key)
		return nil, false, nil
	}
	return slices.Clone(it.val), true, nil
}

// Set stores a copy of val for ttl. A ttl <= 0 stores nothing and removes any
// value already held under key.
func (m *Memory) Set(_ context.Context, key string, val []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	if now.Sub(m.lastSweep) >= sweepEvery {
		for k, it := range m.items {
			if !now.Before(it.expires) {
				delete(m.items, k)
			}
		}
		m.lastSweep = now
	}
	if ttl <= 0 {
		delete(m.items, key)
		return nil
	}
	m.items[key] = memItem{val: slices.Clone(val), expires: now.Add(ttl)}
	return nil
}

// None is a Store that never stores anything: Get always misses and Set does
// nothing. Pass it to disable caching.
var None Store = none{}

type none struct{}

func (none) Get(context.Context, string) ([]byte, bool, error)        { return nil, false, nil }
func (none) Set(context.Context, string, []byte, time.Duration) error { return nil }
