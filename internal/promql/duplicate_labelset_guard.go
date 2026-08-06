package promql

import (
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file carries the INSTANT half of PromQL's duplicate-labelset rule.
// The range half — a name-dropping `*_over_time` / `rate` over a
// multi-name selector — lives in lower.go as [wrapDropNameCollisionGuard],
// wired into lowerRangeVectorCall / lowerSubqueryOverCall.
//
// Upstream does not special-case either half. `rangeEval`
// (prometheus/promql/engine.go) checks EVERY step's output vector for a
// repeated label set and raises `vector cannot contain metrics with the
// same labelset` from one generic site, so every operation that can map
// two distinct input series onto one output label set inherits the check.
// Cerberus lowers each operation into its own SQL shape, so the check has
// to be attached at each shape that can collapse identities — but it is
// the same check, and the two helpers below are the only two ways an
// instant-mode shape can do it:
//
//   - dropping `__name__` (every instant math fn, the clamp family, unary
//     minus, the date fns, scalar⊗vector arithmetic): two series that
//     differed ONLY in their metric name become one label set.
//   - REWRITING the label set (`label_replace`, `label_join`): two series
//     that differed in a source label collapse when the rewrite maps both
//     onto the same destination value.
//
// Both are enforced as a HAVING on a collapsing [chplan.Aggregate] rather
// than as a SELECT-list expression, for the reason spelled out on
// `chplan.Aggregate.Having`: ClickHouse's analyzer prunes a SELECT
// expression nothing downstream reads, `throwIf` side effect included, so
// a column-shaped guard silently never fires.
//
// The Aggregate is also the FIX, not just the detector. Without it the
// name-dropping Project leaves two rows carrying the same label set on the
// wire, and the Prom response format has no shape for that — Grafana shows
// two identically-labelled series. Collapsing is exact rather than lossy
// because the HAVING has already aborted every group that held more than
// one input row, so `any()` over a surviving group reads the only row
// there is.

// guardedValueProjection is the single hoist point for PromQL's
// "derived sample loses `__name__`" rule: it guards the label-set
// collision the drop can create, then projects newValue over the
// surviving rows.
//
// `arg` is the PromQL expression that produced `inner` — the operand the
// name is being dropped from. It is read only to answer "can this operand
// span more than one metric name", which is a question about the QUERY,
// not about the plan: a selector pinned to a single name
// (`ceil(http_requests_total)`) can never collide, so it keeps the
// unguarded, un-aggregated shape it has today.
//
// Every caller that drops the name routes through here rather than
// calling [projectValueOverInner] directly, so a new name-dropping
// lowering inherits the guard by using the same helper its siblings use.
func guardedValueProjection(
	inner chplan.Node,
	arg parser.Expr,
	s schema.Metrics,
	newValue chplan.Expr,
) chplan.Node {
	return projectValueOverInner(guardNameDropCollision(inner, arg, s), s, newValue)
}

// guardKeysOnTimestamp reports whether a collision guard over `inner` must
// put the timestamp column in its group key.
//
// The rule upstream enforces is per EVALUATION STEP: `rangeEval` checks one
// output vector at a time, and a vector is the set of samples that share a
// step. So the guard's group key has to be "whatever identifies a step" —
// and which column that is, is a property of the input node, not of the
// request. Deriving it here rather than reading `lowerCtx.step` is what
// makes the guard correct in all three shapes a lowering can hand it:
//
// The question reduces to a single structural one: has `inner` ALREADY
// been collapsed to one row per series? Only a [chplan.Aggregate] collapses
// rows, so only it can answer no:
//
//   - An Aggregate whose group key does NOT name the timestamp column is
//     the instant-vector seam: one row per (name, label set), each row
//     carrying the series' OWN last sample time. Two series in the same
//     instant vector therefore legitimately hold DIFFERENT timestamps, and
//     keying on the timestamp here would split every colliding pair apart —
//     the guard would never fire. This is the shape a bare selector takes
//     at `/api/v1/query`, where the seam is a `GROUP BY (MetricName,
//     Attributes)` with `max(TimeUnix)` / `argMax(Value, TimeUnix)`.
//   - An Aggregate that DOES group on the timestamp is already per-step,
//     so the timestamp is part of the identity and stays in the key.
//
// Everything else holds many rows per series at distinct timestamps, so the
// timestamp must be in the key:
//
//   - [chplan.RangeLWR] and [chplan.RangeWindowResample] — the two
//     range-mode grid seams. They emit one row per (series, ANCHOR), and
//     every series shares the same anchors, so the timestamp IS the step
//     identity. Keying on it is what makes this a per-step check rather
//     than one that merges a series' own steps into a single row.
//   - a raw sample stream — what a subquery inner sees, since the step grid
//     is applied OUTSIDE the inner expression. Nothing has reduced it yet,
//     so without the timestamp the guard counts a single series' own
//     samples against itself and aborts a correct query.
//
// A request-level `step > 0` test gets the last case wrong: a subquery
// inner lowers with step 0 while carrying a raw stream.
//
// Filter and Project are walked through because they only ever re-shape or
// re-alias columns; the instant seam sits underneath the alias Project that
// renames `lwr_ts` back to the schema timestamp column.
func guardKeysOnTimestamp(inner chplan.Node, s schema.Metrics) bool {
	switch v := inner.(type) {
	case *chplan.Aggregate:
		for _, alias := range v.GroupByAliases {
			if alias == s.TimestampColumn {
				return true
			}
		}
		return false
	case *chplan.Filter:
		return guardKeysOnTimestamp(v.Input, s)
	case *chplan.Project:
		return guardKeysOnTimestamp(v.Input, s)
	}
	return true
}

// guardNameDropCollision re-collapses `inner`'s per-(label set, name) rows
// onto the per-label-set rows the name-dropping caller is about to
// produce, aborting instead when a group was fed by more than one name.
//
// It returns `inner` UNCHANGED when the drop cannot collide, so every
// query shape that was already correct keeps its byte-identical SQL: this
// helper only ever adds a layer to a query that could otherwise return two
// rows under one label set.
//
// The output exposes the label set, the value and the timestamp — every
// column [projectValueOverInner]'s non-RangeWindow branch reads back except
// the metric name, which that branch writes as an empty literal.
// The RangeWindow branch is never reached from here: a RangeWindow has
// already grouped the name away, so [nodeCarriesMetricName] answers false
// for it and the guard is a no-op by construction.
func guardNameDropCollision(inner chplan.Node, arg parser.Expr, s schema.Metrics) chplan.Node {
	if !collidesOnNameDrop(arg, inner, s) {
		return inner
	}

	// The surviving label set is Attributes alone — the name is what is
	// being dropped, so it cannot be part of the output identity.
	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	aliases := []string{s.AttributesColumn}
	aggs := []chplan.AggFunc{
		// `any` is exact, not a pick: the HAVING has already aborted any
		// group that held more than one name, and the selector seam emits
		// one row per (name, label set[, step]), so a surviving group
		// holds exactly one row.
		{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}}, Alias: s.ValueColumn},
	}
	// The metric-name column is deliberately NOT re-exposed. Every caller
	// runs through guardedValueProjection, whose projectValueOverInner
	// writes a literal empty name over this node, so nothing downstream
	// reads it back. Carrying it as `any(MetricName) AS MetricName` would
	// also put an alias of that exact name into the SELECT scope, and
	// ClickHouse resolves a HAVING identifier against the SELECT aliases
	// first: `uniqExact(MetricName)` would bind to the alias and become
	// `uniqExact(any(MetricName))`, rejected with code 184
	// ILLEGAL_AGGREGATION. Omitting it keeps the HAVING on the source
	// column, which is the column whose distinct count the guard is about.

	if guardKeysOnTimestamp(inner, s) {
		groupBy = append(groupBy, &chplan.ColumnRef{Name: s.TimestampColumn})
		aliases = append(aliases, s.TimestampColumn)
	} else {
		// One row per series already, each with its own sample time, so the
		// label set alone is the whole key and the timestamp rides through
		// as a carried value.
		aggs = append(aggs, chplan.AggFunc{
			Name:  "any",
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			Alias: s.TimestampColumn,
		})
	}

	return &chplan.Aggregate{
		Input:          inner,
		GroupBy:        groupBy,
		GroupByAliases: aliases,
		AggFuncs:       aggs,
		Having:         duplicateLabelsetGuardExpr(s),
	}
}

