package promql

import (
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// stripBucketSuffix returns a copy of matchers where any
// `__name__=<X>_bucket` matcher has the `_bucket` suffix removed.
//
// The Prometheus classic-histogram convention exposes one time series per
// bucket under the name `<metric>_bucket` with a `le=<bound>` label
// distinguishing them. OTel-CH stores the same data as one row per
// observation with parallel `BucketCounts` × `ExplicitBounds` arrays
// under the bare metric name (`<metric>`, no suffix), so a query
// like `rate(http_server_request_duration_seconds_bucket[5m])` must
// be translated to a filter against `MetricName='http_server_request_duration_seconds'`
// to find any rows.
//
// Strip is applied at every classic-histogram lowering path (bare or
// aggregated, instant or range). The exponential / native-histogram
// path uses its own metric-name routing (ExpHistogramSuffix) so it
// doesn't share this behaviour.
func stripBucketSuffix(matchers []*labels.Matcher) []*labels.Matcher {
	out := make([]*labels.Matcher, len(matchers))
	for i, m := range matchers {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual && strings.HasSuffix(m.Value, bucketSuffix) {
			// labels.NewMatcher recompiles a regex when applicable; for
			// MatchEqual that's cheap. Build a fresh matcher rather
			// than mutating the input — the parser may reuse the
			// matcher slice across lowering passes.
			copied, err := labels.NewMatcher(m.Type, m.Name, strings.TrimSuffix(m.Value, bucketSuffix))
			if err != nil {
				// Defensive: NewMatcher only errors on regex compile;
				// MatchEqual cannot. Forward the original on the
				// near-impossible failure path so the lowering still
				// produces a valid plan.
				out[i] = m
				continue
			}
			out[i] = copied
			continue
		}
		out[i] = m
	}
	return out
}

// histogramQuantileMatcherPredicate builds the row filter for a
// classic-histogram quantile selector.
//
// A `__name__` matcher pinned with `=` keeps the [stripBucketSuffix]
// rewrite: the user named exactly one metric and the OTel-CH histogram
// row carries its bare base name.
//
// An UNPINNED `__name__` matcher (`=~` / `!~` / `!=`) is instead a
// predicate over the Prometheus WIRE name, which no OTel-CH column
// holds — the wire surface of a classic histogram is the synthetic
// `<base>_bucket` / `<base>_count` / `<base>_sum` triple. Reference
// Prometheus's histogram_quantile skips every input series that carries
// no `le` label, so of that triple only `<base>_bucket` reaches the
// interpolation; the matcher is therefore evaluated against
// `concat(MetricName, '_bucket')` and the `_count` / `_sum` members need
// no arm at all. Testing the bare stored name (as the pinned path does)
// matches no row, answering the empty vector where the reference
// answers a quantile.
//
// A PINNED `__name__=` matcher additionally enforces the strict wire
// contract (#1483): the bare classic-histogram spelling `<base>` (no
// `_bucket` / `_count` / `_sum` suffix) is not a queryable series at
// all — reference Prometheus's classic-histogram wire surface is exactly
// the `<base>_bucket` / `<base>_count` / `<base>_sum` triple, and
// histogram_quantile additionally drops every input series without an
// `le` label, so of that triple only `<base>_bucket` ever reaches
// interpolation. A pin naming anything else (including the bare base
// name, which no Prom wire series exposes) must resolve to zero rows —
// not fall through to the OTel-CH storage row, which DOES live under the
// bare name and would over-answer with a real quantile the reference
// never computes.
//
// A `le` matcher is split out rather than folded into the row predicate:
// OTel-CH classic-histogram rows carry no `le` column (the bucket ladder
// lives in the parallel BucketCounts / ExplicitBounds arrays), so
// resolving it the way every other label resolves — an Attributes-map
// lookup — matches no row. The caller narrows BucketCounts /
// ExplicitBounds to the matched buckets instead (see
// [classicBucketLeRestriction]); this function only reports which
// matchers those are.
//
// Matcher order is preserved so an all-pinned, `le`-free selector folds
// to exactly the predicate `buildPredicate(stripBucketSuffix(...))`
// produced before the unpinned arm and the `le` split existed.
//
// Classification is delegated to wireArms (#1756): WireArmWireBound picks
// out the `le` split, WireArmWireNameUnpinned picks out the synthetic-name
// arm, and WireArmWireNamePinned's ResolveName call reproduces the strict
// bare-name rejection below — DecisionUnsatisfiable short-circuits exactly
// like the pre-migration inline check did.
func histogramQuantileMatcherPredicate(matchers []*labels.Matcher, s schema.Metrics) (chplan.Expr, []*labels.Matcher) {
	bucketWireName := func() chplan.Expr { return syntheticMetricNameExpr(s, bucketSuffix) }
	stripped := stripBucketSuffix(matchers)
	w := wireArms(matchers)
	var out chplan.Expr
	var leMatchers []*labels.Matcher
	for i, m := range matchers {
		switch w.Arms[i] {
		case WireArmWireBound:
			leMatchers = append(leMatchers, m)
		case WireArmWireNameUnpinned:
			out = andExpr(out, metricNamePredicateOn(m, s, bucketWireName))
		case WireArmWireNamePinned:
			if decision, _ := w.ResolveName(i, bucketSuffix); decision == DecisionUnsatisfiable {
				// Strict bare-name rejection (#1483, MAINTAINER DECISION 1):
				// short-circuit the whole predicate to unsatisfiable rather
				// than let a bare-name pin resolve against the OTel-CH
				// storage row.
				return &chplan.LitBool{V: false}, nil
			}
			out = andExpr(out, matcherToExpr(stripped[i], s))
		default: // WireArmStorage
			out = andExpr(out, matcherToExpr(stripped[i], s))
		}
	}
	return out, leMatchers
}

// phiArg carries histogram_quantile's phi argument in either literal
// or computed form. Exactly one side is meaningful: expr == nil means
// the literal `lit` (folded at lowering time — the common case);
// expr != nil means a runtime-computed scalar (a ScalarSubquery built
// by lowerScalarArg from `scalar(<vector>)` and friends) that the
// emitters render in place of the literal. Maps 1:1 onto the
// Phi / PhiExpr field pair on chplan.HistogramQuantile{,Native}.
type phiArg struct {
	lit  float64
	expr chplan.Expr
}

// lowerHistogramQuantile handles `histogram_quantile(phi, X)`. X is
// either:
//
//   - A bare `*parser.VectorSelector` naming a histogram metric —
//     classic (target table `otel_metrics_histogram`) or exponential /
//     native (target table `otel_metrics_exponential_histogram`); OR
//
//   - A composition of `sum [by/without]` aggregations and range-vector
//     functions (`rate`, `increase`) wrapping a bare VectorSelector —
//     i.e. the canonical Prom idiom
//     `histogram_quantile(phi, sum by(le)(rate(metric_bucket[5m])))`.
//     The OTel-CH classic-histogram representation is one row per series
//     with parallel `BucketCounts` × `ExplicitBounds` arrays (no `le`
//     label per row), so the lowering rewrites the inner chain to:
//
//   - Filter the histogram-table Scan to the rate's time window.
//
//   - Collect every in-window row's `ExplicitBounds` × `BucketCounts`
//     pair per by/without group (the `le` label is implicit in the array
//     position and is dropped from the by-clause silently), then merge
//     them over the UNION of their bounds in the cumulative per-`le`
//     domain, reducing each rung with the aggregation's fold — see
//     classicBucketMergedLadderExpr and classicBucketLadderFold.
//
// The native (exp-histogram) path requires a bare VectorSelector; the
// `rate(...)`-wrapped idiom is not modelled on the native side.
//
// Routing decision: the metric-name suffix configured on
// schema.Metrics.ExpHistogramSuffix (default `"_exp_hist"`) selects
// the native path; everything else falls through to the classic
// path. PromQL itself has no naming convention for exp histograms;
// this is a cerberus-side heuristic, configurable per deployment.
//
// Lowering produces either a chplan.HistogramQuantile (classic) or
// chplan.HistogramQuantileNative (exp). The chsql emitter renders the
// quantile arithmetic in two flavours: linear interpolation across
// ExplicitBounds × BucketCounts for the classic case, log-scale
// midpoint estimation across PositiveBucketCounts for the native case.
//
// The result is wrapped in a Project to match the Sample contract
// downstream — `MetricName=”` (Prom quantile drops __name__),
// `Attributes` reconstructed from the per-row Attributes column,
// `TimeUnix = now64(9)` (instant eval anchor), `Value` from the
// interpolated quantile.
func lowerHistogramQuantile(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 2 {
		return nil, fmt.Errorf("promql: histogram_quantile expects 2 arguments, got %d", len(c.Args))
	}
	var phi phiArg
	if lit, ok := tryScalarLiteral(c.Args[0]); ok {
		phi.lit = lit
	} else {
		// Computed phi (`histogram_quantile(scalar(x), b)`): bind phi
		// as a scalar-subquery expression; the emitters render it in
		// place of the literal, with the phi-domain branches
		// (phi <= 0 / phi >= 1 plus a leading isNaN guard) resolved at
		// runtime instead of compile time.
		expr, err := lowerScalarArg(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		phi.expr = expr
	}

	// Recognise the canonical Prom idiom — `sum [by/without](rate(...))`
	// — and dispatch to the aggregated-input path. The walker only accepts
	// shapes whose underlying terminal is a bare VectorSelector; anything
	// else falls through to today's bare-selector path (which still emits
	// the existing error message if the shape isn't recognised).
	shape, matched := matchHistogramAggIdiom(c.Args[1], s)
	if matched {
		// The per-`le` rung fold is resolved here rather than in the
		// matcher because `quantile`'s parameter may be a computed
		// scalar, which needs the schema + lowering context.
		fold, err := classicBucketLadderFold(shape.agg, s, ctx)
		if err != nil {
			return nil, err
		}
		shape.classicFold = fold
	}
	if matched && histogramAggShapeLowerable(shape, s) {
		if s.IsExpHistogramMetric(shape.selector.Name) {
			// Range mode: fan the exp-histogram merge + quantile
			// interpolation across the request's step grid, or — under an
			// absolute `@` — evaluate it once at the pin and broadcast.
			switch rangeGridShapeFor(shape.selector, ctx) {
			case gridFanout:
				return lowerHistogramQuantileNativeAggRange(shape, phi, s, ctx), nil
			case gridBroadcast:
				inner, err := lowerHistogramQuantileNativeAgg(shape, phi, s, ctx)
				if err != nil {
					return nil, err
				}
				return broadcastHistogramAtPin(inner, s, ctx), nil
			}
			return lowerHistogramQuantileNativeAgg(shape, phi, s, ctx)
		}
		// Range mode: build a per-step plan that fans the bucket
		// aggregation + quantile interpolation across the request's step
		// grid, or — under an absolute `@` — evaluate it once at the pin
		// and broadcast that value over the grid.
		switch rangeGridShapeFor(shape.selector, ctx) {
		case gridFanout:
			return lowerHistogramQuantileClassicAggRange(shape, phi, s, ctx), nil
		case gridBroadcast:
			inner, err := lowerHistogramQuantileAgg(shape, phi, s, ctx)
			if err != nil {
				return nil, err
			}
			return broadcastHistogramAtPin(inner, s, ctx), nil
		}
		return lowerHistogramQuantileAgg(shape, phi, s, ctx)
	}

	// A matched CLASSIC-bucket idiom that is not lowerable is precisely one
	// whose aggregation has no entry in classicBucketLadderFold — the
	// series-SHAPING operators, which reduce nothing and so cannot be
	// expressed as a per-`le` rung fold. Prometheus evaluates them over
	// ordinary float bucket series, and so does cerberus here: the argument
	// goes through the ordinary pipeline (which fans the bucket arrays into
	// `le`-labelled samples) and the ladder is reassembled afterwards. The
	// exp-histogram half deliberately stays out: reference Prometheus DROPS
	// native-histogram samples under every aggregation but sum / avg, so
	// the empty fallback below is its answer there, not a gap.
	if matched && !s.IsExpHistogramMetric(shape.selector.Name) {
		return lowerHistogramQuantileClassicFloat(c.Args[1], phi, s, ctx)
	}

	vs, ok := unwrapVectorSelector(c.Args[1])
	if !ok {
		// Unrecognised inner shape — `histogram_quantile(0.9, sum(up))`,
		// `histogram_quantile(0.9, vector(1))`, … . Reference Prometheus
		// accepts any instant-vector second argument: classic-histogram
		// interpolation reads each float sample's `le` label, and
		// samples WITHOUT an `le` label are skipped (with an info
		// annotation) — so a non-bucket input evaluates to an EMPTY
		// vector, not an error (see promql/engine.go's
		// histogram_quantile float-bucket path).
		//
		// In cerberus's OTel-CH model, classic-histogram buckets live as
		// parallel BucketCounts × ExplicitBounds array rows — float
		// pipelines (aggregations, arithmetic, vector()) can never carry
		// `le`-keyed bucket series. Every shape outside the recognised
		// idioms therefore provably holds no bucket data: lower the
		// argument (preserving its own rejection/typing errors) and fold
		// to zero rows, matching the reference's empty result.
		inner, err := lower(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		return &chplan.Filter{
			Input:     inner,
			Predicate: &chplan.LitBool{V: false},
		}, nil
	}

	if s.IsExpHistogramMetric(vs.Name) {
		// Range mode (ctx.step > 0): build a per-step plan that fans the
		// exponential-histogram quantile interpolation across the
		// request's step grid. Each anchor independently runs the LWR
		// projection over the per-row exp-histogram fields and feeds a
		// merged distribution into HistogramQuantileNative, so the matrix
		// pivot sees one quantile row per (series, anchor) rather than
		// the single instant-mode `now64(9)` row repeated for every
		// step. An absolute `@` pins that window for the whole query, so
		// it is evaluated once and broadcast instead.
		switch rangeGridShapeFor(vs, ctx) {
		case gridFanout:
			return lowerHistogramQuantileNativeBareRange(vs, phi, s, ctx), nil
		case gridBroadcast:
			inner, err := lowerHistogramQuantileNative(vs, phi, s, ctx)
			if err != nil {
				return nil, err
			}
			return broadcastHistogramAtPin(inner, s, ctx), nil
		}
		return lowerHistogramQuantileNative(vs, phi, s, ctx)
	}

	// Range mode: build a per-step plan that fans the classic-histogram
	// bucket array forward through a StepGrid + LWR window so each step
	// in `[start, end]` emits its own quantile row. Pool-AK flagged the
	// now64(9) hardcode in this lowering as the `histogram_quantile
	// classic-bucket still hardcodes now64(9) in range mode` bug surfaced
	// when finishing the per-step LWR rework (PR #347). An absolute `@`
	// pins one window for the whole query, so it is evaluated once and
	// broadcast over the grid rather than fanned.
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		return lowerHistogramQuantileClassicBareRange(vs, phi, s, ctx), nil
	case gridBroadcast:
		inner, err := lowerHistogramQuantileClassicBare(vs, phi, s, ctx)
		if err != nil {
			return nil, err
		}
		return broadcastHistogramAtPin(inner, s, ctx), nil
	}
	return lowerHistogramQuantileClassicBare(vs, phi, s, ctx)
}

// lowerHistogramQuantileClassicBare is the instant-mode lowering for
// `histogram_quantile(phi, <bare classic-histogram selector>)` — one
// quantile row per series over the newest sample within the selector's
// anchor. It is the pinned-`@` half of the range-mode split as well as
// the instant path, which is why it is named alongside its three
// siblings (lowerHistogramQuantileAgg / …Native / …NativeAgg) rather
// than left inline.
func lowerHistogramQuantileClassicBare(
	vs *parser.VectorSelector,
	phi phiArg,
	s schema.Metrics,
	ctx lowerCtx,
) (chplan.Node, error) {
	// Target the classic-histogram table directly. OTel-CH classic
	// histograms are one row per series with parallel BucketCounts +
	// ExplicitBounds arrays (no `le` label per row), under the bare
	// metric name. Strip the conventional `_bucket` suffix off the
	// `__name__` matcher so a Grafana query of
	// `rate(<X>_bucket[5m])` filters against `MetricName='<X>'`.
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred, leMatchers := histogramQuantileMatcherPredicate(vs.LabelMatchers, s)
	pred, err := andInstantWindow(pred, vs, s.TimestampColumn, ctx)
	if err != nil {
		return nil, err
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	input = classicBucketLeRestriction(input, leMatchers, s)

	hq := &chplan.HistogramQuantile{
		Input:                latestSampleAgg(input, classicBucketLatestAggs(s), s),
		Phi:                  phi.lit,
		PhiExpr:              phi.expr,
		BucketCountsColumn:   s.BucketCountsColumn,
		ExplicitBoundsColumn: s.ExplicitBoundsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Wrap in a Project to match the Sample-row contract downstream
	// (MetricName='', Attributes=<gkey>, TimeUnix=now64(9), Value=value).
	// Mirrors wrapAggregateForSample in lower.go.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: chplan.NowNano(), Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// histogramQuantilesMinArgs is the smallest legal arity of
// `histogram_quantiles(<vector>, "<label>", <phi>, ...)`: a vector, the
// quantile-label name, and at least one phi.
const histogramQuantilesMinArgs = 3

// lowerHistogramQuantiles handles the experimental, variadic
// `histogram_quantiles(<vector>, "<labelName>", phi0, phi1, ...)`.
//
// Reference Prometheus semantics (promql.funcHistogramQuantiles):
// compute `histogram_quantile(phi_i, <vector>)` for EACH phi in one pass
// over the input, and for every (input series × phi) emit one output
// series whose label set is the input series' labels with `__name__`
// dropped and a new label `<labelName>=<FormatOpenMetricsFloat(phi)>`
// attached. The phi label value uses OpenMetrics float formatting
// (`0` → `"0.0"`, `1` → `"1.0"`, `0.5` → `"0.5"`, `-0.1` → `"-0.1"`).
// Out-of-domain phi (< 0 / > 1 / NaN) carries the same per-quantile
// warning + saturating value as the singular function — that behaviour
// lives entirely inside the per-phi kernel, so reusing it gives parity
// for free.
//
// Lowering strategy: fan out over the variadic phi list. Each phi
// reuses lowerHistogramQuantile as the per-phi kernel by synthesising a
// singular `histogram_quantile(phi_i, <vector>)` call, then wraps the
// kernel's Sample-row output in a Project that overrides the Attributes
// map with `mapConcat(Attributes, map('<labelName>', '<phiStr>'))` —
// the same label-injection shape histogram_bucket.go uses for `le`.
// mapConcat with the synthetic single-entry map on the right overwrites
// any pre-existing `<labelName>` key, matching Prom's
// `NewBuilder(lbls).Set(labelName, phiStr)`. The per-phi arms are then
// stitched with UNION ALL: each arm produces a disjoint label set (the
// phi label differs), so UNION ALL is the correct, dedup-free
// concatenation. A single-phi call collapses to the lone arm (UnionAll
// rejects degenerate single-arm unions), so the phi label is still
// injected but no union is emitted.
func lowerHistogramQuantiles(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < histogramQuantilesMinArgs {
		return nil, fmt.Errorf(
			"promql: histogram_quantiles expects at least %d arguments (vector, label, phi...), got %d",
			histogramQuantilesMinArgs, len(c.Args),
		)
	}
	vectorArg := c.Args[0]
	labelName, err := stringArg(c.Args[1], "histogram_quantiles", "label")
	if err != nil {
		return nil, err
	}

	phiArgs := c.Args[2:]
	arms := make([]chplan.Node, 0, len(phiArgs))
	for _, phiExpr := range phiArgs {
		// Reuse the singular kernel: synthesise
		// `histogram_quantile(phi_i, <vector>)`. The kernel handles every
		// input shape (classic / native bare selector, the sum-rate idiom)
		// plus the phi-domain saturation + computed-phi paths, so the
		// plural function inherits all of it.
		kernelCall := &parser.Call{
			Func: parser.Functions["histogram_quantile"],
			Args: parser.Expressions{phiExpr, vectorArg},
		}
		kernel, err := lowerHistogramQuantile(kernelCall, s, ctx)
		if err != nil {
			return nil, err
		}

		// Attach the quantile label. The label VALUE is the OpenMetrics
		// float rendering of phi. When phi is a literal (the common case)
		// we fold it at lowering time so the label is a static string.
		// A computed phi (`histogram_quantiles(v, "q", scalar(x))`, which
		// reference Prometheus type-checks and answers by reading phi[0].F
		// per step) renders the same formatting at query time — see
		// [openMetricsFloatExpr].
		var phiLabel chplan.Expr
		if phiLit, ok := tryScalarLiteral(phiExpr); ok {
			phiLabel = &chplan.LitString{V: labels.FormatOpenMetricsFloat(phiLit)}
		} else {
			phiLabel, err = openMetricsFloatExpr(func() (chplan.Expr, error) {
				return lowerScalarArg(phiExpr, s, ctx)
			})
			if err != nil {
				return nil, err
			}
		}
		// No canonicalisation here: the kernel below already binds
		// Attributes canonically and mapConcat appends to that, so every
		// row of one logical series gets the same key order.
		attrs := &chplan.FuncCall{
			Name: "mapConcat",
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.AttributesColumn},
				&chplan.FuncCall{
					Name: "map",
					Args: []chplan.Expr{
						&chplan.LitString{V: labelName},
						phiLabel,
					},
				},
			},
		}
		arms = append(arms, projectAttributesOverInner(kernel, s, attrs))
	}

	if len(arms) == 1 {
		return arms[0], nil
	}
	return &chplan.UnionAll{Inputs: arms}, nil
}

// openMetricsFloatExpr renders Prometheus's
// `labels.FormatOpenMetricsFloat` for a RUNTIME float expression — the
// label value `histogram_quantiles` stamps on each per-phi arm when phi
// is computed rather than literal.
//
// Reference (prometheus/model/labels): the hardcoded cases 1 / 0 / -1 /
// NaN / ±Inf, then `strconv.FormatFloat(f, 'g', -1, 64)`, with `".0"`
// appended when that result carries neither `.` nor `e`.
//
// ClickHouse's `toString(Float64)` already produces the same shortest
// round-tripping digits Go's 'g' -1 does — the two disagree only on
// LAYOUT. CH switches to scientific notation at a different threshold
// (`0.00001` where Go writes `1e-05`) and spells the exponent
// differently (`1e-7` / `1e21` where Go writes `1e-07` / `1e+21`). So
// the expression reads the digits and the decimal exponent back out of
// CH's rendering — exactly, from the string, never via log10 — and
// re-lays them out under Go's rule: scientific iff the decimal exponent
// falls outside [-4, 21), exponent always signed and at least two
// digits wide.
//
// `newPhi` mints the phi expression; it is called twice (the value and
// its CH rendering) and the two results are bound as the parameters of
// a single-element `arrayMap`. Every other mention inside the formatter
// is a lambda parameter, so a phi that lowers to a scalar subquery
// appears twice in the SQL rather than once per mention. It is a
// factory rather than a value for the same reason limitRatioPredicate's
// is: chplan Expr trees must stay trees.
func openMetricsFloatExpr(newPhi func() (chplan.Expr, error)) (chplan.Expr, error) {
	const (
		// Lambda parameter names: the phi value and CH's own string
		// rendering of its magnitude.
		valueParam  = "v"
		digitsParam = "u"
		// Go's %g uses scientific notation exactly when the decimal
		// exponent leaves [-4, 21) — equivalently when the magnitude
		// leaves [1e-4, 1e21).
		sciLowerBound = 1e-4
		sciUpperBound = 1e21
		// Exponents below this get a leading zero: Go writes at least
		// two exponent digits ("1e-05", never "1e-5").
		expPadBelow = 10
	)

	call := func(name string, args ...chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Name: name, Args: args}
	}
	// Shape constants go inline rather than through `?` placeholders:
	// they feed `concat`, where a bound parameter leaves the operand
	// type indeterminate and CH mis-dispatches to `arrayConcat`
	// (Code 43). Same reasoning as chplan.InlineString's doc comment.
	str := func(v string) chplan.Expr { return &chplan.InlineString{V: v} }
	i := func(v int64) chplan.Expr { return &chplan.LitInt{V: v} }
	f := func(v float64) chplan.Expr { return &chplan.LitFloat{V: v} }
	bin := func(op chplan.BinaryOp, l, r chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: op, Left: l, Right: r}
	}

	// Every sub-expression is a factory: the IR is a tree, so two
	// mentions of `epos` must be two distinct node graphs.
	v := func() chplan.Expr { return &chplan.BareIdent{Name: valueParam} }
	u := func() chplan.Expr { return &chplan.BareIdent{Name: digitsParam} }

	// `position` and `length` return UInt64, while subtracting from one
	// yields Int64. An `if` with one arm of each has no common supertype,
	// and ClickHouse resolves that pair to `Variant(Int64, UInt64)`;
	// arithmetic or `toString` over a Variant then comes back Nullable.
	// That nullability rides the whole label expression up into
	// `mapConcat`, so Attributes arrives as `Map(String, Nullable(String))`
	// and the production cursor refuses to scan it into `map[string]string`.
	// chDB coerces it away, so only the strict-scan differential sees it.
	// Landing every count on Int64 at the source keeps each `if`
	// single-typed.
	countOf := func(name string, args ...chplan.Expr) chplan.Expr {
		return call("toInt64", call(name, args...))
	}

	// Position of the exponent marker in CH's rendering; 0 when CH chose
	// fixed notation.
	epos := func() chplan.Expr { return countOf("position", u(), str("e")) }
	// The mantissa CH rendered — the whole string in fixed notation.
	mantRaw := func() chplan.Expr {
		return call("if", bin(chplan.OpGt, epos(), i(0)),
			call("substring", u(), i(1), bin(chplan.OpSub, epos(), i(1))),
			u())
	}
	// Mantissa digits with the decimal point removed, then with leading
	// and trailing zeros stripped: the significant digits, most
	// significant first.
	digitsAll := func() chplan.Expr { return call("replaceAll", mantRaw(), str("."), str("")) }
	digitsLead := func() chplan.Expr { return call("replaceRegexpOne", digitsAll(), str("^0+"), str("")) }
	digits := func() chplan.Expr { return call("replaceRegexpOne", digitsLead(), str("0+$"), str("")) }

	pointPos := func() chplan.Expr { return countOf("position", mantRaw(), str(".")) }
	// Digit count left of the decimal point (the whole mantissa when
	// there is no point).
	intLen := func() chplan.Expr {
		return call("if", bin(chplan.OpGt, pointPos(), i(0)),
			bin(chplan.OpSub, pointPos(), i(1)),
			countOf("length", mantRaw()))
	}
	// The decimal exponent: read straight off CH's exponent when it used
	// scientific notation, else derived from where the first significant
	// digit sits relative to the decimal point. Both forms are exact —
	// no floating-point log is involved, so the [-4, 21) boundaries
	// cannot be misclassified.
	expVal := func() chplan.Expr {
		leadingZeros := bin(chplan.OpSub, countOf("length", digitsAll()), countOf("length", digitsLead()))
		return call("if", bin(chplan.OpGt, epos(), i(0)),
			call("toInt64", call("substring", u(), bin(chplan.OpAdd, epos(), i(1)))),
			bin(chplan.OpSub, bin(chplan.OpSub, intLen(), leadingZeros), i(1)))
	}

	// `d` or `d.ddd` — Go's normalised scientific mantissa.
	mantissa := func() chplan.Expr {
		return call("if", bin(chplan.OpLe, countOf("length", digits()), i(1)),
			digits(),
			call("concat", call("substring", digits(), i(1), i(1)), str("."), call("substring", digits(), i(2))))
	}
	expDigits := func() chplan.Expr { return call("toString", call("abs", expVal())) }
	expSuffix := func() chplan.Expr {
		return call("concat",
			call("if", bin(chplan.OpLt, expVal(), i(0)), str("-"), str("+")),
			call("if", bin(chplan.OpLt, call("abs", expVal()), i(expPadBelow)),
				call("concat", str("0"), expDigits()),
				expDigits()))
	}
	// `u` is the magnitude's rendering, so the sign is reattached here
	// for both layouts.
	sign := func() chplan.Expr {
		return call("if", bin(chplan.OpLt, v(), f(0)), str("-"), str(""))
	}
	sci := call("concat", sign(), mantissa(), str("e"), expSuffix())
	// CH's fixed notation already matches Go's over the whole [-4, 21)
	// exponent range; only Go's trailing `.0` for integral values is
	// missing.
	fixed := call("concat", sign(),
		call("if", bin(chplan.OpGt, pointPos(), i(0)), u(), call("concat", u(), str(".0"))))

	body := call("multiIf",
		call("isNaN", v()), str("NaN"),
		bin(chplan.OpEq, v(), f(1)), str("1.0"),
		bin(chplan.OpEq, v(), f(0)), str("0.0"),
		bin(chplan.OpEq, v(), f(-1)), str("-1.0"),
		bin(chplan.OpAnd, call("isInfinite", v()), bin(chplan.OpGt, v(), f(0))), str("+Inf"),
		call("isInfinite", v()), str("-Inf"),
		bin(chplan.OpOr,
			bin(chplan.OpLt, call("abs", v()), f(sciLowerBound)),
			bin(chplan.OpGe, call("abs", v()), f(sciUpperBound))), sci,
		fixed)

	phiValue, err := newPhi()
	if err != nil {
		return nil, err
	}
	phiDigits, err := newPhi()
	if err != nil {
		return nil, err
	}
	return &chplan.Subscript{
		Container: call("arrayMap",
			&chplan.Lambda{Params: []string{valueParam, digitsParam}, Body: body},
			call("array", phiValue),
			call("array", call("toString", call("abs", phiDigits)))),
		Key: i(1),
	}, nil
}

// histogramAggShape collects the bits we need to build the
// aggregated-input plan for `histogram_quantile(phi, <agg>(rate(<sel>[range])))`.
//
// `selector` is the underlying bare VectorSelector (carrying the metric
// name + label matchers). `windowRange` is the `[range]` duration from
// the wrapping `rate`/`increase` (zero if there's no range-vector
// function — currently always set when this struct is built, but
// kept explicit for clarity). `agg` carries
// the AggregateExpr metadata (Op, Grouping, Without) when a wrapping
// aggregation is present; nil means "no aggregation wrap, just rate(...)".
type histogramAggShape struct {
	selector    *parser.VectorSelector
	windowRange time.Duration
	// windowFn is the matched range-vector function name. It decides
	// both how a series' in-window samples reduce to one value per `le`
	// (histogramWindowFold) and how many samples that reduction needs
	// before it emits anything (histogramWindowMinSamples).
	windowFn string
	agg      *parser.AggregateExpr
	// classicFold is the per-`le` rung reduction the classic-histogram
	// paths apply, resolved from `agg` by classicBucketLadderFold (SUM
	// when `agg` is nil — a bare `rate(...)` still folds every in-window
	// row of a series into one ladder). nil means no classic lowering
	// exists for that operator. The matcher leaves this unset; the
	// dispatcher fills it, because resolving `quantile`'s parameter needs
	// the schema + lowering context the matcher does not have.
	classicFold classicBucketRungFold
}

// histogramWindowMinSamples maps a matched range-vector function to the
// number of samples reference PromQL needs inside `[range]` before it emits
// at an anchor. rate / increase span a delta between two points and emit
// nothing with fewer; sum_over_time folds whatever is there, so one sample
// is a valid window.
func histogramWindowMinSamples(fn string) int {
	if fn == "rate" || fn == "increase" {
		return rateMinSamples
	}
	return stalenessMinSamples
}

// minSamples is the matched function's per-series "no sample emitted"
// floor. Reference PromQL applies it to each SERIES independently, so the
// per-series stage is the only place it can be enforced.
func (shape histogramAggShape) minSamples() int {
	return histogramWindowMinSamples(shape.windowFn)
}

// matchHistogramAggIdiom walks the expression tree looking for the
// shape `[sum|avg by/without (...) ((paren))*] rate|increase|sum_over_time((paren)*
// <VectorSelector>[range])`. Returns the captured shape on a match.
//
// Accepted shapes (after peeling ParenExpr / StepInvariantExpr at each
// level):
//   - rate(metric_bucket[5m])
//   - increase(metric_bucket[5m])
//   - sum_over_time(metric_bucket[5m])   (DELTA-histogram aggregation)
//   - sum by(le)(rate(metric_bucket[5m]))
//   - avg by(le)(rate(metric_bucket[5m]))
//   - sum by(le)(sum_over_time(metric_bucket[5m]))
//   - sum without(...) (rate(metric_bucket[5m]))
//   - avg without(...) (increase(metric_bucket[5m]))
//
//   - min / max / count / group / stddev / stdvar / quantile in place of
//     sum / avg (classic histograms only — see classicBucketLadderFold)
//
// Anything else returns ok=false and the caller falls through to the
// bare-selector path: range-vector functions other than rate / increase /
// sum_over_time, and deeper nestings (e.g. `sum(sum(...))`).
//
// WHICH aggregation operators lower is deliberately not decided here.
// The matcher captures any aggregation wrapper; classicBucketLadderFold
// answers whether that operator has a per-`le` rung reduction, and
// histogramAggShapeLowerable combines that with the metric's histogram
// flavour. Keeping the operator table in one place is what let `quantile`
// join the set without a second gate to update.

// classicBucketRungFold reduces one `le` rung's per-row cumulative counts
// into the group's rung for that bound. `rungs` is an Array expression
// holding the contributing rows' cumulative counts at that bound.
type classicBucketRungFold func(rungs chplan.Expr) chplan.Expr

// promGroupSampleValue is the value PromQL's `group` aggregation writes
// into every output sample. It carries no data from the input, only the
// fact that the group exists.
const promGroupSampleValue = 1.0

// classicBucketLadderFold maps a PromQL aggregation to the ClickHouse
// reduction that folds one `le` rung across a group's rows.
//
// Prometheus aggregates classic histograms in the CUMULATIVE domain: each
// input series is one already-cumulative float sample per `le`, and
// `sum by(le)` / `max by(le)` / … reduce those floats rung by rung. This
// table is that reduction transcribed — which is why it is total over
// every non-parameterised aggregation rather than the `sum` / `avg`
// whitelist an element-wise PER-BUCKET collapse forces. A per-bucket
// collapse is only sound for reducers that commute with the running
// total (`sum`, and `avg` by scale-invariance); folding in the cumulative
// domain has no such restriction because the ladder is built BEFORE the
// reduction. See classicBucketMergedLadderExpr for that construction.
//
// `quantile(phi, ...)` is the one PARAMETERISED aggregation with an
// entry: it reduces each `le` group to a single value exactly as `min` /
// `stddev` do, so it folds a rung like any other reducer — see
// promQuantileRungFold. The other three (`topk` / `bottomk` /
// `count_values`) have no entry because they are not reductions at all:
// `topk` / `bottomk` SELECT a subset of the input series and keep their
// full label sets, and `count_values` MINTS a label whose values are
// sample values. Neither collapses an `le` group to one value per rung,
// so no fold could express them without inventing a number.
//
// A nil result means "this operator has no per-rung reduction", and it is
// the routing signal lowerHistogramQuantile uses to send the query through
// lowerHistogramQuantileClassicFloat instead — the float-domain evaluator
// that reproduces what Prometheus does with these operators.
func classicBucketLadderFold(agg *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (classicBucketRungFold, error) {
	arrayFold := func(name string) classicBucketRungFold {
		return func(rungs chplan.Expr) chplan.Expr {
			return &chplan.FuncCall{Name: name, Args: []chplan.Expr{rungs}}
		}
	}
	reduceFold := func(agg string) classicBucketRungFold {
		return func(rungs chplan.Expr) chplan.Expr {
			return &chplan.FuncCall{
				Name: "arrayReduce",
				Args: []chplan.Expr{&chplan.LitString{V: agg}, rungs},
			}
		}
	}
	if agg == nil {
		// A bare `rate(...)` with no aggregation wrapper still folds every
		// in-window row of a series into one ladder, and summing is what
		// that window collapse means.
		return arrayFold("arraySum"), nil
	}
	switch agg.Op {
	case parser.SUM:
		return arrayFold("arraySum"), nil
	case parser.AVG:
		return arrayFold("arrayAvg"), nil
	case parser.MIN:
		return arrayFold("arrayMin"), nil
	case parser.MAX:
		return arrayFold("arrayMax"), nil
	case parser.COUNT:
		// Prom's `count` is the number of contributing series at that
		// `le`; here it is the number of contributing ROWS, the same
		// window model every other fold on this path uses.
		return arrayFold("length"), nil
	case parser.GROUP:
		return func(chplan.Expr) chplan.Expr {
			return &chplan.LitFloat{V: promGroupSampleValue}
		}, nil
	case parser.STDDEV:
		// Prom's stddev / stdvar are POPULATION statistics over the
		// group's samples (promql/engine.go), so stddevPop / varPop.
		return reduceFold("stddevPop"), nil
	case parser.STDVAR:
		return reduceFold("varPop"), nil
	case parser.QUANTILE:
		phi, err := lowerQuantileParamArg(agg.Param, s, ctx)
		if err != nil {
			return nil, err
		}
		return promQuantileRungFold(phi), nil
	}
	return nil, nil
}

// lowerQuantileParamArg resolves `quantile(<param>, ...)`'s scalar
// parameter into the plan expression the rung fold interpolates with:
// a folded literal for the common `quantile(0.9, ...)` form, otherwise
// the scalar-subquery expression lowerScalarArg builds for a computed
// parameter (`quantile(scalar(x), ...)`). Mirrors how the phi argument of
// histogram_quantile itself is resolved — see phiArg.
func lowerQuantileParamArg(e parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Expr, error) {
	if lit, ok := tryScalarLiteral(e); ok {
		return &chplan.LitFloat{V: lit}, nil
	}
	return lowerScalarArg(e, s, ctx)
}

// promQuantileRungFold folds one `le` rung with PromQL's `quantile`
// aggregation — a transcription of prometheus/promql/quantile.go's
// `quantile()` helper, which is exact linear interpolation between the
// two order statistics straddling `phi * (n - 1)`:
//
//	sort(rungs); rank := phi * (n - 1)
//	lo := floor(rank); hi := min(n - 1, lo + 1); w := rank - lo
//	rungs[lo] * (1 - w) + rungs[hi] * w
//
// Transcribed rather than delegated to ClickHouse's `quantile(phi)`
// aggregate because that one is an APPROXIMATE reservoir-sampling
// estimator (non-deterministic above 8192 samples), while this fold's
// result feeds bucket interpolation that reads the RATIOS between rungs —
// so an estimator's error moves the answer. The arithmetic here is
// exact for any rung count.
//
// A rung array is never empty (a union bound exists because at least one
// row's layout carries it, and the +Inf rung folds over every row), so
// upstream's `len(values) == 0 → NaN` guard has no reachable counterpart.
// An out-of-domain phi is not interpolated at all: PromQL replaces the
// aggregation's value with NaN / ±Inf, so every rung of the ladder
// carries that constant. A literal phi resolves that at lowering time; a
// computed one resolves it at runtime, pairing the sentinel-clamped
// parameter with the matching output guard exactly as the generic
// `quantile` aggregation lowering does.
func promQuantileRungFold(phi chplan.Expr) classicBucketRungFold {
	if lit, ok := phi.(*chplan.LitFloat); ok {
		if infValue, outOfRange := outOfRangePhiInf(lit.V); outOfRange {
			return func(chplan.Expr) chplan.Expr {
				return &chplan.LitFloat{V: infValue}
			}
		}
		return promQuantileInterpolateFold(phi)
	}
	return func(rungs chplan.Expr) chplan.Expr {
		return outOfRangePhiGuardExpr(
			phi,
			promQuantileInterpolateFold(sanitizedPhiParamExpr(phi))(rungs),
		)
	}
}

// promQuantileInterpolateFold is the interpolation itself, for a phi
// already known to be in [0, 1].
func promQuantileInterpolateFold(phi chplan.Expr) classicBucketRungFold {
	return func(rungs chplan.Expr) chplan.Expr {
		sorted := chplan.Expr(&chplan.BareIdent{Name: paramSortedRungs})
		rank := chplan.Expr(&chplan.BareIdent{Name: paramQuantileRank})
		lastIdx := subExpr(toFloat64Expr(&chplan.FuncCall{
			Name: "length", Args: []chplan.Expr{sorted},
		}), &chplan.LitInt{V: 1})

		lo := &chplan.FuncCall{Name: "floor", Args: []chplan.Expr{rank}}
		hi := &chplan.FuncCall{Name: "least", Args: []chplan.Expr{
			lastIdx, addExpr(lo, &chplan.LitInt{V: 1}),
		}}
		weight := subExpr(rank, lo)

		interpolated := addExpr(
			mulExpr(rungAt(sorted, lo), subExpr(&chplan.LitInt{V: 1}, weight)),
			mulExpr(rungAt(sorted, hi), weight),
		)

		// `phi` is bound once inside the rank, and the sorted array once
		// by the outer binding, so neither the (possibly subquery-valued)
		// parameter nor the rung array is re-evaluated per reference.
		return bindOnce(
			paramSortedRungs,
			&chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{rungs}},
			bindOnce(paramQuantileRank, mulExpr(phi, lastIdx), interpolated),
		)
	}
}

// rungAt indexes a sorted rung array by a zero-based float position,
// converting to ClickHouse's one-based integer subscript.
func rungAt(sorted, idx chplan.Expr) chplan.Expr {
	return &chplan.Subscript{
		Container: sorted,
		Key: addExpr(
			&chplan.FuncCall{Name: "toUInt64", Args: []chplan.Expr{idx}},
			&chplan.LitInt{V: 1},
		),
	}
}

// bindOnce binds `value` to the lambda parameter `name` for the duration
// of `body`, so a sub-expression referenced several times is evaluated
// once. ClickHouse has no `let`, and repeating a rung array (itself an
// arrayFilter over an arrayMap) per reference would both bloat the SQL
// and re-run the filter — `arrayMap(<name> -> body, array(value))[1]` is
// the single-element-array idiom that stands in for one.
func bindOnce(name string, value, body chplan.Expr) chplan.Expr {
	return &chplan.Subscript{
		Container: &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{name}, Body: body},
			&chplan.FuncCall{Name: "array", Args: []chplan.Expr{value}},
		}},
		Key: &chplan.LitInt{V: 1},
	}
}

