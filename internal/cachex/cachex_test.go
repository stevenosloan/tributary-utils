package cachex

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stevenosloan/tributary-utils/cache"
)

func TestPrefix(t *testing.T) {
	for base, want := range map[string]string{
		"https://API.Example.test/v1": "ns:v1:https://api.example.test:",
		"http://127.0.0.1:8080":       "ns:v1:http://127.0.0.1:8080:",
	} {
		if got := Prefix("ns", "v1", base); got != want {
			t.Errorf("Prefix(%q) = %q, want %q", base, got, want)
		}
	}
	if Prefix("ns", "v1", "http://a") == Prefix("ns", "v1", "https://a") {
		t.Error("prefix ignores scheme")
	}
}

type failing struct{}

func (failing) Get(context.Context, string) ([]byte, bool, error) { return nil, false, errors.New("x") }
func (failing) Set(context.Context, string, []byte, time.Duration) error {
	return errors.New("x")
}

func TestGetSet(t *testing.T) {
	ctx := context.Background()
	m := cache.NewMemory()
	Set(ctx, m, "k", []int{1, 2}, time.Hour)
	var got []int
	if !Get(ctx, m, "k", &got) || len(got) != 2 {
		t.Errorf("got %v", got)
	}
	m.Set(ctx, "bad", []byte("{"), time.Hour)
	if Get(ctx, m, "bad", &got) || Get(ctx, m, "none", &got) {
		t.Error("undecodable or missing value hit")
	}
	if Get(ctx, failing{}, "k", &got) {
		t.Error("store error hit")
	}
	Set(ctx, failing{}, "k", 1, time.Hour) // must not panic
}
