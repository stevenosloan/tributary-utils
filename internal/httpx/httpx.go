// Package httpx holds the HTTP plumbing shared by the gauges and readings
// packages.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

// MaxBody caps how much of a response body Get reads.
const MaxBody = 64 << 20

// Get fetches u as JSON, adding header to the request. status is 0 if no
// response arrived. A non-nil err means the request failed or the body
// couldn't be read; a non-200 status alone isn't an error.
func Get(ctx context.Context, hc *http.Client, u string, header http.Header) (status int, body []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	return resp.StatusCode, body, err
}

// StatusError describes a non-200 response, with the start of its body.
func StatusError(status int, body []byte) error {
	return fmt.Errorf("%d %s: %s", status, http.StatusText(status), Truncate(string(body), 200))
}

// Timeout reports whether a request timed out, either waiting on the server
// or with the server answering 504 itself.
func Timeout(status int, err error) bool {
	var ne net.Error
	return status == http.StatusGatewayTimeout ||
		errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &ne) && ne.Timeout())
}

// Parallel runs fns concurrently, at most limit at a time (no limit if
// limit <= 0), and returns the first error. That error cancels the others, so
// their resulting context errors are not reported.
func Parallel(ctx context.Context, limit int, fns ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if limit <= 0 {
		limit = len(fns)
	}
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		sem   = make(chan struct{}, limit)
	)
	for _, fn := range fns {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := fn(ctx); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		})
	}
	wg.Wait()
	return first
}

// Truncate shortens s to n bytes, marking the cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
