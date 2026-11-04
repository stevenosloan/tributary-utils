package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTest() (*Memory, *clock) {
	c := &clock{t: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)}
	return newMemory(c.now), c
}

func TestHitMiss(t *testing.T) {
	m, _ := newTest()
	if _, ok, err := m.Get(ctx, "a"); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	if err := m.Set(ctx, "a", []byte("one"), time.Minute); err != nil {
		t.Fatal(err)
	}
	v, ok, err := m.Get(ctx, "a")
	if !ok || err != nil || string(v) != "one" {
		t.Fatalf("Get = %q, %v, %v", v, ok, err)
	}
	if _, ok, _ := m.Get(ctx, "b"); ok {
		t.Error("unset key hit")
	}
}

func TestExpiry(t *testing.T) {
	m, c := newTest()
	m.Set(ctx, "a", []byte("x"), time.Minute)
	c.advance(59 * time.Second)
	if _, ok, _ := m.Get(ctx, "a"); !ok {
		t.Error("miss before expiry")
	}
	c.advance(time.Second)
	if _, ok, _ := m.Get(ctx, "a"); ok {
		t.Error("hit at expiry")
	}
}

func TestNonPositiveTTL(t *testing.T) {
	m, _ := newTest()
	m.Set(ctx, "a", []byte("x"), 0)
	m.Set(ctx, "b", []byte("x"), -time.Second)
	for _, k := range []string{"a", "b"} {
		if _, ok, _ := m.Get(ctx, k); ok {
			t.Errorf("%s stored with ttl <= 0", k)
		}
	}
	m.Set(ctx, "c", []byte("x"), time.Minute)
	m.Set(ctx, "c", []byte("y"), 0)
	if _, ok, _ := m.Get(ctx, "c"); ok {
		t.Error("ttl <= 0 left the old value")
	}
}

func TestSweep(t *testing.T) {
	m, c := newTest()
	m.Set(ctx, "old1", []byte("x"), time.Second)
	m.Set(ctx, "old2", []byte("x"), time.Second)
	m.Set(ctx, "live", []byte("x"), time.Hour)

	// Expired but not yet swept: still held, though a miss.
	c.advance(10 * time.Second)
	m.Set(ctx, "other", []byte("x"), time.Hour)
	if n := len(m.items); n != 4 {
		t.Errorf("swept too soon: %d items", n)
	}

	c.advance(sweepEvery)
	m.Set(ctx, "next", []byte("x"), time.Hour)
	if _, ok := m.items["old1"]; ok {
		t.Error("old1 not swept")
	}
	if _, ok := m.items["old2"]; ok {
		t.Error("old2 not swept")
	}
	if n := len(m.items); n != 3 {
		t.Errorf("%d items after sweep, want 3 (live, other, next)", n)
	}
}

func TestCopySafety(t *testing.T) {
	m, _ := newTest()
	in := []byte("abc")
	m.Set(ctx, "a", in, time.Minute)
	in[0] = 'X'
	got, _, _ := m.Get(ctx, "a")
	if string(got) != "abc" {
		t.Errorf("Set kept the caller's slice: %q", got)
	}
	got[1] = 'Y'
	again, _, _ := m.Get(ctx, "a")
	if string(again) != "abc" {
		t.Errorf("Get returned the stored slice: %q", again)
	}
}

func TestNone(t *testing.T) {
	if err := None.Set(ctx, "a", []byte("x"), time.Hour); err != nil {
		t.Fatal(err)
	}
	if v, ok, err := None.Get(ctx, "a"); ok || err != nil || v != nil {
		t.Errorf("None.Get = %q, %v, %v", v, ok, err)
	}
}

func TestConcurrent(t *testing.T) {
	m := NewMemory()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for j := range 200 {
				k := fmt.Sprint("k", j%10)
				m.Set(ctx, k, []byte{byte(i)}, time.Minute)
				m.Get(ctx, k)
			}
		})
	}
	wg.Wait()
}

var _ Store = (*Memory)(nil)

func TestGetDeletesExpired(t *testing.T) {
	m, c := newTest()
	m.Set(ctx, "a", []byte("x"), time.Second)
	c.advance(time.Second)
	if _, ok, _ := m.Get(ctx, "a"); ok {
		t.Fatal("hit after expiry")
	}
	if _, ok := m.items["a"]; ok {
		t.Error("Get left the expired entry")
	}
}
