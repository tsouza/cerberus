//go:build integration

// realch_perfnightly_integration_test.go — the nightly, real-data
// measurement harness (#2370 PR A).
//
// # Why this lane exists
//
// test/perf/smoke's real-ClickHouse sentinel differential (#2370 PR 1,
// merged) proves 3 fixed, calibrated sentinels on SYNTHETIC data would have
// caught the #2364 incident — but it's a snapshot, not a growing net. This
// harness is the first slice of the actual "large-scale, realistic-scale
// performance/resource-cost test lane" #2370 itself is titled: it loads the
// REAL 14-day production sample (test/perf/smoke/testdata/samples/*.parquet,
// #2411, already scrubbed and merged, but never wired into any test) —
// trimmed to one representative day for sustainable nightly LFS bandwidth,
// see testdata/samples/README.md — and issues a curated sentinel corpus
// broader than the smoke tier's 3 fixed queries, covering construct
// families the smoke tier never reaches at all: a CLASSIC (bucket/bounds)
// histogram_quantile (the real sample is classic-shaped, not exponential —
// issue #2408's arrayJoin bucket-rate fan-out), a Gauge aggregation (zero
// smoke-tier coverage of this signal type), and a cross-series ratio shape.
//
// This PR deliberately ships NO baseline or gate yet — see Sentinels' own
// doc comment. Its own test plan is "run it, print the numbers": the
// measured peak_memory_usage/query_duration_ms this test logs is what a
// follow-up PR calibrates a committed statistical baseline against, the
// same "task 4: real numbers, not the plan's starting-point guesses"
// methodology test/perf/smoke's own PR followed.
//
// Gated behind the `integration` build tag (Docker required); wired into
// .github/workflows/perf-nightly.yml via `just perf-nightly-integration`.
package nightly

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// perfNightlyCHImage matches every other strict-scan/perf real-CH lane's
// pinned tag (test/perf/smoke's own perfSmokeCHImage).
const perfNightlyCHImage = "clickhouse/clickhouse-server:25.8-alpine"

const perfNightlyDB = "default"

// perfNightlyMemoryCapBytes matches CERBERUS_CH_QUERY_MAX_MEMORY's default —
// see test/perf/smoke's sentinelMemoryCapBytes for why this is the value a
// real deployment runs under, not a value tuned for this test.
const perfNightlyMemoryCapBytes int64 = 1 << 30 // 1 GiB

