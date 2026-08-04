//go:build chdb

package profile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

// This file extends the corpus-wide fan-factor profiler (see the package
// doc in profile.go) to the three windowless Prometheus metadata-discovery
// endpoints named in issue #1530: /api/v1/series, /api/v1/labels and
// /api/v1/label/<name>/values with no start/end. Every other profiled
// fixture is a PromQL/LogQL/TraceQL query BODY that lowers through chplan
// (see spec.PrepareRoundTrip); these three endpoints build their SQL a
// different way — the no-`match[]` catalog arms
// (unionLabelNamesSQL/unionLabelValuesSQL in internal/api/prom/metadata.go)
// compose chsql Frags directly from matcher + time-range inputs, with no
// chplan stage at all — so they cannot become TXTAR fixtures
// (metadata_endpoints_realch_integration_test.go's doc comment spells out
// the same gap for the real-CH differential lane). /api/v1/series does
// lower through chplan (promql.LowerMetadataRange), but only via an
// unexported Handler method, so it is captured the same way as the other
// two for uniformity.
//
// The technique — drive the real production Handler over HTTP with a
// recording stub Querier, then inline the captured SQL/args for chDB EXPLAIN
// — mirrors internal/api/prom/metadata_scan_bound_explain_chdb_test.go's
// established pattern for the EXPLAIN-based projection-routing guard. This
// file reuses that pattern purely to source the SQL text; the actual
// fan-factor / scan_rows measurement runs through the SAME [Profiler] every
// other fixture uses, so the resulting [Record]s slot into
// cardinality-baseline.json next to the TXTAR-derived rows with no special
// casing in the ratchet.

// metadataProfileDays is the day-partition spread the seed scatters rows
// across. It must stay comfortably inside the handler's default 14-day
// metadata lookback (internal/api/prom/metadata.go's
// defaultMetadataLookback) so every seeded row falls inside the windowless
// scan regardless of the wall-clock date the profiler runs on — the offsets
// are relative ("N days before now"), not absolute calendar dates, so
// scan_rows stays exactly reproducible run to run.
const metadataProfileDays = 10

// metadataProfileRows is the per-table seed row count. Large enough that
// the projection-routed reads (proj_series) measure a real aggregation
// fan-in rather than a handful of rows, small enough to seed quickly inside
// the shared chDB session every other fixture also seeds into.
const metadataProfileRows = 6_000

// metadataProfileMetricCount is the distinct MetricName cardinality the
// seed scatters across each table, index 0 always being "up" — the metric
// name the /api/v1/series?match[]=up fixture selects.
const metadataProfileMetricCount = 50

// metadataProfileInstanceCardinality is the distinct `instance` Attributes
// value count each metric name's rows cycle through, giving the
// GROUP BY (MetricName, Attributes) aggregating projection a realistic
// per-metric series fan-out to fold over instead of a single group.
const metadataProfileInstanceCardinality = 200

// metadataUpMetric is the metric name the /series fixture selects via
// match[]=up — an ordinary bare selector with no dots/underscores, so the
// matcher expansion in internal/api/prom/metadata.go's
// expandMetadataMatchers stays a single variant and the handler issues
// exactly one Query call to capture.
const metadataUpMetric = "up"

// metadataProfileGaugeDDL / metadataProfileSumDDL / metadataProfileHistogramDDL
// are the production-shaped OTel-CH metric tables trimmed to the columns
// the profiled endpoints' SQL references (MetricName, Attributes, TimeUnix,
// Value — see internal/api/prom/handler.go's wrapWithSampleProjection for
// the /series projection and metadata.go's unionLabelNamesSQL /
// unionLabelValuesSQL for the catalog arms), carrying the same
// PARTITION BY toDate(TimeUnix) the OTel exporter's own DDL uses so the
// leaf scan is genuinely partition-bounded rather than single-part.
// ServiceName is a dedicated-column resource label
// (schema.Metrics.ServiceNameColumn) the /series lowering selects
// alongside Attributes for every bare-selector query, regardless of
// whether the fixture's matcher references it.
func metadataProfileTableDDL(table string) string {
	return fmt.Sprintf(`CREATE TABLE %s (
    MetricName String,
    ServiceName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, TimeUnix);`, table)
}

// metadataProfileHistogramTableDDL widens metadataProfileTableDDL's shape
// with the classic-histogram-only columns (Count, Sum, BucketCounts,
// ExplicitBounds, MetricDescription, MetricUnit — the full production
// otel_metrics_histogram shape from internal/schema/otel.go's
// DefaultOTelMetrics, minus ResourceAttributes since the profiled schema
// disables the resource arm): /series's matcher expansion
// (expandBareHistogramMatcher) always probes the histogram table for a
// companion-suffix match, regardless of whether the selected metric is
// itself a histogram, so the histogram table's lowered SELECT references
// these columns even for the "up" gauge fixture. Value carries no
// production meaning here (classic histograms report through Count/Sum,
// not Value) but stays for metadataProfileInsert's shared column list;
// it is simply never selected by any profiled query.
func metadataProfileHistogramTableDDL(table string) string {
	return fmt.Sprintf(`CREATE TABLE %s (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    ServiceName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64,
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree()
PARTITION BY toDate(TimeUnix)
ORDER BY (MetricName, TimeUnix);`, table)
}