func addExpr(l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpAdd, Left: l, Right: r}
}

func subExpr(l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpSub, Left: l, Right: r}
}

func mulExpr(l, r chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpMul, Left: l, Right: r}
}

func toFloat64Expr(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{e}}
}

// expHistogramAggOpIsMergeable reports whether a PromQL aggregation over
// NATIVE (exponential) histogram series has a merged-distribution
// lowering. Prometheus merges native histograms under `sum` and `avg`;
// every other aggregation DROPS histogram samples with an annotation, so
// an empty result is the reference answer there and the dispatcher lets
// the shape fall through to the empty fold.
func expHistogramAggOpIsMergeable(op parser.ItemType) bool {
	return op == parser.SUM || op == parser.AVG
}

// histogramAggShapeLowerable reports whether a matched aggregation idiom
// has a lowering for the histogram flavour its metric is stored in. The
// two flavours diverge because Prometheus itself does: classic buckets
// are ordinary float series that every aggregation reduces, while native
// histogram samples are dropped by everything except `sum` / `avg`.
func histogramAggShapeLowerable(shape histogramAggShape, s schema.Metrics) bool {
	if s.IsExpHistogramMetric(shape.selector.Name) {
		return shape.agg == nil || expHistogramAggOpIsMergeable(shape.agg.Op)
	}
	return shape.classicFold != nil
}

