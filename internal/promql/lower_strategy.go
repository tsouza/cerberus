package promql

import (
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
// Why a per-FUNCTION table rather than one global bool: the timeSeries*ToGrid
// features are independent (the family will grow — native rate today, native
// staleness / increase / over-time next), so the wiring composes per-function.
// cmd/cerberus builds the concrete strategies once from EnabledSet.Has(...) and
// threads the table down through the prom handler -> lang adapter -> LowerOpts.
// The promql package cannot import chopt (the dependency-cone rule), so the
// strategy TYPES live here and the feature/version READ lives at boot — exactly
// where the rule requires it.

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

// RangeLowerers is the boot-wired dispatch table the lowering reads. Each field
// is the concrete strategy for one promql function family, decided once at
// boot. A nil field means "fan-out only" for that family — the zero value of
// RangeLowerers is therefore the all-fan-out default, so any caller that does
// not opt in (every path but the query_range handler) lowers byte-identically
// to the pre-seam behaviour.
type RangeLowerers struct {
	// Rate handles eligible range-mode rate(...) shapes. Nil => fan-out only.
	Rate RateLowerer
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
