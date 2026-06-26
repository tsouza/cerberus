//go:build chdb

package prom_test

// GUARD (Layer 6a — chDB roundtrip; result identity the fix MUST preserve).
//
// The repro side (metadata_scan_bound_repro_test.go +
// metadata_scan_bound_explain_chdb_test.go) demands the windowless
// metadata scan be BOUNDED. This guard pins the other, equally-binding
// half: a windowless discovery query must still return the COMPLETE
// catalog — every metric name / label / value present in the data, however
// long ago its newest sample landed.
//
// Why this matters: reference Prometheus answers a windowless
// /api/v1/label/__name__/values (and /labels, /label/<l>/values) over the
// ENTIRE TSDB retention — every name/value still inside the retention
// horizon, head + all persistent blocks. A "fix" that bounds the
// windowless scan to a short recent window (1h/6h/24h) prunes well but
// silently drops metrics that went quiet within retention — the
// no-allowlists / no-silent-drop failure mode. So the only correct bound
// is the retention horizon (data older than which is gone from a real
// Prometheus too); this guard seeds names whose only sample is days old
// (well inside any plausible retention) and asserts they are STILL
// returned. It is GREEN on main (the scan is unbounded today) and stays
// green only for a retention-scale default — it goes RED for a
// short-recent default, catching that divergence.
//
// The matched-path windowless completeness is covered by W5_omitted in
// handler_chdb_metadata_window_sweep_test.go; this file covers the
// no-`match[]` discovery arms (metricNamesSQL / unionLabelNamesSQL /
// unionLabelValuesSQL).

import (
	"fmt"
	"testing"
	"time"
)

// scanGuardSeed seeds gauge+sum with names whose newest sample spans from
// days-old to seconds-ago, all inside a plausible retention horizon. The
// returned map is the oracle: every distinct `__name__` the windowless
// catalog must list.
func scanGuardSeed() (string, map[string]struct{}) {
	now := time.Now().UTC()
	lit := func(d time.Duration) string {
		return now.Add(-d).Format("2006-01-02 15:04:05.000000000")
	}
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('g_ancient', map('job', 'old'),    toDateTime64('%s', 9), 1.0),
    ('g_recent',  map('job', 'fresh'),  toDateTime64('%s', 9), 1.0);
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, IsMonotonic) VALUES
    ('s_stale_total', map('job', 'mid'), toDateTime64('%s', 9), 1.0, true);`,
		lit(10*24*time.Hour), // g_ancient: newest sample 10 days ago
		lit(time.Minute),     // g_recent: newest sample 1 minute ago
		lit(7*24*time.Hour),  // s_stale_total: newest sample 7 days ago
	)
	want := map[string]struct{}{
		"g_ancient":     {},
		"g_recent":      {},
		"s_stale_total": {},
	}
	return seed, want
}

// TestMetadataScanBound_WindowlessCatalogComplete_Guard pins that a
// windowless /api/v1/label/__name__/values returns the FULL name catalog,
// including names whose only sample is days old. GREEN on main; a
// short-recent default lookback would drop g_ancient (10d) / s_stale_total
// (7d) and turn this RED — exactly the divergence the guard exists to
// catch.
func TestMetadataScanBound_WindowlessCatalogComplete_Guard(t *testing.T) {
	seed, want := scanGuardSeed()
	srv, _ := newChDBServer(t, seed)

	got := stringData(t, getMetaData(t, srv.URL+"/api/v1/label/__name__/values"))
	gotSet := make(map[string]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	for name := range want {
		if _, ok := gotSet[name]; !ok {
			t.Errorf("windowless /label/__name__/values dropped %q (got %v) — a windowless catalog must "+
				"list every retained name, not just recently-active ones", name, got)
		}
	}
}

// TestMetadataScanBound_WindowlessLabelsComplete_Guard pins the label-name
// and label-value discovery arms (unionLabelNamesSQL / unionLabelValuesSQL)
// over the same days-old corpus: a windowless /labels must surface `job`,
// and windowless /label/job/values must surface every job value, including
// those carried only by days-old series.
func TestMetadataScanBound_WindowlessLabelsComplete_Guard(t *testing.T) {
	seed, _ := scanGuardSeed()
	srv, _ := newChDBServer(t, seed)

	labels := stringData(t, getMetaData(t, srv.URL+"/api/v1/labels"))
	if !contains(labels, "__name__") || !contains(labels, "job") {
		t.Errorf("windowless /labels = %v, want at least [__name__ job]", labels)
	}

	jobs := stringData(t, getMetaData(t, srv.URL+"/api/v1/label/job/values"))
	for _, want := range []string{"old", "fresh", "mid"} {
		if !contains(jobs, want) {
			t.Errorf("windowless /label/job/values dropped %q (got %v) — every retained value must list",
				want, jobs)
		}
	}
}
