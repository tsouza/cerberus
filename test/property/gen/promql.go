package gen

import (
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/test/property"
)

// PromQLQuery returns a rapid generator that produces a random
// property.Query targeted at d. The generator builds a parser.Expr AST
// directly (never a string) so we can't produce a query that fails to
// re-parse later. The query string surfaced on the Query value is the
// AST's String() method — guaranteed to round-trip through
// promparser.ParseExpr by the upstream parser contract.
//
// Accept-set as of Phase 1 PR 2 (widened from PR 1 with the from-
// scratch oracle in place):
//
//   - Bare vector selector: `metric{label="value", …}`
//   - Aggregation:           `sum(metric{...})`,
//     `sum by(label)(metric{...})`
//   - Range function:        `rate(metric{...}[60s])`,
//     `sum(rate(metric{...}[60s]))`
//
// PR 1 narrowed the set to bare selectors because the bridge oracle
// (Prom's own engine) and cerberus disagreed on two real semantic
// points: cerberus's `sum` sums every stored sample rather than the
// LWR per series, and cerberus's vector path doesn't honour the
// eval-ts boundary the way Prom does. The from-scratch oracle (PR 2)
// implements both rules correctly — so widening the generator surfaces
// those production-side divergences as property-test failures (as
// intended). The generator is wired up the way the production path
// exercises it; TestPromQL_Property_FromScratch runs unconditionally in
// the chdb lane.
//
// EvalTs is anchored to the dataset's window so every query has at
// least one matching sample within Prometheus's 5-minute LookbackDelta.
func PromQLQuery(d property.Dataset) *rapid.Generator[property.Query] {
	return rapid.Custom(func(t *rapid.T) property.Query {
		shapeID := rapid.SampledFrom(PromQLShapeIDs()).Draw(t, "shapeID")
		return drawPromQLQuery(t, d, shapeID)
	})
}

// PromQLQueryForShape returns the same generator as [PromQLQuery] with the
// semantic shape fixed to one exact roster member. Deterministic enrollment
// tests use it to execute every shape once without relying on random reach.
func PromQLQueryForShape(d property.Dataset, shapeID ShapeID) *rapid.Generator[property.Query] {
	if !containsShapeID(promQLShapeRoster[:], shapeID) {
		panic("gen/promql: unknown shape " + string(shapeID))
	}
	return rapid.Custom(func(t *rapid.T) property.Query {
		return drawPromQLQuery(t, d, shapeID)
	})
}

func drawPromQLQuery(t *rapid.T, d property.Dataset, shapeID ShapeID) property.Query {
	names := d.Metrics.NamesPresent()
	if len(names) == 0 {
		// The framework rejects this generator defect before execution;
		// preserve a non-panicking diagnostic value for direct callers.
		return property.Query{ShapeID: property.ShapeID(shapeID)}
	}

	name := rapid.SampledFrom(names).Draw(t, "metric")
	matchers := drawMatchers(t, name, d.Metrics)
	groupLabels := mapKeys(d.Metrics.LabelsPresentFor(name))

	expr := drawExpr(t, name, matchers, groupLabels, shapeID, maxComposableWrapDepth)

	// EvalTs: pick a timestamp AFTER every dataset sample but
	// well within the 5-minute LookbackDelta (Prom's default,
	// which the from-scratch oracle also uses).
	//
	// The dataset generator emits at most 10 points per series
	// at 15-second spacing (max sample ts = anchor + 9*15s =
	// 135s). Picking anchor + 200s leaves a comfortable margin
	// past the last sample so the per-series LWR rule has a
	// fresh sample to surface.
	evalTs := AnchorTime().Add(200 * time.Second).Unix()

	return property.Query{
		ShapeID: property.ShapeID(shapeID),
		String:  expr.String(),
		EvalTs:  evalTs,
	}
}

// maxComposableWrapDepth bounds how many composable wrapper shapes (today
// just promQLLabelReplaceShape / promQLRangeLabelReplaceShape) drawExpr and
// drawRangeExpr may nest before they must fall back to drawing a base
// (non-wrapping) shape. The generator has exactly one wrapper case today, so
// a single unit of budget already guarantees termination on its own; the
// constant exists — and is threaded through as an explicit parameter rather
// than left implicit — so a second wrapper shape added later inherits the
// same bound instead of the recursion silently becoming unbounded (and, with
// it, the per-iteration generated-expression size and rapid draw count
// blowing up).
const maxComposableWrapDepth = 1

