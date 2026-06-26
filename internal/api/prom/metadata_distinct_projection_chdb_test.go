//go:build chdb

package prom_test

// PARITY (Layer 6a — chDB roundtrip; result identity under the projection).
//
// The windowless metric-name enumeration changed shape from
// `SELECT DISTINCT MetricName WHERE TimeUnix >= <lookback>` to
// `SELECT MetricName GROUP BY MetricName HAVING max(TimeUnix) >= <lookback>`
// so it can route to the proj_metric_name aggregating projection
// (metadata_scan_bound_explain_chdb_test.go pins the routing). This test
// pins that the routed emit returns the IDENTICAL name set the bounded
// DISTINCT did — including the retention-horizon boundary:
//
//   - names with a sample inside the default lookback (recent / days-old)
//     are returned;
//   - a name whose newest sample is older than the lookback is excluded,
//     exactly as the `WHERE TimeUnix >= <lookback>` DISTINCT excluded it.
//
// Because samples are never future-dated, max(TimeUnix) >= lookback ⇔ a
// sample exists in [lookback, now], so the two shapes are byte-for-byte the
// same answer. The table carries the projection (materialized) so the result
// is also proven correct when served from the projection, not just the base
// table.

import (
	"fmt"
	"testing"
	"time"
)

// projectionParitySeed seeds gauge+sum with names spanning the default
// retention lookback boundary, then installs + materializes the metric-name
// projection (mirroring the cerberus DDL apply path). The returned sets are
// the oracle: names inside the lookback must list, the over-horizon name
// must not.
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
ALTER TABLE otel_metrics_gauge ADD PROJECTION proj_metric_name (SELECT MetricName, max(TimeUnix) GROUP BY MetricName);
ALTER TABLE otel_metrics_sum ADD PROJECTION proj_metric_name (SELECT MetricName, max(TimeUnix) GROUP BY MetricName);
ALTER TABLE otel_metrics_gauge MATERIALIZE PROJECTION proj_metric_name;
ALTER TABLE otel_metrics_sum MATERIALIZE PROJECTION proj_metric_name;`,
		lit(time.Minute),             // g_recent: 1 minute ago
		lit(13*24*time.Hour),         // g_within: 13 days ago (inside 14d lookback)
		lit(lookback+2*24*time.Hour), // g_overhorizon: 16 days ago (outside)
		lit(time.Minute),             // s_recent_total: 1 minute ago
	)
	want = map[string]struct{}{"g_recent": {}, "g_within": {}, "s_recent_total": {}}
	excluded = map[string]struct{}{"g_overhorizon": {}}
	return seed, want, excluded
}

// TestMetadataDistinctProjection_WindowlessParity pins that the windowless
// /api/v1/label/__name__/values served from the aggregating projection
// returns exactly the bounded DISTINCT's name set: every name inside the
// default lookback, and none whose newest sample is over the horizon.
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
