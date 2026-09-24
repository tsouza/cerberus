//go:build integration

// Real-ClickHouse pins for the metrics Flags column that marks a Prometheus
// stale marker (internal/promql/stale_marker.go):
//
//   - when the boot probe (preflight.Run) finds Flags on every metric table
//     of the exporter's own DDL, the resolved schema ends a series at its
//     stale marker and keeps the marker out of range windows, while the
//     unprobed default schema never reads the column;
//   - when a metric table lacks Flags, the probe names it and the resolved
//     schema reads no Flags column, so both a single-table scan and a
//     merge() whose members disagree answer instead of failing.
//
// Production ClickHouse resolves identifiers more strictly than chDB, so
// both halves run against real servers, one per pinned image. Run locally
// with:
//
//	just stale-marker-flags-integration
package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// staleMarkerCHImages are the pinned servers the stale-marker pins run on:
// the native time-series floor, the last build before ClickHouse #115920,
// and the first build carrying it.
var staleMarkerCHImages = []string{
	"clickhouse/clickhouse-server:25.9-alpine",
	"clickhouse/clickhouse-server:26.7.13.12-alpine",
	"clickhouse/clickhouse-server:26.8.1.2041-alpine",
}

// staleMarkerScrapeInterval is the fixture's scrape spacing.
const staleMarkerScrapeInterval = 15 * time.Second

// staleMarkerSamples is how many real samples precede the stale marker.
const staleMarkerSamples = 8

// otelNoRecordedValue is the OTel data-point flag bit a scraped stale
// marker carries.
const otelNoRecordedValue = 1

func TestStaleMarkerFlags_RealCH(t *testing.T) {
	for _, image := range staleMarkerCHImages {
		t.Run(image, func(t *testing.T) {
			client := startStaleMarkerCH(t, image)
			ctx := t.Context()
			m := schema.DefaultOTelMetrics()
			if err := ddl.ApplyWithConfig(ctx, client.Conn(), ddl.Config{Database: "otel"}, []ddl.Signal{ddl.Metrics}); err != nil {
				t.Fatalf("apply metrics DDL: %v", err)
			}

			// stale_requests is a gauge whose target disappears; an
			// unsuffixed name scans merge(gauge, sum).
			// flagless_requests_total is a counter, scanned from the sum
			// table alone.
			base := time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
			markerAt := base.Add(staleMarkerSamples * staleMarkerScrapeInterval)
			last := markerAt.Add(-staleMarkerScrapeInterval)
			for i := range staleMarkerSamples {
				ts := base.Add(time.Duration(i) * staleMarkerScrapeInterval)
				insertStaleMarkerRow(t, client, m.GaugeTable, "stale_requests", ts, float64(100+10*i), true, 0)
				insertStaleMarkerRow(t, client, m.SumTable, "flagless_requests_total", ts, float64(i), false, 0)
			}
			insertStaleMarkerRow(t, client, m.GaugeTable, "stale_requests", markerAt, 0, true, otelNoRecordedValue)
			afterMarker := markerAt.Add(time.Minute)
			req := preflight.Requirements{Database: "otel", Metrics: m, Signals: preflight.Signals{Metrics: true}}

			t.Run("every table carries flags", func(t *testing.T) {
				res := preflight.Run(ctx, client, req)
				if res.Fatal != nil || res.Unreachable || !res.StaleMarkerFlagsPresent {
					t.Fatalf("probe over the exporter DDL: fatal=%v unreachable=%v present=%v", res.Fatal, res.Unreachable, res.StaleMarkerFlagsPresent)
				}
				probed := staleMarkerServer(t, client, res.ResolveStaleMarkerFlags(m))
				if got := staleMarkerSeries(t, probed, "stale_requests", afterMarker); got != 0 {
					t.Fatalf("stale_requests one minute after its stale marker: %d series, want 0", got)
				}
				if got := staleMarkerValue(t, probed, "stale_requests", last); got != float64(100+10*(staleMarkerSamples-1)) {
					t.Fatalf("stale_requests before its stale marker = %v", got)
				}
				if got := staleMarkerValue(t, probed, "last_over_time(stale_requests[2m])", afterMarker); got != float64(100+10*(staleMarkerSamples-1)) {
					t.Fatalf("last_over_time over a window holding the stale marker = %v, want the last real sample", got)
				}

				// The unprobed default reads no Flags column: the marker's
				// Value 0 is an ordinary sample.
				unprobed := staleMarkerServer(t, client, m)
				if got := staleMarkerValue(t, unprobed, "stale_requests", afterMarker); got != 0 {
					t.Fatalf("stale_requests on the unprobed default = %v, want the marker read as the sample 0", got)
				}
			})

			t.Run("a table without flags leaves the column unread", func(t *testing.T) {
				if err := client.Exec(ctx, "ALTER TABLE "+m.SumTable+" DROP COLUMN "+m.FlagsColumn); err != nil {
					t.Fatalf("drop %s.%s: %v", m.SumTable, m.FlagsColumn, err)
				}
				res := preflight.Run(ctx, client, req)
				if res.Fatal != nil || res.StaleMarkerFlagsPresent ||
					len(res.StaleMarkerFlagsMissing) != 1 || res.StaleMarkerFlagsMissing[0] != m.SumTable {
					t.Fatalf("probe: fatal=%v present=%v missing=%v, want [%s] missing",
						res.Fatal, res.StaleMarkerFlagsPresent, res.StaleMarkerFlagsMissing, m.SumTable)
				}
				resolved := staleMarkerServer(t, client, res.ResolveStaleMarkerFlags(m))
				if got := staleMarkerValue(t, resolved, "flagless_requests_total", last); got != staleMarkerSamples-1 {
					t.Fatalf("flagless_requests_total on the resolved schema = %v, want %d", got, staleMarkerSamples-1)
				}
				// merge(gauge, sum) where only the gauge carries Flags.
				if got := staleMarkerValue(t, resolved, "stale_requests", last); got != float64(100+10*(staleMarkerSamples-1)) {
					t.Fatalf("stale_requests over the merge on the resolved schema = %v", got)
				}

				// Forcing the column on shows what the probe prevents: the
				// sum-only scan names a column its table lacks.
				forced := m
				forced.FlagsColumnProbed = true
				if status := staleMarkerStatus(t, staleMarkerServer(t, client, forced), "flagless_requests_total", last); status == http.StatusOK {
					t.Fatalf("a query naming Flags against a table without it answered %d", status)
				}
			})
		})
	}
}

