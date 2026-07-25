package seed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// readyPoll is the gap between readiness polls. The distroless reference
// images carry no probe binary, so compose cannot express a healthcheck for
// them and readiness is proven from the outside instead.
const readyPoll = time.Second

// readyRequestTimeout bounds a single readiness request so a hung listener
// costs one poll interval rather than the whole budget.
const readyRequestTimeout = 5 * time.Second

// WaitHTTPOK polls url until it answers 200, or timeout elapses (or ctx is
// cancelled). On timeout it returns the last observed failure, so a CI log
// names why the endpoint never came up instead of reporting a bare deadline.
func WaitHTTPOK(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(readyPoll)
	defer ticker.Stop()
	var last error
	for {
		last = probeHTTPOK(ctx, url)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s: %w", url, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// probeHTTPOK issues one readiness request, returning nil only on a 200.
func probeHTTPOK(ctx context.Context, url string) error {
	reqCtx, cancel := context.WithTimeout(ctx, readyRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	if readErr != nil {
		return fmt.Errorf("read %s body: %w", url, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
