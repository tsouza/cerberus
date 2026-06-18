package promql

import (
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file defines the BOOT-WIRED POLYMORPHIC lowering seam for the
// ClickHouse-native timeSeries*ToGrid family. The decision of WHICH lowering
// to use (native aggregate vs. the generic SQL fan-out) is made ONCE at boot —
// from the already-resolved chopt.EnabledSet — and injected as a RangeLowerers
// table. The per-query lowering path then calls through that table with NO
// feature-flag or server-version conditional: the only per-query decisions are
// AST node-type dispatch and query-SHAPE eligibility, which live INSIDE the
// chosen strategy (a shape it can't handle delegates to the fan-out fallback by
// returning nil).
//
// Why a per-FUNCTION table rather than one global bool: the features are
// independent (native rate may be on while native resample is off, and vice
// versa), so the wiring composes per-function. cmd/cerberus builds the concrete
// strategies once from EnabledSet.Has(chopt.FeatureTSGridRange) /
// .Has(chopt.FeatureTSGridResample) and threads the table down through the prom
// handler -> lang adapter -> LowerOpts. The promql package cannot import chopt
// (the dependency-cone rule), so the strategy TYPES live here and the
// feature/version READ lives at boot — exactly where the rule requires it.

// RateLowerer lowers an eligible range-mode rate RangeWindow to its native
// chplan node, or returns nil to fall through to the generic fan-out
// (RangeWindow). The shape eligibility (rate-over-counter with a materialised
// grid and a plain Scan/Filter input) is intrinsic and lives inside the
// implementation; it is NOT a feature-flag branch.
type RateLowerer interface {
	// LowerRate returns a non-nil RangeWindowNative for a shape it handles, or
	// nil to keep the caller's fan-out RangeWindow.
	LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// StalenessLowerer lowers an eligible range-mode bare instant-vector selection
// (the staleness shape) to its native chplan node, or returns nil to fall
// through to the generic fan-out (RangeLWR). The build closure carries the
// already-resolved scan/pred/anchor/grid for the selector; the strategy reads
// only intrinsic shape from it (never a feature flag).
type StalenessLowerer interface {
	// LowerStaleness returns a non-nil RangeWindowResample for a shape it
	// handles, or nil to keep the caller's fan-out RangeLWR.
	LowerStaleness(in stalenessLowerInput) chplan.Node
}

// stalenessLowerInput carries the resolved inputs the range-mode staleness wrap
// has already computed (the matchers-filtered scan, the eval grid, the offset,
// and the schema column names), so a native strategy can build the resample
// node without re-deriving them. It is the intrinsic SHAPE description — no
// feature/version state rides here.
type stalenessLowerInput struct {
	// input is the matchers-filtered scan (Scan, Filter-over-Scan, or the
	// gauge+sum merge / companion union) exposing the canonical column names.
	input chplan.Node
	// start / end / step define the materialised query_range grid; lookback is
	// the staleness horizon (instantLookback). offset folds the PromQL offset
	// onto the membership window.
	start, end             time.Time
	step, lookback, offset time.Duration

	metricNameCol, attributesCol string
	timestampCol, valueCol       string
}

// RangeLowerers is the boot-wired dispatch table the lowering reads. Each field
// is the concrete strategy for one promql function family, decided once at
// boot. A nil field means "fan-out only" for that family — the zero value of
// RangeLowerers is therefore the all-fan-out default, so any caller that does
// not opt in (every path but the query_range handler) lowers byte-identically
// to the pre-seam behaviour.
type RangeLowerers struct {
	// Rate handles eligible range-mode rate(...) shapes. Nil => fan-out only.
	Rate RateLowerer
	// Staleness handles eligible range-mode bare instant-vector selection
	// (staleness) shapes. Nil => fan-out only.
	Staleness StalenessLowerer
}

// lowerRate dispatches through the boot-wired rate strategy when one is wired,
// else returns nil (the caller keeps its fan-out RangeWindow). This is the
// per-query seam: NO feature flag is read here — the strategy presence already
// encodes the boot decision.
func (l RangeLowerers) lowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if l.Rate == nil {
		return nil
	}
	return l.Rate.LowerRate(rw, s)
}

// lowerStaleness dispatches through the boot-wired staleness strategy when one
// is wired, else returns nil (the caller keeps its fan-out RangeLWR). Per-query
// seam: NO feature flag is read here.
func (l RangeLowerers) lowerStaleness(in stalenessLowerInput) chplan.Node {
	if l.Staleness == nil {
		return nil
	}
	return l.Staleness.LowerStaleness(in)
}

// NativeRateLowerer is the boot-wired RateLowerer that emits the native
// timeSeriesRateToGrid lowering (a chplan.RangeWindowNative) for shape-eligible
// rate range-windows. cmd/cerberus wires it ONLY when chopt resolved the
// ts_grid_range feature at boot. It embeds no fallback: a shape it cannot
// handle returns nil and the caller keeps the fan-out — the embedded-fallback
// composition is the RangeLowerers table itself (a nil strategy is the
// fan-out).
type NativeRateLowerer struct{}

// LowerRate returns a RangeWindowNative for an eligible range-mode rate shape,
// or nil otherwise. The eligibility predicate is the intrinsic SHAPE check
// (rate func, materialised grid, plain Scan/Filter input) — see
// nativeTSGridRateNode.
func (NativeRateLowerer) LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if n := nativeTSGridRateNode(rw, s); n != nil {
		return n
	}
	return nil
}

// NativeStalenessLowerer is the boot-wired StalenessLowerer that emits the
// native timeSeriesResampleToGridWithStaleness lowering (a
// chplan.RangeWindowResample). cmd/cerberus wires it ONLY when chopt resolved
// the ts_grid_resample feature at boot. A shape it cannot handle returns nil
// and the caller keeps the fan-out RangeLWR.
type NativeStalenessLowerer struct{}

// LowerStaleness returns a RangeWindowResample for the (already shape-validated
// by the caller) range-mode staleness input. The caller only reaches the
// staleness seam in range mode over a non-@-pinned bare selector, so the
// strategy builds the node directly; the nil-return path is reserved for future
// shape carve-outs without changing the seam contract.
func (NativeStalenessLowerer) LowerStaleness(in stalenessLowerInput) chplan.Node {
	return &chplan.RangeWindowResample{
		Input:         in.input,
		Start:         in.start,
		End:           in.end,
		Step:          in.step,
		Lookback:      in.lookback,
		Offset:        in.offset,
		MetricNameCol: in.metricNameCol,
		AttributesCol: in.attributesCol,
		TimestampCol:  in.timestampCol,
		ValueCol:      in.valueCol,
	}
}
