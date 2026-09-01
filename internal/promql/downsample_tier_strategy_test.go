package promql_test

// Resolution-aware eligibility for the operator opt-in downsampled
// long-range tier (cerberus issue #2751). These are plain lowering-shape
// tests (no ClickHouse / chDB involved) — they pin WHICH shapes the
// boot-wired DownsampleTier{Irate,Idelta,LastOverTime}Lowerer strategies
// route to the tier by inspecting the lowered chplan.RangeWindow.DownsampleTier
// / .DownsampleTierInput fields directly, mirroring how
// native_strategy_coverage_test.go and the family's own dual-emit chDB
// tests split "does the RIGHT shape route" (here, cheap) from "is the SQL
// correct" (internal/chsql's downsample-tier chDB test, which needs a real
// engine).
//
// The critical, explicitly-required-by-the-issue property under test:
// resolution-awareness. A query whose step is FINER than the tier's fixed
// bucket (schema.DownsampleTierBucket, 5 minutes) must NEVER route to the
// tier — "a 15s-step query must never touch a 5m tier".

import (
	"context"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// downsampleTierLowerers builds a RangeLowerers with the DownsampleTier
// strategy wired for irate/idelta/last_over_time, wrapping the plain
// fan-out (no ts_grid_irate/idelta/last_over_time native layering) — the
// minimal wiring cmd/cerberus's nativeRangeLowerers would produce with ONLY
// chopt.FeatureDownsampleTier resolved.
func downsampleTierLowerers() promql.RangeLowerers {
	return promql.RangeLowerers{
		Irate:        promql.DownsampleTierIrateLowerer{Fallback: promql.FanoutIrateLowerer{}},
		Idelta:       promql.DownsampleTierIdeltaLowerer{Fallback: promql.FanoutIdeltaLowerer{}},
		LastOverTime: promql.DownsampleTierLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}},
	}
}

// lowerDownsampleTierQuery lowers query in range mode [start, end] at step,
// with the downsample-tier strategy wired, and returns the first
// *chplan.RangeWindow found in the plan (depth-first) plus whether one was
// found at all.
func lowerDownsampleTierQuery(t *testing.T, query string, start, end time.Time, step time.Duration) (*chplan.RangeWindow, bool) {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		start, end, step, promql.LowerOpts{Lowerers: downsampleTierLowerers()})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	var found *chplan.RangeWindow
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && found == nil {
			found = rw
			return false
		}
		return true
	})
	return found, found != nil
}

// bucketAlignedStart is an absolute instant aligned to schema.DownsampleTierBucket
// (an exact multiple of 5 minutes since the Unix epoch) — every eligible
// case below anchors on it; the misaligned cases perturb it.
var bucketAlignedStart = time.Date(2026, 1, 1, 0, 25, 0, 0, time.UTC)

func TestDownsampleTierEligible_IrateBucketAlignedStepEqualsBucket(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if !rw.DownsampleTier {
		t.Error("expected DownsampleTier=true for a bucket-aligned, step==bucket irate() query")
	}
	if rw.DownsampleTierInput == nil {
		t.Error("expected DownsampleTierInput to be populated")
	}
}

func TestDownsampleTierEligible_IdeltaStepMultipleOfBucket(t *testing.T) {
	// step = 2x bucket — still an integer multiple, so still eligible
	// ("step >= bucket", not "step == bucket").
	rw, ok := lowerDownsampleTierQuery(t, `idelta(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(time.Hour), 2*schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if !rw.DownsampleTier {
		t.Error("expected DownsampleTier=true for a bucket-aligned idelta() query whose step is a multiple of the bucket")
	}
}

func TestDownsampleTierEligible_LastOverTime(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `last_over_time(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if !rw.DownsampleTier {
		t.Error("expected DownsampleTier=true for a bucket-aligned last_over_time() query")
	}
}

// TestDownsampleTierIneligible_StepFinerThanBucket is the issue's own
// headline resolution-awareness requirement: "a 15s-step query must never
// touch a 5m tier".
func TestDownsampleTierIneligible_StepFinerThanBucket(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(time.Minute), 15*time.Second)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Error("a 15s-step query must never route to the 5m tier, but DownsampleTier=true")
	}
}

func TestDownsampleTierIneligible_StepNotMultipleOfBucket(t *testing.T) {
	// step = 7 minutes: coarser than the 5m bucket, but NOT an integer
	// multiple of it, so successive anchors drift off the bucket grid.
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(21*time.Minute), 7*time.Minute)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Error("a step that is not an integer multiple of the bucket must never route to the tier")
	}
}

