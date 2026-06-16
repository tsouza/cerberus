//go:build chdb

// chDB-backed HEAP benchmark for the rc.5 ResourceAttributes-as-Prom-labels
// feature (the OOM regression investigation). Seeds a high-series-count
// dataset whose rows carry a realistic resource-attribute payload (k8s.*,
// service.*, cloud.* keys live ONLY in ResourceAttributes) then drives the
// full handler stack for a range query over many samples. Reports B/op +
// allocs/op so the per-query heap delta between merge-ON (default schema)
// and merge-OFF (ResourceAttributesColumn cleared) is measurable, and so a
// future regression in the merge cost is visible from
// `go test -tags chdb -bench`.
//
// Run: go test -tags chdb -run xxx -bench BenchmarkResourceAttr -benchmem \
//        ./internal/api/prom/

package prom_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// benchSeries × benchSamplesPerSeries × (range-step output rows) is held
// under chdb-go v1.11.0's 512-row parquet batch ceiling — that driver's
// (*parquetRows).Next panics with "index out of range [512]" on a wider
// result set. The merge cost we measure is per-SCANNED-row, so a modest
// distinct-series count still exercises the sanitize+mapUpdate on every
// row of the window before the LWR/range reduction.
const (
	benchSeries           = 16
	benchSamplesPerSeries = 16
)

// benchResourceSeedRows builds a sum-table seed with `series` distinct
// series, each carrying `samplesPerSeries` rows over a 10-minute window.
// Every row's ResourceAttributes carries seven realistic OTel resource
// keys (the k8s + service + cloud surface a typical OTel-CH deployment
// writes); the per-datapoint Attributes carry three metric labels. This is
// the shape that makes the merge expensive: the merge sanitizes BOTH maps
// for every scanned row.
func benchResourceSeedRows(series, samplesPerSeries int) string {
	var b strings.Builder
	b.WriteString(resourceAttrGaugeDDL)
	b.WriteString(resourceAttrSumDDL)
	b.WriteString(resourceAttrHistogramDDL)
	base := time.Now().UTC().Add(-10 * time.Minute)
	b.WriteString("INSERT INTO otel_metrics_sum (MetricName, Attributes, ResourceAttributes, TimeUnix, Value) VALUES ")
	first := true
	for s := 0; s < series; s++ {
		ra := fmt.Sprintf(
			"map('k8s.namespace.name','ns-%d','k8s.pod.name','pod-%d','k8s.node.name','node-%d',"+
				"'k8s.deployment.name','dep-%d','service.instance.id','inst-%d','cloud.region','us-east-%d',"+
				"'cloud.availability_zone','az-%d')",
			s%8, s, s%16, s%8, s, s%4, s%3,
		)
		attrs := fmt.Sprintf("map('route','/api/%d','method','GET','status_code','200')", s%32)
		for n := 0; n < samplesPerSeries; n++ {
			if !first {
				b.WriteString(",")
			}
			first = false
			ts := base.Add(time.Duration(n) * 30 * time.Second).Format("2006-01-02 15:04:05.000")
			fmt.Fprintf(&b, "('http_requests_total',%s,%s,toDateTime64('%s',9),%d.0)", attrs, ra, ts, n+1)
		}
	}
	b.WriteString(";")
	return b.String()
}

// benchSeedChDB opens an ephemeral chDB session, applies the seed, and
// returns a wired prom handler server. clearRA toggles the resource-attr
// merge OFF (cleared ResourceAttributesColumn) so the two benchmarks
// isolate the merge cost.
func benchSeedChDB(b *testing.B, seed string, clearRA bool) *httptest.Server {
	b.Helper()
	c := chclienttest.NewChDB(b)
	c.Seed(b, seed)
	s := schema.DefaultOTelMetrics()
	if clearRA {
		s.ResourceAttributesColumn = ""
	}
	h := prom.New(c, s, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	b.Cleanup(srv.Close)
	return srv
}

func runBenchQuery(b *testing.B, srv *httptest.Server, query string) {
	end := time.Now().UTC()
	start := end.Add(-9 * time.Minute)
	u := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=30",
		srv.URL, query, start.Unix(), end.Unix())
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(u)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status=%d body=%s", resp.StatusCode, body)
		}
	}
}

func BenchmarkResourceAttr_RangeMerge_On(b *testing.B) {
	srv := benchSeedChDB(b, benchResourceSeedRows(benchSeries, benchSamplesPerSeries), false)
	runBenchQuery(b, srv, "http_requests_total")
}

func BenchmarkResourceAttr_RangeMerge_Off(b *testing.B) {
	srv := benchSeedChDB(b, benchResourceSeedRows(benchSeries, benchSamplesPerSeries), true)
	runBenchQuery(b, srv, "http_requests_total")
}

func BenchmarkResourceAttr_RateMerge_On(b *testing.B) {
	srv := benchSeedChDB(b, benchResourceSeedRows(benchSeries, benchSamplesPerSeries), false)
	runBenchQuery(b, srv, "rate(http_requests_total[2m])")
}

func BenchmarkResourceAttr_RateMerge_Off(b *testing.B) {
	srv := benchSeedChDB(b, benchResourceSeedRows(benchSeries, benchSamplesPerSeries), true)
	runBenchQuery(b, srv, "rate(http_requests_total[2m])")
}
