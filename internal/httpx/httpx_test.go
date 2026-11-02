package httpx

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestParallelLimit(t *testing.T) {
	var running, peak atomic.Int32
	fns := make([]func(context.Context) error, 10)
	for i := range fns {
		fns[i] = func(context.Context) error {
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			running.Add(-1)
			return nil
		}
	}
	if err := Parallel(context.Background(), 3, fns...); err != nil {
		t.Fatal(err)
	}
	if p := peak.Load(); p > 3 {
		t.Errorf("peak concurrency %d, want <= 3", p)
	}
}

func TestParallelFirstError(t *testing.T) {
	boom := errors.New("boom")
	err := Parallel(context.Background(), 0,
		func(context.Context) error { return boom },
		func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
	)
	if err != boom {
		t.Fatalf("got %v, want boom", err)
	}
}
