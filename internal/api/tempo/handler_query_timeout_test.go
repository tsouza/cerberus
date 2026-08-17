package tempo_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// configuredQueryTimeout is the Handler.QueryTimeout every test in this
// file installs — small enough that the deadline fires well inside a test
// timeout, but large enough that ordinary scheduling jitter cannot make an
// UNCAPPED request look capped by accident.
const configuredQueryTimeout = 50 * time.Millisecond

// hangSafetyNet is how long hangingQuerier waits for ctx cancellation
// before giving up and returning its own error. It exists purely so a
// wiring regression (the deadline never gets installed) fails the test
// promptly instead of hanging the suite — a real deployment has no such
// ceiling.
const hangSafetyNet = 5 * time.Second

// unblockBound is the ceiling TestHungQuery_UnblocksAtConfiguredTimeout
// allows between issuing the request and getting a response. It sits well
// above configuredQueryTimeout (scheduling slack) and well below
// hangSafetyNet, so passing this bound is only possible if the context
// deadline — not the safety net — is what unblocked the handler.
const unblockBound = 2 * time.Second

// hangingQuerier never returns on its own: every Query / QueryStrings call
// blocks until its context is cancelled (the property issue #2302 is
// about — a genuinely hung ClickHouse round-trip must still release the
// handler) or hangSafetyNet elapses, whichever comes first.
type hangingQuerier struct{}

func (hangingQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(hangSafetyNet):
		return nil, errors.New("hangingQuerier: safety net elapsed without ctx cancellation")
	}
}

func (hangingQuerier) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(hangSafetyNet):
		return nil, errors.New("hangingQuerier: safety net elapsed without ctx cancellation")
	}
}

// newTimeoutServer stands up a Tempo handler with QueryTimeout wired to
// configuredQueryTimeout, mirroring newServerWithTimeout in the prom head's
// handler_query_timeout_test.go.
func newTimeoutServer(q tempo.Querier) *testServer {
	h := tempo.New(q, schema.DefaultOTelTraces(), "v1.0.0-test", nil)
	h.QueryTimeout = configuredQueryTimeout
	mux := http.NewServeMux()
	h.Mount(mux)
	return &testServer{Server: httptest.NewServer(mux)}
}

// tempoTimeoutEntrypoint names one query-handling Tempo route this file
// exercises and the URL that reaches it. It exists so both
// TestHungQuery_UnblocksAtConfiguredTimeout and
// TestEveryQueryEntrypoint_InstallsConfiguredDeadline run the identical
// route list — the reliability property and the coverage-completeness
// property are pinned against the same set of entrypoints, not two
// independently-typed lists that can silently drift apart.
type tempoTimeoutEntrypoint struct {
	name string
	url  func(base string) string
}

// tempoTimeoutEntrypoints is every Tempo HTTP route that reaches
// ClickHouse — search, search/recent, trace-by-id (v1 + v2), the V1 + V2
// tag-discovery routes, and both metrics routes. handleEcho /
// handleVersion / handleStatusBuildinfo are deliberately excluded: they
// never touch h.Client or h.Engine, so there is nothing for a query
// timeout to bound.
func tempoTimeoutEntrypoints() []tempoTimeoutEntrypoint {
	metricsParams := func(extra map[string]string) string {
		vals := url.Values{}
		vals.Set("q", "{} | rate()")
		vals.Set("start", fixtureStartUnix)
		vals.Set("end", fixtureEndUnix)
		for k, v := range extra {
			vals.Set(k, v)
		}
		return vals.Encode()
	}
	return []tempoTimeoutEntrypoint{
		{"search", func(base string) string { return base + "/api/search?q=%7B%7D" }},
		{"search/recent", func(base string) string { return base + "/api/search/recent" }},
		{"traces/{id}", func(base string) string { return base + "/api/traces/0123456789abcdef" }},
		{"v2/traces/{id}", func(base string) string { return base + "/api/v2/traces/0123456789abcdef" }},
		{"search/tags", func(base string) string { return base + "/api/search/tags" }},
		{"v2/search/tags", func(base string) string { return base + "/api/v2/search/tags" }},
		{"search/tag/{name}/values", func(base string) string { return base + "/api/search/tag/name/values" }},
		{"v2/search/tag/{name}/values", func(base string) string { return base + "/api/v2/search/tag/name/values" }},
		{"metrics/query_range", func(base string) string {
			return base + "/api/metrics/query_range?" + metricsParams(map[string]string{"step": "60s"})
		}},
		{"metrics/query", func(base string) string { return base + "/api/metrics/query?" + metricsParams(nil) }},
	}
}