func startStaleMarkerCH(t *testing.T, image string) *chclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	container, err := tcclickhouse.Run(
		ctx, image,
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
		tcclickhouse.WithDatabase("otel"),
	)
	if err != nil {
		t.Fatalf("start %s: %v", image, err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	client, err := chclient.New(chclient.Config{
		Addr: host + ":" + port.Port(), Database: "otel", Username: "cerberus", Password: "cerberus",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func insertStaleMarkerRow(t *testing.T, client *chclient.Client, table, name string, ts time.Time, v float64, withFlags bool, flags int) {
	t.Helper()
	cols := "ResourceAttributes, ServiceName, MetricName, Attributes, TimeUnix, Value"
	vals := fmt.Sprintf("map(), '', '%s', map('job', 'api'), toDateTime64('%s', 9), %g",
		name, ts.Format("2006-01-02 15:04:05.000"), v)
	if withFlags {
		cols += ", Flags"
		vals += fmt.Sprintf(", %d", flags)
	}
	if err := client.Exec(t.Context(), fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table, cols, vals)); err != nil {
		t.Fatalf("insert into %s: %v", table, err)
	}
}

func staleMarkerServer(t *testing.T, client *chclient.Client, m schema.Metrics) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	prom.New(client, m, nil).Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type staleMarkerVector struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Value [2]any `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func staleMarkerQuery(t *testing.T, srv *httptest.Server, query string, at time.Time) (int, staleMarkerVector) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/v1/query?" + url.Values{
		"query": {query},
		"time":  {strconv.FormatInt(at.Unix(), 10)},
	}.Encode())
	if err != nil {
		t.Fatalf("GET %q: %v", query, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body staleMarkerVector
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode %q: %v", query, err)
		}
	}
	return resp.StatusCode, body
}

func staleMarkerStatus(t *testing.T, srv *httptest.Server, query string, at time.Time) int {
	t.Helper()
	status, _ := staleMarkerQuery(t, srv, query, at)
	return status
}

func staleMarkerSeries(t *testing.T, srv *httptest.Server, query string, at time.Time) int {
	t.Helper()
	status, body := staleMarkerQuery(t, srv, query, at)
	if status != http.StatusOK {
		t.Fatalf("%q answered HTTP %d", query, status)
	}
	return len(body.Data.Result)
}

func staleMarkerValue(t *testing.T, srv *httptest.Server, query string, at time.Time) float64 {
	t.Helper()
	status, body := staleMarkerQuery(t, srv, query, at)
	if status != http.StatusOK || len(body.Data.Result) != 1 {
		t.Fatalf("%q answered HTTP %d with %d series, want one", query, status, len(body.Data.Result))
	}
	s, _ := body.Data.Result[0].Value[1].(string)
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("%q value %v: %v", query, body.Data.Result[0].Value[1], err)
	}
	return v
}
