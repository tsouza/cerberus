//go:build chdb

// Exercises the audit against a REAL engine, because both of its queries lean
// on ClickHouse-specific semantics a fake would simply agree with:
// `mapFilter` over a Map(String,String), and `arrayJoin(mapKeys(...))` to
// enumerate label keys per row. A hand-rolled stub would prove the Go
// arithmetic and nothing about whether the audit can actually be answered.
package chaudit_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chaudit"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestProbe_NamesTheAmplifyingLabelAndTheBudgetStanding(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	ctx := context.Background()

	for _, stmt := range []string{`
CREATE OR REPLACE TABLE aud (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, TimeUnix)`, `
INSERT INTO aud
SELECT 'http_requests',
       map('route', concat('/r', toString(number % 7)),
           'pod',   concat('pod-', toString(number % 40))),
       now64(9) - toIntervalSecond(number % 600),
       arrayMap(x -> toFloat64(x), range(12))
FROM numbers(2000)`, `
INSERT INTO aud
SELECT 'well_shaped',
       map('route', concat('/w', toString(number % 8))),
       now64(9) - toIntervalSecond(number % 600),
       arrayMap(x -> toFloat64(x), range(12))
FROM numbers(2000)`} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	rep, err := chaudit.Probe(ctx, db, chaudit.Options{
		Table:             "aud",
		WindowSeconds:     3600,
		Anchors:           360,
		DensityUnitBudget: 1_000_000,
		Top:               10,
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if len(rep.Metrics) != 2 {
		t.Fatalf("audited %d metrics, want 2: %+v", len(rep.Metrics), rep.Metrics)
	}

	byName := map[string]chaudit.MetricAudit{}
	for _, m := range rep.Metrics {
		byName[m.Metric] = m
	}

	// The defective metric must NAME the offending label, which is the whole
	// point: reporting only "320 series" leaves the operator where they
	// started, with no way to tell an expensive dimension from a leaked one.
	bad := byName["http_requests"]
	if bad.AmplifyingLabel != "pod" {
		t.Errorf("amplifying label = %q (ratio %.1f), want \"pod\" — an audit that cannot name\n"+
			"the offending label reports a symptom instead of a remedy",
			bad.AmplifyingLabel, bad.AmplificationRatio)
	}
	if bad.AmplificationRatio < 4 {
		t.Errorf("amplification ratio = %.1f, want >= 4 (320 series collapsing to 8 without `pod`)",
			bad.AmplificationRatio)
	}
	if bad.Series <= byName["well_shaped"].Series {
		t.Errorf("the defective metric (%d series) should out-cardinal the well-shaped one (%d)",
			bad.Series, byName["well_shaped"].Series)
	}

	// The well-shaped metric must NOT be accused. A report that flags
	// everything is one an operator learns to ignore.
	if good := byName["well_shaped"]; good.AmplifyingLabel != "" {
		t.Errorf("well-shaped metric was accused of amplifying via %q (ratio %.1f); its single\n"+
			"label IS the dimension the panel groups by", good.AmplifyingLabel, good.AmplificationRatio)
	}

	// Budget arithmetic must be real, and worst-first ordering must hold so the
	// metric needing action is the one read first.
	for _, m := range rep.Metrics {
		if m.CostUnits <= 0 || m.Budget != 1_000_000 {
			t.Errorf("%s: cost/budget not evaluated: %+v", m.Metric, m)
		}
	}
	if rep.Metrics[0].HeadroomPct > rep.Metrics[1].HeadroomPct {
		t.Errorf("report is not worst-first: %.1f%% before %.1f%%",
			rep.Metrics[0].HeadroomPct, rep.Metrics[1].HeadroomPct)
	}
	t.Logf("worst: %s cost=%d budget=%d headroom=%.1f%% amplifier=%q x%.1f",
		rep.Metrics[0].Metric, rep.Metrics[0].CostUnits, rep.Metrics[0].Budget,
		rep.Metrics[0].HeadroomPct, rep.Metrics[0].AmplifyingLabel, rep.Metrics[0].AmplificationRatio)
}
