//go:build chdb

// chDB-backed wire-level pins for the duplicate-labelset abort raised by
// the INSTANT-shape sites (#1839): the instant math fns, the clamp family,
// unary minus, the date fns, scalar⊗vector arithmetic, and the two label
// rewrites (`label_replace` / `label_join`).
//
// handler_chdb_duplicate_labelset_test.go covers the RANGE half (#1811) —
// a name-dropping `rate` / `*_over_time` over a multi-metric selector. The
// two halves exist because cerberus lowers each operation into its own SQL
// shape; upstream needs no such split, because `rangeEval` checks EVERY
// step's output vector for a repeated label set from one generic site
// (prometheus/promql/engine.go:1531, :1552) and every operation inherits
// it. That asymmetry is the whole content of #1839: the guard was wired
// into two lowering functions instead of into the property they share.
//
// Both collision routes are covered here, because they are not the same
// check:
//
//   - DROPPING `__name__` makes two series that differed only by name
//     collide, and `uniqExact(MetricName) > 1` detects it.
//   - REWRITING the label set makes two series with the SAME name collide
//     when the rewrite is non-injective, and only `count() > 1` over the
//     preserved identity detects that — distinct-name counting answers 1.
//
// Every assertion is at the wire (status, errorType, message text), which
// is what Grafana surfaces as an error rather than as a silently-merged
// panel. Each rejecting test is paired with a distinct-labelset control
// that must still answer 200, so the guard is pinned as a match for
// upstream rather than merely stricter than it.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// sameMetricTwoSeriesSeed writes ONE metric (`cpu_temp`) as TWO series
// distinguished only by the caller-supplied attribute maps, one sample per
// minute across [start-15m, end].
//
// dupLabelsetSeed cannot stand in for this: it writes two DIFFERENT metric
// names, which is the right shape for the name-drop half and the wrong one
// for the label-rewrite half. A rewrite preserves `__name__`, so two series
// only collide under it when they already share a name — exactly what this
// seed builds.
func sameMetricTwoSeriesSeed(t *testing.T, start, end time.Time, attrsA, attrsB string) string {
	t.Helper()
	const (
		seedLead  = 15 * time.Minute
		seedEvery = time.Minute
	)
	rows := ""
	value := 0
	for ts := start.Add(-seedLead); !ts.After(end); ts = ts.Add(seedEvery) {
		lit := ts.Format("2006-01-02 15:04:05.000000000")
		if rows != "" {
			rows += ",\n    "
		}
		rows += fmt.Sprintf(
			"('cpu_temp', '', '', %[2]s, toDateTime64('%[1]s', 9), %[4]d.0),\n    "+
				"('cpu_temp', '', '', %[3]s, toDateTime64('%[1]s', 9), %[5]d.0)",
			lit, attrsA, attrsB, 40+value, 70+value,
		)
		value++
	}
	return metaShapedMetricsDDL + `
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ` + rows + ";"
}

// assertNamelessSeriesByHost pins a SUCCESSFUL answer to a name-dropping
// query: 200, one series per distinct host, and no `__name__` on any of
// them. It is the control half of every rejection below — the shape the
// guard must leave untouched.
func assertNamelessSeriesByHost(t *testing.T, body string, status int, query string, wantHosts ...string) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
	}
	vec := decodeVector(t, body, query)
	if len(vec) != len(wantHosts) {
		t.Fatalf("%s: got %d series, want %d (distinct label sets must still answer): %+v",
			query, len(vec), len(wantHosts), vec)
	}
	seen := map[string]bool{}
	for _, s := range vec {
		if name, ok := s.Metric["__name__"]; ok {
			t.Errorf("%s: __name__ %q survived the name-dropping operation (full metric: %+v)",
				query, name, s.Metric)
		}
		seen[s.Metric["host"]] = true
	}
	for _, want := range wantHosts {
		if !seen[want] {
			t.Errorf("%s: expected a series with host=%q, got %v", query, want, seen)
		}
	}
}

// decodeVector unwraps an instant-query body into its vector samples.
func decodeVector(t *testing.T, body, query string) []prom.VectorSample {
	t.Helper()
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("%s: unmarshal: %v; body=%s", query, err, body)
	}
	rawResult, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("%s: re-marshal result: %v; body=%s", query, err, body)
	}
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("%s: decode vector: %v; body=%s", query, err, body)
	}
	return vec
}

