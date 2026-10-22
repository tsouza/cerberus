//go:build integration

package chserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// spillKeyBuilds are the builds the spilled-aggregation probe runs on: the
// supported floor, whose mapSort returns a Map type unequal to the one its
// spilled blocks are read back as, and a build whose mapSort returns the
// parsed type itself.
var spillKeyBuilds = []string{
	"clickhouse/clickhouse-server:24.8.14.39-alpine",
	"clickhouse/clickhouse-server:26.6.1.1193-alpine",
}

// Spill probe shape. Every series carries one sample, so the series-identity
// GROUP BY under the instant selector holds one entry per series; at
// spillKeySeries series that state is far larger than half of
// spillKeyQueryMemoryBytes, the threshold the engine stamps as
// max_bytes_before_external_group_by, so the aggregation spills.
const (
	spillKeyMetric            = "spill_probe_status"
	spillKeyReasons           = 7
	spillKeySeriesPerReason   = 200_000
	spillKeySeries            = spillKeyReasons * spillKeySeriesPerReason
	spillKeyNamespaces        = 13
	spillKeyQueryMemoryBytes  = 256 << 20
	spillKeySampleOffsetSecs  = 1
	spillKeyAggregationSpills = "ExternalAggregationWritePart"
)

// spillKeyPromQL aggregates over every series: its lowering groups by the
// canonical series identity before the outer `by (reason)`.
const spillKeyPromQL = "sum by (reason) (" + spillKeyMetric + ")"

// spillKeyEvalTime is the instant the probe evaluates at; every sample lands
// spillKeySampleOffsetSecs before it, inside the lookback.
var spillKeyEvalTime = time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC)

// TestSpill_CanonicalSeriesKeyAcrossBuilds drives a PromQL aggregation whose
// series-identity GROUP BY spills to disk through the production Prometheus
// handler, on a client whose per-query memory cap makes the engine's own
// spill threshold fire, and requires on every build that the query succeeds
// with every series counted once under its reason, and that it did spill —
// system.query_log records the spilled parts — so a pass is not a query that
// stayed in memory.
func TestSpill_CanonicalSeriesKeyAcrossBuilds(t *testing.T) {
	for _, image := range spillKeyBuilds {
		t.Run(image, func(t *testing.T) {
			ctx := context.Background()
			s := startServer(ctx, t, image)
			seedSpillKeyProbe(ctx, t, s)

			client := s.client(t, adminUser, adminPassword, chclient.Config{MaxQueryMemoryBytes: spillKeyQueryMemoryBytes})
			mux := http.NewServeMux()
			prom.New(client, schema.DefaultOTelMetrics(), nil).Mount(mux)

			qid := fmt.Sprintf("spill-key-probe-%d", time.Now().UnixNano())
			params := url.Values{}
			params.Set("query", spillKeyPromQL)
			params.Set("time", strconv.FormatInt(spillKeyEvalTime.Unix(), 10))
			var resp struct {
				Data struct {
					Result []struct {
						Metric map[string]string `json:"metric"`
						Value  []any             `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			// serveJSON fails the test on any status but 200: on a build
			// whose spilled read-back rejects the key, that is a 502.
			serveJSON(chclient.WithQueryID(ctx, qid), t, mux, "/api/v1/query?"+params.Encode(), &resp)
			if len(resp.Data.Result) != spillKeyReasons {
				t.Fatalf("%s on %s: %d series, want one per reason (%d): %v",
					spillKeyPromQL, s.version, len(resp.Data.Result), spillKeyReasons, resp.Data.Result)
			}
			want := strconv.Itoa(spillKeySeriesPerReason)
			for _, r := range resp.Data.Result {
				if len(r.Value) != promSampleFields || r.Value[1] != want {
					t.Errorf("%s on %s: reason %q = %v, want %s (every seeded series counted once)",
						spillKeyPromQL, s.version, r.Metric["reason"], r.Value, want)
				}
			}

			s.flushLogs(ctx, t)
			var spilledParts uint64
			if err := s.admin.Conn().QueryRow(ctx,
				"SELECT sum(ProfileEvents[?]) FROM system.query_log WHERE type = 'QueryFinish' AND query_id = ?",
				spillKeyAggregationSpills, qid).Scan(&spilledParts); err != nil {
				t.Fatalf("read query_log for %s: %v", qid, err)
			}
			if spilledParts == 0 {
				t.Fatalf("%s on %s did not spill its aggregation (%s = 0); the probe no longer exercises the spilled read-back",
					spillKeyPromQL, s.version, spillKeyAggregationSpills)
			}
		})
	}
}

// seedSpillKeyProbe applies cerberus's metrics DDL and writes one sample per
// series: spillKeySeries series of spillKeyMetric, series n carrying reason
// n mod spillKeyReasons, so each reason holds spillKeySeriesPerReason series.
func seedSpillKeyProbe(ctx context.Context, t *testing.T, s *server) {
	t.Helper()
	if err := ddl.Apply(ctx, s.admin.Conn(), []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("%s: apply DDL: %v", s.image, err)
	}
	s.exec(ctx, t, fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value)
SELECT
    'svc', '%s',
    map('reason', concat('r', toString(number %% %d)),
        'pod', concat('pod-', toString(number)),
        'namespace', concat('ns', toString(number %% %d))),
    toDateTime64(%d, 9),
    1
FROM numbers(%d)`,
		spillKeyMetric, spillKeyReasons, spillKeyNamespaces,
		spillKeyEvalTime.Unix()-spillKeySampleOffsetSecs, spillKeySeries))
}
