//go:build chdb

// chDB-backed end-to-end coverage for /api/v1/query_exemplars. The
// default (untagged) test lane in exemplars_test.go exercises the
// handler against a stubQuerier; this file rounds the same flow trip
// through real ClickHouse semantics: the handler emits the
// arrayJoin/arrayEnumerate fan-out SQL via chsql.EmitQueryExemplars,
// chDB executes it against a seeded otel_metrics_sum table whose
// `Exemplars` Nested column carries multiple exemplars per series, and
// the JSON envelope decoded back matches the upstream Prom wire shape.
//
// The seed is shaped so the per-series cardinality assertions are
// load-bearing: two series (job=api, job=db) each carrying three
// exemplars per row, every exemplar with a non-empty trace_id /
// span_id and a `request_id` filtered attribute. That covers the three
// per-exemplar wire-shape contracts the handler maintains:
//
//   - `seriesLabels` carries `__name__`, `service.name`, and the
//     per-series Attributes map (groupExemplars folds rows by
//     (MetricName, Attributes, ServiceName) — the seed surfaces this
//     by varying only `job` across series).
//   - `exemplars[].labels` merges the FilteredAttributes map (from the
//     Nested column) with the dedicated `trace_id` / `span_id`
//     columns; the merge gives the dedicated columns precedence (per
//     plan §7 "Reserved-key precedence") which the seed indirectly
//     pins by surfacing the trace/span IDs that did NOT exist as keys
//     in the FilteredAttributes map.
//   - `timestamp` is a numeric unix-seconds float (not the stringified
//     PromQL Sample convention). The seed places each exemplar at a
//     distinct sub-second offset so the decoded float carries
//     fractional nanos through the round-trip.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// sumExemplarsDDL is the OTel-metrics-sum-shaped table the chDB-backed
// exemplars test seeds. The full upstream OTel exporter DDL has many
// more columns than the handler reads (see
// $GOMODCACHE/github.com/tsouza/opentelemetry-collector-contrib/exporter/
// clickhouseexporter@.../sqltemplates/metrics_sum_table.sql); this
// minimal shape carries exactly the columns chsql.EmitQueryExemplars
// projects (MetricName / Attributes / ServiceName / TimeUnix) plus the
// Nested `Exemplars` column with its five sub-fields (TimeUnix /
// Value / TraceId / SpanId / FilteredAttributes).
//
// `flatten_nested = 1` is the ClickHouse default; under it the Nested
// column surfaces server-side as a set of parallel `Array(...)`
// columns accessed via `Exemplars.TimeUnix`, `Exemplars.Value`, etc.
// The SQL chsql.EmitQueryExemplars renders targets that parallel-array
// form (the cerberus whole-codebase invariant). Engine = Memory keeps
// the seed fast and avoids MergeTree's PREWHERE / sort-key
// constraints; the exemplars SQL does not depend on either.
const sumExemplarsDDL = `CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ServiceName String,
    TimeUnix DateTime64(9),
    Value Float64,
    Exemplars Nested (
        FilteredAttributes Map(String, String),
        TimeUnix DateTime64(9),
        Value Float64,
        SpanId String,
        TraceId String
    )
) ENGINE = Memory;`