func matchHistogramAggIdiom(e parser.Expr, s schema.Metrics) (histogramAggShape, bool) {
	e = peelWrappers(e)

	// Try an outer aggregation wrapper. Which operators actually have a
	// classic-histogram lowering is decided by classicBucketLadderFold,
	// not here — this walker only captures the shape.
	var agg *parser.AggregateExpr
	if a, ok := e.(*parser.AggregateExpr); ok {
		agg = a
		e = peelWrappers(a.Expr)
	}

	// #1692: `histogram_quantile(phi, sum by(le)(<bucket-selector>))` with
	// NO wrapping rate / increase / sum_over_time. This form only exists
	// here when an aggregation DID wrap the selector — with no aggregation
	// at all, `e` is already a bare VectorSelector that the caller's own
	// unwrapVectorSelector fallback routes through lowerHistogramQuantileClassicBare
	// / the native bare-selector path; matching it again here would just
	// duplicate that routing. windowFn="" is the sentinel this shape
	// carries: histogramWindowFold treats it as ordinary instant-vector
	// "latest sample in the staleness lookback" semantics (latestSampleFold)
	// rather than a windowed rate/increase/sum_over_time reduction, and
	// windowRange is set to instantLookback (Prom's 5m default) so the SQL
	// time-window filter matches what a bare selector would use.
	//
	// Gated on the same strict wire-name test #1483 enforces downstream
	// (histogramQuantileMatcherPredicate): a PINNED `__name__` that names
	// neither a `_bucket`-suffixed classic series nor a registered
	// exp-histogram metric can never carry bucket data, so it must keep
	// falling through to the "unrecognised inner shape" empty-Filter
	// fallback below — matching it here would wrongly route provably
	// non-bucket aggregations like `sum(up)` through the histogram
	// machinery instead of the compile-time-empty shape that pins.
	// An UNPINNED name (regex / negated matcher, vs.Name == "") is left
	// to the same downstream wire-name handling the rate/increase/
	// sum_over_time arm already relies on without a name check here.
	if agg != nil {
		if vs, ok := e.(*parser.VectorSelector); ok {
			if vs.Name == "" || strings.HasSuffix(vs.Name, bucketSuffix) || s.IsExpHistogramMetric(vs.Name) {
				return histogramAggShape{
					selector:    vs,
					windowRange: instantLookback,
					windowFn:    "",
					agg:         agg,
				}, true
			}
		}
	}

	// Inner must be a rate/increase call over a MatrixSelector.
	call, ok := e.(*parser.Call)
	if !ok {
		return histogramAggShape{}, false
	}
	switch call.Func.Name {
	case "rate", "increase", "sum_over_time":
		// Supported range-vector functions on the histogram-array path.
		// `histogram_quantile` is SCALE-INVARIANT — it interpolates the
		// ratio of cumulative bucket counts, so it yields the identical
		// quantile whether the per-`le` window aggregate is a per-second
		// `rate`, a windowed `increase`, or a windowed `sum_over_time`.
		// `sum_over_time(<base>_bucket[w])` is the canonical aggregation for
		// DELTA-temporality histograms (each point is a per-window delta;
		// summing the deltas over the window is the total per `le`), so
		// `histogram_quantile(phi, sum by(le)(sum_over_time(<base>_bucket[w])))`
		// — the shape GCP / OTel delta-histogram dashboards emit — must
		// resolve, not collapse to the empty non-bucket fallback.
	default:
		return histogramAggShape{}, false
	}
	if len(call.Args) != 1 {
		return histogramAggShape{}, false
	}
	ms, ok := peelWrappers(call.Args[0]).(*parser.MatrixSelector)
	if !ok {
		return histogramAggShape{}, false
	}
	vs, ok := peelWrappers(ms.VectorSelector).(*parser.VectorSelector)
	if !ok {
		return histogramAggShape{}, false
	}
	return histogramAggShape{
		selector:    vs,
		windowRange: ms.Range,
		windowFn:    call.Func.Name,
		agg:         agg,
	}, true
}

