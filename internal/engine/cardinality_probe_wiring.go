package engine

import (
	"context"
	"sync"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// cardinality_probe_wiring.go closes cerberus issue #2788: it complements
// explain_estimate_wiring.go's EXPLAIN ESTIMATE advisor (issue #2787, PR
// #2836) with a SECOND, independent advisory input the marks-level estimate
// cannot answer — real distinct-series fan-out — by running one bounded,
// REAL aggregate (`count()`, `uniqUpTo(100)(...)`, and — #2840 —
// `uniqCombined64(...)`) over the plan's already-pruned scan window.
//
// Both advisors gate on the EXACT SAME boundary explain_estimate_wiring.go's
// own doc names (ModeAuto candidates whose baseline classification reached
// the cost-grid section), reuse the SAME per-shape cache key
// (routememo.Key, via this package's own shapeKey/reachedCostGrid/
// hasExistingVerdict helpers — see below for why those are reused rather
// than re-derived), and share the identical "no new live round-trip on
// every per-rung request" cost constraint per_rung_admission.go's own doc
// established first. They are deliberately kept as two SEPARATE advisors
// rather than folded into one: reusing ScanEstimateAdvisor's own cache would
// mean stretching its routememo.Key-only cache to also carry a metric name,
// which this file's own cache needs (cardinality varies by WHICH metric is
// being scanned, in a way a plan's literal-free structural shape alone
// cannot capture — see cardinalityProbeKey's own doc) and routememo.Key
// explicitly promises never to (routememo/key.go: "never a metric name,
// label name, label value"). Two advisors with two narrow, purpose-built
// keys is simpler and more honest than one advisor whose key type lies
// about what it is keyed on.
//
// SCOPE (deliberately narrow, matching this repository's culture of
// evidence-first landings — see #2787's own "what was scoped down"):
//
//  1. Carrier kind: findCardinalityProbeCarrier recognises the SIX
//     GridCarrier kinds whose real-matrix emitters share the identical
//     (Start - Offset - <span>, End - Offset] scan-bound formula:
//     *chplan.RangeWindow (chsql's innerScanTsBoundsFrags — the carrier
//     #2709's own incident and #2788's dashboard-panel example both
//     concern, and by far the most common ModeAuto shape) plus, as of
//     #2840, *chplan.RangeWindowGridNative, *chplan.RangeBucketFanout,
//     *chplan.RangeBucketGridNative and *chplan.RangeLWR, and — added in
//     the same ACPR pass that corrected this doc — *chplan.
//     RangeWindowStaleResample: all five via chsql's shared
//     maybePushRangeScanTimeBound helper (internal/chsql/range_lwr.go).
//     #2840 set out to find each carrier's "own real-matrix
//     time-bound-pushdown formula to mirror" and instead found chsql had
//     already collapsed all of them onto that ONE shared helper — there
//     was no per-carrier formula left to rediscover, which is why
//     cardinalityProbeCarrier (below) can express every one of the six
//     with a single (tsCol, start, end, offset, span) shape.
//     RangeWindowStaleResample was excluded from the original #2840 count
//     purely because it wasn't one of the four kinds that PR happened to
//     add — not because its formula differs — so once noticed the honest
//     fix is to add it, not to leave it out on that circular basis. Every
//     other [chplan.GridCarrier] kind (StepGrid, AbsentOverTime) still
//     fails open — neither shares the formula.
//  2. Metric identity: the probe only fires when the carrier's scan is
//     gated by exactly one literal `MetricName = '...'` equality anywhere
//     in its nearest Filter's predicate (cardinalityProbeMetricName). A
//     regex `__name__` matcher, a multi-metric selector, or no Filter at
//     all skips the probe — there is no single "metric" to key the cache on.
//  3. Payload: `count()`, `uniqUpTo(100)(...)` and — #2840 —
//     `uniqCombined64(...)`, the uncapped approximate sibling
//     cardinalityProbeEffectiveDistinctSeries's own doc explains. The
//     issue's own SQL sketch also brackets a FOURTH, OPTIONAL
//     `avg(length(ExplicitBounds))` term (a classic-histogram bucket-width
//     bias signal) — still omitted: unlike uniqCombined64, it has no
//     consumer today (neither the K-clamp's Rows input nor
//     maybeSeedPerRungPrior reads a bucket-width signal), and #2840's own
//     text names it the lowest-priority of its three items, gated on a real
//     consumer existing first.
//
// A probe failure — emission error, breaker-open, transport error, timeout —
// is treated as "no signal" and never surfaces to the caller, exactly like
// explain_estimate_wiring.go's own fail-open contract: this is advisory-only
// and must never become a reason a query fails that would otherwise have
// succeeded.

const (
	// cardinalityProbeUniqUpToCap is the uniqUpTo(N) parameter this probe
	// always uses. ClickHouse's own uniqUpTo caps N at 100 — issue #2788
	// verified that uniqUpTo(100*16) throws rather than saturating — so this
	// is not a tuning knob but the documented hard ceiling; DistinctSeries
	// reports 101 (never a false exact count) once a probed window's true
	// cardinality exceeds it (chplan.FnUniqUpTo's own doc).
	cardinalityProbeUniqUpToCap = 100

	// cardinalityProbeTimeout is the strict, probe-specific deadline this
	// file applies to every round trip — both server-side (chclient.
	// WithQueryTimeout, min'd against the operator's own configured
	// CERBERUS_QUERY_TIMEOUT so this only ever TIGHTENS it) and client-side
	// (a context.WithTimeout wrapping the call), so a slow or loaded cluster
	// cannot stall an admission decision waiting on this optional signal —
	// the issue's own risk boundary. Unlike EXPLAIN ESTIMATE (a no-execution
	// metadata operation), this probe DOES read data, so it is the one
	// cardinality-probe-specific round trip that needs its own bound rather
	// than inheriting whatever the real request's deadline happens to be.
	// Generous relative to the issue's own "milliseconds on indexed
	// predicates" expectation for a granule-pruned window, so a healthy but
	// momentarily busy cluster still gets an answer.
	cardinalityProbeTimeout = 2 * time.Second
)

// cardinalityProbeRowsAlias / cardinalityProbeDistinctSeriesAlias /
// cardinalityProbeDistinctSeriesApproxAlias name the probe's three output
// columns. chclient.Client.ProbeCardinality scans them POSITIONALLY (rows,
// then distinct_series, then distinct_series_approx) —
// buildCardinalityProbePlan's AggFuncs order is what that positional
// contract actually binds; these aliases only make the emitted SQL
// self-describing, they are not themselves load-bearing for the scan order.
const (
	cardinalityProbeRowsAlias                 = "rows"
	cardinalityProbeDistinctSeriesAlias       = "distinct_series"
	cardinalityProbeDistinctSeriesApproxAlias = "distinct_series_approx"
)

// cardinalityProbeMetricNameColumn / cardinalityProbeAttributeMapColumns
// resolve the OTel-CH metrics schema DEFAULTS once, mirroring
// internal/chsql/emit.go's own attributeMapColumns var and its own doc for
// why: this file's gating runs before any per-request schema is in hand
// (the emit chokepoint has none either), and a deployment that RENAMES a
// column is covered by its head's lowering being schema-aware — this is
// defence-in-depth advisory machinery, not a correctness path, so the
// bounded blast radius of the default-schema assumption is "probe skipped
// or mis-keyed on a renamed deployment", never a wrong answer.
var (
	cardinalityProbeMetricNameColumn    = schema.DefaultOTelMetrics().MetricNameColumn
	cardinalityProbeAttributeMapColumns = func() chplan.AttributeMapColumns {
		m := schema.DefaultOTelMetrics()
		return chplan.NewAttributeMapColumns(m.AttributesColumn, m.ResourceAttributesColumn, m.ScopeAttributesColumn)
	}()
)

// cardinalityProbeCacheCapacity / cardinalityProbeCacheTTL mirror
// scanEstimateCacheCapacity / scanEstimateCacheTTL (explain_estimate_wiring.go)
// exactly — the same bounded-resident-cache-with-coarse-eviction posture,
// reused by name rather than re-derived so the two advisors' caching
// contracts cannot silently drift apart.
const (
	cardinalityProbeCacheCapacity = scanEstimateCacheCapacity
	cardinalityProbeCacheTTL      = scanEstimateCacheTTL
)

// cardinalityProbeKey is this file's cache key: a plan SHAPE
// (routememo.Key, the same literal-free fingerprint the route memo, the
// per-rung admission learner, and ScanEstimateAdvisor all already key on)
// paired with the ONE literal this signal cannot do without — the metric
// name. See this file's own top-level doc for why that pairing cannot live
// inside routememo.Key itself.
type cardinalityProbeKey struct {
	shape  routememo.Key
	metric string
}

// cardinalityProbeCacheEntry is one cached probe result.
type cardinalityProbeCacheEntry struct {
	estimate chclient.CardinalityEstimate
	cachedAt time.Time
}

// CardinalityProbe is the narrow chclient seam CardinalityProbeAdvisor
// depends on — *chclient.Client in production, faked in tests, mirroring
// Estimator's own role for ScanEstimateAdvisor.
type CardinalityProbe interface {
	ProbeCardinality(ctx context.Context, sql string, args ...any) (chclient.CardinalityEstimate, error)
}

// CardinalityProbeAdvisor is the OPTIONAL, per-Engine-instance cache and
// gate implementing this file's own doc. A nil *CardinalityProbeAdvisor
// behaves exactly like a nil Engine.ScanEstimateAdvisor: every call site is
// reached only through Engine.classify's own nil guard, so an Engine that
// never wires one is byte-unchanged.
type CardinalityProbeAdvisor struct {
	client CardinalityProbe
	// perRungAdmission is OPTIONAL, exactly like ScanEstimateAdvisor's own
	// field of the same name: when set, a probe proving the window carries
	// too few distinct series to matter per anchor also seeds a prior into
	// it — see maybeSeedPerRungPrior.
	perRungAdmission *PerRungAdmissionLearner

	mu      sync.Mutex
	entries map[cardinalityProbeKey]cardinalityProbeCacheEntry
}

// NewCardinalityProbeAdvisor constructs an advisor bound to client.
// perRungAdmission may be nil.
func NewCardinalityProbeAdvisor(client CardinalityProbe, perRungAdmission *PerRungAdmissionLearner) *CardinalityProbeAdvisor {
	return &CardinalityProbeAdvisor{
		client:           client,
		perRungAdmission: perRungAdmission,
		entries:          make(map[cardinalityProbeKey]cardinalityProbeCacheEntry),
	}
}

// Advise returns an updated *solver.ScanEstimate folding this probe's
// result into current (ScanEstimateAdvisor's own return value, or nil when
// that advisor is unwired or itself skipped) — see mergeCardinalityEstimate
// for the merge rule. It returns current UNCHANGED whenever the round trip
// is skipped or fails, so a caller can always chain this after
// ScanEstimateAdvisor.Advise regardless of whether that one fired.
func (a *CardinalityProbeAdvisor) Advise(
	ctx context.Context,
	routeMemo *routememo.Memo,
	plan chplan.Node,
	baseline *solver.Decision,
	current *solver.ScanEstimate,
) *solver.ScanEstimate {
	if a == nil || baseline == nil {
		return current
	}
	// Narrowing (1) — reuses explain_estimate_wiring.go's own gate: only a
	// plan whose baseline classification reached the cost-grid section is
	// worth probing at all.
	if !reachedCostGrid(baseline.Reason) {
		return current
	}
	carrier, ok := findCardinalityProbeCarrier(plan)
	if !ok {
		return current
	}
	metric, ok := cardinalityProbeMetricName(carrier, cardinalityProbeMetricNameColumn)
	if !ok {
		return current
	}
	// Reuses explain_estimate_wiring.go's own shapeKey/hasExistingVerdict —
	// narrowing (3): the route memo or the per-rung admission learner
	// already knows what to do with this shape, so an advisory signal would
	// buy nothing.
	shape := shapeKey(plan, baseline)
	if hasExistingVerdict(routeMemo, a.perRungAdmission, shape) {
		return current
	}
	key := cardinalityProbeKey{shape: shape, metric: metric}
	// Narrowing (2): per-(shape, metric) cache, ahead of the round trip.
	if cached, ok := a.cached(key); ok {
		return mergeCardinalityEstimate(current, cached)
	}

	probePlan, ok := buildCardinalityProbePlan(carrier)
	if !ok {
		return current
	}
	sql, args, err := chsql.Emit(ctx, probePlan)
	if err != nil {
		// Fail open: the real dispatch path will hit and report the SAME
		// emission error momentarily if it is a genuine problem.
		return current
	}
	probeCtx := chclient.WithQueryTimeout(ctx, cardinalityProbeTimeout)
	probeCtx, cancel := context.WithTimeout(probeCtx, cardinalityProbeTimeout)
	defer cancel()
	est, err := a.client.ProbeCardinality(probeCtx, sql, args...)
	if err != nil {
		// Advisory-only, fail-open: breaker-open, transport error, or the
		// deadline above firing must never turn into a query failure for a
		// signal that was never a correctness gate.
		return current
	}

	a.store(key, est)
	a.maybeSeedPerRungPrior(shape, est, baseline)
	return mergeCardinalityEstimate(current, est)
}

// cached returns key's cached estimate when present and still fresh.
func (a *CardinalityProbeAdvisor) cached(key cardinalityProbeKey) (chclient.CardinalityEstimate, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[key]
	if !ok || time.Since(entry.cachedAt) > cardinalityProbeCacheTTL {
		return chclient.CardinalityEstimate{}, false
	}
	return entry.estimate, true
}

// store records key's estimate, evicting one arbitrary entry at capacity —
// the same coarse-eviction posture ScanEstimateAdvisor.store and
// PerRungAdmissionLearner.record already document: the cost of evicting the
// wrong entry here is trivial (one shape probes again a little sooner).
func (a *CardinalityProbeAdvisor) store(key cardinalityProbeKey, est chclient.CardinalityEstimate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.entries[key]; !ok && len(a.entries) >= cardinalityProbeCacheCapacity {
		for k := range a.entries {
			delete(a.entries, k)
			break
		}
	}
	a.entries[key] = cardinalityProbeCacheEntry{estimate: est, cachedAt: time.Now()}
}

// maybeSeedPerRungPrior seeds a.perRungAdmission with a prior derived from
// est, mirroring explain_estimate_wiring.go's own maybeSeedPerRungPrior —
// same one-directional-only contract (only ever cheap=true, never
// cheap=false; see that method's own doc for the full argument), same
// reused perRungCheapRowsPerAnchor threshold (per_rung_admission.go), and
// the SAME reason the failure-driven route memo is deliberately NOT seeded
// here either: routememo's minCorroboratingFailures + pressure damper exist
// specifically to reject single-shot, non-corroborated evidence, and this
// probe — real execution though it is — is still one non-drain observation,
// not the repeated real-traffic corroboration the memo's own design
// requires (docs/solver.md's "Why the failure-driven route memo is NOT
// seeded", written for #2787, applies unchanged to this probe).
//
// est.DistinctSeries is a MORE DIRECT proxy for per-rung admission's own
// question than EXPLAIN ESTIMATE's raw scan-row upper bound: per-rung
// carriers fan a classic-histogram bucket ladder out per SERIES, so the
// composed output row count Observe() measures scales with distinct series,
// not raw scanned rows. Comparing it against the SAME
// perRungCheapRowsPerAnchor-per-anchor threshold Observe applies to real
// composed output is therefore "answer the rows/anchor question directly"
// (issue #2788's own phrase), not a new, independently-tuned threshold.
//
// It compares cardinalityProbeEffectiveDistinctSeries(est), not est.
// DistinctSeries directly — see that function's own doc for why: #2840
// found the raw uniqUpTo(100) reading saturates at 101 on EVERY dense real
// window in test/perf/smoke/testdata/samples/, which made a bare
// est.DistinctSeries comparison here a near-constant, falsely-cheap signal
// for exactly the traffic this seeding targets whenever the threshold below
// (routinely in the thousands) sat above the 101 saturation floor.
func (a *CardinalityProbeAdvisor) maybeSeedPerRungPrior(key routememo.Key, est chclient.CardinalityEstimate, baseline *solver.Decision) {
	if a.perRungAdmission == nil || baseline.NAnchors <= 0 {
		return
	}
	// Real probe distinct-series / anchor counts never approach int64 overflow.
	if int64(cardinalityProbeEffectiveDistinctSeries(est)) >= int64(baseline.NAnchors)*perRungCheapRowsPerAnchor { //nolint:gosec // G115
		return
	}
	a.perRungAdmission.SeedPriorFromEstimate(key, true)
}

// cardinalityProbeEffectiveDistinctSeries resolves the distinct-series
// reading maybeSeedPerRungPrior actually compares against its threshold.
//
// uniqUpTo(100) is EXACT below its cap and reports a fixed 101 once the
// group holds more than 100 distinct values (chplan.FnUniqUpTo's own doc) —
// #2840 verified against test/perf/smoke/testdata/samples/ that every dense
// real window in that corpus crosses the cap, so est.DistinctSeries alone is
// "101" on essentially all the traffic maybeSeedPerRungPrior's threshold
// (routinely NAnchors * perRungCheapRowsPerAnchor, in the thousands) exists
// to distinguish — a constant reading below a variable threshold always
// reads as cheap, regardless of how much larger the TRUE count is. Once
// est.DistinctSeries reports the saturation value, this instead trusts
// est.DistinctSeriesApprox — uniqCombined64(...)'s uncapped, approximate
// sibling — which keeps answering past 100 at the cost of being an estimate
// rather than an exact count.
//
// Below the cap, est.DistinctSeries IS the exact count and is preferred
// over the approximate sibling — there is nothing to gain by trading an
// exact small count for a sketch estimate of the same value.
func cardinalityProbeEffectiveDistinctSeries(est chclient.CardinalityEstimate) uint64 {
	if est.DistinctSeries > cardinalityProbeUniqUpToCap {
		return est.DistinctSeriesApprox
	}
	return est.DistinctSeries
}

// mergeCardinalityEstimate folds a cardinality-probe result into base
// (ScanEstimateAdvisor's own return value, or nil). Rows is OVERWRITTEN
// with the probe's real count() — strictly more precise than EXPLAIN
// ESTIMATE's granule-resolution upper bound for the SAME K-clamp arithmetic
// planner.go already applies (solver.ScanEstimate.Rows' own doc) — while
// Parts/Marks (EXPLAIN-ESTIMATE-only, unread by classify()) pass through
// untouched. DistinctSeries is always set from the probe: base never
// carries one (ScanEstimateAdvisor has no comparable per-series signal).
func mergeCardinalityEstimate(base *solver.ScanEstimate, est chclient.CardinalityEstimate) *solver.ScanEstimate {
	merged := solver.ScanEstimate{}
	if base != nil {
		merged = *base
	}
	merged.Rows = est.Rows
	merged.DistinctSeries = est.DistinctSeries
	return &merged
}

// cardinalityProbeCarrier is the carrier-agnostic view findCardinalityProbeCarrier
// extracts from whichever of the six recognised [chplan.GridCarrier] kinds
// (this file's own top-level doc, point 1) it locates. Every one of those
// six kinds carries its OWN field names for the same underlying shape — a
// scanned Input, a series-identity key, a timestamp column, and a scan
// window expressed as (Start, End, Offset, <backward reach>) — so this
// struct is the one normalised shape buildCardinalityProbePlan and
// cardinalityProbeMetricName actually operate on, extracted once at the
// findCardinalityProbeCarrier type switch rather than re-derived at every
// call site.
type cardinalityProbeCarrier struct {
	// Input is the matchers-filtered scan (Scan, or Filter-over-Scan) the
	// carrier reduces. Never nil for a value this struct's own construction
	// returns ok=true for.
	Input chplan.Node
	// SeriesKey is the series-identity key(s) the probe's uniqUpTo /
	// uniqCombined64 aggregates group by — the carrier's own GroupBy for the
	// four kinds that carry one (RangeWindow, RangeWindowGridNative,
	// RangeBucketFanout, RangeBucketGridNative), or a single bare reference
	// to AttributesCol for RangeLWR, which carries no explicit GroupBy
	// field because a bare selector's series identity IS its full
	// Attributes column (see chplan.RangeLWR's own doc). Never empty for a
	// value this struct's own construction returns ok=true for — an empty
	// GroupBy on a GroupBy-bearing carrier means "no series-identity keys",
	// the same "nothing to count distinct values of" case that keeps the
	// probe out of scope.
	SeriesKey []chplan.Expr
	// TimestampCol names the per-sample timestamp column on Input.
	TimestampCol string
	// Start / End / Offset mirror the carrier's own eval-grid fields
	// verbatim.
	Start, End time.Time
	Offset     time.Duration
	// Span is the carrier's own backward reach from each anchor — Range for
	// RangeWindow / RangeWindowGridNative / RangeBucketGridNative, Lookback
	// for RangeBucketFanout / RangeLWR — the SAME spanNS
	// maybePushRangeScanTimeBound's own doc names (internal/chsql/
	// range_lwr.go): it widens the scan bound's lower edge so a sample that
	// belongs to the earliest in-grid anchor's window survives the prune.
	Span time.Duration
}

// findCardinalityProbeCarrier locates plan's OUTERMOST [chplan.GridCarrier]
// — the exact same stop condition solver.GridOf itself uses (first carrier
// whose EvalGrid reports a positive step) — and reports it only when that
// carrier is one of the six kinds this file's own top-level doc names
// (point 1), carrying at least one series-identity key and a non-nil Input.
// Every other carrier kind (StepGrid, AbsentOverTime) — and a recognised
// carrier with no series-identity keys or Input at all — reports ok=false:
// this file's own top-level doc names this the deliberate carrier-kind
// scope narrowing.
func findCardinalityProbeCarrier(plan chplan.Node) (*cardinalityProbeCarrier, bool) {
	var found chplan.Node
	chplan.Walk(plan, func(n chplan.Node) bool {
		if found != nil {
			return false
		}
		if gc, ok := n.(chplan.GridCarrier); ok {
			if _, _, step := gc.EvalGrid(); step > 0 {
				found = n
				return false
			}
		}
		return true
	})
	switch c := found.(type) {
	case *chplan.RangeWindow:
		return cardinalityProbeCarrierFromGroupBy(c.Input, c.GroupBy, c.TimestampColumn, c.Start, c.End, c.Offset, c.Range)
	case *chplan.RangeWindowGridNative:
		return cardinalityProbeCarrierFromGroupBy(c.Input, c.GroupBy, c.TimestampColumn, c.Start, c.End, c.Offset, c.Range)
	case *chplan.RangeBucketFanout:
		return cardinalityProbeCarrierFromGroupBy(c.Input, c.GroupBy, c.TimestampCol, c.Start, c.End, c.Offset, c.Lookback)
	case *chplan.RangeBucketGridNative:
		return cardinalityProbeCarrierFromGroupBy(c.Input, c.GroupBy, c.TimestampCol, c.Start, c.End, c.Offset, c.Range)
	case *chplan.RangeLWR:
		if c.Input == nil || c.AttributesCol == "" {
			return nil, false
		}
		seriesKey := []chplan.Expr{&chplan.ColumnRef{Name: c.AttributesCol}}
		return cardinalityProbeCarrierFromGroupBy(c.Input, seriesKey, c.TimestampCol, c.Start, c.End, c.Offset, c.Lookback)
	case *chplan.RangeWindowStaleResample:
		// Shares RangeLWR's exact field shape (Input, a series-key column,
		// TimestampCol, Start, End, Offset, Lookback) and its emitter
		// (chsql/range_window_stale_resample.go) calls the SAME
		// maybePushRangeScanTimeBound helper with the identical (tsCol,
		// start, end, offset, span) argument shape — see this file's own
		// top-level doc, point 1: the stated selection criterion is
		// sharing that one formula, which this sixth carrier honestly
		// meets too, so it is no longer excluded.
		if c.Input == nil || c.AttributesCol == "" {
			return nil, false
		}
		seriesKey := []chplan.Expr{&chplan.ColumnRef{Name: c.AttributesCol}}
		return cardinalityProbeCarrierFromGroupBy(c.Input, seriesKey, c.TimestampCol, c.Start, c.End, c.Offset, c.Lookback)
	default:
		return nil, false
	}
}

// cardinalityProbeCarrierFromGroupBy builds a *cardinalityProbeCarrier from
// one of the six recognised carriers' own fields, reporting ok=false when
// input is nil or groupBy carries no series-identity keys — the shared
// gate every findCardinalityProbeCarrier case above applies, factored out
// so the six-way type switch cannot drift on which fields it checks.
func cardinalityProbeCarrierFromGroupBy(
	input chplan.Node,
	groupBy []chplan.Expr,
	tsCol string,
	start, end time.Time,
	offset, span time.Duration,
) (*cardinalityProbeCarrier, bool) {
	if input == nil || len(groupBy) == 0 {
		return nil, false
	}
	return &cardinalityProbeCarrier{
		Input:        input,
		SeriesKey:    groupBy,
		TimestampCol: tsCol,
		Start:        start,
		End:          end,
		Offset:       offset,
		Span:         span,
	}, true
}

// cardinalityProbeMetricName reports the single literal metric name gating
// carrier's scan — the equality operand of a `<metricNameColumn> = '<name>'`
// (or reversed) Binary leaf anywhere in the nearest enclosing Filter's
// predicate below carrier.Input. Reports ok=false when carrier.Input carries
// no Filter, the Filter's predicate carries no such equality, or it carries
// more than one DIFFERENT literal for metricNameColumn (an ambiguous /
// regex-backed multi-metric selector) — this file's own top-level doc names
// this the deliberate metric-identity scope narrowing: the probe only ever
// keys its cache on an unambiguous single metric.
func cardinalityProbeMetricName(carrier *cardinalityProbeCarrier, metricNameColumn string) (string, bool) {
	var pred chplan.Expr
	chplan.Walk(carrier.Input, func(n chplan.Node) bool {
		if pred != nil {
			return false
		}
		if f, ok := n.(*chplan.Filter); ok {
			pred = f.Predicate
			return false
		}
		return true
	})
	if pred == nil {
		return "", false
	}
	var name string
	var found, ambiguous bool
	chplan.InspectExpr(pred, func(e chplan.Expr) bool {
		if ambiguous {
			return false
		}
		b, ok := e.(*chplan.Binary)
		if !ok || b.Op != chplan.OpEq {
			return true
		}
		lit, matched := cardinalityProbeMetricEqLiteral(b, metricNameColumn)
		if !matched {
			return true
		}
		if found && lit != name {
			ambiguous = true
			return false
		}
		name, found = lit, true
		return true
	})
	if ambiguous || !found {
		return "", false
	}
	return name, true
}

// cardinalityProbeMetricEqLiteral checks both operand orders of an Eq
// Binary for a `<metricNameColumn> = <string literal>` shape.
func cardinalityProbeMetricEqLiteral(b *chplan.Binary, metricNameColumn string) (string, bool) {
	if lit, ok := cardinalityProbeMetricLiteral(b.Left, b.Right, metricNameColumn); ok {
		return lit, true
	}
	return cardinalityProbeMetricLiteral(b.Right, b.Left, metricNameColumn)
}

// cardinalityProbeMetricLiteral reports whether colSide is a bare reference
// to metricNameColumn and litSide is a string literal, returning that
// literal's value.
func cardinalityProbeMetricLiteral(colSide, litSide chplan.Expr, metricNameColumn string) (string, bool) {
	col, ok := colSide.(*chplan.ColumnRef)
	if !ok || col.Name != metricNameColumn {
		return "", false
	}
	lit, ok := litSide.(*chplan.LitString)
	if !ok {
		return "", false
	}
	return lit.V, true
}

// buildCardinalityProbePlan builds the bounded aggregate this file's own doc
// describes — `count()`, `uniqUpTo(100)(...)` and `uniqCombined64(...)` over
// carrier's already-pruned scan window — as a chplan tree, rendered through
// the ordinary chsql.Emit pipeline (invariant 10: no raw SQL). Reports
// ok=false when carrier carries no usable Start/End/TimestampCol (defensive:
// every caller already gates on a routed baseline, so this should not fire
// in practice — mirrors chsql's own maybePushRangeScanTimeBound "gated on
// BOTH Start and End being set" posture).
//
// Shape:
//
//	SELECT rows, distinct_series, distinct_series_approx
//	FROM (
//	    SELECT toFloat64(count()) AS rows,
//	           uniqUpTo(?)(<series key>) AS distinct_series,
//	           uniqCombined64(<series key>) AS distinct_series_approx,
//	           count() AS _cerb_n
//	    FROM (
//	        SELECT * FROM (<carrier.Input>)
//	        WHERE <TimestampCol> > (Start - Offset - Span)
//	          AND <TimestampCol> <= (End - Offset)
//	    )
//	) WHERE _cerb_n > 0
//
// `count()` renders wrapped in toFloat64(...) — chsql's ordinary
// intReturningAggregates guard (internal/chsql/emit_node.go), the SAME
// UInt64→Float64 coercion every other count()-family column this emitter
// produces already carries — and `uniqUpTo`'s cap renders as a bound `?`
// parameter (chplan.AggFunc.Params), the same parametric-aggregate shape
// production's own quantile(phi)(...) lowering already uses
// (internal/promql/lower.go). uniqCombined64 takes no parameter. chclient.
// Client.ProbeCardinality's own doc explains the resulting scan shape.
//
// The (Start - Offset - Span, End - Offset] bound is the EXACT window
// chsql's own maybePushInnerScanTimeBounds / maybePushRangeScanTimeBound
// pushes down for this SAME carrier's real emission (internal/chsql/
// range_window.go, range_lwr.go) — so the probe scans PRECISELY the rows
// the real dispatch would scan, no more, no less. The outer `_cerb_n > 0`
// guard (chplan.Aggregate's own DropEmptyOnNoGroup path,
// emitAggregateNoGroup) is what makes an empty window return ZERO rows
// rather than ClickHouse's default one-row-of-zeros — chclient.Client.
// ProbeCardinality's own doc explains why that already IS the correct
// "near empty" reading, with no special-casing needed here.
//
// The series-identity key is carrier.SeriesKey, canonicalised via
// chplan.CanonicalizeSeriesKeyExprs exactly like chsql/exemplars.go's own
// call site — so a raw attribute-Map key's ClickHouse key-order variance
// cannot inflate either distinct-series aggregate — collapsed into one
// argument via chplan.FnTuple when carrier carries more than one key.
func buildCardinalityProbePlan(carrier *cardinalityProbeCarrier) (chplan.Node, bool) {
	if carrier.Start.IsZero() || carrier.End.IsZero() || carrier.TimestampCol == "" {
		return nil, false
	}
	seriesKey := chplan.CanonicalizeSeriesKeyExprs(carrier.SeriesKey, []chplan.Node{carrier.Input}, cardinalityProbeAttributeMapColumns)
	filtered := &chplan.Filter{Input: carrier.Input, Predicate: cardinalityProbeTimeBound(carrier)}
	return &chplan.Aggregate{
		Input:              filtered,
		DropEmptyOnNoGroup: true,
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnCount, Alias: cardinalityProbeRowsAlias},
			{
				Fn:     chplan.FnUniqUpTo,
				Params: []chplan.Expr{&chplan.LitInt{V: cardinalityProbeUniqUpToCap}},
				Args:   []chplan.Expr{cardinalityProbeSeriesKeyExpr(seriesKey)},
				Alias:  cardinalityProbeDistinctSeriesAlias,
			},
			{
				Fn:    chplan.FnUniqCombined64,
				Args:  []chplan.Expr{cardinalityProbeSeriesKeyExpr(seriesKey)},
				Alias: cardinalityProbeDistinctSeriesApproxAlias,
			},
		},
	}, true
}

