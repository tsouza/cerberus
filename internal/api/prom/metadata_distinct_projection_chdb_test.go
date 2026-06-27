//go:build chdb

package prom_test

// PARITY (Layer 6a — chDB roundtrip; result identity under the projection).
//
// The windowless metadata enumerations changed shape from
// `SELECT DISTINCT … WHERE TimeUnix >= <lookback>` to the grouped
// `… GROUP BY MetricName[, Attributes] HAVING max(TimeUnix) >= <lookback>`
// so they route onto the proj_series aggregating projection
// (metadata_scan_bound_explain_chdb_test.go pins the routing). These tests pin
// that the routed emit returns the IDENTICAL result set the bounded DISTINCT
// did — including the retention-horizon boundary:
//
//   - values with a sample inside the default lookback (recent / days-old)
//     are returned;
//   - a value whose newest sample is older than the lookback is excluded,
//     exactly as the `WHERE TimeUnix >= <lookback>` DISTINCT excluded it.
//
// Because samples are never future-dated, max(TimeUnix) >= lookback ⇔ a
// sample exists in [lookback, now], so the two shapes are byte-for-byte the
// same answer. The tables carry the projection (materialized) so the result is
// also proven correct when served from the projection, not just the base table.

import (
	"fmt"
	"testing"
	"time"
)

// addSeriesProjection installs + materializes proj_series on a metric table,
// mirroring the curated registry in internal/schema/ddl.
func addSeriesProjection(table string) string {
	return fmt.Sprintf(
		"ALTER TABLE %s ADD PROJECTION proj_series "+
			"(SELECT MetricName, Attributes, max(TimeUnix) GROUP BY MetricName, Attributes);\n"+
			"ALTER TABLE %s MATERIALIZE PROJECTION proj_series;\n",
		table, table,
	)
}

// projectionParitySeed seeds gauge+sum with names spanning the default
// retention lookback boundary, then installs + materializes proj_series
// (mirroring the cerberus DDL apply path). The returned sets are the oracle:
// names inside the lookback must list, the over-horizon name must not.
func projectionParitySeed() (seed string, want, excluded map[string]struct{}) {
	now := time.Now().UTC()
	lit := func(d time.Duration) string {
		return now.Add(-d).Format("2006-01-02 15:04:05.000000000")
	}
	// defaultMetadataLookback is 14d (internal/api/prom/metadata.go); seed one
	// name just inside it (13d) and one well outside (20d) to pin the boundary.
	const lookback = 14 * 24 * time.Hour
	seed = metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('g_recent',      map('job', 'fresh'), toDateTime64('%s', 9), 1.0),
    ('g_within',      map('job', 'mid'),   toDateTime64('%s', 9), 1.0),
    ('g_overhorizon', map('job', 'old'),   toDateTime64('%s', 9), 1.0);
INSERT INTO otel_metrics_sum (MetricName, Attributes, TimeUnix, Value, IsMonotonic) VALUES
    ('s_recent_total', map('job', 'fresh'), toDateTime64('%s', 9), 1.0, true);
`,
		lit(time.Minute),             // g_recent: 1 minute ago
		lit(13*24*time.Hour),         // g_within: 13 days ago (inside 14d lookback)
		lit(lookback+2*24*time.Hour), // g_overhorizon: 16 days ago (outside)
		lit(time.Minute),             // s_recent_total: 1 minute ago
	) + addSeriesProjection("otel_metrics_gauge") + addSeriesProjection("otel_metrics_sum")
	want = map[string]struct{}{"g_recent": {}, "g_within": {}, "s_recent_total": {}}
	excluded = map[string]struct{}{"g_overhorizon": {}}
	return seed, want, excluded
}

// TestMetadataDistinctProjection_WindowlessParity pins that the windowless
// /api/v1/label/__name__/values served from proj_series (re-aggregated to
// GROUP BY MetricName) returns exactly the bounded DISTINCT's name set: every
// name inside the default lookback, and none whose newest sample is over the
// horizon.
func TestMetadataDistinctProjection_WindowlessParity(t *testing.T) {
	seed, want, excluded := projectionParitySeed()
	srv, _ := newChDBServer(t, seed)

	got := stringData(t, getMetaData(t, srv.URL+"/api/v1/label/__name__/values"))
	gotSet := make(map[string]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	for name := range want {
		if _, ok := gotSet[name]; !ok {
			t.Errorf("windowless /label/__name__/values dropped in-lookback name %q (got %v)", name, got)
		}
	}
	for name := range excluded {
		if _, ok := gotSet[name]; ok {
			t.Errorf("windowless /label/__name__/values returned over-horizon name %q (got %v) — "+
				"the projection emit must match the bounded DISTINCT, which excludes it", name, got)
		}
	}
}

// labelValuesParitySeed seeds gauge with a `team` attribute whose values span
// the lookback boundary: one value carried only by an in-lookback series, one
// carried only by an over-horizon series. proj_series is installed so the
// generic label_values emit is served from the projection.
func labelValuesParitySeed() (seed string, want, excluded map[string]struct{}) {
	now := time.Now().UTC()
	lit := func(d time.Duration) string {
		return now.Add(-d).Format("2006-01-02 15:04:05.000000000")
	}
	const lookback = 14 * 24 * time.Hour
	seed = metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(
		`
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('g_a', map('team', 'fresh'),   toDateTime64('%s', 9), 1.0),
    ('g_a', map('team', 'recent'),  toDateTime64('%s', 9), 1.0),
    ('g_a', map('team', 'stale'),   toDateTime64('%s', 9), 1.0);
`,
		lit(time.Minute),             // team=fresh: 1 minute ago
		lit(13*24*time.Hour),         // team=recent: 13 days ago (inside lookback)
		lit(lookback+2*24*time.Hour), // team=stale: 16 days ago (over horizon)
	) + addSeriesProjection("otel_metrics_gauge")
	want = map[string]struct{}{"fresh": {}, "recent": {}}
	excluded = map[string]struct{}{"stale": {}}
	return seed, want, excluded
}

// TestMetadataLabelValuesProjection_WindowlessParity pins that the windowless
// /api/v1/label/team/values served from proj_series returns exactly the
// bounded DISTINCT's value set: in-lookback attribute values, none over horizon.
func TestMetadataLabelValuesProjection_WindowlessParity(t *testing.T) {
	seed, want, excluded := labelValuesParitySeed()
	srv, _ := newChDBServer(t, seed)

	got := stringData(t, getMetaData(t, srv.URL+"/api/v1/label/team/values"))
	gotSet := make(map[string]struct{}, len(got))
	for _, v := range got {
		gotSet[v] = struct{}{}
	}
	for v := range want {
		if _, ok := gotSet[v]; !ok {
			t.Errorf("windowless /label/team/values dropped in-lookback value %q (got %v)", v, got)
		}
	}
	for v := range excluded {
		if _, ok := gotSet[v]; ok {
			t.Errorf("windowless /label/team/values returned over-horizon value %q (got %v) — "+
				"the proj_series emit must match the bounded DISTINCT, which excludes it", v, got)
		}
	}
}