// promQLBaseShapeRoster is the non-wrapping subset of promQLShapeRoster
// (gen/shapes.go): every shape drawLabelReplaceWrap may pick as the inner
// expression it wraps in label_replace(...). Kept as its own slice, rather
// than filtering promQLShapeRoster at call time, so "these are the shapes a
// wrapper can wrap" is visible at a glance and a future wrapper shape isn't
// forgotten from the exclusion the way a filter predicate could silently
// miss it.
var promQLBaseShapeRoster = []ShapeID{
	promQLSelectorShape,
	promQLSumShape,
	promQLSumByShape,
	promQLRateShape,
	promQLSumRateShape,
}

// drawExpr picks the random expression shape per the PR 2 accept-set, plus
// the composable label_replace wrapper added for #2383. Each draw is uniform
// over the candidate shapes; the breakdown:
//
//   - bare vector selector             — exercises the LWR rule
//   - sum(selector)                    — exercises aggregation + LWR
//   - sum by(label)(selector)          — exercises grouping
//   - rate(selector[60s] offset <d>)   — exercises range function; the
//     range selector also carries an `offset <d>` drawn from RangeOffsetPool
//     (positive, zero, and negative) so the offset output-timestamp labeling
//     is exercised against the oracle, not just window membership.
//   - sum(rate(selector[60s] offset <d>)) — exercises composition + offset
//   - label_replace(<any of the above>, ...) — exercises compositionality:
//     an arbitrary already-generated expression wrapped in another
//     construct, the exact "guard × aggregation" shape #2383 documents
//     (`label_replace(sum by (uri)(rate(X[5m])), ...)`) was unreachable
//     without.
//
// Aggregations strip __name__; the bare selector keeps it. Both
// shapes are valid Prom queries.
//
// wrapBudget is the remaining [maxComposableWrapDepth] budget: drawExpr
// decrements it by one on every wrapper draw and panics rather than
// recursing once it's exhausted, so a generator defect that somehow routed a
// wrapper shape into its own inner-shape pool fails loudly instead of
// looping.
func drawExpr(
	t *rapid.T,
	name string,
	matchers []*labels.Matcher,
	groupLabels []string,
	shapeID ShapeID,
	wrapBudget int,
) parser.Expr {
	sel := &parser.VectorSelector{Name: name, LabelMatchers: matchers}
	switch shapeID {
	case promQLSelectorShape:
		return sel
	case promQLSumShape:
		return &parser.AggregateExpr{Op: parser.SUM, Expr: sel}
	case promQLSumByShape:
		if len(groupLabels) == 0 {
			panic("gen/promql: sum-by shape has no observed grouping label")
		}
		group := rapid.SampledFrom(groupLabels).Draw(t, "groupLabel")
		return &parser.AggregateExpr{Op: parser.SUM, Expr: sel, Grouping: []string{group}}
	case promQLRateShape:
		return &parser.Call{
			Func: parser.Functions["rate"],
			Args: []parser.Expr{drawRangeSelector(t, name, matchers)},
		}
	case promQLSumRateShape:
		return &parser.AggregateExpr{
			Op: parser.SUM,
			Expr: &parser.Call{
				Func: parser.Functions["rate"],
				Args: []parser.Expr{drawRangeSelector(t, name, matchers)},
			},
		}
	case promQLLabelReplaceShape:
		if wrapBudget <= 0 {
			panic("gen/promql: label-replace shape exceeded maxComposableWrapDepth")
		}
		innerShape := rapid.SampledFrom(promQLBaseShapeRoster).Draw(t, "labelReplaceInnerShape")
		inner := drawExpr(t, name, matchers, groupLabels, innerShape, wrapBudget-1)
		return drawLabelReplaceWrap(t, inner, groupLabels, innerShape)
	}
	panic("gen/promql: unhandled shape " + string(shapeID))
}

