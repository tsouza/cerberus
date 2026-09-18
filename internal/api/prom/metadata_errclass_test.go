package prom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// metadataErrSentinels is every classified sentinel the Prometheus head
// knows — the errors a ClickHouse call can surface that are NOT a backend
// fault — with the status and errorType the QUERY path answers for each.
func metadataErrSentinels() []struct {
	name   string
	err    error
	status int
	kind   string
} {
	return []struct {
		name   string
		err    error
		status int
		kind   string
	}{
		{"client cancel", fmt.Errorf("drain: %w", context.Canceled), http.StatusServiceUnavailable, ErrCanceled},
		{"context deadline", fmt.Errorf("drain: %w", context.DeadlineExceeded), http.StatusServiceUnavailable, ErrTimeout},
		{"max_execution_time", fmt.Errorf("drain: %w", chclient.ErrQueryTimeout), http.StatusServiceUnavailable, ErrTimeout},
		{"memory limit (241)", &chclient.MemoryLimitError{Limit: 1 << 30}, http.StatusUnprocessableEntity, ErrExecution},
		{"sample budget", &chclient.TooManySamplesError{Limit: 1000}, http.StatusUnprocessableEntity, ErrExecution},
		{"drain byte budget", &chclient.DrainByteBudgetError{Limit: 1 << 20}, http.StatusUnprocessableEntity, ErrExecution},
		{"breaker open", fmt.Errorf("query: %w", chclient.ErrCircuitOpen), http.StatusServiceUnavailable, ErrUnavailable},
		{"shard unavailable", fmt.Errorf("query: %w", chclient.ErrShardUnavailable), http.StatusServiceUnavailable, ErrUnavailable},
		{"stale replica denied", fmt.Errorf("query: %w", chclient.ErrStaleReplicaFallbackDenied), http.StatusServiceUnavailable, ErrUnavailable},
	}
}

// TestClassifyMetadataError_SentinelsMatchTheQueryPath pins that every
// metadata endpoint (/labels, /label/<name>/values, /series, /metadata)
// answers a classified sentinel exactly as the query path does. A Grafana
// label-browser cancel on /api/v1/labels used to come back as a 502
// "internal" — a backend fault on the wire — because fetchLabelNames and
// its siblings wrapped ANY error as StatusBadGateway; reference Prometheus
// routes a cancelled /labels through the same returnAPIError as /query and
// answers 503 errorType=canceled.
func TestClassifyMetadataError_SentinelsMatchTheQueryPath(t *testing.T) {
	t.Parallel()

	for _, tc := range metadataErrSentinels() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got, want *apiError
			if !errors.As(classifyMetadataError(tc.err), &got) {
				t.Fatalf("classifyMetadataError returned no *apiError")
			}
			if !errors.As(classifyEngineError(tc.err), &want) {
				t.Fatalf("classifyEngineError returned no *apiError")
			}
			if got.Status != tc.status || got.Kind != tc.kind {
				t.Errorf("metadata: %d %s; want %d %s", got.Status, got.Kind, tc.status, tc.kind)
			}
			if got.Status != want.Status || got.Kind != want.Kind || got.Reason != want.Reason {
				t.Errorf("metadata answers %d %s (reason %q) where the query path answers %d %s (reason %q); the two paths must classify a sentinel identically",
					got.Status, got.Kind, got.Reason, want.Status, want.Kind, want.Reason)
			}
		})
	}
}

// TestClassifyMetadataError_UnclassifiedIsATransportFault pins the other
// half: a metadata query never carries an `engine: execute:` marker, so an
// unclassified ClickHouse failure IS the upstream transport fault — 502.
func TestClassifyMetadataError_UnclassifiedIsATransportFault(t *testing.T) {
	t.Parallel()

	var got *apiError
	if !errors.As(classifyMetadataError(errors.New("code: 60, DB::Exception: Table otel.otel_metrics_gauge does not exist")), &got) {
		t.Fatal("no *apiError")
	}
	if got.Status != http.StatusBadGateway || got.Kind != ErrInternal {
		t.Errorf("unclassified metadata failure = %d %s; want 502 %s", got.Status, got.Kind, ErrInternal)
	}
}
