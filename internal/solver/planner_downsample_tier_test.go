package solver

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// downsampleTierGuardStep / Range / OuterRange define the synthetic
// K-clipping-guard fixture for issue #2859: a tier-eligible idelta() shape
// whose Range and Step are both positive multiples of
// schema.DownsampleTierBucket and whose Start is bucket-aligned —
// downsampleTierEligible's own contract (internal/promql/lower_strategy.go).
//
// OuterRange is chosen at exactly 800m / 240m = 3.33x its own Range, so the
// PRE-#2859 D=Range ceiling (floor(OuterRange/Range) = floor(800m/240m) = 3)
// sits BELOW the structural MaxK=8 backstop and is therefore the BINDING
// constraint, while the shape's own N/MinAnchorsPerSlice-derived K
// (floor(161/16) = 10) clears both ceilings — so which ceiling binds is what
// this fixture isolates.
//
// Step == schema.DownsampleTierBucket (the smallest step downsampleTierEligible
// ever admits, since it requires Step to be a positive integer multiple of the
// bucket) is also why the POST-fix ceiling lands on Step rather than on the
// bucket width itself: denom = max(D, Step) always resolves to Step for a real
// tier-eligible plan, because D can never be narrower than the bucket it is
// now capped at, and Step can never be narrower than that same bucket either.
var (
	downsampleTierGuardStep       = schema.DownsampleTierBucket      // 5m — the minimum tier-eligible step
	downsampleTierGuardRange      = 48 * schema.DownsampleTierBucket // 4h
	downsampleTierGuardOuterRange = 160 * downsampleTierGuardStep    // 13h20m, N = 161
	downsampleTierGuardStart      = gridStart.Truncate(schema.DownsampleTierBucket)
	downsampleTierGuardEnd        = downsampleTierGuardStart.Add(downsampleTierGuardOuterRange)
)

// downsampleTierGuardWindow builds the fixture's RangeWindow node.
// tierEligible controls DownsampleTier alone: with it false, the node is
// BYTE-IDENTICAL to what carrierGeometryOf saw before issue #2859's fix
// (which never branched on the flag, so a DownsampleTierInput-populated node
// got the same D=Range treatment as a plain raw-scan one) — the exact
// pre-fix cost model, reproduced by construction rather than by reverting
// code. With it true, the node is what internal/promql/lower_strategy.go's
// downsampleTierNode actually emits for this shape once the boot-wired
// DownsampleTier*Lowerer routes it (idelta / irate / last_over_time only).
func downsampleTierGuardWindow(tierEligible bool) chplan.Node {
	return &chplan.RangeWindow{
		Input:               leafScan(),
		Func:                "idelta",
		Range:               downsampleTierGuardRange,
		Step:                downsampleTierGuardStep,
		OuterRange:          downsampleTierGuardOuterRange,
		Start:               downsampleTierGuardStart,
		End:                 downsampleTierGuardEnd,
		TimestampColumn:     "TimeUnix",
		ValueColumn:         "Value",
		GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		DownsampleTierInput: leafScan(),
		DownsampleTier:      tierEligible,
	}
}

func downsampleTierGuardMeta() RequestMeta {
	return RequestMeta{
		Lang:  "promql",
		Start: downsampleTierGuardStart,
		End:   downsampleTierGuardEnd,
		Step:  downsampleTierGuardStep,
	}
}