// collidesOnNameDrop reports whether dropping `__name__` from `inner` can
// produce two rows under one label set. It is the instant-shape sibling of
// [rangeFnCollidesOnNameDrop] and asks the same three questions, minus the
// "does this function drop the name" one — every caller of
// [guardedValueProjection] drops it unconditionally.
//
//   - The schema has to HAVE a metric-name column. An override that maps
//     it away leaves no identity to read.
//   - The operand must not be pinned to ONE metric name.
//     `ceil(http_requests_total)` reads a single name, so after the drop
//     every output row still comes from a distinct label set — there is
//     nothing to collide with. Only a regex name matcher
//     (`{__name__=~"a|b"}`) or a selector carrying no name matcher at all
//     (`{job="x"}`) spans several names.
//   - The plan must still expose a real per-series name to compare. An
//     inner that already dropped it (a RangeWindow, an Aggregate, or an
//     earlier guarded projection whose MetricName is the empty literal)
//     has no identity left to guard, and inventing one would be worse than
//     leaving the shape alone. That is also what keeps `ceil(abs(m))` from
//     stacking two guards: `abs`'s output carries `” AS MetricName`, and
//     [nodeCarriesMetricName] reads that as "no name".
func collidesOnNameDrop(arg parser.Expr, inner chplan.Node, s schema.Metrics) bool {
	return s.MetricNameColumn != "" &&
		pinnedMetricName(arg) == "" &&
		nodeCarriesMetricName(inner, s)
}

