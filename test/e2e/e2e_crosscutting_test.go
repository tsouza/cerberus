//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestUnknownRoute — unknown paths return 404 (Go's net/http default
// for unrouted patterns), not a generic 500. Cerberus shouldn't
// accidentally route them to a handler that crashes on unexpected
// shape.
func TestUnknownRoute(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/this/does/not/exist", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

// TestConcurrentQueries — 20 concurrent /api/v1/query?query=up
// requests all return 200. Catches connection-pool exhaustion or
// race conditions in middleware (e.g., the chMillisCounter context
// value).
func TestConcurrentQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL()+"/api/v1/query?query=up", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errs[idx] = err
				return
			}
			codes[idx] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("req %d: %v", i, err)
		}
	}
	for i, code := range codes {
		if code != http.StatusOK && code != 0 { // 0 means errs[i] was set above
			t.Errorf("req %d: status %d, want 200", i, code)
		}
	}
}