// newChDBExemplarsServer wires a chDB-backed prom handler with the
// exemplars seed already applied. Mirrors newChDBServer in
// handler_chdb_test.go; lives here as a separate constructor so a
// future seed shape can drift without disrupting the gauge-shaped
// chDB tests.
func newChDBExemplarsServer(t *testing.T, ddl string) (*httptest.Server, *chclienttest.Client) {
	t.Helper()
	c := chclienttest.NewChDB(t)
	if ddl != "" {
		c.Seed(t, ddl)
	}
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

// TestQueryExemplars_ChDB_Roundtrip seeds otel_metrics_sum with two
// series for `http_requests_total` (job=api, job=db); each row carries
// three exemplars with distinct trace_id / span_id pairs and a
// `request_id` filtered attribute. The handler is invoked over HTTP
// with `query=http_requests_total{job=~"api|db"}` and the JSON envelope
// is decoded back. Assertions:
//
//   - top-level `data` is an array of two ExemplarSeries (one per
//     series-key);
//   - each series carries three exemplars (the row's full exemplar
//     fan-out from the arrayJoin/arrayEnumerate);
//   - per-exemplar `labels` carries non-empty `trace_id` + `span_id`
//     (the dedicated columns surfaced via projectExemplar) AND the
//     `request_id` key that lived in the FilteredAttributes map
//     (so groupExemplars's reserved-key merge is exercised);
//   - per-exemplar `timestamp` is a unix-seconds float (numeric, not
//     stringified) and matches the seeded sub-second offset to within
//     CH's DateTime64(9) precision;
//   - per-exemplar `value` is a numeric Float64 (the V observed at
//     the exemplar's timestamp, NOT the parent data-point's Value).
func TestQueryExemplars_ChDB_Roundtrip(t *testing.T) {
	// Anchor well in the past so request `start` / `end` framing the
	// query window is deterministic across any wall-clock drift in CI.
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	const tsFmt = "2006-01-02 15:04:05.000000000"
	tsBase := base.Format(tsFmt)
	// Three exemplars per row, each at a distinct sub-second offset
	// (10ms / 250ms / 500ms past the row's TimeUnix). The offsets are
	// chosen so the resulting unix-seconds float carries a non-trivial
	// fractional component — sanity check that the timestamp
	// projection round-trips through CH's DateTime64(9) without
	// dropping precision.
	tsEx1 := base.Add(10 * time.Millisecond).Format(tsFmt)
	tsEx2 := base.Add(250 * time.Millisecond).Format(tsFmt)
	tsEx3 := base.Add(500 * time.Millisecond).Format(tsFmt)

	seed := sumExemplarsDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_sum (
    MetricName, Attributes, ServiceName, TimeUnix, Value,
    Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value,
    Exemplars.SpanId, Exemplars.TraceId
) VALUES
    (
        'http_requests_total',
        map('job', 'api'),
        'checkout',
        toDateTime64('%s', 9),
        100.0,
        [map('request_id', 'req-a1'), map('request_id', 'req-a2'), map('request_id', 'req-a3')],
        [toDateTime64('%s', 9), toDateTime64('%s', 9), toDateTime64('%s', 9)],
        [0.001, 0.002, 0.003],
        ['span-a1', 'span-a2', 'span-a3'],
        ['trace-a1', 'trace-a2', 'trace-a3']
    ),
    (
        'http_requests_total',
        map('job', 'db'),
        'checkout',
        toDateTime64('%s', 9),
        200.0,
        [map('request_id', 'req-b1'), map('request_id', 'req-b2'), map('request_id', 'req-b3')],
        [toDateTime64('%s', 9), toDateTime64('%s', 9), toDateTime64('%s', 9)],
        [0.004, 0.005, 0.006],
        ['span-b1', 'span-b2', 'span-b3'],
        ['trace-b1', 'trace-b2', 'trace-b3']
    );`,
		tsBase, tsEx1, tsEx2, tsEx3,
		tsBase, tsEx1, tsEx2, tsEx3,
	)

	srv, _ := newChDBExemplarsServer(t, seed)

	startUnix := base.Add(-1 * time.Minute).Unix()
	endUnix := base.Add(1 * time.Minute).Unix()
	// `query=http_requests_total{job=~"api|db"}` — concrete __name__
	// matcher (handler requires it per PR #520) plus a regex label
	// matcher that selects both seeded series.
	url := fmt.Sprintf(
		`%s/api/v1/query_exemplars?query=http_requests_total%%7Bjob%%3D~%%22api%%7Cdb%%22%%7D&start=%d&end=%d`,
		srv.URL, startUnix, endUnix,
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out struct {
		Status string                `json:"status"`
		Data   []prom.ExemplarSeries `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q want success", out.Status)
	}

	// Plan §6 assertion: top-level `data` length == number of seeded
	// series. groupExemplars folds the six fanned-out rows (2 series *
	// 3 exemplars) into one ExemplarSeries per (MetricName, Attributes,
	// ServiceName) key. The two seeded series differ only in
	// Attributes['job'], so two ExemplarSeries land in the response.
	if got, want := len(out.Data), 2; got != want {
		t.Fatalf("len(data) = %d; want %d (one ExemplarSeries per series-key); got %+v",
			got, want, out.Data)
	}

	// Index the response by job-label so the per-series assertions
	// don't depend on the deterministic-key sort order surface
	// (groupExemplars uses format.CanonicalKey + sort.Strings).
	byJob := map[string]prom.ExemplarSeries{}
	for _, s := range out.Data {
		job := s.SeriesLabels["job"]
		if job == "" {
			t.Errorf("series missing 'job' label: %+v", s.SeriesLabels)
			continue
		}
		byJob[job] = s
	}
	for _, want := range []string{"api", "db"} {
		if _, ok := byJob[want]; !ok {
			t.Errorf("expected series with job=%s; got jobs=%v", want, exemplarSeriesKeys(byJob))
		}
	}

	// Per-series assertions: each series MUST carry __name__ + service_name
	// + job, and exactly three exemplars (the seeded fan-out). The
	// dotted OTel `service.name` collapses to the Prom-grammar
	// `service_name` form via format.NormalizeLabelMap at the emit site
	// in groupExemplars — the wire shape Grafana's overlay expects.
	for job, series := range byJob {
		if got := series.SeriesLabels["__name__"]; got != "http_requests_total" {
			t.Errorf("job=%s: __name__=%q want http_requests_total", job, got)
		}
		if got := series.SeriesLabels["service_name"]; got != "checkout" {
			t.Errorf("job=%s: service_name=%q want checkout", job, got)
		}
		if _, leaked := series.SeriesLabels["service.name"]; leaked {
			t.Errorf("job=%s: dotted service.name leaked through normalisation: %+v",
				job, series.SeriesLabels)
		}
		if got := series.SeriesLabels["job"]; got != job {
			t.Errorf("job=%s: SeriesLabels[job]=%q (inconsistent index)", job, got)
		}

		if got, want := len(series.Exemplars), 3; got != want {
			t.Errorf("job=%s: len(exemplars) = %d; want %d", job, got, want)
		}

		// Per-exemplar assertions:
		//   - trace_id + span_id non-empty (dedicated columns surface
		//     through projectExemplar's reserved-key merge);
		//   - request_id key from FilteredAttributes carries through;
		//   - value is non-zero (the seeded micro-second-scale latency);
		//   - timestamp is in the [start, end] window and carries the
		//     sub-second offset within DateTime64(9) precision.
		var prefix string
		switch job {
		case "api":
			prefix = "a"
		case "db":
			prefix = "b"
		}
		for i, ex := range series.Exemplars {
			if got := ex.Labels["trace_id"]; got == "" {
				t.Errorf("job=%s exemplar[%d]: trace_id empty; labels=%+v", job, i, ex.Labels)
			} else if want := fmt.Sprintf("trace-%s%d", prefix, i+1); got != want {
				t.Errorf("job=%s exemplar[%d]: trace_id=%q want %q", job, i, got, want)
			}
			if got := ex.Labels["span_id"]; got == "" {
				t.Errorf("job=%s exemplar[%d]: span_id empty; labels=%+v", job, i, ex.Labels)
			} else if want := fmt.Sprintf("span-%s%d", prefix, i+1); got != want {
				t.Errorf("job=%s exemplar[%d]: span_id=%q want %q", job, i, got, want)
			}
			if got := ex.Labels["request_id"]; got == "" {
				t.Errorf("job=%s exemplar[%d]: request_id (from FilteredAttributes) empty; labels=%+v",
					job, i, ex.Labels)
			} else if want := fmt.Sprintf("req-%s%d", prefix, i+1); got != want {
				t.Errorf("job=%s exemplar[%d]: request_id=%q want %q", job, i, got, want)
			}
			if ex.Value == 0 {
				t.Errorf("job=%s exemplar[%d]: value=0; want non-zero seeded value", job, i)
			}
			// Timestamp must fall in the request window. groupExemplars
			// fans out per-row exemplars verbatim — no bucketisation —
			// so the timestamps land at the exact seeded sub-second
			// offsets. Allow CH's DateTime64(9) round-trip slop
			// (~1ns) by widening the comparison to a full second on
			// each side; the load-bearing assertion is just that the
			// timestamp is a float (not a stringified value) in the
			// window.
			startF := float64(startUnix)
			endF := float64(endUnix)
			if ex.Timestamp < startF || ex.Timestamp > endF+1 {
				t.Errorf("job=%s exemplar[%d]: timestamp=%v outside [%v, %v]",
					job, i, ex.Timestamp, startF, endF)
			}
			// Sub-second component MUST be non-zero — exemplars are
			// seeded at 10ms / 250ms / 500ms offsets past the row's
			// TimeUnix. A zero fractional component means
			// timestampSeconds lost the nanos.
			frac := ex.Timestamp - float64(int64(ex.Timestamp))
			if frac == 0 {
				t.Errorf("job=%s exemplar[%d]: timestamp=%v has zero fractional second; "+
					"DateTime64(9) round-trip dropped nanos", job, i, ex.Timestamp)
			}
		}
	}
}