// instantQuery issues an /api/v1/query at `end` and returns status + body.
func instantQuery(t *testing.T, srvURL, query string, end time.Time) (int, string) {
	t.Helper()
	return getBody(t, fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
		srvURL, url.QueryEscape(query), end.Unix()))
}

// TestQuery_InstantNameDrop_DuplicateLabelset_ChDB drives every instant
// shape that drops `__name__` over one multi-metric selector whose two
// series share an attribute set. Each must be refused, because after the
// drop both rows carry the identical label set `{host="a"}`.
//
// They are driven as one table rather than as one test per function
// because the guard is ONE mechanism — [guardedValueProjection] — and the
// point of the table is that every caller of it inherits the check. A
// per-function test would pass just as well against six per-site patches,
// which is the shape #1839 exists to retire.
func TestQuery_InstantNameDrop_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"))

	const selector = `{__name__=~"cpu_temp|gpu_temp"}`
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"instant_math_fn", `ceil(` + selector + `)`},
		{"round_to_nearest", `round(` + selector + `, 5)`},
		{"clamp_max", `clamp_max(` + selector + `, 100)`},
		{"clamp_three_arg", `clamp(` + selector + `, 0, 100)`},
		{"unary_minus", `-` + selector},
		{"date_fn", `day_of_month(` + selector + `)`},
		{"scalar_times_vector", `2 * ` + selector},
		{"vector_divided_by_scalar", selector + ` / 2`},
		{"bool_comparison", selector + ` > bool 0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := instantQuery(t, srv.URL, tc.query, end)
			assertDuplicateLabelsetRejected(t, body, status, tc.query)
		})
	}
}

// TestQuery_InstantNameDrop_DistinctLabelsets_ChDB is the other half of
// the boundary: the SAME multi-metric name-dropping queries over series
// whose attribute sets DIFFER must still answer, one nameless series per
// source series. Erroring here would be the same class of bug in the other
// direction, and it is the assertion that proves the guard aborts on a
// collision rather than on the mere presence of two metric names.
func TestQuery_InstantNameDrop_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"))

	const selector = `{__name__=~"cpu_temp|gpu_temp"}`
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"instant_math_fn", `ceil(` + selector + `)`},
		{"clamp_max", `clamp_max(` + selector + `, 1000)`},
		{"unary_minus", `-` + selector},
		{"scalar_times_vector", `2 * ` + selector},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := instantQuery(t, srv.URL, tc.query, end)
			assertNamelessSeriesByHost(t, body, status, tc.query, "a", "b")
		})
	}
}

// TestQuery_PinnedNameDrop_NoGuard_ChDB pins the narrowing that keeps the
// guard off the overwhelmingly common query shape: a selector PINNED to
// one metric name cannot produce two rows under one label set, so
// `ceil(cpu_temp)` must answer exactly as it did before the guard existed.
//
// Without this, "the guard is correct" and "the guard fires on everything"
// are indistinguishable — and a guard that fires on everything is a
// learned-threshold degeneracy, not a fix.
func TestQuery_PinnedNameDrop_NoGuard_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, sameMetricTwoSeriesSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"))

	const query = `ceil(cpu_temp)`
	status, body := instantQuery(t, srv.URL, query, end)
	assertNamelessSeriesByHost(t, body, status, query, "a", "b")
}

// TestQuery_LabelRewrite_DuplicateLabelset_ChDB drives the rewrite half.
// Both queries take TWO SERIES OF ONE METRIC and map them onto a single
// label set:
//
//   - label_replace overwrites `host` with a constant, so `host=a` and
//     `host=b` both become `host=z`.
//   - label_join with zero src labels joins to the empty string, which the
//     emit path's outer mapFilter drops — so `host` disappears from both.
//
// Their `__name__` values are EQUAL (`cpu_temp`) throughout, which is
// precisely why the name-drop half's `uniqExact(MetricName) > 1` cannot
// serve here: it would count 1 and let both collisions through.
func TestQuery_LabelRewrite_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, sameMetricTwoSeriesSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"))

	for _, tc := range []struct {
		name  string
		query string
	}{
		{
			"label_replace_overwrites_distinguishing_label",
			`label_replace(cpu_temp, "host", "z", "host", ".*")`,
		},
		{
			"label_join_no_srcs_drops_distinguishing_label",
			`label_join(cpu_temp, "host", "-")`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := instantQuery(t, srv.URL, tc.query, end)
			assertDuplicateLabelsetRejected(t, body, status, tc.query)
		})
	}
}

// TestQuery_LabelRewrite_InjectiveRewrite_ChDB is the rewrite half's
// control, and it carries one assertion the name-drop controls cannot: a
// label rewrite PRESERVES `__name__`, so the answer must still name
// `cpu_temp`.
//
// That is the load-bearing pin on the outer canonical Project the guard
// wraps itself in. A `chplan.Aggregate` is classified derived-shape, and
// the HTTP layer stamps an EMPTY MetricName over a derived shape — so
// wrapping the rewrite in a bare guard Aggregate would silently strip the
// name from every label_replace answer while every status code stayed 200.
func TestQuery_LabelRewrite_InjectiveRewrite_ChDB(t *testing.T) {
	start, end, _ := subqueryNameWindow()
	srv, _ := newChDBServer(t, sameMetricTwoSeriesSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"))

	// Writes a CONSTANT into a label that was not previously present, so
	// the two series stay distinguished by `host` and nothing collides.
	const query = `label_replace(cpu_temp, "env", "prod", "", ".*")`
	status, body := instantQuery(t, srv.URL, query, end)
	if status != http.StatusOK {
		t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
	}

	vec := decodeVector(t, body, query)
	if len(vec) != 2 {
		t.Fatalf("%s: got %d series, want 2 (host=a and host=b stay distinct): %+v", query, len(vec), vec)
	}
	for _, s := range vec {
		if got := s.Metric["__name__"]; got != "cpu_temp" {
			t.Errorf("%s: __name__: got %q, want %q — label_replace preserves the metric name, "+
				"so the guard must not leave the plan classified derived-shape (full metric: %+v)",
				query, got, "cpu_temp", s.Metric)
		}
		if got := s.Metric["env"]; got != "prod" {
			t.Errorf("%s: env: got %q, want %q (full metric: %+v)", query, got, "prod", s.Metric)
		}
	}
}

// TestQueryRange_InstantNameDrop_DuplicateLabelset_ChDB pins the PER-STEP
// boundary. In range mode the guard adds the step timestamp to its
// grouping key, because upstream's check runs once per output vector and
// a key of the label set alone would instead merge a single series' own
// steps into one row — under-reporting the collision AND corrupting the
// answer that survives.
func TestQueryRange_InstantNameDrop_DuplicateLabelset_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'a')"))

	const query = `ceil({__name__=~"cpu_temp|gpu_temp"})`
	status, body := getBody(t, fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
	))
	assertDuplicateLabelsetRejected(t, body, status, query)
}

// TestQueryRange_InstantNameDrop_DistinctLabelsets_ChDB is the range-mode
// control. It is the assertion that would fail if the guard keyed on the
// label set alone in range mode: two distinct series over N steps would
// collapse to two single-point series instead of two N-point ones.
func TestQueryRange_InstantNameDrop_DistinctLabelsets_ChDB(t *testing.T) {
	start, end, step := subqueryNameWindow()
	srv, _ := newChDBServer(t, dupLabelsetSeed(t, start, end,
		"map('host', 'a')", "map('host', 'b')"))

	const query = `ceil({__name__=~"cpu_temp|gpu_temp"})`
	status, body := getBody(t, fmt.Sprintf(
		"%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		srv.URL, url.QueryEscape(query), start.Unix(), end.Unix(), int(step.Seconds()),
	))
	if status != http.StatusOK {
		t.Fatalf("%s: status: got %d, want 200; body=%s", query, status, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, body)
	}
	rawResult, err := json.Marshal(parsed.Data.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v; body=%s", err, body)
	}
	var matrix []prom.MatrixSample
	if err := json.Unmarshal(rawResult, &matrix); err != nil {
		t.Fatalf("decode matrix: %v; body=%s", err, body)
	}
	if len(matrix) != 2 {
		t.Fatalf("%s: got %d series, want 2 (host=a and host=b are distinct label sets): %+v",
			query, len(matrix), matrix)
	}

	wantSteps := int(end.Sub(start)/step) + 1
	for _, series := range matrix {
		if name, ok := series.Metric["__name__"]; ok {
			t.Errorf("%s: __name__ %q survived ceil (full metric: %+v)", query, name, series.Metric)
		}
		if len(series.Values) != wantSteps {
			t.Errorf("%s: series %+v has %d points, want %d — a guard keyed on the label set "+
				"WITHOUT the step timestamp would collapse every step into one row",
				query, series.Metric, len(series.Values), wantSteps)
		}
	}
}
