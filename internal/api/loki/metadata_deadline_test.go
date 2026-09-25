package loki_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// hangingQuerier models a ClickHouse that never answers: every method
// blocks until the ctx it was handed is done and returns ctx.Err(). A
// handler that installs no deadline on that ctx therefore hangs for the
// life of the request — holding its admit slot and its pooled connection —
// which is what the tests below make visible as a client-side timeout.
type hangingQuerier struct{}

func hang(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (hangingQuerier) Query(ctx context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return nil, hang(ctx)
}

func (hangingQuerier) QueryStrings(ctx context.Context, _ string, _ ...any) ([]string, error) {
	return nil, hang(ctx)
}

func (hangingQuerier) QueryDetectedFieldRows(ctx context.Context, _ string, _ ...any) ([]chclient.DetectedFieldRow, error) {
	return nil, hang(ctx)
}

func (hangingQuerier) QueryTimestampedLines(ctx context.Context, _ string, _ ...any) ([]chclient.TimestampedLine, error) {
	return nil, hang(ctx)
}

func (hangingQuerier) QueryIndexStats(ctx context.Context, _ string, _ ...any) (chclient.IndexStatsRow, error) {
	return chclient.IndexStatsRow{}, hang(ctx)
}

func (hangingQuerier) QueryIndexVolume(ctx context.Context, _ string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	return nil, hang(ctx)
}

func (hangingQuerier) QueryLabelSets(ctx context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, hang(ctx)
}

func (hangingQuerier) QueryLabelCardinalities(ctx context.Context, _ string, _ ...any) ([]chclient.LabelCardinalityRow, error) {
	return nil, hang(ctx)
}

// metadataDeadlineBudget is the configured QueryTimeout the deadline
// tests run under: short enough to keep the test fast, long enough that
// the request reaches the querier before it fires.
const metadataDeadlineBudget = 100 * time.Millisecond

// metadataDeadlineClientTimeout bounds how long a test waits for the
// handler; a handler that installs no deadline never answers a hanging
// backend, so this is what turns that hang into a failure.
const metadataDeadlineClientTimeout = 5 * time.Second

// TestMetadataEndpoints_HangingBackendReleasesAtTheQueryTimeout pins that
// every Loki metadata endpoint runs its ClickHouse round trip under the
// configured QueryTimeout — the same Go-side watchdog /query and
// /query_range install — so a hung backend answers 503 errorType=timeout
// at the deadline and the handler returns (releasing its admit slot)
// instead of blocking until the driver's own read timeout. Before this,
// only the two query routes installed the budget; every metadata route
// ran on the bare request context.
func TestMetadataEndpoints_HangingBackendReleasesAtTheQueryTimeout(t *testing.T) {
	t.Parallel()

	h := loki.New(hangingQuerier{}, schema.DefaultOTelLogs(), nil)
	h.QueryTimeout = metadataDeadlineBudget
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: metadataDeadlineClientTimeout}

	const window = "start=1717995600&end=1717999200"
	routes := []string{
		"/loki/api/v1/labels?" + window,
		"/loki/api/v1/label/job/values?" + window,
		"/loki/api/v1/series?match%5B%5D=%7Bjob%3D%22api%22%7D&" + window,
		"/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&" + window,
		"/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&" + window,
		"/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&" + window,
		"/loki/api/v1/detected_labels?query=%7Bjob%3D%22api%22%7D&" + window,
		"/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&" + window,
		"/loki/api/v1/detected_field/status/values?query=%7Bjob%3D%22api%22%7D&" + window,
	}
	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			started := time.Now()
			resp, err := client.Get(srv.URL + route)
			if err != nil {
				t.Fatalf("no response within %s: the handler ran its ClickHouse call with no deadline and hung on the backend (%v)",
					metadataDeadlineClientTimeout, err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503 (errorType timeout) once the query budget expires", resp.StatusCode)
			}
			if took := time.Since(started); took > metadataDeadlineClientTimeout/2 {
				t.Errorf("answered after %s; the %s budget should have unblocked the handler long before", took, metadataDeadlineBudget)
			}
		})
	}
}