// TestQueryExemplars_ChDB_EmptyWindow seeds the same shape but requests
// a window entirely after the seed. The handler emits SQL with
// `TimeUnix >= ?` / `TimeUnix <= ?` bounds; chDB filters out every row;
// the response is `data:[]` (non-nil) — exercising the
// SQL-emits-time-bounds contract end-to-end.
func TestQueryExemplars_ChDB_EmptyWindow(t *testing.T) {
	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	const tsFmt = "2006-01-02 15:04:05.000000000"
	tsBase := base.Format(tsFmt)
	tsEx1 := base.Add(10 * time.Millisecond).Format(tsFmt)

	seed := sumExemplarsDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_sum (
    MetricName, Attributes, ServiceName, TimeUnix, Value,
    Exemplars.FilteredAttributes, Exemplars.TimeUnix, Exemplars.Value,
    Exemplars.SpanId, Exemplars.TraceId
) VALUES (
    'http_requests_total',
    map('job', 'api'),
    'checkout',
    toDateTime64('%s', 9),
    100.0,
    [map()],
    [toDateTime64('%s', 9)],
    [0.001],
    ['span-a1'],
    ['trace-a1']
);`, tsBase, tsEx1)

	srv, _ := newChDBExemplarsServer(t, seed)

	// Window 1 hour after the seed — excludes every row.
	startUnix := base.Add(1 * time.Hour).Unix()
	endUnix := base.Add(2 * time.Hour).Unix()
	url := fmt.Sprintf(
		`%s/api/v1/query_exemplars?query=http_requests_total%%7Bjob%%3D%%22api%%22%%7D&start=%d&end=%d`,
		srv.URL, startUnix, endUnix,
	)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected data:[] (non-nil empty array) on empty-window response; got %s", body)
	}
}

// exemplarSeriesKeys returns the sorted keys of m for deterministic
// error messages. Local helper — kept here rather than in the shared
// handler_test.go so the chdb-tagged file doesn't drag the helper
// into the untagged lane, and named distinctly from the `mapKeys`
// helper that handler_label_values_matched_test.go owns (Go would
// reject the same-package redeclaration in the chdb build).
func exemplarSeriesKeys(m map[string]prom.ExemplarSeries) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
