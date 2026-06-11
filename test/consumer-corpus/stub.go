package consumercorpus

import (
	"context"
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// StubQuerier serves canned rows for every Querier method the three
// handlers (api/prom, api/loki, api/tempo) define. It is the default
// lane's backend: requests flow through the full handler pipeline
// (param parsing, QL parse, lowering, SQL emission, response shaping)
// and only the ClickHouse execution step is replaced by canned rows.
//
// The canned-row shapes mirror the wire projections the engine's
// wrap-projections emit (reserved __cerberus_* label slots etc.) —
// they are lifted from the per-handler stub fixtures in
// internal/api/*/conformance_test.go and friends.
type StubQuerier struct {
	Samples      []chclient.Sample
	Strings      []string
	LabelSets    []map[string]string
	MetaRows     []chclient.MetricMetaRow
	DetectedRows []chclient.DetectedFieldRow
}

func (s *StubQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return s.Samples, nil
}

func (s *StubQuerier) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	return &sliceCursor{samples: s.Samples}, nil
}

func (s *StubQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	return s.Strings, nil
}

func (s *StubQuerier) QueryLabelSets(context.Context, string, ...any) ([]map[string]string, error) {
	return s.LabelSets, nil
}

func (s *StubQuerier) QueryMetricMeta(_ context.Context, _, metricType string, _ ...any) ([]chclient.MetricMetaRow, error) {
	// The metadata handler fans out once per table kind and stamps
	// the kind itself; serve the canned rows only for the kind they
	// declare so the fan-out doesn't triple-report.
	var out []chclient.MetricMetaRow
	for _, r := range s.MetaRows {
		if r.Type == metricType {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *StubQuerier) QueryExemplars(context.Context, string, ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

func (s *StubQuerier) QueryDetectedFieldRows(context.Context, string, ...any) ([]chclient.DetectedFieldRow, error) {
	return s.DetectedRows, nil
}

func (s *StubQuerier) QueryTimestampedLines(context.Context, string, ...any) ([]chclient.TimestampedLine, error) {
	return nil, nil
}

func (s *StubQuerier) QueryIndexStats(context.Context, string, ...any) (chclient.IndexStatsRow, error) {
	return chclient.IndexStatsRow{}, nil
}

func (s *StubQuerier) QueryIndexVolume(context.Context, string, ...any) ([]chclient.IndexVolumeRow, error) {
	return nil, nil
}

// sliceCursor adapts canned samples to the chclient.Cursor contract.
type sliceCursor struct {
	samples []chclient.Sample
	i       int
}

func (c *sliceCursor) Next() bool {
	if c.i >= len(c.samples) {
		return false
	}
	c.i++
	return true
}

func (c *sliceCursor) Sample() chclient.Sample { return c.samples[c.i-1] }
func (c *sliceCursor) Err() error              { return nil }
func (c *sliceCursor) Close() error            { return nil }

// Stub-lane time anchors. Every canned fixture timestamps its rows
// against these, and StubTokens derives the request windows from the
// same values, so windows and rows always agree.
var (
	stubTempoAnchor = time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	stubLokiAnchor  = time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	stubPromAnchor  = time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
)

// StubTraceID is the trace the trace-by-id fixtures carry; the
// stub lane resolves ${TRACE_ID} to it.
const StubTraceID = "f48694fee9f78da6f98ec5a8cd7d3274"

// StubTokens returns the ${TOKEN} map for the stub lane, per
// datasource.
func StubTokens(datasource string) map[string]string {
	var start, end time.Time
	switch datasource {
	case "tempo":
		start, end = stubTempoAnchor, stubTempoAnchor.Add(3*time.Minute)
	case "loki":
		start, end = stubLokiAnchor, stubLokiAnchor.Add(5*time.Minute)
	case "prom":
		start, end = stubPromAnchor.Add(-5*time.Minute), stubPromAnchor
	}
	return map[string]string{
		"START_UNIX": fmt.Sprintf("%d", start.Unix()),
		"END_UNIX":   fmt.Sprintf("%d", end.Unix()),
		"START_NS":   fmt.Sprintf("%d", start.UnixNano()),
		"END_NS":     fmt.Sprintf("%d", end.UnixNano()),
		"TRACE_ID":   StubTraceID,
	}
}

// StubFixture resolves a corpus entry's named stub fixture.
func StubFixture(name string) (*StubQuerier, error) {
	fn, ok := stubFixtures[name]
	if !ok {
		return nil, fmt.Errorf("unknown stub fixture %q", name)
	}
	return fn(), nil
}

// KnownStubFixture reports fixture-name validity for the ratchet.
func KnownStubFixture(name string) bool { _, ok := stubFixtures[name]; return ok }

var stubFixtures = map[string]func() *StubQuerier{
	// empty backs entries whose default-lane contract is routing +
	// status + envelope decodability with no canned rows (e.g. the
	// drilldown breakdown queries, whose historical failure mode was
	// a 422 at parse/lower time — before any SQL runs).
	"empty": func() *StubQuerier { return &StubQuerier{} },

	// tempo-trace-by-id mirrors traceByIDFixtureSamples in
	// internal/api/tempo/handler_trace_v2_test.go: the reserved-key
	// label contract the engine's trace-by-id wrap-projection emits.
	"tempo-trace-by-id": func() *StubQuerier {
		span := func(svc, name, spanID, parentID string, at time.Time) chclient.Sample {
			return chclient.Sample{
				MetricName: name,
				Labels: map[string]string{
					"service.name":             svc,
					"__cerberus_traceID":       StubTraceID,
					"__cerberus_spanID":        spanID,
					"__cerberus_parentSpanID":  parentID,
					"__cerberus_spanKind":      "Server",
					"__cerberus_statusCode":    "Ok",
					"__cerberus_spanAttrsJSON": `{"http.method":"GET"}`,
				},
				Timestamp: at,
				Value:     1_500_000,
			}
		}
		return &StubQuerier{Samples: []chclient.Sample{
			span("frontend", "GET /", "aaaa000000000001", "", stubTempoAnchor),
			span("backend", "SELECT users", "bbbb000000000002", "aaaa000000000001", stubTempoAnchor.Add(2*time.Millisecond)),
		}}
	},

	// tempo-search mirrors searchSpanRow in
	// internal/api/tempo/handler_test.go: canonical /api/search rows
	// with the reserved trace/span/parent ID slots toTraceSummaries
	// pivots into spanSets.
	"tempo-search": func() *StubQuerier {
		row := func(spanID, name string, at time.Time, durNs float64) chclient.Sample {
			return chclient.Sample{
				MetricName: name,
				Labels: map[string]string{
					"service.name":            "checkout",
					"__cerberus_traceID":      StubTraceID,
					"__cerberus_parentSpanID": "0000000000000000",
					"__cerberus_spanID":       spanID,
				},
				Timestamp: at,
				Value:     durNs,
			}
		}
		return &StubQuerier{Samples: []chclient.Sample{
			row("0000000000000001", "GET /api/users", stubTempoAnchor, 150_000_000),
			row("0000000000000002", "db.query", stubTempoAnchor.Add(10*time.Millisecond), 50_000_000),
		}}
	},

	// tempo-search-select mirrors the select() rows in
	// internal/api/tempo/search_select_attrs_test.go: selected values
	// ride reserved __cerberus_sel_* slots (int-typed nested-set
	// intrinsics, string-typed attributes, the span name).
	"tempo-search-select": func() *StubQuerier {
		return &StubQuerier{Samples: []chclient.Sample{
			{
				MetricName: "GET /home",
				Labels: map[string]string{
					"service.name":                       "frontend",
					"__cerberus_traceID":                 StubTraceID,
					"__cerberus_parentSpanID":            "",
					"__cerberus_spanID":                  "1000000000000001",
					"__cerberus_sel_name":                "GET /home",
					"__cerberus_sel_int_nestedSetParent": "-1",
					"__cerberus_sel_int_nestedSetLeft":   "1",
					"__cerberus_sel_int_nestedSetRight":  "8",
					"__cerberus_sel_str_status":          "unset",
					"__cerberus_sel_str_service.name":    "frontend",
				},
				Timestamp: stubTempoAnchor,
				Value:     1000,
			},
			{
				MetricName: "checkout",
				Labels: map[string]string{
					"service.name":                       "checkout",
					"__cerberus_traceID":                 StubTraceID,
					"__cerberus_parentSpanID":            "1000000000000001",
					"__cerberus_spanID":                  "1000000000000003",
					"__cerberus_sel_name":                "checkout",
					"__cerberus_sel_int_nestedSetParent": "1",
					"__cerberus_sel_int_nestedSetLeft":   "4",
					"__cerberus_sel_int_nestedSetRight":  "7",
					"__cerberus_sel_str_status":          "error",
					"__cerberus_sel_str_service.name":    "checkout",
				},
				Timestamp: stubTempoAnchor.Add(2 * time.Millisecond),
				Value:     700,
			},
		}}
	},

	// tempo-compare mirrors compareRow in
	// internal/api/tempo/metrics_query_range_compare_test.go: the raw
	// __is_sel / __attr / __val projection wrapCompareForSample emits.
	"tempo-compare": func() *StubQuerier {
		row := func(isSel, attr, val string, count float64) chclient.Sample {
			return chclient.Sample{
				Labels:    map[string]string{"__is_sel": isSel, "__attr": attr, "__val": val},
				Timestamp: stubTempoAnchor.Add(time.Minute),
				Value:     count,
			}
		}
		return &StubQuerier{Samples: []chclient.Sample{
			row("0", "resource.service.name", "shop", 3),
			row("1", "resource.service.name", "shop", 1),
		}}
	},

	// loki-detected-fields mirrors the logfmt fixture in
	// internal/api/loki/detected_fields_test.go (duration-typed field
	// included — the type drives Logs Drilldown's unwrap query
	// generation).
	"loki-detected-fields": func() *StubQuerier {
		return &StubQuerier{DetectedRows: []chclient.DetectedFieldRow{
			{Line: `level=info method=GET status=200 duration=12ms`},
			{Line: `level=error method=POST status=500 duration=1s`},
		}}
	},

	// loki-streams: log lines ride MetricName (the body slot of the
	// log-sample projection) with stream labels in Labels.
	"loki-streams": func() *StubQuerier {
		return &StubQuerier{Samples: []chclient.Sample{
			{MetricName: "level=info duration=100ms msg=ok id=1", Labels: map[string]string{"service_name": "api"}, Timestamp: stubLokiAnchor},
			{MetricName: "level=error duration=400ms msg=boom id=2", Labels: map[string]string{"service_name": "api"}, Timestamp: stubLokiAnchor.Add(time.Second)},
		}}
	},

	// prom-vector: an instant-vector result for the Metrics Drilldown
	// regex __name__ selector.
	"prom-vector": func() *StubQuerier {
		return &StubQuerier{Samples: []chclient.Sample{
			{MetricName: "cerberus_query_inflight", Labels: map[string]string{"cerberus_ql": "promql"}, Timestamp: stubPromAnchor, Value: 2},
			{MetricName: "cerberus_query_inflight", Labels: map[string]string{"cerberus_ql": "logql"}, Timestamp: stubPromAnchor, Value: 1},
		}}
	},

	// prom-labels: /api/v1/labels string rows.
	"prom-labels": func() *StubQuerier {
		return &StubQuerier{Strings: []string{"__name__", "cerberus_ql"}}
	},

	// prom-label-values: /api/v1/label/cerberus_ql/values rows.
	"prom-label-values": func() *StubQuerier {
		return &StubQuerier{Strings: []string{"logql", "promql", "traceql"}}
	},

	// prom-series: /api/v1/series reuses the instant-query pipeline
	// (fetchSeries → executeInstant), so the canned rows are Samples;
	// the handler dedupes them into label sets.
	"prom-series": func() *StubQuerier {
		return &StubQuerier{Samples: []chclient.Sample{
			{MetricName: "cerberus_query_inflight", Labels: map[string]string{"cerberus_ql": "promql"}, Timestamp: stubPromAnchor, Value: 2},
			{MetricName: "cerberus_query_inflight", Labels: map[string]string{"cerberus_ql": "logql"}, Timestamp: stubPromAnchor, Value: 1},
		}}
	},

	// prom-metadata: /api/v1/metadata rows (one per table-kind
	// fan-out arm the stub answers).
	"prom-metadata": func() *StubQuerier {
		return &StubQuerier{MetaRows: []chclient.MetricMetaRow{
			{Name: "cerberus_query_inflight", Description: "in-flight queries", Unit: "", Type: "gauge"},
		}}
	},
}