// lowerHistogramQuantileAgg builds the chplan tree for
// `histogram_quantile(phi, sum [by/without] (rate(<sel>[range])))`
// against the OTel-CH classic-histogram table.
//
// The shape of the produced tree:
//
//	Project [Sample-row contract]
//	  HistogramQuantile phi=phi, cumulative, groupBy=[Attributes]
//	    Project [Attributes, monotonic(ladder) AS BucketCounts, ExplicitBounds]
//	      Project [Attributes (rebuilt from gkeys), merged ladder, union bounds]
//	        Aggregate groupBy=[<user labels>] funcs=[groupArray(ExplicitBounds), groupArray(BucketCounts)]
//	          Filter <metric matchers> AND TimeUnix in (anchor-Range, anchor]
//	            Scan(otel_metrics_histogram)
//
// When `agg` is nil (bare `rate(...)` with no surrounding `sum`),
// the Aggregate groups by the full Attributes map (preserving per-series
// identity), and the inner Project re-surfaces Attributes as-is.
//
// The `le` label is silently dropped from any user-supplied `by(...)`
// grouping because OTel-CH classic histograms never carry an `le`
// label — the bucket distribution lives in the parallel arrays. So
// `sum by(le)(...)` collapses to a single group, while
// `sum by(le, job)(...)` groups by `job` alone.
//
// For `sum without (k1, k2, ...)`, the standard MapWithoutKeys group
// expression is used (le is not special; if the user lists it, the
// downstream `mapFilter` simply removes a non-existent key).
func lowerHistogramQuantileAgg(shape histogramAggShape, phi phiArg, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector

	// Build the Scan + Filter. The metric-name matcher and any
	// user-supplied label matchers go straight through buildPredicate;
	// the rate's [range] adds the time-bound window. `_bucket` suffix
	// strip mirrors the bare-selector path — see stripBucketSuffix.
	scan := &chplan.Scan{Table: s.HistogramTable}
	pred, leMatchers := histogramQuantileMatcherPredicate(vs.LabelMatchers, s)

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	// Anchor backfill mirrors selectorAnchor: keep the eval anchor sticky
	// to the surrounding query's end timestamp so the time window stays
	// deterministic across calls (matches what timeBoundExpr does for the
	// bare-selector path under hasModifier).
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}
	// Upper bound: TimeUnix <= anchor (Prom's right-closed window).
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	// Lower bound: TimeUnix > anchor - Range (Prom's left-open window).
	if shape.windowRange > 0 {
		pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))
	}

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	input = classicBucketLeRestriction(input, leMatchers, s)

	// Stage 1: reduce each SERIES' in-window samples to one row, applying
	// the range-vector function's own reduction and its per-series sample
	// floor. Without it the stage below would fold across time and series
	// at once — see histogram_quantile_window.go.
	perSeries := classicBucketWindowStage(input, shape, s)

	// Stage 2: the user's aggregation across those per-series rows.
	// GroupBy comes from the surrounding `sum` clause (dropping `le` from
	// by-lists since OTel-CH classic histograms have no per-bucket rows).
	// The bucket aggregates collect every row's layout + counts so the
	// reshape can merge them across layouts — see
	// classicBucketMergeShaping.
	userGroupBy, userAliases, attrsRebuild := histogramAggGroupBy(
		shape.agg, &chplan.ColumnRef{Name: s.AttributesColumn}, s,
	)
	shaping := classicBucketMergeShaping(shape.classicFold, s)
	agg := &chplan.Aggregate{
		Input:              perSeries,
		GroupBy:            userGroupBy,
		GroupByAliases:     userAliases,
		AggFuncs:           shaping.aggs,
		DropEmptyOnNoGroup: true,
	}

	// Inner Projects re-shape the aggregate output back into the
	// histogram-row contract HistogramQuantile expects: an Attributes
	// column (rebuilt from the gkey aliases) plus the merged
	// BucketCounts + ExplicitBounds pair.
	rebuilt, cumulative := shaping.reshape(agg, []chplan.Projection{
		{Expr: attrsRebuild, Alias: s.AttributesColumn},
	}, s)

	hq := &chplan.HistogramQuantile{
		Input:                  rebuilt,
		Phi:                    phi.lit,
		PhiExpr:                phi.expr,
		BucketCountsColumn:     s.BucketCountsColumn,
		ExplicitBoundsColumn:   s.ExplicitBoundsColumn,
		BucketCountsCumulative: cumulative,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Final Sample-row wrapping, same as the bare-selector path.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: chplan.NowNano(), Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// Aliases for the classic-histogram layout merge. The `_hq_` prefix keeps
// them out of the user-label namespace, matching the native-histogram
// merge aliases further down this file.
const (
	// hqAggBoundsListAlias / hqAggCountsListAlias hold the group's
	// groupArray of each row's ExplicitBounds / BucketCounts array.
	hqAggBoundsListAlias = "_hq_bounds_list"
	hqAggCountsListAlias = "_hq_counts_list"
	// hqAggLadderAlias holds the merged, not-yet-repaired cumulative
	// ladder. It gets its own Project layer because the monotonic repair
	// reads the ladder twice and inlining it would square the SQL.
	hqAggLadderAlias = "_hq_ladder"
)

// Lambda parameter names for the layout-merge expressions. `u` is the
// union bound currently being folded; `bs` / `cs` are one row's
// ExplicitBounds / BucketCounts; `b` / `c` are one bucket's bound and
// count inside a row; `v` / `pbs` pair a row's cumulative count with the
// bounds array it came from; `i` indexes the ladder during the repair.
const (
	paramUnionBound  = "u"
	paramRowBounds   = "bs"
	paramRowCounts   = "cs"
	paramBucketBound = "b"
	paramBucketCount = "c"
	paramRowCum      = "v"
	paramRowLayout   = "pbs"
	paramLadderIdx   = "i"
	// paramSortedRungs / paramQuantileRank are promQuantileRungFold's
	// single-evaluation bindings: one rung group sorted ascending, and
	// the interpolation rank `phi * (n - 1)` over it.
	paramSortedRungs  = "qs"
	paramQuantileRank = "qr"
)

// classicBucketShaping bundles a classic-histogram group's bucket
// aggregates with the reshape they owe the HistogramQuantile node.
type classicBucketShaping struct {
	aggs []chplan.AggFunc
	// fold is nil when the aggregate already surfaces BucketCounts +
	// ExplicitBounds by name (the argMax newest-row bare-selector path,
	// which picks ONE row per group and so has no layouts to merge).
	// Non-nil selects the layout-merge reshape and names the per-rung
	// reduction it applies.
	fold classicBucketRungFold
}

// classicBucketMergeShaping is the shaping for the aggregated classic
// paths: collect every row's layout and counts, then merge them.
func classicBucketMergeShaping(fold classicBucketRungFold, s schema.Metrics) classicBucketShaping {
	return classicBucketShaping{
		aggs: []chplan.AggFunc{
			{
				Name:  "groupArray",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ExplicitBoundsColumn}},
				Alias: hqAggBoundsListAlias,
			},
			{
				Name:  "groupArray",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.BucketCountsColumn}},
				Alias: hqAggCountsListAlias,
			},
		},
		fold: fold,
	}
}