func TestPerfNightlyRealCH(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	container, client := startPerfNightlyCH(ctx, t)
	conn := client.Conn()

	if err := ddl.Apply(ctx, conn, []ddl.Signal{ddl.Metrics}); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}

	histRows, err := loadHistogramSample(ctx, container, client, sampleParquetPath(t, "svc_http_request_duration_seconds.parquet"))
	if err != nil {
		t.Fatalf("load histogram sample: %v", err)
	}
	t.Logf("loaded classic histogram sample: %d rows", histRows)

	sumRows, err := loadSumSample(ctx, container, client, sampleParquetPath(t, "svc_http_requests_total.parquet"))
	if err != nil {
		t.Fatalf("load sum sample: %v", err)
	}
	t.Logf("loaded counter sample: %d rows", sumRows)

	gaugeRows, err := loadGaugeSample(ctx, container, client, sampleParquetPath(t, "kube_pod_status_reason.parquet"))
	if err != nil {
		t.Fatalf("load gauge sample: %v", err)
	}
	t.Logf("loaded gauge sample: %d rows", gaugeRows)

	// OPTIMIZE ... FINAL before measuring: controls background-merge-state
	// noise so a sentinel's memory reading reflects the query itself, not an
	// in-flight merge competing for the same cap (same rationale as
	// test/perf/smoke's optimizeTableFinal).
	for _, table := range []string{"otel_metrics_histogram", "otel_metrics_sum", "otel_metrics_gauge"} {
		if err := conn.Exec(ctx, "OPTIMIZE TABLE "+table+" FINAL"); err != nil {
			t.Fatalf("optimize table %s: %v", table, err)
		}
	}

	metricsHandler := prom.New(client, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	metricsHandler.Mount(mux)

	logSource := optcorpus.NewCHQueryLogSource(conn, 30*time.Second, time.Hour)

	// This PR ships NO baseline or gate (Sentinels' own doc comment) — its
	// test plan is "run it, print the numbers," so a sentinel hitting the
	// real memory cap is itself a finding to report, not a test bug to fail
	// on. Every outcome (success or cap-exceeded) still reads back
	// query_log, since ClickHouse logs a terminated query's peak memory at
	// the point it was killed too — that number is exactly the kind of real
	// data point a follow-up calibration or investigation needs.
	for _, sentinel := range Sentinels {
		t.Run(sentinel.Name, func(t *testing.T) {
			queryID := fmt.Sprintf("perfnightly-%s", sentinel.Name)
			code := runSentinelOnce(t, mux, sentinel, queryID)

			if err := conn.Exec(ctx, "SYSTEM FLUSH LOGS"); err != nil {
				t.Fatalf("%s: flush logs: %v", sentinel.Name, err)
			}
			rows, err := logSource.FinishedByQueryID(ctx, []string{queryID})
			if err != nil {
				t.Fatalf("%s: read query_log: %v", sentinel.Name, err)
			}
			if len(rows) != 1 {
				t.Fatalf("%s: expected 1 query_log row for query_id %q, got %d",
					sentinel.Name, queryID, len(rows))
			}

			if code != http.StatusOK || rows[0].ExitStatus != optcorpus.ExitOK {
				t.Logf("FINDING %s (%s): HTTP %d, query_log exit=%v, peak memory_usage at termination = %d bytes (%.1f%% of %d-byte cap) — a real query over real data did not complete within the configured cap",
					sentinel.Name, sentinel.Family, code, rows[0].ExitStatus, rows[0].MemoryUsage,
					100*float64(rows[0].MemoryUsage)/float64(perfNightlyMemoryCapBytes), perfNightlyMemoryCapBytes)
				return
			}

			t.Logf("%s (%s): peak memory_usage = %d bytes (%.1f%% of %d-byte cap), duration = %d ms",
				sentinel.Name, sentinel.Family, rows[0].MemoryUsage,
				100*float64(rows[0].MemoryUsage)/float64(perfNightlyMemoryCapBytes),
				perfNightlyMemoryCapBytes, rows[0].QueryDurationMS)
		})
	}
}

func runSentinelOnce(t *testing.T, mux *http.ServeMux, sentinel Sentinel, queryID string) int {
	t.Helper()
	params := sentinel.Params(sampleWindowStart, sampleWindowEnd)
	params.Set("start", formatPromTime(sampleWindowStart))
	params.Set("end", formatPromTime(sampleWindowEnd))
	params.Set("step", sentinelStep.String())

	req := httptest.NewRequest(http.MethodGet, sentinel.Path+"?"+params.Encode(), nil)
	req = req.WithContext(chclient.WithQueryID(req.Context(), queryID))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Logf("%s: HTTP %d body: %s", sentinel.Name, rec.Code, rec.Body.String())
	}
	return rec.Code
}

func formatPromTime(t time.Time) string {
	return fmt.Sprintf("%.3f", float64(t.UnixNano())/1e9)
}

// sampleParquetPath resolves a trimmed sample's path relative to this test
// file's own package directory, independent of the working directory `go
// test` is invoked from.
func sampleParquetPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", "samples", name)
}

func startPerfNightlyCH(ctx context.Context, t *testing.T) (*tcclickhouse.ClickHouseContainer, *chclient.Client) {
	t.Helper()
	container, err := tcclickhouse.Run(
		ctx,
		perfNightlyCHImage,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase(perfNightlyDB),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	client, err := chclient.New(chclient.Config{
		Addr:                host + ":" + port.Port(),
		Database:            perfNightlyDB,
		Username:            "cerberus",
		Password:            "cerberus",
		BreakerDisabled:     true,
		MaxQueryMemoryBytes: perfNightlyMemoryCapBytes,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return container, client
}