// TestPlan_DownsampleTierRelievesKClippingGuard is issue #2859's required
// proof: the SAME tier-eligible shape's K-clipping-guard decision (the
// floor(OuterRange/max(D,Step)) ceiling tied to issue #2685's MaxK clamp)
// actually changes once the solver's cost model is told DownsampleTier is
// set, and does not merely stay routed at the same K.
func TestPlan_DownsampleTierRelievesKClippingGuard(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := downsampleTierGuardMeta()

	before, routedBefore := p.Plan(downsampleTierGuardWindow(false), meta)
	if !routedBefore {
		t.Fatalf("pre-fix (raw-scan cost model) shape must route; reason=%q", before.Reason)
	}
	if before.Reason != ReasonRouted {
		t.Fatalf("pre-fix reason = %q, want %q", before.Reason, ReasonRouted)
	}
	if before.K != 3 {
		t.Fatalf("pre-fix K = %d, want 3 (D=Range=4h clips floor(OuterRange/Range)=floor(800m/240m)=3, below MaxK=8)", before.K)
	}

	after, routedAfter := p.Plan(downsampleTierGuardWindow(true), meta)
	if !routedAfter {
		t.Fatalf("tier-aware shape must route; reason=%q", after.Reason)
	}
	if after.Reason != ReasonRouted {
		t.Fatalf("tier-aware reason = %q, want %q", after.Reason, ReasonRouted)
	}
	if after.K != 8 {
		t.Fatalf("tier-aware K = %d, want 8 (D capped at schema.DownsampleTierBucket=5m <= Step, so Step binds "+
			"the ceiling and the structural MaxK=8 backstop decides instead)", after.K)
	}

	if after.K <= before.K {
		t.Fatalf("DownsampleTier awareness must raise K above the pre-fix value: before=%d after=%d", before.K, after.K)
	}
}

// TestPlan_DownsampleTierLeavesFanoutUnchanged pins the OTHER half of the
// same fix: F (the ModeAuto fan-out/MinFanout memory proxy) is deliberately
// LEFT ALONE by the DownsampleTier override — the tier emitter
// (chsql.emitRangeWindowDownsampleTier) still arrayJoins each tier row across
// its covering anchors exactly like the raw fan-out does, so F stays
// Range/Step regardless of the flag. Only D (the redundancy/K-ceiling term)
// changes — see carrierGeometry.redundancyLookback's own doc for why the two
// answer different questions here.
func TestPlan_DownsampleTierLeavesFanoutUnchanged(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := downsampleTierGuardMeta()

	before, routedBefore := p.Plan(downsampleTierGuardWindow(false), meta)
	if !routedBefore {
		t.Fatalf("pre-fix shape must route; reason=%q", before.Reason)
	}
	after, routedAfter := p.Plan(downsampleTierGuardWindow(true), meta)
	if !routedAfter {
		t.Fatalf("tier-aware shape must route; reason=%q", after.Reason)
	}

	wantFanout := int64(downsampleTierGuardRange / downsampleTierGuardStep) // 240m/5m = 48
	if before.Fanout != wantFanout {
		t.Fatalf("pre-fix Fanout = %d, want %d", before.Fanout, wantFanout)
	}
	if after.Fanout != before.Fanout {
		t.Fatalf("DownsampleTier must not change Fanout: before=%d after=%d", before.Fanout, after.Fanout)
	}
}

// TestPlan_DownsampleTierIneligible_ByteIdenticalToNoSignal pins the
// default-off contract at the STRUCT level rather than the flag level: a
// RangeWindow whose DownsampleTierInput is nil and DownsampleTier is false —
// every deployment that has not wired cerberus issue #2751's feature at
// all — reproduces the ORIGINAL (pre-#2859) K decision for the identical
// grid geometry, confirming the new redundancyLookback branch is truly
// unreachable off this one flag.
func TestPlan_DownsampleTierIneligible_ByteIdenticalToNoSignal(t *testing.T) {
	t.Parallel()
	p := &Planner{Cfg: autoCfg()}
	meta := downsampleTierGuardMeta()

	rw := &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "idelta",
		Range:           downsampleTierGuardRange,
		Step:            downsampleTierGuardStep,
		OuterRange:      downsampleTierGuardOuterRange,
		Start:           downsampleTierGuardStart,
		End:             downsampleTierGuardEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		// DownsampleTierInput and DownsampleTier both left zero-valued.
	}
	d, routed := p.Plan(rw, meta)
	if !routed {
		t.Fatalf("shape must route; reason=%q", d.Reason)
	}
	if d.K != 3 {
		t.Fatalf("K = %d, want 3 (unchanged from the raw-scan cost model; DownsampleTierInput is nil)", d.K)
	}
}
