package chsql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestMetricsTableShapeLeadsWithMetricName is the cheap, always-on
// check-gate guard for the landed metrics-table ORDER BY win (#791):
// every OTel-CH metrics table's TableShape.SortColumns must lead with
// MetricName, in the exact order
//
//	[MetricName, Attributes, ServiceName, TimeUnix]
//
// This pins the cerberus-ddl fork's ORDER BY decision *in cerberus's
// own granule-pruning model*. The emitter ranks predicates by their
// position in SortColumns (see SortRank / emitFilter), so a regression
// that re-led the sort key with ServiceName — the OTel upstream default
// — would silently re-introduce the generic-exclusion granule scan the
// fork patch was built to avoid (measured 8–17× more granules touched
// on the common metric-name-first, no-service.name Grafana query; see
// test/perf/orderby_chdb_test.go for the chDB EXPLAIN proof).
//
// Unlike that chDB harness, this test compiles under the default tag and
// runs on the always-on `check` gate, so an in-repo revert of
// tableshape.go's metricsShape ordering fails instantly on every PR —
// no libchdb.so, no EXPLAIN, no wall-clock. It is a pure-function pin
// over the static shape table.
func TestMetricsTableShapeLeadsWithMetricName(t *testing.T) {
	t.Parallel()

	m := schema.DefaultOTelMetrics()

	want := []string{
		m.MetricNameColumn,  // "MetricName" — MUST be rank 0
		m.AttributesColumn,  // "Attributes"
		m.ServiceNameColumn, // "ServiceName"
		m.TimestampColumn,   // "TimeUnix"
	}

	// All five metrics tables share the single metricsShape value, so the
	// pin must hold for each — a future per-table override that diverged
	// the gauge table's sort key from the histogram table's would also be
	// caught here.
	tables := map[string]string{
		"gauge":     m.GaugeTable,
		"sum":       m.SumTable,
		"histogram": m.HistogramTable,
		"exphist":   m.ExpHistogramTable,
		"summary":   m.SummaryTable,
	}

	for label, tbl := range tables {
		shape := tableShapeFor(tbl)
		got := shape.SortColumns

		if len(got) != len(want) {
			t.Fatalf("%s table %q: SortColumns length = %d (%v), want %d (%v)",
				label, tbl, len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s table %q: SortColumns[%d] = %q, want %q "+
					"(full got=%v, want=%v).\n"+
					"The metrics ORDER BY MUST lead with MetricName — leading with "+
					"ServiceName (the OTel upstream default) re-introduces the "+
					"generic-exclusion granule scan the cerberus-ddl fork patch "+
					"removed (8–17× more granules on metric-name-first queries).",
					label, tbl, i, got[i], want[i], got, want)
			}
		}

		// MetricName MUST be the leading (rank-0) sort column — the
		// single most load-bearing fact for the granule-prune win.
		if r := shape.SortRank(m.MetricNameColumn); r != 0 {
			t.Fatalf("%s table %q: SortRank(MetricName) = %d, want 0 "+
				"(MetricName must lead the ORDER BY)", label, tbl, r)
		}
	}
}