// metadataProfileInsert scatters metadataProfileRows rows across
// metadataProfileDays day-partitions, cycling through
// metadataProfileMetricCount metric names (index 0 fixed to "up"), each
// carrying a `job` + `instance` Attributes pair — a realistic series
// fan-out shape for the GROUP BY (MetricName, Attributes) aggregating
// projection to fold over.
func metadataProfileInsert(table string) string {
	// The day-offset expression deliberately does NOT reduce `number`
	// modulo metadataProfileMetricCount (or any divisor of it): doing so
	// correlates the day offset with the MetricName selection below,
	// since metadataProfileMetricCount (50) is a multiple of
	// metadataProfileDays (10) — every "up" row (number % 50 == 0) would
	// then also satisfy number % 10 == 0 and land on day-offset 0 for
	// EVERY row, i.e. AT now64(), which is evaluated when this INSERT
	// actually runs — strictly after captureMetadataFixtures() computed
	// the HTTP request's window `end` bound. Those rows would then sit
	// microseconds outside the captured upper bound and vanish from
	// EVERY profiled fixture, silently reporting scan_rows=0. Deriving
	// the offset from `intDiv(number, metadataProfileMetricCount)`
	// instead decorrelates it from the MetricName cycle, and shifting the
	// range to 1..metadataProfileDays (never 0) keeps every seeded row at
	// least a full day older than "now" at insert time — comfortably past
	// any capture/insert timing skew and still well inside the 14-day
	// default lookback.
	return fmt.Sprintf(`INSERT INTO %s (MetricName, Attributes, TimeUnix, Value)
SELECT
    if(number %% %d = 0, '%s', concat('m_', toString(number %% %d))) AS MetricName,
    map('job', 'api', 'instance', toString(number %% %d)) AS Attributes,
    now64(9) - INTERVAL (1 + intDiv(number, %d) %% %d) DAY AS TimeUnix,
    toFloat64(number) AS Value
FROM numbers(%d);`, table, metadataProfileMetricCount, metadataUpMetric, metadataProfileMetricCount,
		metadataProfileInstanceCardinality, metadataProfileMetricCount, metadataProfileDays, metadataProfileRows)
}

// metadataProfileProjections installs + materializes the curated
// proj_series aggregating projection (internal/schema/ddl's
// seriesProjection) every no-`match[]` catalog arm routes onto when
// nowAnchored — see internal/api/prom/metadata_scan_bound_explain_chdb_test.go
// for the guard that pins this routing. Seeding it here means the
// profiler's fan_factor/scan_rows reflect the SAME projection-routed
// (or, for /series, genuinely raw-scanned) shape production serves, not an
// unprojected worst case that would misrepresent the actual cost.
func metadataProfileProjections(table string) []string {
	return []string{
		"ALTER TABLE " + table + " ADD PROJECTION proj_series " +
			"(SELECT MetricName, Attributes, max(TimeUnix) GROUP BY MetricName, Attributes)",
		"ALTER TABLE " + table + " MATERIALIZE PROJECTION proj_series",
	}
}

// metadataProfileSeed is the full seed script shared by all three
// metadata-endpoint fixtures: the three metric tables
// (internal/api/prom.Handler.metricTables()'s default set), each seeded +
// projected + OPTIMIZE-FINAL'd so the projection collapses to real merged
// parts instead of unmerged insert parts.
func metadataProfileSeed() string {
	var seed string
	tableDDL := map[string]func(string) string{
		"otel_metrics_gauge":     metadataProfileTableDDL,
		"otel_metrics_sum":       metadataProfileTableDDL,
		"otel_metrics_histogram": metadataProfileHistogramTableDDL,
	}
	for _, table := range []string{"otel_metrics_gauge", "otel_metrics_sum", "otel_metrics_histogram"} {
		seed += tableDDL[table](table) + "\n"
		seed += metadataProfileInsert(table) + "\n"
		for _, stmt := range metadataProfileProjections(table) {
			seed += stmt + ";\n"
		}
		seed += "OPTIMIZE TABLE " + table + " FINAL;\n"
	}
	return seed
}

// metadataCaptureQuerier is a prom.Querier stub that records every SQL
// statement + bound args issued through it without touching a database. It
// exists purely to capture the exact production SQL the metadata HTTP
// handlers emit — the handler is driven end to end over real HTTP so the
// captured SQL is byte-identical to what a live deployment sends, matching
// the recording-stub technique
// internal/api/prom/metadata_scan_bound_explain_chdb_test.go already
// established.
type metadataCaptureQuerier struct {
	allSQL  []string
	allArgs [][]any
}

func (s *metadataCaptureQuerier) record(sql string, args []any) {
	s.allSQL = append(s.allSQL, sql)
	s.allArgs = append(s.allArgs, args)
}

func (s *metadataCaptureQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.record(sql, args)
	return nil, nil
}

func (s *metadataCaptureQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	s.record(sql, args)
	return nil, nil
}