// labelReplaceWrap is one (dst, replacement, regex) trio drawLabelReplaceWrap
// draws from. labelReplaceWrapCopy and labelReplaceWrapAbsent are both pure
// copy-or-tag rewrites — neither depends on the source value's content
// beyond whether it is empty — so every draw is guaranteed to produce a
// valid, deterministic label_replace call regardless of which src label
// ends up selected.
type labelReplaceWrap struct {
	dst, replacement, regex string
}

// labelReplaceWrapCopy / labelReplaceWrapAbsent are the two branches
// evalLabelReplace (test/property/oracle/promql/label_transform.go)
// implements: labelReplaceWrapCopy's regex matches any non-empty value and
// copies it verbatim to dst (the exact "endpoint" rename shape from #2383's
// own reproducer, `label_replace(sum by (uri)(rate(X[5m])), "endpoint",
// "$1", "uri", "(.+)")`); labelReplaceWrapAbsent's regex matches only an
// ABSENT src label, tagging dst with a fixed sentinel — exercising
// label_replace's pass-through path when the wrapped inner expression's
// shape drops the src label entirely.
var (
	labelReplaceWrapCopy   = labelReplaceWrap{dst: "endpoint", replacement: "$1", regex: "(.+)"}
	labelReplaceWrapAbsent = labelReplaceWrap{dst: "region_missing", replacement: "unknown", regex: "^$"}
)

// promQLLabelStrippingShapes names the promQLBaseShapeRoster entries whose
// evaluated output drops every label, including __name__ — a bare sum() or
// sum(rate()) with no by(...) clause (see drawExpr's own doc comment,
// "Aggregations strip __name__"). drawLabelReplaceWrap consults this so it
// never pairs labelReplaceWrapCopy with one of these inner shapes: src would
// be absent regardless of which label got drawn, silently degrading the
// "rename and copy an existing label" branch into an accidental no-op
// indistinguishable from labelReplaceWrapAbsent's own dedicated case — the
// exact regression this map exists to prevent.
var promQLLabelStrippingShapes = map[ShapeID]bool{
	promQLSumShape:     true,
	promQLSumRateShape: true,
}

// drawLabelReplaceWrap wraps inner in label_replace(inner, dst, replacement,
// src, regex). src is drawn from groupLabels when the metric has any
// observed labels (so the "copy an existing label" branch has real data to
// match); when the metric carries none, src falls back to "__name__" — a
// label every selector-derived series always carries — so the wrap never
// panics on an empty label pool the way promQLSumByShape's inner draw would.
//
// innerShape gates which wrap variant can be drawn: when it is one of
// promQLLabelStrippingShapes, inner has no label left for
// labelReplaceWrapCopy to actually copy — that branch is forced to
// labelReplaceWrapAbsent instead of being offered a draw it could only ever
// degrade into a no-op. Every other inner shape (including ones outside
// promQLBaseShapeRoster, e.g. drawRangeExpr's roster, which has no fully
// label-stripping entry) draws uniformly across both variants as before.
func drawLabelReplaceWrap(t *rapid.T, inner parser.Expr, groupLabels []string, innerShape ShapeID) parser.Expr {
	src := "__name__"
	if len(groupLabels) > 0 {
		src = rapid.SampledFrom(groupLabels).Draw(t, "labelReplaceSrc")
	}
	wrap := labelReplaceWrapAbsent
	if !promQLLabelStrippingShapes[innerShape] {
		wrap = rapid.SampledFrom([]labelReplaceWrap{labelReplaceWrapCopy, labelReplaceWrapAbsent}).Draw(t, "labelReplaceWrap")
	}
	return &parser.Call{
		Func: parser.Functions["label_replace"],
		Args: parser.Expressions{
			inner,
			&parser.StringLiteral{Val: wrap.dst},
			&parser.StringLiteral{Val: wrap.replacement},
			&parser.StringLiteral{Val: src},
			&parser.StringLiteral{Val: wrap.regex},
		},
	}
}

// rangeSelectorWindow is the fixed `[range]` span attached to every
// generated range selector. 60s over the dataset's 15s sample spacing
// admits up to four samples in a full window.
const rangeSelectorWindow = 60 * time.Second