// pinnedMetricName returns the single metric name e's underlying vector
// selector pins, or "" when it pins none — either because the selector
// matches the name by regex / omits it entirely, or because e is not a
// bare selector at all.
//
// "" is the conservative answer for every caller: it means "this operand
// may span several metric names", which only ever turns the guard ON.
// [collidesOnNameDrop] then requires the plan to actually carry a name
// before anything is emitted, so a false "unpinned" on a shape that lost
// its name already costs nothing.
//
// ParenExpr and StepInvariantExpr are unwrapped because neither changes
// which series the operand reads: `ceil((m))` and `ceil(m @ 100)` pin the
// same one name `ceil(m)` does.
func pinnedMetricName(e parser.Expr) string {
	switch v := e.(type) {
	case *parser.ParenExpr:
		return pinnedMetricName(v.Expr)
	case *parser.StepInvariantExpr:
		return pinnedMetricName(v.Expr)
	case *parser.VectorSelector:
		return metricNameFromMatchers(v.LabelMatchers)
	}
	return ""
}

// guardLabelRewriteCollision wraps a `label_replace` / `label_join`
// rewrite in the same abort-on-collision check, keyed on the identity the
// rewrite PRESERVES.
//
// The difference from the name-drop half is which columns form the output
// identity and therefore what "more than one input row" means. A label
// rewrite keeps `__name__`, so two series with the SAME name and different
// source labels collide the moment the rewrite maps both onto one
// destination value — `label_replace(m, "dst", "x", "src", ".*")` flattens
// every distinct `src` onto the constant `x`. Counting distinct metric
// names would miss that entirely, so the guard counts ROWS per output
// identity instead.
//
// `rewritten` is the Project [projectAttributesOverInner] built, so the
// group key reads the REWRITTEN Attributes — grouping the pre-rewrite
// column would check the wrong label set.
func guardLabelRewriteCollision(rewritten *chplan.Project, s schema.Metrics) chplan.Node {
	if rw, ok := rewritten.Input.(*chplan.RangeWindow); ok && rw.OuterRange > 0 {
		// A MATRIX RangeWindow emits one row per (series, anchor) and
		// publishes the anchor under `anchor_ts`, which
		// api/prom.wrapWithSampleProjection reads back to bucket each
		// series' points. Reaching it means walking the plan root past the
		// Projects above the window (api/prom.isMatrixRangeWindow), and an
		// Aggregate is not one of the nodes that walk crosses — so wrapping
		// this shape in the guard would silently reclassify the query as
		// non-matrix and answer an EMPTY matrix.
		//
		// Guarding this shape needs the classifier to be able to see
		// through a shape-preserving Aggregate, which is a change to the
		// plan-shape contract rather than to this guard. Tracked in #1899.
		return rewritten
	}

	cols := canonicalSampleColumns(s)
	canonical := chplan.ProjectExposesCanonical(rewritten, cols)
	keyOnStep := guardKeysOnTimestamp(rewritten, s)

	groupBy := []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}}
	aliases := []string{s.AttributesColumn}
	aggs := []chplan.AggFunc{
		{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}}, Alias: s.ValueColumn},
	}

	// Which columns the guard has to carry is DERIVED from the rewrite's
	// own projection list rather than assumed, because the list differs by
	// input shape: a canonical rewrite exposes the four Sample columns, a
	// rewrite over a matrix RangeWindow exposes (Attributes, anchor_ts,
	// TimeUnix, Value), and one over an instant RangeWindow exposes only
	// (Attributes, Value). An Aggregate outputs exactly its key plus its
	// aggregates, so any column left out here is DROPPED — and dropping
	// `anchor_ts` silently empties the matrix response, because
	// wrapWithSampleProjection reads that column back to bucket each
	// series' points by anchor.
	for _, proj := range rewritten.Projections {
		switch name := chplan.ProjectionOutputName(proj); name {
		case s.AttributesColumn, s.ValueColumn, "":
			// Already placed above, or a computed projection that names no
			// output column and so cannot be referenced by the wrapper.
		case s.MetricNameColumn:
			// The rewrite preserved the name, so it is part of the output
			// identity and belongs in the key rather than in an `any()`.
			groupBy = append(groupBy, &chplan.ColumnRef{Name: name})
			aliases = append(aliases, name)
		default:
			// A step-identifying column: the schema timestamp, or the
			// RangeWindow per-anchor grid column. Whether it belongs in the
			// key is the same question the name-drop half asks — see
			// [guardKeysOnTimestamp].
			if keyOnStep {
				groupBy = append(groupBy, &chplan.ColumnRef{Name: name})
				aliases = append(aliases, name)
				continue
			}
			aggs = append(aggs, chplan.AggFunc{
				Name:  "any",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: name}},
				Alias: name,
			})
		}
	}

	guarded := chplan.Node(&chplan.Aggregate{
		Input:          rewritten,
		GroupBy:        groupBy,
		GroupByAliases: aliases,
		AggFuncs:       aggs,
		Having:         duplicateLabelsetRowCountGuardExpr(),
	})
	if !canonical {
		// A derived-shape rewrite exposes no MetricName; the Aggregate
		// exposes the same columns it was given, and an Aggregate is
		// already classified derived, so the shape a consumer sees is
		// unchanged.
		return guarded
	}
	// A canonical rewrite must STAY canonical: an Aggregate is classified
	// derived-shape, which would make the HTTP layer stamp an empty
	// MetricName over the name `label_replace` deliberately preserved.
	// Naming all four canonical outputs restores the classification.
	return &chplan.Project{
		Input: guarded,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}},
		},
	}
}

// duplicateLabelsetRowCountGuardExpr is the HAVING gate for a shape whose
// output identity KEEPS the metric name: it aborts when more than one
// input row landed on one output label set.
//
// [duplicateLabelsetGuardExpr]'s `uniqExact(MetricName) > 1` is the right
// gate only where the name is being dropped — there, "two names, one label
// set" IS the collision. Where the rewrite preserves the name, two rows
// under one (name, label set) is the collision and their names are equal,
// so counting distinct names would answer 1 and let it through.
//
// `throwIf` returns 0 on success, so `= 0` is the gate — the same idiom
// [duplicateLabelsetGuardExpr] uses.
func duplicateLabelsetRowCountGuardExpr() chplan.Expr {
	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Name: "throwIf",
			Args: []chplan.Expr{
				&chplan.Binary{
					Op:    chplan.OpGt,
					Left:  &chplan.FuncCall{Name: "count"},
					Right: &chplan.LitInt{V: 1},
				},
				&chplan.InlineString{V: chplan.DuplicateLabelsetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}