// TestHungQuery_UnblocksAtConfiguredTimeout is the core reliability
// property issue #2302 is about: with every Tempo entrypoint wired to
// h.QueryTimeout, a ClickHouse round-trip that never returns on its own
// still unblocks the HTTP handler at roughly the configured budget — not
// at hangSafetyNet, and not never. Before this fix Tempo had no
// context.WithTimeout anywhere, so hangingQuerier would have hung every
// one of these requests for the full hangSafetyNet.
func TestHungQuery_UnblocksAtConfiguredTimeout(t *testing.T) {
	t.Parallel()

	srv := newTimeoutServer(hangingQuerier{})
	t.Cleanup(srv.Close)

	for _, ep := range tempoTimeoutEntrypoints() {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()

			before := time.Now()
			resp, err := http.Get(ep.url(srv.URL))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			elapsed := time.Since(before)
			body := readBody(t, resp)

			// The context-deadline backstop fires as context.DeadlineExceeded,
			// which errclass.go's ClassifyErr maps to ErrClassUnavailable (503)
			// on the engine-routed routes and — via tagsErrStatus's identical
			// sentinel handling — the same 503 on the metadata routes that call
			// h.Client directly. A 503 here is itself part of the proof: it can
			// only be the deadline firing, not some unrelated success path.
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status: got %d, want %d (context deadline exceeded); body=%s",
					resp.StatusCode, http.StatusServiceUnavailable, body)
			}
			if elapsed > unblockBound {
				t.Fatalf("handler took %s to respond; want well under %s — "+
					"the %s safety net firing (not the %s configured timeout) means "+
					"the context deadline was never installed on this entrypoint's ctx",
					elapsed, unblockBound, hangSafetyNet, configuredQueryTimeout)
			}
		})
	}
}

// deadlineCapturingQuerier records whether the context it was called with
// carried a deadline, and if so, the absolute deadline time — so a test
// can assert the handler installed exactly the configured default (no
// ?timeout= is sent in this file, matching the decision that Tempo has no
// `?timeout=` convention of its own; see applyQueryTimeout's doc-comment).
//
// The deadline is recorded as an absolute time.Time rather than a
// time.Until(dl) delta computed here: under a full, heavily parallel test
// run this method can itself be scheduled well after the context was
// created (handler parsing/lowering plus Go scheduler contention from
// every other concurrent subtest), which would make a delta captured at
// call time read anywhere from "smaller than expected" to outright
// negative even though the deadline the handler installed was exactly on
// budget. Recording the absolute deadline and letting the caller diff it
// against ITS OWN pre-request timestamp isolates the assertion from that
// scheduling jitter.
type deadlineCapturingQuerier struct {
	mu          sync.Mutex
	sawDeadline bool
	deadline    time.Time
}

func (q *deadlineCapturingQuerier) note(ctx context.Context) {
	q.mu.Lock()
	defer q.mu.Unlock()
	dl, ok := ctx.Deadline()
	q.sawDeadline = ok
	if ok {
		q.deadline = dl
	}
}

func (q *deadlineCapturingQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	q.note(ctx)
	return nil, nil
}

func (q *deadlineCapturingQuerier) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	q.note(ctx)
	return nil, nil
}

// TestEveryQueryEntrypoint_InstallsConfiguredDeadline is the wiring-
// completeness half of the #2302 fix: every route in
// tempoTimeoutEntrypoints must derive its query context from
// applyQueryTimeout, not from the bare r.Context(). A route that still
// used r.Context() would pass with no deadline at all — sawDeadline would
// be false — so this fails loudly for exactly the entrypoints the scope
// list in #2302 named (metrics_query_range.go, metrics_query_instant.go,
// search_tags.go, search_tag_values.go, and handler.go's /api/search,
// /api/search/recent and /api/traces/{id} paths).
func TestEveryQueryEntrypoint_InstallsConfiguredDeadline(t *testing.T) {
	t.Parallel()

	for _, ep := range tempoTimeoutEntrypoints() {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()

			q := &deadlineCapturingQuerier{}
			srv := newTimeoutServer(q)
			t.Cleanup(srv.Close)

			before := time.Now()
			resp, err := http.Get(ep.url(srv.URL))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)

			q.mu.Lock()
			sawDeadline, deadline := q.sawDeadline, q.deadline
			q.mu.Unlock()

			if !sawDeadline {
				t.Fatalf("query ctx carried no deadline; want one from the configured "+
					"QueryTimeout default (status=%d body=%s)", resp.StatusCode, body)
			}
			// The deadline must sit close to configuredQueryTimeout out from
			// request start — comfortably inside unblockBound's slack, well
			// below hangSafetyNet — proving it is the configured default and
			// not some unrelated (e.g. accidentally huge) value. Diffed against
			// this goroutine's own `before` (not a delta captured inside the
			// querier) so scheduling delay between request start and the
			// querier actually running cannot manufacture a false negative.
			delta := deadline.Sub(before)
			if delta <= 0 || delta > unblockBound {
				t.Fatalf("deadline %s out from request start; want roughly %s (the configured default)",
					delta, configuredQueryTimeout)
			}
		})
	}
}
