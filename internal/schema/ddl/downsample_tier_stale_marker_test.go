package ddl

import (
	"strings"
	"testing"
)

// downsampleTierRecordedValueWhere is the rendered stale-marker exclusion both
// tier views carry. internal/downsampletier's backfill / rebuild / verify
// SELECTs pin the identical text (its recordedValueSQL), so a live-MV row and
// a backfilled row fold the same set of raw rows.
const downsampleTierRecordedValueWhere = " WHERE bitAnd(`Flags`, 1) = 0 GROUP BY "

// TestRenderDownsampleTierViews_ExcludeStaleMarkers pins that both tier views
// drop OTel NoRecordedValue rows before folding: a stale marker's Value is
// the exporter's placeholder 0, which timeSeriesLastTwoSamplesState would
// otherwise keep as the bucket's trailing sample — a counter reset for
// irate(), a spurious 0 for last_over_time().
//
// The filter is gated on DownsampleTierFlagsColumn: a deployment whose metric
// tables do not all carry the Flags column reads every row as a sample on
// the PromQL read path, and its views must not name the column at all.
func TestRenderDownsampleTierViews_ExcludeStaleMarkers(t *testing.T) {
	cfg := Config{DownsampleTierEnabled: true, DownsampleTierFlagsColumn: "Flags"}.withDefaults()
	unprobed := Config{DownsampleTierEnabled: true}.withDefaults()
	for name, stmts := range map[string][2]string{
		"sum":   {renderDownsampleTierView(cfg), renderDownsampleTierView(unprobed)},
		"gauge": {renderDownsampleTierGaugeView(cfg), renderDownsampleTierGaugeView(unprobed)},
	} {
		if !strings.Contains(stmts[0], downsampleTierRecordedValueWhere) {
			t.Errorf("%s tier view lacks %q:\n%s", name, downsampleTierRecordedValueWhere, stmts[0])
		}
		if strings.Contains(stmts[1], "Flags") {
			t.Errorf("%s tier view without a Flags column names one:\n%s", name, stmts[1])
		}
	}
}

// TestDownsampleTierReprovisionSQL pins the re-provision sequence
// `cerberus schema downsample-tier-rebuild` runs: both tier views dropped
// (IF EXISTS, ON CLUSTER when configured), then each view RenderAll would
// provision re-created from the current render — the Gauge view only when
// Gauge and Sum are distinct tables, though its DROP is issued regardless.
func TestDownsampleTierReprovisionSQL(t *testing.T) {
	t.Run("distinct-gauge", func(t *testing.T) {
		cfg := Config{Cluster: "c1"}
		got, err := DownsampleTierReprovisionSQL(cfg)
		if err != nil {
			t.Fatalf("DownsampleTierReprovisionSQL: %v", err)
		}
		d := cfg.withDefaults()
		d.DownsampleTierEnabled = true
		want := []string{
			"DROP VIEW IF EXISTS default.otel_metrics_sum_downsample_tier_mv ON CLUSTER `c1`",
			"DROP VIEW IF EXISTS default.otel_metrics_sum_downsample_tier_gauge_mv ON CLUSTER `c1`",
			renderDownsampleTierView(d),
			renderDownsampleTierGaugeView(d),
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		// The CREATEs are exactly the view statements RenderAll provisions.
		all, err := RenderAll(Config{Cluster: "c1", DownsampleTierEnabled: true}, []Signal{Metrics})
		if err != nil {
			t.Fatalf("RenderAll: %v", err)
		}
		for _, create := range got[2:] {
			found := false
			for _, stmt := range all {
				found = found || stmt == create
			}
			if !found {
				t.Errorf("re-provision CREATE is not a RenderAll statement:\n%s", create)
			}
		}
	})
	t.Run("gauge-equals-sum", func(t *testing.T) {
		cfg := Config{}
		cfg.Tables.MetricsGauge = defaultMetricsSumTable
		got, err := DownsampleTierReprovisionSQL(cfg)
		if err != nil {
			t.Fatalf("DownsampleTierReprovisionSQL: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("got %d statements; want 3 (two DROPs, the Sum view CREATE):\n%s", len(got), strings.Join(got, "\n"))
		}
		if !strings.HasPrefix(got[1], "DROP VIEW IF EXISTS default.otel_metrics_sum_downsample_tier_gauge_mv") {
			t.Errorf("the Gauge view must still be dropped, got %q", got[1])
		}
	})
}