// TestDownsampleTierEligible_RangeMultipleOfBucket proves cerberus issue
// #2857's headline extension: a range spanning several tier buckets (15m ==
// 3x the 5m bucket) now routes, not just the exact-bucket N==1 case #2751
// shipped.
func TestDownsampleTierEligible_RangeMultipleOfBucket(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[15m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if !rw.DownsampleTier {
		t.Error("a range that is an integer multiple of the bucket (15m == 3x5m) must route to the tier")
	}
}

func TestDownsampleTierIneligible_RangeNotMultipleOfBucket(t *testing.T) {
	// range (7m) is coarser than the 5m bucket but NOT an integer multiple
	// of it, so it cannot be covered by a whole run of tier buckets.
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[7m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Error("range that is not an integer multiple of the bucket must never route to the tier")
	}
}

func TestDownsampleTierIneligible_RangeNarrowerThanBucket(t *testing.T) {
	// range (1m) is narrower than a single 5m bucket.
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[1m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Error("range narrower than one bucket must never route to the tier")
	}
}

func TestDownsampleTierIneligible_UnalignedStart(t *testing.T) {
	// bucketAlignedStart + 1 minute is NOT itself a multiple of 5m since
	// epoch, even though the step is a clean bucket multiple.
	unaligned := bucketAlignedStart.Add(time.Minute)
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[5m])`,
		unaligned, unaligned.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Error("an eval grid whose Start is not bucket-aligned must never route to the tier")
	}
}

func TestDownsampleTierIneligible_NonzeroOffset(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `irate(cpu_seconds_total[5m] offset 5m)`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Error("a nonzero offset must never route to the tier (v1 scope)")
	}
}

func TestDownsampleTierIneligible_InstantQuery(t *testing.T) {
	// Step == 0 (an instant query, not query_range) is never eligible — the
	// tier only ever routes a materialised grid.
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`irate(cpu_seconds_total[5m])`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Step==0 is instant-query mode (a single anchor at End, matching
	// LowerAtRangeOpts's own "Zero means instant query" contract) — there
	// is no dedicated LowerAtOpts entry point, so this drives the same
	// range-mode entry point with Step==0 and Start==End.
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		bucketAlignedStart, bucketAlignedStart, 0, promql.LowerOpts{Lowerers: downsampleTierLowerers()})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	var found *chplan.RangeWindow
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && found == nil {
			found = rw
			return false
		}
		return true
	})
	if found == nil {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if found.DownsampleTier {
		t.Error("an instant (Step==0) query must never route to the tier")
	}
}

// TestDownsampleTierIneligible_RateNeverWired proves rate()/increase() have
// structurally NO downsample-tier routing at all: RangeLowerers carries no
// DownsampleTier-wrapped Rate/Increase field for cmd/cerberus to ever wire
// (see chopt.FeatureDownsampleTier's own doc on the hard scope boundary),
// so a rate() query lowered with the SAME downsampleTierLowerers() table
// this file uses for irate/idelta/last_over_time never sets
// DownsampleTierInput at all — there is no field on RangeLowerers that
// COULD route it there.
func TestDownsampleTierIneligible_RateNeverWired(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `rate(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier {
		t.Fatal("rate() must never set DownsampleTier — there is no DownsampleTierRateLowerer")
	}
	if rw.DownsampleTierInput != nil {
		t.Error("rate() must never populate DownsampleTierInput — downsampleTierEligibleFunc excludes it")
	}
}

func TestDownsampleTierIneligible_IncreaseNeverWired(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `increase(cpu_seconds_total[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTierInput != nil {
		t.Error("increase() must never populate DownsampleTierInput — downsampleTierEligibleFunc excludes it")
	}
}

func TestDownsampleTierIneligible_DeltaNeverWired(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `delta(cpu_gauge[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTierInput != nil {
		t.Error("delta() must never populate DownsampleTierInput — downsampleTierEligibleFunc excludes it")
	}
}

// TestDownsampleTierDisabled_FallsThroughToFanout proves the whole
// mechanism is inert unless a caller wires it in RangeLowerers — mirroring
// the family's own "off by default" contract. Lowering the SAME eligible
// query with the ZERO-VALUE RangeLowerers (what a deployment without
// chopt.FeatureDownsampleTier resolved gets — see RangeLowerers.withDefaults)
// must never set DownsampleTier.
func TestDownsampleTierDisabled_FallsThroughToFanout(t *testing.T) {
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`irate(cpu_seconds_total[5m])`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket,
		promql.LowerOpts{}) // zero-value Lowerers — no strategy wired at all
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	var found *chplan.RangeWindow
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && found == nil {
			found = rw
			return false
		}
		return true
	})
	if found == nil {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if found.DownsampleTier {
		t.Error("DownsampleTier must stay false when no DownsampleTier*Lowerer was wired")
	}
}

// --- Gauge-table eligibility for last_over_time() (cerberus issue #2858) ---
//
// These are the plain lowering-shape counterparts of the Sum-table tests
// above, using rangeVectorSingleGaugeTable's own unambiguous-resolution
// requirement (internal/promql/lower.go) instead of
// rangeVectorCounterTemporalityColumn's. An UNSUFFIXED metric name is
// otherwise ambiguous between Gauge and Sum (schema.Metrics.TablesForUnknownName's
// own doc — the SAME pre-existing restriction #1628/#2751 already impose on
// an unsuffixed COUNTER metric's own irate()/idelta() eligibility), so every
// test below clears SumTable to give the fixture metric name an unambiguous
// Gauge-only resolution — see
// internal/chsql's range_window_downsample_tier_gauge_chdb_test.go
// (downsampleTierGaugeOnlySchema) for the identical reasoning against a real
// ClickHouse engine.

// lowerDownsampleTierQueryWithSchema mirrors lowerDownsampleTierQuery but
// takes an explicit schema.Metrics rather than hard-coding
// schema.DefaultOTelMetrics() — needed so a test can clear SumTable to make
// an unsuffixed metric name resolve unambiguously to Gauge.
func lowerDownsampleTierQueryWithSchema(t *testing.T, s schema.Metrics, query string, start, end time.Time, step time.Duration) (*chplan.RangeWindow, bool) {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s,
		start, end, step, promql.LowerOpts{Lowerers: downsampleTierLowerers()})
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	var found *chplan.RangeWindow
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if rw, ok := n.(*chplan.RangeWindow); ok && found == nil {
			found = rw
			return false
		}
		return true
	})
	return found, found != nil
}