// reshape wraps a grouped node in the Projects that surface the
// histogram-row contract (ExplicitBoundsColumn + BucketCountsColumn),
// carrying `passthrough` through every layer it adds. It reports whether
// the resulting BucketCounts is an already-cumulative ladder, which the
// caller hands to chplan.HistogramQuantile.BucketCountsCumulative.
func (sh classicBucketShaping) reshape(
	group chplan.Node,
	passthrough []chplan.Projection,
	s schema.Metrics,
) (chplan.Node, bool) {
	if sh.fold == nil {
		projections := make([]chplan.Projection, 0, len(passthrough)+2)
		projections = append(projections, passthrough...)
		projections = append(
			projections,
			chplan.Projection{Expr: &chplan.ColumnRef{Name: s.BucketCountsColumn}, Alias: s.BucketCountsColumn},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
		)
		return &chplan.Project{Input: group, Projections: projections}, false
	}

	// Layer 1: the merged layout (union of every row's bounds) and the
	// per-`le` ladder folded across the group's rows.
	merged := make([]chplan.Projection, 0, len(passthrough)+2)
	merged = append(merged, passthrough...)
	merged = append(
		merged,
		chplan.Projection{Expr: classicBucketMergedLadderExpr(sh.fold), Alias: hqAggLadderAlias},
		chplan.Projection{Expr: classicBucketUnionBoundsExpr(), Alias: s.ExplicitBoundsColumn},
	)

	// Layer 2: Prometheus's ensureMonotonicAndIgnoreSmallDeltas over that
	// ladder, aliased into the BucketCounts slot HistogramQuantile reads.
	repaired := make([]chplan.Projection, 0, len(passthrough)+2)
	for _, p := range passthrough {
		repaired = append(repaired, chplan.Projection{Expr: &chplan.ColumnRef{Name: p.Alias}, Alias: p.Alias})
	}
	repaired = append(
		repaired,
		chplan.Projection{Expr: classicBucketMonotonicLadderExpr(), Alias: s.BucketCountsColumn},
		chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ExplicitBoundsColumn}, Alias: s.ExplicitBoundsColumn},
	)

	return &chplan.Project{
		Input:       &chplan.Project{Input: group, Projections: merged},
		Projections: repaired,
	}, true
}

