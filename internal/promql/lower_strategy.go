package promql

import (
	"math"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file defines the BOOT-WIRED POLYMORPHIC lowering seam for the
// ClickHouse-native timeSeries*ToGrid family. The decision of WHICH lowering
// to use (native aggregate vs. the generic SQL fan-out) is made ONCE at boot —
// from the already-resolved chopt.EnabledSet — and injected as a RangeLowerers
// table. The per-query lowering path then calls through that table with NO
// feature-flag or server-version conditional, AND NO nil/presence check on the
// strategy: every field is ALWAYS a concrete non-nil impl, so the dispatch site
// is a plain interface method call. The only per-query decisions are AST
// node-type dispatch and query-SHAPE eligibility, which live INSIDE the chosen
// strategy: a native impl that cannot handle a shape DELEGATES to its embedded
// fan-out fallback (never returns nil), so the interface method ALWAYS returns
// a valid lowering.
//
// Why a per-FUNCTION table rather than one global bool: the features are
// independent (native rate may be on while native resample is off, and vice
// versa), so the wiring composes per-function. cmd/cerberus builds the concrete
// strategies once from EnabledSet.Has(chopt.FeatureTSGridRange) /
// .Has(chopt.FeatureTSGridResample) and threads the table down through the prom
// handler -> lang adapter -> LowerOpts. The promql package cannot import chopt
// (the dependency-cone rule), so the strategy TYPES live here and the
// feature/version READ lives at boot — exactly where the rule requires it.
//
// Wiring shape (at boot, the ONLY place the feature read happens):
//
//	rate = enabled ? NativeRateLowerer{Fallback: FanoutRateLowerer{}} : FanoutRateLowerer{}
//
// The fan-out impl is the concrete DEFAULT; it is never nil. The zero value of
// RangeLowerers carries nil fields, which is the "no caller opted in" sentinel
// resolved to the all-fan-out table by [RangeLowerers.withDefaults] at the
// single lowering-entry seam — never at the per-query dispatch site.

// RateLowerer lowers a range-mode rate RangeWindow to a chplan node. It ALWAYS
// returns a valid lowering: the native impl emits the native node for a
// shape-eligible window and delegates to its embedded fan-out fallback for any
// other shape; the fan-out impl returns the generic RangeWindow directly. The
// shape eligibility (rate-over-counter with a materialised grid and a plain
// Scan/Filter input) is intrinsic and lives inside the implementation; it is
// NOT a feature-flag branch.
type RateLowerer interface {
	// LowerRate returns the chplan node for rw — the native RangeWindowNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// StalenessLowerer lowers a range-mode bare instant-vector selection (the
// staleness shape) to a chplan node. It ALWAYS returns a valid lowering: the
// native impl emits the native resample node and the fan-out impl emits the
// generic RangeLWR. The build closure carries the already-resolved
// scan/pred/anchor/grid for the selector; the strategy reads only intrinsic
// shape from it (never a feature flag).
type StalenessLowerer interface {
	// LowerStaleness returns the chplan node for in — the native
	// RangeWindowResample for a shape the impl handles, or the fan-out RangeLWR
	// otherwise. It never returns nil.
	LowerStaleness(in stalenessLowerInput) chplan.Node
}

// stalenessLowerInput carries the resolved inputs the range-mode staleness wrap
// has already computed (the matchers-filtered scan, the eval grid, the offset,
// and the schema column names), so a strategy can build the resample / RangeLWR
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

// ChangesLowerer lowers a range-mode changes(<v>[range]) RangeWindow to a
// chplan node. It ALWAYS returns a valid lowering: the native impl emits the
// native RangeWindowNative (Func="changes" -> timeSeriesChangesToGrid) for a
// shape-eligible window and delegates to its embedded fan-out fallback for any
// other shape; the fan-out impl returns the generic RangeWindow directly. The
// shape eligibility is intrinsic and lives inside the implementation; it is NOT
// a feature-flag branch.
type ChangesLowerer interface {
	// LowerChanges returns the chplan node for rw — the native
	// RangeWindowNative for a shape the impl handles, or the fan-out lowering
	// otherwise. It never returns nil.
	LowerChanges(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// ResetsLowerer lowers a range-mode resets(<counter>[range]) RangeWindow to a
// chplan node, mirroring [ChangesLowerer]: native impl emits RangeWindowNative
// (Func="resets" -> timeSeriesResetsToGrid) for an eligible window, fan-out
// fallback otherwise. It never returns nil.
type ResetsLowerer interface {
	// LowerResets returns the chplan node for rw — the native RangeWindowNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerResets(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// DerivLowerer lowers a range-mode deriv(<gauge>[range]) RangeWindow to a
// chplan node, mirroring [ChangesLowerer]: the native impl emits a
// RangeWindowNative (Func="deriv" -> timeSeriesDerivToGrid, the per-window
// simple-linear-regression slope) for an eligible window, the fan-out fallback
// otherwise. It never returns nil.
type DerivLowerer interface {
	// LowerDeriv returns the chplan node for rw — the native RangeWindowNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerDeriv(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// PredictLinearLowerer lowers a range-mode predict_linear(<gauge>[range], t)
// RangeWindow to a chplan node, mirroring [ChangesLowerer]: the native impl
// emits a RangeWindowNative (Func="predict_linear" ->
// timeSeriesPredictLinearToGrid, the per-window slope*t + intercept forecast)
// for an eligible window, the fan-out fallback otherwise. Only a single
// whole-second literal horizon t is native-eligible — the aggregate's 5th
// parametric arg is a constant, so computed / fractional horizons delegate to
// the fan-out. It never returns nil.
type PredictLinearLowerer interface {
	// LowerPredictLinear returns the chplan node for rw — the native
	// RangeWindowNative for a shape the impl handles, or the fan-out lowering
	// otherwise. It never returns nil.
	LowerPredictLinear(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// RangeLowerers is the boot-wired dispatch table the lowering reads. Each field
// is the CONCRETE strategy for one promql function family, decided once at
// boot. Every field is ALWAYS non-nil on the lowering path — a fan-out-only
// deployment wires the concrete fan-out impl, NOT nil. The zero value (nil
// fields) is the "no caller opted in" sentinel, resolved to the all-fan-out
// table by [withDefaults] at the single lowering-entry seam; the per-query
// dispatch then calls the interface method directly, with no nil/presence
// check.
type RangeLowerers struct {
	// Rate handles range-mode rate(...) shapes. Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	Rate RateLowerer
	// Staleness handles range-mode bare instant-vector selection (staleness)
	// shapes. Concrete fan-out impl when the native path is off; never nil on
	// the lowering path.
	Staleness StalenessLowerer
	// Changes handles range-mode changes(...) shapes (native
	// timeSeriesChangesToGrid, server >= 25.9). Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	Changes ChangesLowerer
	// Resets handles range-mode resets(...) shapes (native
	// timeSeriesResetsToGrid, server >= 25.9). Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	Resets ResetsLowerer
	// Deriv handles range-mode deriv(...) shapes (native timeSeriesDerivToGrid,
	// server >= 25.9). Concrete fan-out impl when the native path is off; never
	// nil on the lowering path.
	Deriv DerivLowerer
	// PredictLinear handles range-mode predict_linear(..., t) shapes (native
	// timeSeriesPredictLinearToGrid, server >= 25.9). Concrete fan-out impl when
	// the native path is off; never nil on the lowering path.
	PredictLinear PredictLinearLowerer
}

// withDefaults returns a copy of l with any nil strategy field filled with its
// concrete fan-out impl. This is the SINGLE normalization seam (called once at
// the lowering entry, never per query) that turns the zero-value
// "no caller opted in" sentinel into the all-fan-out table. After this, every
// field is a concrete non-nil impl and the per-query dispatch is a plain
// interface method call with no nil check.
func (l RangeLowerers) withDefaults() RangeLowerers {
	if l.Rate == nil {
		l.Rate = FanoutRateLowerer{}
	}
	if l.Staleness == nil {
		l.Staleness = FanoutStalenessLowerer{}
	}
	if l.Changes == nil {
		l.Changes = FanoutChangesLowerer{}
	}
	if l.Resets == nil {
		l.Resets = FanoutResetsLowerer{}
	}
	if l.Deriv == nil {
		l.Deriv = FanoutDerivLowerer{}
	}
	if l.PredictLinear == nil {
		l.PredictLinear = FanoutPredictLinearLowerer{}
	}
	return l
}

// FanoutRateLowerer is the concrete DEFAULT RateLowerer: it returns the generic
// fan-out RangeWindow unchanged. It is the fallback the native impl embeds AND
// the strategy a fan-out-only deployment wires directly, so the dispatch site
// never needs a nil check.
type FanoutRateLowerer struct{}

// LowerRate returns the fan-out RangeWindow rw unchanged.
func (FanoutRateLowerer) LowerRate(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeRateLowerer is the boot-wired RateLowerer that emits the native
// timeSeriesRateToGrid lowering (a chplan.RangeWindowNative) for shape-eligible
// rate range-windows. cmd/cerberus wires it ONLY when chopt resolved the
// ts_grid_range feature at boot. It embeds a concrete Fallback (the fan-out
// impl): a shape it cannot handle delegates to Fallback rather than returning
// nil, so the interface method ALWAYS yields a valid lowering and the dispatch
// site stays branch-free.
type NativeRateLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutRateLowerer{}.
	Fallback RateLowerer
}

// LowerRate returns a RangeWindowNative for an eligible range-mode rate shape,
// or delegates to the embedded Fallback otherwise. The eligibility predicate is
// the intrinsic SHAPE check (rate func, materialised grid, plain Scan/Filter
// input) — see nativeTSGridRateNode.
func (n NativeRateLowerer) LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridRateNode(rw, s); native != nil {
		return native
	}
	return n.Fallback.LowerRate(rw, s)
}

// FanoutStalenessLowerer is the concrete DEFAULT StalenessLowerer: it builds the
// generic fan-out RangeLWR from the resolved staleness input. It is the
// fallback the native impl embeds AND the strategy a fan-out-only deployment
// wires directly.
type FanoutStalenessLowerer struct{}

// LowerStaleness builds the fan-out RangeLWR node from in.
func (FanoutStalenessLowerer) LowerStaleness(in stalenessLowerInput) chplan.Node {
	return &chplan.RangeLWR{
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

// NativeStalenessLowerer is the boot-wired StalenessLowerer that emits the
// native timeSeriesResampleToGridWithStaleness lowering (a
// chplan.RangeWindowResample). cmd/cerberus wires it ONLY when chopt resolved
// the ts_grid_resample feature at boot. It embeds a concrete Fallback (the
// fan-out impl) for future shape carve-outs, so the interface method always
// yields a valid lowering and the dispatch site stays branch-free.
type NativeStalenessLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutStalenessLowerer{}.
	Fallback StalenessLowerer
}

// LowerStaleness returns a RangeWindowResample for the (already shape-validated
// by the caller) range-mode staleness input. The caller only reaches the
// staleness seam in range mode over a non-@-pinned bare selector, so the native
// shape always applies; the embedded Fallback is reserved for future shape
// carve-outs without changing the seam contract.
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

// FanoutChangesLowerer is the concrete DEFAULT ChangesLowerer: it returns the
// generic fan-out RangeWindow (the arrayPopBack/arrayPopFront `c != p` count)
// unchanged. It is the fallback the native impl embeds AND the strategy a
// fan-out-only deployment wires directly.
type FanoutChangesLowerer struct{}

// LowerChanges returns the fan-out RangeWindow rw unchanged.
func (FanoutChangesLowerer) LowerChanges(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeChangesLowerer is the boot-wired ChangesLowerer that emits the native
// timeSeriesChangesToGrid lowering (a chplan.RangeWindowNative with
// Func="changes") for shape-eligible changes range-windows. cmd/cerberus wires
// it ONLY when chopt resolved the ts_grid_changes feature (server >= 25.9) at
// boot. It embeds a concrete Fallback (the fan-out impl): a shape it cannot
// handle delegates to Fallback rather than returning nil, so the interface
// method ALWAYS yields a valid lowering and the dispatch site stays branch-free.
type NativeChangesLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutChangesLowerer{}.
	Fallback ChangesLowerer
}

// LowerChanges returns a RangeWindowNative for an eligible range-mode changes
// shape, or delegates to the embedded Fallback otherwise. The eligibility
// predicate is the intrinsic SHAPE check (changes func, materialised grid,
// plain Scan/Filter input) — see nativeTSGridMatrixNode.
func (n NativeChangesLowerer) LowerChanges(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "changes", s); native != nil {
		return native
	}
	return n.Fallback.LowerChanges(rw, s)
}

// FanoutResetsLowerer is the concrete DEFAULT ResetsLowerer: it returns the
// generic fan-out RangeWindow (the arrayPopBack/arrayPopFront `c < p` count)
// unchanged. It is the fallback the native impl embeds AND the strategy a
// fan-out-only deployment wires directly.
type FanoutResetsLowerer struct{}

// LowerResets returns the fan-out RangeWindow rw unchanged.
func (FanoutResetsLowerer) LowerResets(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeResetsLowerer is the boot-wired ResetsLowerer that emits the native
// timeSeriesResetsToGrid lowering (a chplan.RangeWindowNative with
// Func="resets") for shape-eligible resets range-windows. cmd/cerberus wires it
// ONLY when chopt resolved the ts_grid_resets feature (server >= 25.9) at boot.
// It embeds a concrete Fallback for shapes it cannot handle, so the interface
// method always yields a valid lowering and the dispatch site stays branch-free.
type NativeResetsLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutResetsLowerer{}.
	Fallback ResetsLowerer
}

// LowerResets returns a RangeWindowNative for an eligible range-mode resets
// shape, or delegates to the embedded Fallback otherwise. Same intrinsic SHAPE
// check as changes (resets func, materialised grid, plain Scan/Filter input).
func (n NativeResetsLowerer) LowerResets(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "resets", s); native != nil {
		return native
	}
	return n.Fallback.LowerResets(rw, s)
}

// FanoutDerivLowerer is the concrete DEFAULT DerivLowerer: it returns the
// generic fan-out RangeWindow (the simpleLinearRegression slope) unchanged. It
// is the fallback the native impl embeds AND the strategy a fan-out-only
// deployment wires directly.
type FanoutDerivLowerer struct{}

// LowerDeriv returns the fan-out RangeWindow rw unchanged.
func (FanoutDerivLowerer) LowerDeriv(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeDerivLowerer is the boot-wired DerivLowerer that emits the native
// timeSeriesDerivToGrid lowering (a chplan.RangeWindowNative with Func="deriv")
// for shape-eligible deriv range-windows. cmd/cerberus wires it ONLY when the
// chopt resolved the ts_grid_deriv feature (server >= 25.9) at boot. It embeds
// a concrete Fallback for shapes it cannot handle, so the interface method
// always yields a valid lowering and the dispatch site stays branch-free.
type NativeDerivLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutDerivLowerer{}.
	Fallback DerivLowerer
}

// LowerDeriv returns a RangeWindowNative for an eligible range-mode deriv
// shape, or delegates to the embedded Fallback otherwise. Same intrinsic SHAPE
// check as changes/resets (deriv func, materialised grid, plain Scan/Filter
// input) — deriv takes no scalar, so no extra parameter gate applies.
func (n NativeDerivLowerer) LowerDeriv(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "deriv", s); native != nil {
		return native
	}
	return n.Fallback.LowerDeriv(rw, s)
}

// FanoutPredictLinearLowerer is the concrete DEFAULT PredictLinearLowerer: it
// returns the generic fan-out RangeWindow (the simpleLinearRegression
// intercept + slope*t forecast) unchanged. It is the fallback the native impl
// embeds AND the strategy a fan-out-only deployment wires directly.
type FanoutPredictLinearLowerer struct{}

// LowerPredictLinear returns the fan-out RangeWindow rw unchanged.
func (FanoutPredictLinearLowerer) LowerPredictLinear(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativePredictLinearLowerer is the boot-wired PredictLinearLowerer that emits
// the native timeSeriesPredictLinearToGrid lowering (a chplan.RangeWindowNative
// with Func="predict_linear") for shape-eligible predict_linear range-windows.
// cmd/cerberus wires it ONLY when the chopt resolved the ts_grid_predict_linear
// feature (server >= 25.9) at boot. It embeds a concrete Fallback for shapes it
// cannot handle, so the interface method always yields a valid lowering and the
// dispatch site stays branch-free.
type NativePredictLinearLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutPredictLinearLowerer{}.
	Fallback PredictLinearLowerer
}

// LowerPredictLinear returns a RangeWindowNative for an eligible range-mode
// predict_linear shape, or delegates to the embedded Fallback otherwise. On top
// of the shared shape check (predict_linear func, materialised grid, plain
// Scan/Filter input) the horizon t must be a single whole-second literal:
// timeSeriesPredictLinearToGrid takes the offset as its 5th parametric arg (a
// constant), so a computed horizon (ScalarExprs) or a fractional t cannot ride
// the native aggregate and stays on the exact fan-out arithmetic.
func (n NativePredictLinearLowerer) LowerPredictLinear(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if nativePredictLinearHorizonEligible(rw) {
		if native := nativeTSGridMatrixNode(rw, "predict_linear", s); native != nil {
			return native
		}
	}
	return n.Fallback.LowerPredictLinear(rw, s)
}

// nativePredictLinearHorizonEligible reports whether rw's predict_linear
// horizon t can be threaded into timeSeriesPredictLinearToGrid's 5th parametric
// arg: exactly one literal scalar (no computed ScalarExprs) whose value is a
// whole number of seconds. A fractional or computed horizon is byte-exact only
// on the fan-out's `intercept + slope*t` Float64 arithmetic, so it delegates.
func nativePredictLinearHorizonEligible(rw *chplan.RangeWindow) bool {
	if len(rw.ScalarExprs) != 0 || len(rw.Scalars) != 1 {
		return false
	}
	t := rw.Scalars[0]
	return t == math.Trunc(t)
}
