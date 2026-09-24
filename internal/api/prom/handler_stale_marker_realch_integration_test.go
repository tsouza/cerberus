//go:build integration

// Real-ClickHouse pins for the metrics Flags column that marks a Prometheus
// stale marker (internal/promql/stale_marker.go):
//
//   - on a schema whose tables carry Flags, an instant selection ends a
//     series at its stale marker and a range function does not read the
//     marker's `Value = 0` as a counter reset;
//   - on a schema where a metric table lacks Flags, the boot probe
//     (preflight.Run) names the table, ResolveStaleMarkerFlags clears the
//     column, and queries answer instead of failing on the missing column.
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
)

// staleMarkerCHImages are the pinned servers the stale-marker pins run on:
// the native time-series floor, the last build before ClickHouse #115920,
// and the first build carrying it.
var staleMarkerCHImages = []string{
	"clickhouse/clickhouse-server:25.9",
	"clickhouse/clickhouse-server:26.7.13.12",
	"clickhouse/clickhouse-server:26.8.1.2041",
}

// staleMarkerTableDDL is the OTel-CH exporter's value-table shape, reduced
// to the columns the PromQL read path touches. %s is the table name and
// the optional trailing column list.
const staleMarkerTableDDL = `CREATE TABLE %s (
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64,
    AggregationTemporality Int32 DEFAULT 2%s
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix)`

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

			// The gauge table carries Flags and the sum table does not. An
			// unsuffixed name scans merge(gauge, sum), which ClickHouse
			// resolves while any merged table has the column; a `_total`
			// name scans the sum table alone, which cannot.
			if err := client.Exec(ctx, fmt.Sprintf(staleMarkerTableDDL, m.GaugeTable, ",\n    Flags UInt32 DEFAULT 0")); err != nil {
				t.Fatalf("create %s: %v", m.GaugeTable, err)
			}
			if err := client.Exec(ctx, fmt.Sprintf(staleMarkerTableDDL, m.SumTable, "")); err != nil {
				t.Fatalf("create %s: %v", m.SumTable, err)
			}

			base := time.Now().UTC().Truncate(time.Minute).Add(-time.Hour)
			markerAt := base.Add(staleMarkerSamples * staleMarkerScrapeInterval)
			for i := range staleMarkerSamples {
				ts := base.Add(time.Duration(i) * staleMarkerScrapeInterval)
				insertStaleMarkerRow(t, client, m.GaugeTable, "stale_requests", ts, float64(100+10*i), true, 0)
				insertStaleMarkerRow(t, client, m.SumTable, "flagless_requests_total", ts, float64(i), false, 0)
			}
			insertStaleMarkerRow(t, client, m.GaugeTable, "stale_requests", markerAt, 0, true, otelNoRecordedValue)
			afterMarker := markerAt.Add(time.Minute)

			t.Run("flags on every table read markers", func(t *testing.T) {
				srv := staleMarkerServer(t, client, m)
				if got := staleMarkerSeries(t, srv, "stale_requests", afterMarker); got != 0 {
					t.Fatalf("stale_requests one minute after its stale marker: %d series, want 0", got)
				}
				if got := staleMarkerSeries(t, srv, "stale_requests", markerAt.Add(-time.Second)); got != 1 {
					t.Fatalf("stale_requests before its stale marker: %d series, want 1", got)
				}
				rate := staleMarkerValue(t, srv, "increase(stale_requests[2m])", afterMarker)
				if rate < 0 || rate > float64(10*staleMarkerSamples) {
					t.Fatalf("increase over a window holding the stale marker = %v: the marker's 0 was read as a counter reset", rate)
				}
			})

			t.Run("a table without flags resolves to reading samples", func(t *testing.T) {
				res := preflight.Run(ctx, client, preflight.Requirements{
					Database: "otel",
					Metrics:  m,
					Signals:  preflight.Signals{Metrics: true},
				})
				if res.Unreachable || res.DatabaseAbsent {
					t.Fatalf("preflight could not probe: %+v", res)
				}
				if len(res.StaleMarkerFlagsMissing) != 1 || res.StaleMarkerFlagsMissing[0] != m.SumTable {
					t.Fatalf("StaleMarkerFlagsMissing = %v, want [%s]", res.StaleMarkerFlagsMissing, m.SumTable)
				}

				unresolved := staleMarkerServer(t, client, m)
				if status := staleMarkerStatus(t, unresolved, "flagless_requests_total", afterMarker); status == http.StatusOK {
					t.Fatalf("a query naming Flags against a table without it answered %d; the probe's resolution would be untested", status)
				}

				resolved := staleMarkerServer(t, client, res.ResolveStaleMarkerFlags(m))
				last := base.Add((staleMarkerSamples - 1) * staleMarkerScrapeInterval)
				if got := staleMarkerValue(t, resolved, "flagless_requests_total", last); got != staleMarkerSamples-1 {
					t.Fatalf("flagless_requests_total = %v on the resolved schema, want %d", got, staleMarkerSamples-1)
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
	cols, vals := "MetricName, Attributes, TimeUnix, Value", fmt.Sprintf("'%s', map('job', 'api'), toDateTime64('%s', 9), %g",
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