// gaugeOnlySchema returns schema.DefaultOTelMetrics() with SumTable cleared
// — see this section's own doc for why an unambiguous Gauge resolution
// needs it.
func gaugeOnlySchema() schema.Metrics {
	m := schema.DefaultOTelMetrics()
	m.SumTable = ""
	return m
}

func TestDownsampleTierEligible_LastOverTimeGaugeTable(t *testing.T) {
	rw, ok := lowerDownsampleTierQueryWithSchema(t, gaugeOnlySchema(), `last_over_time(cpu_temperature[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if !rw.DownsampleTier {
		t.Error("expected DownsampleTier=true for a bucket-aligned last_over_time() query over an unambiguous Gauge-table metric")
	}
	if rw.DownsampleTierInput == nil {
		t.Error("expected DownsampleTierInput to be populated")
	}
}

// TestDownsampleTierIneligible_IrateGaugeTable proves irate() stays
// Sum/Histogram-only even over an UNAMBIGUOUS Gauge-table resolution — a
// gauge has no counter-reset semantics for irate() to apply.
// TestDownsampleTierIneligible_IdeltaGaugeTable mirrors it for idelta().
func TestDownsampleTierIneligible_IrateGaugeTable(t *testing.T) {
	rw, ok := lowerDownsampleTierQueryWithSchema(t, gaugeOnlySchema(), `irate(cpu_temperature[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier || rw.DownsampleTierInput != nil {
		t.Error("irate() must never route to the tier for a Gauge-table metric, even unambiguously resolved")
	}
}

func TestDownsampleTierIneligible_IdeltaGaugeTable(t *testing.T) {
	rw, ok := lowerDownsampleTierQueryWithSchema(t, gaugeOnlySchema(), `idelta(cpu_temperature[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier || rw.DownsampleTierInput != nil {
		t.Error("idelta() must never route to the tier for a Gauge-table metric, even unambiguously resolved")
	}
}

// TestDownsampleTierIneligible_LastOverTimeNoGaugeTableConfigured proves
// rangeVectorSingleGaugeTable's own s.GaugeTable == "" guard: a deployment
// with no Gauge table configured at all can never have an "unambiguous
// Gauge-table resolution", so last_over_time() stays off the tier for it —
// as opposed to falling through to resolveUnambiguousScanTable and
// (incorrectly) matching an empty string against an equally-empty
// s.GaugeTable. SumTable stays configured (unlike gaugeOnlySchema's own
// Sum-disabled shape) so `_total`-suffixed selectors elsewhere in this
// schema still resolve normally — only the Gauge arm is disabled.
func TestDownsampleTierIneligible_LastOverTimeNoGaugeTableConfigured(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.GaugeTable = ""
	rw, ok := lowerDownsampleTierQueryWithSchema(t, s, `last_over_time(cpu_temperature[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier || rw.DownsampleTierInput != nil {
		t.Error("last_over_time() must not route to the tier when no Gauge table is configured at all")
	}
}

// TestDownsampleTierIneligible_LastOverTimeAmbiguousGaugeSum proves the
// pre-existing (Gauge, Sum) ambiguity fan-out for an unsuffixed metric name
// (schema.Metrics.TablesForUnknownName, both tables configured) keeps
// last_over_time() OUT of the tier — the same restriction #1628/#2751
// already impose on irate()/idelta() for an unsuffixed counter metric,
// applied symmetrically here rather than newly introduced.
func TestDownsampleTierIneligible_LastOverTimeAmbiguousGaugeSum(t *testing.T) {
	rw, ok := lowerDownsampleTierQuery(t, `last_over_time(cpu_temperature[5m])`,
		bucketAlignedStart, bucketAlignedStart.Add(20*time.Minute), schema.DownsampleTierBucket)
	if !ok {
		t.Fatal("expected a RangeWindow in the plan")
	}
	if rw.DownsampleTier || rw.DownsampleTierInput != nil {
		t.Error("last_over_time() must not route to the tier for an unsuffixed name ambiguous between Gauge and Sum")
	}
}
