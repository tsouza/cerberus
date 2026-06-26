package prom_test

// REPRO (Layer 7/8 — HTTP-handler SQL-shape, default tag, no chDB).
//
// Pins the unbounded metadata-scan bug: a Grafana variable/metric-picker
// query (`label_values(__name__)`, `label_values(<l>)`, `/labels`,
// `/series`, `/api/v1/metadata`) that carries NO `start`/`end` lowers to
// SQL with NO time predicate at all, so ClickHouse reads every
// `toDate(TimeUnix)` partition — the ~30B-row / 59GB full-column scan seen
// on prod.
//
// The honest knob that makes this visible without chDB: the metadata
// emitters bound the scan with a `toDateTime64('…', 9)` TimeUnix literal
// (dateTime64Frag for the no-`match[]` arms, metadataBoundExpr for the
// matched path) ONLY when a window is present. metadataWindowPred returns
// nil for the zero/zero case, so every windowless arm emits a WHERE-less
// (or, for /api/v1/metadata, GROUP-BY-only) full-table scan. This test
// asserts the inverse — that EVERY windowless arm carries the
// `toDateTime64(` bound — so it is RED on current main and turns green
// once a default lookback is bounded onto the windowless path.
//
// These assertions are the SQL-shape side of the bug. The result-identity
// side (a bounded windowless scan must still return the SAME catalog it
// does today — no silently-dropped recently-quiet metric) is pinned
// separately by the chDB completeness guard in
// metadata_scan_bound_guard_chdb_test.go and the existing W5_omitted
// case in handler_chdb_metadata_window_sweep_test.go. Both halves must
// hold: the fix bounds the scan AND preserves the answer.

import (
	"net/http"
	"strings"
	"testing"
)

// dt64Bound is the inline TimeUnix window literal both metadata emit paths
// render — present in the SQL iff a partition-pruning bound was pushed.
const dt64Bound = "toDateTime64("

// anyContains reports whether any recorded statement contains sub.
func anyContains(stmts []string, sub string) bool {
	for _, s := range stmts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// getDiscard GETs u and discards the body; the assertion is on the SQL the
// stub recorded, not the response.
func getDiscard(t *testing.T, u string) {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	resp.Body.Close()
}

// TestMetadataScanBound_NoMatchWindowless_Repro covers the no-`match[]`
// discovery arms — the exact shapes a Grafana variable query with default
// "On dashboard load" refresh sends (no start/end):
//
//   - /label/__name__/values  -> metricNamesSQL        (the 59% prod shape)
//   - /labels                 -> unionLabelNamesSQL
//   - /label/<l>/values       -> unionLabelValuesSQL
//
// On main each emits a WHERE-less DISTINCT/mapKeys scan; the assertion
// that the bound is present fails.
func TestMetadataScanBound_NoMatchWindowless_Repro(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"metric_names", "/api/v1/label/__name__/values"},
		{"label_names", "/api/v1/labels"},
		{"label_values", "/api/v1/label/job/values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{strings: []string{"up"}}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			getDiscard(t, srv.URL+tc.path)

			if !anyContains(q.allSQL, dt64Bound) {
				t.Errorf("windowless %s emitted no %s TimeUnix bound — full-partition scan; arms=%q",
					tc.path, dt64Bound, q.allSQL)
			}
		})
	}
}

// TestMetadataScanBound_MatchedWindowless_Repro covers the matched path —
// `match[]` present but no start/end — which lowers through
// promql.LowerMetadataRange -> wrapMetadataFullRange. With both bounds
// zero the Filter node is omitted entirely, so the GROUP BY
// (MetricName, Attributes) collapse runs over the whole table.
func TestMetadataScanBound_MatchedWindowless_Repro(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"series", "/api/v1/series?match%5B%5D=up"},
		{"label_values_matched", "/api/v1/label/job/values?match%5B%5D=up"},
		{"labels_matched", "/api/v1/labels?match%5B%5D=up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{
				strings:   []string{"api"},
				labelSets: []map[string]string{{"__name__": "up", "job": "api"}},
			}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			getDiscard(t, srv.URL+tc.path)

			if !anyContains(q.allSQL, dt64Bound) {
				t.Errorf("windowless matched %s emitted no %s TimeUnix bound — whole-table GROUP BY; arms=%q",
					tc.path, dt64Bound, q.allSQL)
			}
		})
	}
}

// TestMetadataScanBound_MetricMeta_Repro covers /api/v1/metadata
// (metricMetaSQL). This arm is UNCONDITIONALLY unbounded today: it accepts
// no time input at all, so `SELECT MetricName, any(desc), any(unit) FROM
// <table> GROUP BY MetricName` runs a whole-table GROUP BY on every call.
// Unlike the others it cannot be fixed by threading the request window
// alone — a default bound must be injected here too. The assertion that
// the emit carries a TimeUnix bound fails on main.
func TestMetadataScanBound_MetricMeta_Repro(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	getDiscard(t, srv.URL+"/api/v1/metadata")

	if !anyContains(q.allSQL, dt64Bound) {
		t.Errorf("/api/v1/metadata emitted no %s TimeUnix bound — unconditional whole-table GROUP BY; arms=%q",
			dt64Bound, q.allSQL)
	}
}