// classicBucketUnionBoundsExpr renders the merged bucket layout: every
// distinct upper bound any row in the group carried, ascending.
//
// This is the output layout, and it is what makes the merged quantile
// match Prometheus. Prometheus never sees a "layout" at all — a classic
// histogram reaches it as one float series per `le`, so `sum by(le)`
// over two producers with different boundaries yields the UNION of their
// `le` values, each rung summed over whichever series actually reported
// it. Keying the group on the layout instead (one output row per layout)
// answers a question nobody asked: the caller asked for one series per
// group, not one per boundary set.
func classicBucketUnionBoundsExpr() chplan.Expr {
	return &chplan.FuncCall{Name: "arraySort", Args: []chplan.Expr{
		&chplan.FuncCall{Name: "arrayDistinct", Args: []chplan.Expr{
			&chplan.FuncCall{Name: "arrayFlatten", Args: []chplan.Expr{
				&chplan.ColumnRef{Name: hqAggBoundsListAlias},
			}},
		}},
	}}
}

// classicBucketMergedLadderExpr renders the group's cumulative per-`le`
// ladder over the merged layout: one rung per union bound, plus the
// trailing +Inf rung, each reduced across the group's rows by `fold`.
//
// The merge happens in the CUMULATIVE domain because that is the only
// domain in which rows carrying different layouts can be combined at all.
// A per-bucket count is meaningless without the bounds array it indexes —
// position i of a `[0.1, 0.5, 1]` row and position i of a `[1, 10, 100]`
// row measure different things — whereas a cumulative count at a bound is
// a self-contained quantity: "observations at or below u". That is
// exactly Prometheus's own representation (one already-cumulative float
// per `le`), so folding there reproduces its answer rung for rung.
//
// Per union bound u:
//
//	fold(arrayFilter((v, pbs) -> has(pbs, u),
//	     arrayMap((bs, cs) -> <cumulative count at u>, BL, CL), BL))
//
// The arrayFilter is load-bearing, not an optimisation: a row whose
// layout does not contain u contributes NO `{le=u}` series in
// Prometheus's model, so it must be absent from the reduction rather than
// contribute a zero. Under `sum` the distinction is invisible; under
// `min` / `avg` / `count` it is the whole answer.
//
// The +Inf rung folds over every row unconditionally — the overflow
// bucket exists in every layout, so `{le="+Inf"}` is reported by all of
// them.
// classicBucketRowCumulativeExpr renders ONE row's cumulative count at
// the union bound bound to paramUnionBound: the sum of every bucket whose
// upper bound is <= it. The row's layout and counts are bound to
// paramRowBounds / paramRowCounts by the caller's lambda.
//
// BucketCounts runs one element longer than ExplicitBounds (the trailing
// +Inf overflow), so the pairing slices that rung off — it belongs to no
// finite bound.
//
// Shared by both merge stages: the per-series window reduction and the
// across-series aggregation read a row's cumulative count identically,
// and the two would silently disagree about the +Inf slice if each kept
// its own copy.
func classicBucketRowCumulativeExpr() chplan.Expr {
	return &chplan.FuncCall{Name: "arraySum", Args: []chplan.Expr{
		&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramBucketBound, paramBucketCount},
				Body: &chplan.FuncCall{Name: "if", Args: []chplan.Expr{
					&chplan.Binary{
						Op:    chplan.OpLe,
						Left:  &chplan.BareIdent{Name: paramBucketBound},
						Right: &chplan.BareIdent{Name: paramUnionBound},
					},
					// The ladder is a FLOAT domain throughout. Stored
					// counts are unsigned, and the per-series stage
					// differences its ladder into per-bucket counts, so
					// leaving them unsigned makes a legitimate negative
					// difference underflow — and makes CH infer a
					// Variant(Int64, UInt64) that no array aggregate
					// accepts. Prometheus's classic buckets are float
					// samples anyway.
					&chplan.FuncCall{
						Name: "toFloat64",
						Args: []chplan.Expr{&chplan.BareIdent{Name: paramBucketCount}},
					},
					&chplan.LitFloat{V: 0},
				}},
			},
			&chplan.BareIdent{Name: paramRowBounds},
			&chplan.FuncCall{Name: "arraySlice", Args: []chplan.Expr{
				&chplan.BareIdent{Name: paramRowCounts},
				&chplan.LitInt{V: 1},
				&chplan.FuncCall{Name: "length", Args: []chplan.Expr{&chplan.BareIdent{Name: paramRowBounds}}},
			}},
		}},
	}}
}

// classicBucketRowTotalExpr renders ONE row's +Inf rung — its total
// observation count — from the row's counts array bound to
// paramRowCounts. Float64 for the same reason
// classicBucketRowCumulativeExpr is.
func classicBucketRowTotalExpr() chplan.Expr {
	return &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{
		&chplan.FuncCall{
			Name: "arraySum",
			Args: []chplan.Expr{&chplan.BareIdent{Name: paramRowCounts}},
		},
	}}
}

func classicBucketMergedLadderExpr(fold classicBucketRungFold) chplan.Expr {
	boundsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggBoundsListAlias})
	countsList := chplan.Expr(&chplan.ColumnRef{Name: hqAggCountsListAlias})

	rowCumulativeAtBound := classicBucketRowCumulativeExpr()

	contributingRungs := &chplan.FuncCall{Name: "arrayFilter", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowCum, paramRowLayout},
			Body: &chplan.FuncCall{Name: "has", Args: []chplan.Expr{
				&chplan.BareIdent{Name: paramRowLayout},
				&chplan.BareIdent{Name: paramUnionBound},
			}},
		},
		&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramRowBounds, paramRowCounts},
				Body:   rowCumulativeAtBound,
			},
			boundsList,
			countsList,
		}},
		boundsList,
	}}

	infRungs := &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramRowCounts},
			Body:   classicBucketRowTotalExpr(),
		},
		countsList,
	}}

	return &chplan.FuncCall{Name: "arrayConcat", Args: []chplan.Expr{
		&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{paramUnionBound}, Body: fold(contributingRungs)},
			classicBucketUnionBoundsExpr(),
		}},
		&chplan.FuncCall{Name: "array", Args: []chplan.Expr{fold(infRungs)}},
	}}
}

// classicBucketMonotonicLadderExpr renders Prometheus's
// ensureMonotonicAndIgnoreSmallDeltas (promql/quantile.go) over the
// merged ladder in hqAggLadderAlias, as a prefix maximum.
//
// The repair is mandatory here, not defensive. A ladder accumulated from
// non-negative per-bucket counts is non-decreasing by construction, but
// this one is not accumulated — each rung is folded independently over
// whichever rows reported that bound, so a bound only one producer
// carries can sit BELOW the rung beneath it. The mixed-layout case makes
// it concrete: bounds [1,2,3] with counts [10,0,0,0] and bounds
// [100,200,300] with counts [0,0,0,10] sum to the ladder
// [10,10,10,0,0,0,20], which the repair lifts to [10,10,10,10,10,10,20] —
// exactly what upstream produces for the same two series.
//
// Upstream also smooths deltas within a 1e-12 relative tolerance, which
// exists to absorb float error accumulated by ITS callers' repeated
// additions; the prefix maximum already subsumes every case where that
// smoothing changes a value (it can only raise a dip back to the running
// maximum), so there is nothing left for a tolerance to do.
func classicBucketMonotonicLadderExpr() chplan.Expr {
	return classicBucketMonotonicExpr(&chplan.ColumnRef{Name: hqAggLadderAlias})
}

// classicBucketMonotonicExpr is the repair itself over an arbitrary ladder
// expression — a prefix maximum, so every rung is at least as high as the
// one below it. Both cumulative-input producers owe it, and both derive
// their rungs independently rather than by accumulating non-negative
// counts, so both can hand up a ladder that dips.
func classicBucketMonotonicExpr(ladder chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramLadderIdx},
			Body: &chplan.FuncCall{Name: "arrayMax", Args: []chplan.Expr{
				&chplan.FuncCall{Name: "arraySlice", Args: []chplan.Expr{
					ladder,
					&chplan.LitInt{V: 1},
					&chplan.BareIdent{Name: paramLadderIdx},
				}},
			}},
		},
		&chplan.FuncCall{Name: "arrayEnumerate", Args: []chplan.Expr{ladder}},
	}}
}

// histogramAggGroupBy translates the user's `sum [by/without]` clause
// into the chplan.Aggregate.GroupBy + GroupByAliases + the
// Attributes-rebuild expression for the wrapping Project.
//
// agg == nil collapses to a single-group aggregation (the user only
// wrote `rate(...)` with no `sum` wrapper) — still useful because the
// rate's time window still applies.
//
// `identity` is the expression every key is derived from — the series'
// Prometheus-visible label set. It is a parameter rather than a fixed
// [histogramIdentityExpr] because this runs as the SECOND stage of the
// classic-bucket lowering, over the per-series stage's output, where the
// label set is the already-canonicalised Attributes COLUMN that stage
// projected rather than the raw table columns it was built from. Passing
// the raw-column expression there would reference columns the per-series
// grouping has already consumed.
//
// When the caller does pass the raw-column expression, that is a
// series-identity binding site: the whole-Map shapes go through
// [canonicalGroupKeyExpr]. See its doc for why the histogram paths bind
// their own keys instead of routing through the selector projection. A
// key derived from an earlier stage's aliased output inherits that
// canonicalisation and must not be re-wrapped.
func histogramAggGroupBy(agg *parser.AggregateExpr, identity chplan.Expr, s schema.Metrics) ([]chplan.Expr, []string, chplan.Expr) {
	if agg == nil {
		// `histogram_quantile(phi, rate(metric[5m]))` — group by series
		// identity so each series gets its own bucket-rate vector.
		return []chplan.Expr{identity},
			[]string{"gkey_0"},
			&chplan.ColumnRef{Name: "gkey_0"}
	}
	if agg.Without {
		// `sum without (...)` — single group key derived from
		// mapFilter on Attributes. `le` doesn't exist in OTel-CH but
		// listing it is harmless (no-op key removal). Empty `without ()`
		// is the degenerate "remove nothing" shape: group by the full
		// Attributes map directly (CH rejects mapFilter with an empty
		// IN list as a syntax error).
		if len(agg.Grouping) == 0 {
			return []chplan.Expr{identity},
				[]string{"gkey_0"},
				&chplan.ColumnRef{Name: "gkey_0"}
		}
		return []chplan.Expr{
				&chplan.MapWithoutKeys{
					Map:  identity,
					Keys: append([]string(nil), agg.Grouping...),
				},
			},
			[]string{"gkey_0"},
			&chplan.ColumnRef{Name: "gkey_0"}
	}
	// `sum by (...)` — drop `le` from the user's list (the bucket
	// distribution lives in the array, not in an Attributes key).
	labels := dropLabel(agg.Grouping, bucketBoundLabel)
	if len(labels) == 0 {
		// Either `sum by()` or `sum by(le)` — collapse to a single
		// group and project an empty Attributes map. This is the same
		// path emptyAttrsMap takes in wrapAggregateForSample.
		return nil, nil, emptyAttrsMap()
	}
	groupBy := make([]chplan.Expr, len(labels))
	aliases := make([]string, len(labels))
	mapArgs := make([]chplan.Expr, 0, len(labels)*2)
	for i, label := range labels {
		alias := fmt.Sprintf("gkey_%d", i)
		groupBy[i] = attributeLookupExpr(identity, label)
		aliases[i] = alias
		mapArgs = append(
			mapArgs,
			&chplan.LitString{V: label},
			&chplan.ColumnRef{Name: alias},
		)
	}
	attrs := &chplan.FuncCall{Name: "map", Args: mapArgs}
	return groupBy, aliases, attrs
}