func (s *metadataCaptureQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.record(sql, args)
	return nil, nil
}

func (s *metadataCaptureQuerier) QueryLabelSets(_ context.Context, sql string, args ...any) ([]map[string]string, error) {
	s.record(sql, args)
	return nil, nil
}

func (s *metadataCaptureQuerier) QueryMetricMeta(_ context.Context, sql, _ string, args ...any) ([]chclient.MetricMetaRow, error) {
	s.record(sql, args)
	return nil, nil
}

func (s *metadataCaptureQuerier) QueryExemplars(_ context.Context, sql string, args ...any) ([]chclient.ExemplarRow, error) {
	s.record(sql, args)
	return nil, nil
}

// captureMetadataSQL issues a GET against a fresh capture-backed
// prom.Handler and returns every (sql, args) pair the request issued. The
// resource-attributes arm is disabled (mirroring
// metadata_scan_bound_explain_chdb_test.go's newCaptureServer) so a
// /labels or /label/<name>/values request captures only the
// Attributes-map catalog arm — the path proj_series serves and the one
// the issue's evidence cites (internal/api/prom/metadata.go's
// unionLabelNamesSQL / unionLabelValuesSQL).
func captureMetadataSQL(path string) *metadataCaptureQuerier {
	q := &metadataCaptureQuerier{}
	sch := schema.DefaultOTelMetrics()
	sch.ResourceAttributesColumn = ""
	h := prom.New(q, sch, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + path)
	if err == nil {
		_ = resp.Body.Close()
	}
	return q
}

// metadataFixture describes one no-bounds metadata-endpoint SQL statement
// captured from the real handler, ready to profile through the shared
// [Profiler] alongside the TXTAR corpus.
type metadataFixture struct {
	id    string
	query string
	args  []any
}

// captureMetadataFixtures drives the three windowless metadata endpoints
// named in issue #1530 and returns the dominant captured SQL statement for
// each, paired with a fixture id under the "metadata/" namespace (parallel
// to the "<head>/<name>" TXTAR ids ProfileCorpus uses).
//
//   - metadata/series_no_bounds — GET /api/v1/series?match[]=up. Requires
//     match[] (the endpoint 400s without one — internal/api/prom/metadata.go's
//     handleSeries), so "windowless" here means no start/end, the same
//     Grafana-variable-refresh shape metadata_scan_bound_repro_test.go's
//     "series" case exercises. This lowers through chplan
//     (promql.LowerMetadataRange), unlike the other two, and is a genuine
//     [start,end]-bounded (not aggregating-projection-routed) scan.
//   - metadata/labels_no_bounds — GET /api/v1/labels. No match[], no
//     bounds: the catalog-wide label-name enumeration
//     (unionLabelNamesSQL).
//   - metadata/label_values_no_bounds — GET /api/v1/label/job/values. No
//     match[], no bounds: the catalog-wide label-value enumeration
//     (unionLabelValuesSQL) for an ordinary Attributes-map key.
//
// Each capture takes the LAST recorded statement: the single-matcher /
// single-arm fast paths these requests hit (metadataUpMetric has no
// dotted/underscored form to expand, and the resource arm is disabled)
// issue exactly one statement, so "last" is simply "the" statement; taking
// the last rather than indexing [0] keeps this robust if a future matcher
// expansion adds a leading catalog lookup ahead of the sample query.
func captureMetadataFixtures() []metadataFixture {
	specs := []struct {
		id   string
		path string
	}{
		{"metadata/series_no_bounds", "/api/v1/series?match[]=" + metadataUpMetric},
		{"metadata/labels_no_bounds", "/api/v1/labels"},
		{"metadata/label_values_no_bounds", "/api/v1/label/job/values"},
	}
	out := make([]metadataFixture, 0, len(specs))
	for _, s := range specs {
		q := captureMetadataSQL(s.path)
		if len(q.allSQL) == 0 {
			out = append(out, metadataFixture{id: s.id})
			continue
		}
		last := len(q.allSQL) - 1
		out = append(out, metadataFixture{id: s.id, query: q.allSQL[last], args: q.allArgs[last]})
	}
	return out
}

// ProfileMetadataEndpoints profiles the three windowless metadata-discovery
// endpoints from issue #1530 (see the package-level doc above) through the
// SAME [Profiler] ProfileCorpus uses, so their [Record]s carry the same
// fan_factor / scan_rows / operator-flag signal as every TXTAR-derived
// fixture and slot into cardinality-baseline.json without special-casing.
func ProfileMetadataEndpoints() ([]Record, error) {
	p, err := NewProfiler()
	if err != nil {
		return nil, err
	}
	defer p.Close()

	seed := metadataProfileSeed()
	fixtures := captureMetadataFixtures()
	recs := make([]Record, 0, len(fixtures))
	for _, f := range fixtures {
		if f.query == "" {
			recs = append(recs, Record{Fixture: f.id, Err: "handler issued no SQL for this request"})
			continue
		}
		prep := &spec.PreparedRoundTrip{Seed: seed, Query: f.query, Args: f.args}
		recs = append(recs, p.ProfileFixture(f.id, prep))
	}
	return recs, nil
}