// cardinalityProbeSeriesKeyExpr collapses groupBy (already canonicalised by
// the caller) into the single Expr uniqUpTo needs: the bare key itself when
// there is exactly one, else a tuple(...) of all of them.
func cardinalityProbeSeriesKeyExpr(groupBy []chplan.Expr) chplan.Expr {
	if len(groupBy) == 1 {
		return groupBy[0]
	}
	return &chplan.FuncCall{Fn: chplan.FnTuple, Args: groupBy}
}

// cardinalityProbeTimeBound builds carrier's own (Start - Offset - Span,
// End - Offset] scan bound as a chplan Filter predicate — see
// buildCardinalityProbePlan's own doc for why this exact interval mirrors
// chsql's real emitter, and cardinalityProbeTimeLit's doc for the literal
// shape.
func cardinalityProbeTimeBound(carrier *cardinalityProbeCarrier) chplan.Expr {
	lower := carrier.Start.Add(-carrier.Offset).Add(-carrier.Span)
	upper := carrier.End.Add(-carrier.Offset)
	tsCol := &chplan.ColumnRef{Name: carrier.TimestampCol}
	return &chplan.Binary{
		Op:   chplan.OpAnd,
		Left: &chplan.Binary{Op: chplan.OpGt, Left: tsCol, Right: cardinalityProbeTimeLit(lower)},
		Right: &chplan.Binary{
			Op:    chplan.OpLe,
			Left:  &chplan.ColumnRef{Name: carrier.TimestampCol},
			Right: cardinalityProbeTimeLit(upper),
		},
	}
}

// cardinalityProbeTimeLit renders t as a `toDateTime64('YYYY-MM-DD
// HH:MM:SS.fffffffff', 9)` literal — the exact same shape
// internal/promql/lower.go's metadataBoundExpr already uses for an absolute
// window bound, reused here (not re-derived) so this file introduces no new
// literal-time idiom for a reviewer to separately trust.
func cardinalityProbeTimeLit(t time.Time) chplan.Expr {
	return &chplan.FuncCall{
		Fn: chplan.FnToDateTime64,
		Args: []chplan.Expr{
			&chplan.LitString{V: t.UTC().Format("2006-01-02 15:04:05.000000000")},
			&chplan.LitInt{V: chplan.NanoScale},
		},
	}
}