// dropLabel returns a copy of labels with every occurrence of `name`
// removed. Used to strip `le` from PromQL `by(...)` lists on the
// histogram-aggregation path (cerberus's classic histograms have no
// per-bucket rows, so `le` cannot be a real grouping key).
func dropLabel(labels []string, name string) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == name {
			continue
		}
		out = append(out, l)
	}
	return out
}

// andExpr returns `a AND b`. Either operand may be nil, in which case
// the other is returned unchanged. nil + nil → nil.
func andExpr(a, b chplan.Expr) chplan.Expr {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: a, Right: b}
}

// lowerHistogramQuantileNative builds the chplan.HistogramQuantileNative
// IR for the exp-histogram path. Mirrors the classic-path scaffold:
// Scan or Filter against the exp-histogram table, then wrap in a
// Project to satisfy the Sample-row contract downstream.
//
// This is the instant-mode entry point — query_range traffic dispatches
// to lowerHistogramQuantileNativeBareRange (bare selector) or
// lowerHistogramQuantileNativeAggRange (aggregated idiom), which fan
// the same quantile interpolation across a StepGrid + per-anchor
// lookback window. The instant lowering keeps `TimeUnix = now64(9)`
// because instant queries have a single evaluation anchor.
func lowerHistogramQuantileNative(vs *parser.VectorSelector, phi phiArg, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)
	pred, err := andInstantWindow(pred, vs, s.TimestampColumn, ctx)
	if err != nil {
		return nil, err
	}
	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      latestSampleAgg(input, nativeExpHistLatestAggs(s), s),
		Phi:                        phi.lit,
		PhiExpr:                    phi.expr,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: chplan.NowNano(), Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// Aliases used by lowerHistogramQuantileNativeAgg to thread per-row
// exp-histogram fields through the Aggregate → Project stack. The
// `_hq_` prefix avoids collision with user-supplied labels (Prom's
// `__name__` is reserved; cerberus's gkey aliases use `gkey_<n>`;
// nothing else writes `_hq_*` columns).
const (
	hqAggMergedScaleAlias     = "_hq_merged_scale"
	hqAggScalesArrayAlias     = "_hq_scales"
	hqAggPosOffsetsArrayAlias = "_hq_pos_offsets"
	hqAggPosBucketsArrayAlias = "_hq_pos_buckets"
	hqAggNegOffsetsArrayAlias = "_hq_neg_offsets"
	hqAggNegBucketsArrayAlias = "_hq_neg_buckets"
)

// lowerHistogramQuantileNativeAgg builds the chplan tree for
// `histogram_quantile(phi, sum [by/without] (rate(<sel>_exp_hist[range])))`
// against the OTel-CH exponential (native) histogram table.
//
// The shape of the produced tree mirrors lowerHistogramQuantileAgg's
// classic-histogram counterpart, but the inner Project does the
// per-row exp-histogram merge (scale-fold + offset-align + zero-pad)
// before HistogramQuantileNative walks the merged distribution:
//
//	Project [Sample-row contract]
//	  HistogramQuantileNative phi=phi, groupBy=[Attributes]
//	    Project [Attributes (rebuilt from gkeys), Scale, ZeroCount, ZeroThreshold,
//	             PositiveOffset, PositiveBucketCounts,
//	             NegativeOffset, NegativeBucketCounts]
//	      Aggregate groupBy=[<user labels>] funcs=[
//	          min(Scale)                       AS _hq_merged_scale,
//	          sum(ZeroCount)                   AS ZeroCount,
//	          max(ZeroThreshold)               AS ZeroThreshold,
//	          groupArray(Scale)                AS _hq_scales,
//	          groupArray(PositiveOffset)       AS _hq_pos_offsets,
//	          groupArray(PositiveBucketCounts) AS _hq_pos_buckets,
//	          groupArray(NegativeOffset)       AS _hq_neg_offsets,
//	          groupArray(NegativeBucketCounts) AS _hq_neg_buckets,
//	      ]
//	        Filter <metric matchers> AND TimeUnix in (anchor-Range, anchor]
//	          Scan(otel_metrics_exponential_histogram)
//
// The merge algorithm in the inner Project (see
// expHistogramMergeOffsetExpr + expHistogramMergeBucketsExpr) mirrors
// Prometheus's FloatHistogram.Add semantics:
//
//   - Scale folding: per-row downscale to min(Scale) via the canonical
//     "absolute bucket idx >> (origScale - targetScale)" mapping
//     (model/histogram/float_histogram.go § targetIdx). Uniform-Scale
//     groups (the common case) collapse to identity since delta = 0.
//
//   - Offset alignment: each row's downscaled bucket array contributes
//     to the merged array starting at "PositiveOffset >> delta"
//     (absolute bucket index at merged scale). The merged array spans
//     [arrayMin(downscaled_offset), arrayMax(downscaled_offset+downscaled_length))
//     across rows, zero-padding rows that don't cover the full range.
//
//   - ZeroCount sums trivially; ZeroThreshold takes the max across
//     rows (the merged zero bucket spans the largest individual zero
//     bucket).
//
// The `le` label is silently dropped from any user-supplied `by(...)`
// grouping on the native path too (cerberus's exp histograms never
// carry an `le` label — the bucket distribution lives in the
// PositiveBucketCounts array with PositiveOffset shifting the
// starting absolute index per series). Mirrors classic-agg's behaviour.
func lowerHistogramQuantileNativeAgg(shape histogramAggShape, phi phiArg, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	vs := shape.selector

	// Build the Scan + Filter. Same shape as the classic-agg path: the
	// metric-name + label matchers go through buildPredicate; the
	// rate's [range] adds the time-bound window.
	scan := &chplan.Scan{Table: s.ExpHistogramTable}
	pred := buildPredicate(vs.LabelMatchers, s)

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	if anchor.End.IsZero() && !ctx.end.IsZero() {
		anchor.End = ctx.end.UTC()
	}
	pred = andExpr(pred, timeBoundExpr(s.TimestampColumn, anchor))
	if shape.windowRange > 0 {
		pred = andExpr(pred, stalenessLowerBoundExpr(s.TimestampColumn, anchor, shape.windowRange))
	}

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}

	// Aggregate: collect per-row exp-histogram fields into groupArrays so
	// the wrapping Project can fold them into a single merged distribution.
	// The simple aggregates (min Scale, sum ZeroCount, max ZeroThreshold)
	// land on the same aggregate so the wrapping Project can refer to
	// them by alias.
	// Single-stage: the native merge folds across time and series at
	// once, so its keys bind straight to the raw table columns. Splitting
	// it into a per-series stage the way the classic path now does is
	// tracked in #1629.
	groupBy, groupByAliases, attrsRebuild := histogramAggGroupBy(shape.agg, histogramIdentityExpr(s), s)
	aggFuncs := []chplan.AggFunc{
		{Name: "min", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggMergedScaleAlias},
		{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroCountColumn}}, Alias: s.ZeroCountColumn},
	}
	// max(ZeroThreshold) only exists when the physical schema persists
	// the OTLP zero_threshold field — the upstream OTel-CH DDL doesn't,
	// so the default schema leaves the column empty and the emitter
	// renders a constant-0 zero-bucket width.
	if s.ZeroThresholdColumn != "" {
		aggFuncs = append(aggFuncs, chplan.AggFunc{Name: "max", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ZeroThresholdColumn}}, Alias: s.ZeroThresholdColumn})
	}
	aggFuncs = append(aggFuncs, []chplan.AggFunc{
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ScaleColumn}}, Alias: hqAggScalesArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveOffsetColumn}}, Alias: hqAggPosOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.PositiveBucketCountsColumn}}, Alias: hqAggPosBucketsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeOffsetColumn}}, Alias: hqAggNegOffsetsArrayAlias},
		{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.NegativeBucketCountsColumn}}, Alias: hqAggNegBucketsArrayAlias},
	}...)
	agg := &chplan.Aggregate{
		Input:              input,
		GroupBy:            groupBy,
		GroupByAliases:     groupByAliases,
		AggFuncs:           aggFuncs,
		DropEmptyOnNoGroup: true,
	}

	// Inner Project re-shapes the aggregate output into the exp-histogram
	// row contract HistogramQuantileNative expects: Attributes (rebuilt
	// from gkeys) + the merged Scale / ZeroCount / ZeroThreshold +
	// the folded {Positive,Negative}{Offset,BucketCounts}.
	rebuiltProjs := []chplan.Projection{
		{Expr: attrsRebuild, Alias: s.AttributesColumn},
		{Expr: &chplan.ColumnRef{Name: hqAggMergedScaleAlias}, Alias: s.ScaleColumn},
		{Expr: &chplan.ColumnRef{Name: s.ZeroCountColumn}, Alias: s.ZeroCountColumn},
	}
	if s.ZeroThresholdColumn != "" {
		rebuiltProjs = append(rebuiltProjs, chplan.Projection{Expr: &chplan.ColumnRef{Name: s.ZeroThresholdColumn}, Alias: s.ZeroThresholdColumn})
	}
	rebuiltProjs = append(rebuiltProjs, []chplan.Projection{
		{Expr: expHistogramMergeOffsetExpr(hqAggPosOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveOffsetColumn},
		{Expr: expHistogramMergeBucketsExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.PositiveBucketCountsColumn},
		{Expr: expHistogramMergeOffsetExpr(hqAggNegOffsetsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeOffsetColumn},
		{Expr: expHistogramMergeBucketsExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, hqAggScalesArrayAlias, hqAggMergedScaleAlias), Alias: s.NegativeBucketCountsColumn},
	}...)
	rebuilt := &chplan.Project{
		Input:       agg,
		Projections: rebuiltProjs,
	}

	hq := &chplan.HistogramQuantileNative{
		Input:                      rebuilt,
		Phi:                        phi.lit,
		PhiExpr:                    phi.expr,
		ScaleColumn:                s.ScaleColumn,
		ZeroCountColumn:            s.ZeroCountColumn,
		ZeroThresholdColumn:        s.ZeroThresholdColumn,
		PositiveOffsetColumn:       s.PositiveOffsetColumn,
		PositiveBucketCountsColumn: s.PositiveBucketCountsColumn,
		NegativeOffsetColumn:       s.NegativeOffsetColumn,
		NegativeBucketCountsColumn: s.NegativeBucketCountsColumn,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: s.AttributesColumn},
		},
		GroupByAliases:   []string{s.AttributesColumn},
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
	}

	// Final Sample-row wrapping, same as the bare-selector / classic-agg paths.
	return &chplan.Project{
		Input: hq,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: chplan.NowNano(), Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// expHistogramMergeOffsetExpr renders the merged PositiveOffset (or
// NegativeOffset) for a group of native-histogram rows: the minimum of
// per-row downscaled-to-merged-scale offsets.
//
// Emitted CH expression:
//
//	arrayMin(arrayMap((s, off) -> bitShiftRight(off, s - <mergedScale>),
//	                   <scalesArr>, <offArr>))
//
// CH's bitShiftRight on signed Int32 performs arithmetic right shift,
// matching Prometheus's "(idx >> delta)" semantics for negative bucket
// indices (sub-1 latencies). When all rows share Scale the delta is 0
// for every row, so the shift is identity and the merged offset
// reduces to arrayMin(offArr) — identical to classic-histogram
// min-offset semantics.
func expHistogramMergeOffsetExpr(offArrAlias, scalesArrAlias, mergedScaleAlias string) chplan.Expr {
	return &chplan.FuncCall{
		Name: "arrayMin",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{"s", "off"},
						Body: &chplan.FuncCall{
							Name: "bitShiftRight",
							Args: []chplan.Expr{
								&chplan.BareIdent{Name: "off"},
								&chplan.Binary{
									Op:    chplan.OpSub,
									Left:  &chplan.BareIdent{Name: "s"},
									Right: &chplan.ColumnRef{Name: mergedScaleAlias},
								},
							},
						},
					},
					&chplan.ColumnRef{Name: scalesArrAlias},
					&chplan.ColumnRef{Name: offArrAlias},
				},
			},
		},
	}
}