// RangeOffsetPool is the `offset <d>` sweep attached to range selectors.
// PromQL's offset shifts only WHICH samples the (T-range, T] window
// reads; the result is still reported at the unshifted eval ts. This is
// exactly the axis the range-mode RangeWindow emitter mislabeled (output
// stamped at T-offset), so the pool exists to drive that path.
//
// The values are chosen against the fixed generator geometry — samples
// at anchor+0..135s, evalTs anchor+200s, range 60s — to cover three
// distinct window placements plus a negative shift:
//
//   - 0              — the un-shifted baseline (no offset token emitted).
//   - 75s / 120s     — window lands FULLY on seeded data (>=2 samples,
//     so rate is defined and the output-ts label is actually exercised).
//   - 170s / 200s    — window's left edge falls BEFORE the seed start,
//     partially (170s) or almost entirely (200s) off the data.
//   - -30s           — negative offset slides the window FORWARD, past
//     the last sample (empty on both sides — an agreement check).
//
// Every value is a whole number of seconds so the AST's String() form
// round-trips through the parser cleanly.
var RangeOffsetPool = []time.Duration{
	-30 * time.Second,
	0,
	75 * time.Second,
	120 * time.Second,
	170 * time.Second,
	200 * time.Second,
}

// drawRangeSelector builds the `metric{…}[range]` matrix selector and
// draws an `offset <d>` modifier from RangeOffsetPool. The offset lives
// on the inner VectorSelector's OriginalOffset (where MatrixSelector's
// printer renders it as a trailing `offset …`), and a value of 0 emits
// no offset token — so the baseline shape is preserved. A fresh
// VectorSelector is minted here (not the caller's `sel`) so the offset
// never leaks into the bare-selector / aggregation shapes.
func drawRangeSelector(t *rapid.T, name string, matchers []*labels.Matcher) *parser.MatrixSelector {
	vs := &parser.VectorSelector{Name: name, LabelMatchers: matchers}
	vs.OriginalOffset = rapid.SampledFrom(RangeOffsetPool).Draw(t, "rangeOffset")
	return &parser.MatrixSelector{VectorSelector: vs, Range: rangeSelectorWindow}
}

// drawMatchers picks a 0-or-1 label matcher to attach to the
// selector. The __name__ matcher is added unconditionally — PromQL's
// vector-selector printer requires either `metricName{…}` or
// `{__name__="…", …}` shape; without it the String() form would be
// `{job="api"}`, which Prometheus parses as a name-less selector and
// emits no series.
func drawMatchers(t *rapid.T, name string, m *property.MetricsModel) []*labels.Matcher {
	out := []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, "__name__", name),
	}

	labelsPresent := m.LabelsPresentFor(name)
	if len(labelsPresent) == 0 {
		return out
	}

	// 50% chance of adding a label matcher. Kept low so each query
	// has a decent shot at matching multiple series — important for
	// the aggregate path, which collapses to a single series when
	// only one input matches.
	if rapid.Bool().Draw(t, "hasMatcher") {
		// Pick from the labels that have at least one value in the
		// dataset (i.e. present on at least one series for this
		// metric). The values list ranges over the union of values
		// the generator stamped on that label.
		labelNames := mapKeys(labelsPresent)
		labelName := rapid.SampledFrom(labelNames).Draw(t, "matcherLabel")
		labelValue := rapid.SampledFrom(labelsPresent[labelName]).Draw(t, "matcherValue")
		out = append(out, labels.MustNewMatcher(labels.MatchEqual, labelName, labelValue))
	}

	return out
}

// mapKeys returns the (string) keys of m as a slice, sorted. Used by
// drawMatchers so rapid's draw is over a deterministic list rather
// than the map's range order.
func mapKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Insertion order isn't stable for maps; the dataset generator
	// stores its label pool with a fixed name order, but the
	// LabelsPresentFor() pivot loses it. Sort here so the generator
	// is reproducible across runs with the same rapid seed.
	sortStrings(out)
	return out
}

// sortStrings is a thin wrapper so this file doesn't add a `sort`
// import for one call.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