// expHistogramMergeBucketsExpr renders the merged PositiveBucketCounts
// (or NegativeBucketCounts) for a group of native-histogram rows: a
// scale-folded, offset-aligned, zero-padded, element-wise sum.
//
// Algorithm: for each target absolute bucket index `T` in
// [merged_offset, merged_offset + merged_length), the merged value is
//
//	Σ_{row i} arraySum(arrayMap(j ->
//	    if((off_i + j - 1) >> delta_i == T, arr_i[j], 0),
//	    arrayEnumerate(arr_i)))
//
// where delta_i = scales_arr[i] - merged_scale (per-row downscale
// distance), j is 1-based (array position inside row i's bucket
// array), and (off_i + j - 1) is the absolute bucket index of position
// j at row i's original scale.
//
// merged_length is computed as
//
//	max((off_i + length(arr_i) - 1) >> delta_i) -
//	    min(off_i >> delta_i) + 1
//
// across rows. Rows with empty bucket arrays contribute zero to every
// target position (no `j` in arrayEnumerate of empty array).
func expHistogramMergeBucketsExpr(offArrAlias, bucArrAlias, scalesArrAlias, mergedScaleAlias string) chplan.Expr {
	const paramT = "t"

	mergedScale := &chplan.ColumnRef{Name: mergedScaleAlias}
	scalesArr := &chplan.ColumnRef{Name: scalesArrAlias}
	offArr := &chplan.ColumnRef{Name: offArrAlias}
	bucArr := &chplan.ColumnRef{Name: bucArrAlias}

	mergedStart, mergedLength := expHistogramMergeBucketsBoundsExpr(scalesArr, offArr, bucArr, mergedScale)
	rowsSum := expHistogramMergeBucketsRowsSumExpr(scalesArr, offArr, bucArr, mergedScale, mergedStart, paramT)

	// Outer: arrayMap(t -> rowsSum, range(toUInt64(mergedLength))).
	// `t` is 0-based; the inner expression reconstructs the absolute
	// target index as mergedStart + t. CH's `range(N)` produces
	// [0, N) over UInt64; toUInt64 keeps the cast explicit so the SQL
	// parses cleanly even when mergedLength is computed from signed
	// values.
	return &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramT},
				Body:   rowsSum,
			},
			&chplan.FuncCall{
				Name: "range",
				Args: []chplan.Expr{
					&chplan.FuncCall{Name: "toUInt64", Args: []chplan.Expr{mergedLength}},
				},
			},
		},
	}
}

// expHistogramMergeBucketsBoundsExpr builds (mergedStart, mergedLength)
// for the bucket-merge expression. Returned mergedStart is the
// arrayMin of per-row downscaled offsets; mergedLength is
// greatest(0, mergedEnd - mergedStart + 1), clamped so an all-empty
// group produces a zero-length output array.
func expHistogramMergeBucketsBoundsExpr(scalesArr, offArr, bucArr, mergedScale chplan.Expr) (mergedStart, mergedLength chplan.Expr) {
	const (
		paramScalesInner = "sm"
		paramOffInner    = "om"
		paramArrInner    = "am"
	)

	// per-row downscaled start: arrayMap((sm, om) -> bitShiftRight(om, sm - merged_scale), scalesArr, offArr)
	downscaledStarts := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramScalesInner, paramOffInner},
				Body: &chplan.FuncCall{
					Name: "bitShiftRight",
					Args: []chplan.Expr{
						&chplan.BareIdent{Name: paramOffInner},
						&chplan.Binary{
							Op:    chplan.OpSub,
							Left:  &chplan.BareIdent{Name: paramScalesInner},
							Right: mergedScale,
						},
					},
				},
			},
			scalesArr,
			offArr,
		},
	}

	// per-row downscaled end: arrayMap((sm, om, am) -> bitShiftRight(om + length(am) - 1, sm - merged_scale), scalesArr, offArr, bucArr).
	// Rows with empty arrays produce (om + 0 - 1) = om - 1 — slightly below their start, which is fine since they contribute nothing.
	downscaledEnds := &chplan.FuncCall{
		Name: "arrayMap",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramScalesInner, paramOffInner, paramArrInner},
				Body: &chplan.FuncCall{
					Name: "bitShiftRight",
					Args: []chplan.Expr{
						&chplan.Binary{
							Op:   chplan.OpAdd,
							Left: &chplan.BareIdent{Name: paramOffInner},
							Right: &chplan.Binary{
								Op:    chplan.OpSub,
								Left:  &chplan.FuncCall{Name: "length", Args: []chplan.Expr{&chplan.BareIdent{Name: paramArrInner}}},
								Right: &chplan.LitInt{V: 1},
							},
						},
						&chplan.Binary{
							Op:    chplan.OpSub,
							Left:  &chplan.BareIdent{Name: paramScalesInner},
							Right: mergedScale,
						},
					},
				},
			},
			scalesArr,
			offArr,
			bucArr,
		},
	}

	mergedStart = &chplan.FuncCall{Name: "arrayMin", Args: []chplan.Expr{downscaledStarts}}
	mergedEnd := &chplan.FuncCall{Name: "arrayMax", Args: []chplan.Expr{downscaledEnds}}
	// merged_length = mergedEnd - mergedStart + 1.
	// Guard the "no rows contribute" case by clamping to 0 via greatest(0, …).
	mergedLength = &chplan.FuncCall{
		Name: "greatest",
		Args: []chplan.Expr{
			&chplan.LitInt{V: 0},
			&chplan.Binary{
				Op: chplan.OpAdd,
				Left: &chplan.Binary{
					Op:    chplan.OpSub,
					Left:  mergedEnd,
					Right: mergedStart,
				},
				Right: &chplan.LitInt{V: 1},
			},
		},
	}
	return mergedStart, mergedLength
}

// expHistogramMergeBucketsRowsSumExpr builds the per-target-bucket
// row-sum used inside the outer arrayMap. For target offset `t`
// (0-based; absolute index = mergedStart + t), it sums every row's
// contribution at that bucket: arraySum over rows of the inner
// arraySum-of-arrayMap that picks bucket[j] when (off + j - 1) >>
// (s - merged_scale) == mergedStart + t, else 0.
func expHistogramMergeBucketsRowsSumExpr(scalesArr, offArr, bucArr, mergedScale, mergedStart chplan.Expr, paramT string) chplan.Expr {
	const (
		paramScale = "s"
		paramOff   = "off"
		paramArr   = "arr"
		paramJ     = "j"
	)

	// Inner-most: for one (s, off, arr) tuple and target absolute index T,
	// arraySum(arrayMap(j -> if(bitShiftRight(off + j - 1, s - merged_scale) = T, arr[j], 0), arrayEnumerate(arr))).
	innerContrib := &chplan.FuncCall{
		Name: "arraySum",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramJ},
						Body: &chplan.FuncCall{
							Name: "if",
							Args: []chplan.Expr{
								&chplan.Binary{
									Op: chplan.OpEq,
									Left: &chplan.FuncCall{
										Name: "bitShiftRight",
										Args: []chplan.Expr{
											&chplan.Binary{
												Op:   chplan.OpAdd,
												Left: &chplan.BareIdent{Name: paramOff},
												Right: &chplan.Binary{
													Op:    chplan.OpSub,
													Left:  &chplan.BareIdent{Name: paramJ},
													Right: &chplan.LitInt{V: 1},
												},
											},
											&chplan.Binary{
												Op:    chplan.OpSub,
												Left:  &chplan.BareIdent{Name: paramScale},
												Right: mergedScale,
											},
										},
									},
									// target absolute index = mergedStart + t (t is 0-based).
									Right: &chplan.Binary{
										Op:    chplan.OpAdd,
										Left:  mergedStart,
										Right: &chplan.BareIdent{Name: paramT},
									},
								},
								&chplan.Subscript{
									Container: &chplan.BareIdent{Name: paramArr},
									Key:       &chplan.BareIdent{Name: paramJ},
								},
								&chplan.LitInt{V: 0},
							},
						},
					},
					&chplan.FuncCall{
						Name: "arrayEnumerate",
						Args: []chplan.Expr{&chplan.BareIdent{Name: paramArr}},
					},
				},
			},
		},
	}

	// Sum over rows. arraySum(arrayMap((s, off, arr) -> innerContrib, scalesArr, offArr, bucArr)).
	return &chplan.FuncCall{
		Name: "arraySum",
		Args: []chplan.Expr{
			&chplan.FuncCall{
				Name: "arrayMap",
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramScale, paramOff, paramArr},
						Body:   innerContrib,
					},
					scalesArr,
					offArr,
					bucArr,
				},
			},
		},
	}
}

// unwrapVectorSelector peels ParenExpr / StepInvariantExpr wrappers off
// the argument and returns the bare VectorSelector if any. Mirrors what
// tryScalarLiteral does for NumberLiteral, but for the vector arg.
func unwrapVectorSelector(e parser.Expr) (*parser.VectorSelector, bool) {
	for {
		switch v := e.(type) {
		case *parser.VectorSelector:
			return v, true
		case *parser.ParenExpr:
			e = v.Expr
		case *parser.StepInvariantExpr:
			e = v.Expr
		default:
			return nil, false
		}
	}
}
