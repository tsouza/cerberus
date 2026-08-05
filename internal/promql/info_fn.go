package promql

import (
	"fmt"
	"slices"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// targetInfoMetric is the default info metric name PromQL's `info(v)`
// enriches from when no second-argument `__name__` matcher narrows it —
// the OpenTelemetry `target_info` resource-attribute series. Mirrors the
// reference engine's `targetInfo` constant (promql/info.go).
const targetInfoMetric = "target_info"

// infoNameFallbackPattern is the synthetic `__name__` regex the reference
// engine substitutes when a second-argument selector carries ONLY negated
// name matchers: with no positive matcher to anchor the scan, upstream
// narrows the candidate set to the conventional `*_info` metric family
// rather than scanning every metric. Mirrors promql/info.go's
// `effectiveInfoNameMatchers`.
const infoNameFallbackPattern = ".+_info"

// infoIdentityLabels are the labels the info-enrichment join keys on. The
// reference engine hard-codes `{instance, job}` (promql/info.go's
// `identifyingLabels`); the SQL join matches base ↔ info series on these.
var infoIdentityLabels = []string{"instance", "job"}

// lowerInfo lowers PromQL's
//
//	info(v)
//	info(v, {label matchers})
//
// the label-enrichment join. The reference engine (promql/info.go::
// evalInfo) joins target-identifying data labels from a companion
// `target_info`-style series onto the input vector `v` by the data-source
// identity labels (`instance` / `job`), enriching each input series' label
// set while leaving the sample values + timestamps unchanged.
//
// Lowering:
//
//   - arg[0] (`v`) lowers normally to the canonical per-series-latest
//     Sample shape.
//   - The info metric (default `target_info`, or the name a second-arg
//     `__name__` matcher selects) lowers through the same VectorSelector
//     path so the info side is also per-series-latest — each base series
//     matches at most one info series per identity key.
//   - The two sides wrap in a chplan.InfoJoin keyed on `{instance, job}`.
//
// The optional second argument is a label-selector: its `__name__`
// matchers (of any type — equality, regex, or negated) pick the info
// metrics; its non-`__name__` matchers both filter the info series and
// restrict which info labels copy onto the output (the
// InfoJoin.DataLabels list). Those same non-`__name__` matchers also
// decide whether a base sample that matches no info series survives at
// all — see infoDropsUnmatched.
//
// A base series that is ITSELF an info series is never enriched. The
// reference engine collects those into an `ignoreSeries` set (evalInfo)
// and appends them to the output verbatim, ahead of the data-label and
// drop logic (combineWithInfoVector) — so such a series is neither
// enriched nor dropped, whatever the second argument says. Membership is
// "the base series' `__name__` matches EVERY effective name matcher", the
// same predicate that selects the info side. Cerberus splits the base
// vector on that predicate: see infoIgnorePredicate.
func lowerInfo(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < 1 || len(c.Args) > 2 {
		return nil, fmt.Errorf("promql: info expects 1 or 2 arguments, got %d", len(c.Args))
	}

	base, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	var nameMatchers, dataMatchers []*labels.Matcher
	if len(c.Args) == 2 {
		sel, ok := c.Args[1].(*parser.VectorSelector)
		if !ok {
			return nil, fmt.Errorf("promql: info(...) second argument must be a label selector, got %T", c.Args[1])
		}
		for _, m := range sel.LabelMatchers {
			if m.Name == model.MetricNameLabel {
				nameMatchers = append(nameMatchers, m)
				continue
			}
			dataMatchers = append(dataMatchers, m)
		}
	}
	nameMatchers = effectiveInfoNameMatchers(nameMatchers)

	// When the base vector pins its own metric name, ignore-set membership
	// is decidable here and the split collapses to one of its two arms —
	// keeping the common `info(up)` shape a plain join over an unfiltered
	// base, and making `info(target_info)` the pass-through it is upstream.
	if name, ok := staticBaseMetricName(c.Args[0]); ok {
		if matchesEveryNameMatcher(nameMatchers, name) {
			return base, nil
		}
		return lowerInfoJoin(base, nameMatchers, dataMatchers, s, ctx)
	}

	// Undecidable statically (the base selects several metric names, or is
	// an expression rather than a selector) — split at query time.
	join, err := lowerInfoJoin(
		&chplan.Filter{Input: base, Predicate: infoEnrichablePredicate(nameMatchers, s)},
		nameMatchers, dataMatchers, s, ctx,
	)
	if err != nil {
		return nil, err
	}
	ignored := &chplan.Filter{
		// The arms are independent subtrees: chplan is rewritten in place,
		// so the second arm gets its own copy of the base rather than an
		// alias of the first arm's.
		Input:     chplan.CloneNode(base),
		Predicate: infoIgnorePredicate(nameMatchers, s),
	}
	// Both arms carry the canonical Sample quadruple in the same order —
	// the shape InfoJoin's own base side already requires — so the
	// positional UNION ALL lines up column for column. The Project on top
	// states that shape rather than leaving it implicit: UNION ALL matches
	// columns by POSITION, so a downstream consumer reading the union's
	// output needs one named column list to read, not two arms to compare.
	union := &chplan.UnionAll{Inputs: []chplan.Node{join, ignored}}
	return &chplan.Project{
		Input: union,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}, nil
}

// lowerInfoJoin builds the enrichment join over an already-lowered base
// arm. Split out of lowerInfo so the ignore-set carve-out can feed it
// either the whole base vector or just its enrichable arm.
func lowerInfoJoin(input chplan.Node, nameMatchers, dataMatchers []*labels.Matcher, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	infoNode, err := lowerInfoMetric(nameMatchers, dataMatchers, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.InfoJoin{
		Input:            input,
		Info:             infoNode,
		IdentityLabels:   slices.Clone(infoIdentityLabels),
		DataLabels:       infoDataLabelNames(dataMatchers),
		DropUnmatched:    infoDropsUnmatched(dataMatchers),
		MergeInfoMetrics: !pinsSingleInfoMetric(nameMatchers),
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}, nil
}

// staticBaseMetricName returns the single metric name the info() base
// argument can produce, when the argument pins one. A VectorSelector
// carries a non-empty Name exactly when a `__name__` equality matcher
// pins it, so every series it yields has that name and ignore-set
// membership is a compile-time answer. Anything else — a regex-named
// selector, or a function/binary expression — reports false and takes the
// query-time split.
func staticBaseMetricName(e parser.Expr) (string, bool) {
	sel, ok := e.(*parser.VectorSelector)
	if !ok || sel.Name == "" {
		return "", false
	}
	return sel.Name, true
}

// matchesEveryNameMatcher reports whether name satisfies all of the
// effective `__name__` matchers — the reference engine's ignoreSeries
// test (promql/info.go::evalInfo's `matchesAllMatchers` loop).
func matchesEveryNameMatcher(nameMatchers []*labels.Matcher, name string) bool {
	for _, m := range nameMatchers {
		if !m.Matches(name) {
			return false
		}
	}
	return true
}

// infoIgnorePredicate renders the ignore-set test as a plan predicate
// over the base side's metric-name column: an AND-fold of every effective
// `__name__` matcher. effectiveInfoNameMatchers never returns an empty
// set, so the fold always yields a predicate.
func infoIgnorePredicate(nameMatchers []*labels.Matcher, s schema.Metrics) chplan.Expr {
	var pred chplan.Expr
	for _, m := range nameMatchers {
		pred = foldPredicate(pred, chplan.OpAnd, metricNamePredicate(m, s))
	}
	return pred
}

// infoEnrichablePredicate renders the complement of infoIgnorePredicate —
// the base series that DO get enriched. It is built by inverting each
// matcher and OR-folding rather than by wrapping the AND-fold in a NOT,
// because chplan has no NOT expression: negating a matcher's TYPE yields
// the exact complement of its predicate (an `=` becomes `!=`, and the
// regex form's `single OR normalized` becomes `single AND normalized`),
// so De Morgan holds term for term.
func infoEnrichablePredicate(nameMatchers []*labels.Matcher, s schema.Metrics) chplan.Expr {
	var pred chplan.Expr
	for _, m := range nameMatchers {
		pred = foldPredicate(pred, chplan.OpOr, metricNamePredicate(invertMatcher(m), s))
	}
	return pred
}

// foldPredicate appends next to acc under op, treating a nil acc as "no
// terms yet".
func foldPredicate(acc chplan.Expr, op chplan.BinaryOp, next chplan.Expr) chplan.Expr {
	if acc == nil {
		return next
	}
	return &chplan.Binary{Op: op, Left: acc, Right: next}
}

// invertMatcher returns the matcher that matches exactly the label values
// m rejects. The value is unchanged, so a regex that compiled for m
// compiles here too.
func invertMatcher(m *labels.Matcher) *labels.Matcher {
	var inverted labels.MatchType
	switch m.Type {
	case labels.MatchEqual:
		inverted = labels.MatchNotEqual
	case labels.MatchNotEqual:
		inverted = labels.MatchEqual
	case labels.MatchRegexp:
		inverted = labels.MatchNotRegexp
	case labels.MatchNotRegexp:
		inverted = labels.MatchRegexp
	default:
		panic(fmt.Sprintf("promql: unknown label match type %v", m.Type))
	}
	return labels.MustNewMatcher(inverted, m.Name, m.Value)
}

// effectiveInfoNameMatchers returns the `__name__` matcher set that
// actually selects info series, given the ones a second-argument
// selector spelled out. Ports promql/info.go's function of the same
// name:
//
//   - At least one positive matcher (`=` or `=~`) → use the set as-is;
//     the negated matchers ride along as extra narrowing.
//   - Only negated matchers → prepend the synthetic `.+_info` regex so
//     the scan is anchored to the conventional info-metric family
//     instead of every metric in the store.
//   - No matchers at all (bare `info(v)`, or a selector carrying only
//     data-label matchers) → the default `target_info` equality.
func effectiveInfoNameMatchers(nameMatchers []*labels.Matcher) []*labels.Matcher {
	for _, m := range nameMatchers {
		if m.Type == labels.MatchEqual || m.Type == labels.MatchRegexp {
			return nameMatchers
		}
	}
	if len(nameMatchers) > 0 {
		fallback := labels.MustNewMatcher(labels.MatchRegexp, model.MetricNameLabel, infoNameFallbackPattern)
		return append([]*labels.Matcher{fallback}, nameMatchers...)
	}
	return []*labels.Matcher{
		labels.MustNewMatcher(labels.MatchEqual, model.MetricNameLabel, targetInfoMetric),
	}
}

// pinsSingleInfoMetric reports whether the effective `__name__` matcher
// set can only ever select one info metric — a lone equality matcher.
// Anything else (a regex, a negation, or several matchers) may match
// several info metrics at once, and the reference engine then unions the
// data labels of every matched info series onto a single output sample
// (promql/info.go::combineWithInfoVector iterates `seenInfoMetrics` and
// merges into one label builder). The join emitter needs that signal to
// fold its per-info-metric join rows back into one row per base sample.
func pinsSingleInfoMetric(nameMatchers []*labels.Matcher) bool {
	return len(nameMatchers) == 1 && nameMatchers[0].Type == labels.MatchEqual
}

// infoDropsUnmatched reports whether a base sample that matches no info
// series must be dropped from the result rather than passed through
// unenriched. The reference engine keeps such a sample only when every
// data-label matcher also matches the empty string
// (promql/info.go::combineWithInfoVector's `allMatchersMatchEmpty`
// branch): a matcher like `k="v"` or `k=~".+"` cannot be satisfied by a
// series carrying no `k` at all, so the sample is not part of the
// result. `k=~".*"` / `k!="v"` do match the empty string, so unmatched
// samples survive.
func infoDropsUnmatched(dataMatchers []*labels.Matcher) bool {
	for _, m := range dataMatchers {
		if !m.Matches("") {
			return true
		}
	}
	return false
}

// lowerInfoMetric lowers the info-metric side of the join: a synthetic
// VectorSelector carrying the effective `__name__` matchers plus any
// data-label matchers, run through the regular VectorSelector lowering
// so the info side gets the same per-series-latest (LWR) / per-step-
// matrix treatment as the base vector — and so regex / negated name
// matchers reuse the same unpinned-name scan fan-out an ordinary
// `{__name__=~"…"}` selector gets. The result is then collapsed to one
// row per (metric name, identity-label values) signature — see
// collapseInfoSeriesBySignature — because lowerVectorSelector's
// per-series-latest shape is keyed on the FULL label set, one level
// finer than the signature upstream's info() enrichment keys on.
func lowerInfoMetric(nameMatchers, dataMatchers []*labels.Matcher, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	matchers := make([]*labels.Matcher, 0, len(nameMatchers)+len(dataMatchers))
	matchers = append(matchers, nameMatchers...)
	matchers = append(matchers, dataMatchers...)

	sel := &parser.VectorSelector{
		// Empty unless a `__name__` equality pins the name, matching the
		// parser's own convention for `{__name__=~"…"}` selectors.
		Name:          metricNameFromMatchers(matchers),
		LabelMatchers: matchers,
	}
	node, err := lowerVectorSelector(sel, s, ctx)
	if err != nil {
		return nil, err
	}
	return collapseInfoSeriesBySignature(node, s), nil
}

// infoSignatureKeyAlias names the synthetic GROUP BY column
// collapseInfoSeriesBySignature mints for the i-th identity label's
// extracted value. These never survive past the wrapping Project either —
// they exist only to make the GROUP BY key for that label addressable in
// GROUP BY without re-rendering (and thereby risking a shadowed-alias
// mismatch — see groupKeyFrags).
func infoSignatureKeyAlias(i int) string {
	return fmt.Sprintf("_cerberus_info_sig_%d", i)
}

// infoWinnerAttrsAlias, infoWinnerTimeAlias and infoWinnerValueAlias name
// the synthetic aggregate-output columns collapseInfoSeriesBySignature
// mints for the winning row's Attributes / TimeUnix / Value, mirroring the
// LWR aggregate's lwr_ts/lwr_value convention (see lowerVectorSelector):
// the wrapping Project is what renames these to the canonical Sample
// column names, so the aggregate itself never aliases an output to the
// SAME name as a raw column it also reads.
//
// That distinction is load-bearing, not stylistic: aliasing straight to
// s.AttributesColumn (i.e. `argMax(Attributes, TimeUnix) AS Attributes`)
// collides with the bare `Attributes` column collapseInfoSeriesBySignature
// also reads inside the GROUP BY key expressions (`Attributes["instance"]`
// for the signature columns), and ClickHouse's GROUP BY key elimination
// then rejects the query with ILLEGAL_AGGREGATION ("argMax(...) AS
// Attributes is found in GROUP BY"), reading the aggregate's own output
// alias as if it re-entered the grouping key set.
const (
	infoWinnerAttrsAlias = "_cerberus_info_winner_attrs"
	infoWinnerTimeAlias  = "_cerberus_info_winner_ts"
	infoWinnerValueAlias = "_cerberus_info_winner_value"
)

// collapseInfoSeriesBySignature collapses the info side of an info() join
// down to one row per (metric name, identity-label values) signature,
// keeping the sample with the greatest timestamp — mirroring the
// reference engine's combineWithInfoSeries/sigFunction (promql/info.go).
//
// lowerVectorSelector produces one row per FULL label set, which is
// finer-grained than the signature upstream keys on: two info series
// sharing every identity label but differing on a data label (e.g.
// target_info{job="api",instance="i1",version="1.2.3"} @ t0 and
// target_info{job="api",instance="i1",version="4.5.6"} @ t1, t1 > t0) must
// collapse into the single, newest row rather than fan the join out to
// both — see issue #1630.
//
// An exact tie on the winning timestamp is not picked arbitrarily: CH's
// argMax is documented as choosing an unspecified one of the tied rows,
// so the aggregate also computes whether more than one row shares the
// group's maximum timestamp and, if so, aborts the query with
// chplan.InfoConflictingLabelMessage — mirroring upstream's "found
// duplicate series for info metric" error (the shared message is the
// closest constant-string abort cerberus can raise; see
// chplan.InfoConflictingLabelMessage's doc for why the wording differs).
//
// The tie check rides as a real HAVING clause (`chplan.Aggregate.Having`),
// not an extra unreferenced SELECT-list column: ClickHouse's analyzer
// prunes a SELECT expression nothing downstream reads, throwIf side
// effect included, so a column-shaped guard silently never fires. HAVING
// gates row production directly and cannot be pruned. See
// chplan.Aggregate.Having's doc.
func collapseInfoSeriesBySignature(node chplan.Node, s schema.Metrics) chplan.Node {
	groupBy := make([]chplan.Expr, 0, 1+len(infoIdentityLabels))
	groupByAliases := make([]string, 0, 1+len(infoIdentityLabels))
	groupBy = append(groupBy, &chplan.ColumnRef{Name: s.MetricNameColumn})
	groupByAliases = append(groupByAliases, s.MetricNameColumn)
	for i, id := range infoIdentityLabels {
		groupBy = append(groupBy, attributeLookup(s.AttributesColumn, id))
		groupByAliases = append(groupByAliases, infoSignatureKeyAlias(i))
	}

	timeUnix := &chplan.ColumnRef{Name: s.TimestampColumn}
	maxTimeUnix := &chplan.FuncCall{Name: "max", Args: []chplan.Expr{timeUnix}}
	tieCount := &chplan.FuncCall{
		Name: "countEqual",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "groupArray", Args: []chplan.Expr{timeUnix}},
			maxTimeUnix,
		},
	}

	// throwIf returns 0 on success, so `= 0` is the HAVING gate — the same
	// idiom internal/chsql/info_join.go's infoConflictGuardFrag uses for
	// the sibling merge-conflict guard.
	tieCheck := &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Name: "throwIf",
			Args: []chplan.Expr{
				&chplan.Binary{Op: chplan.OpGt, Left: tieCount, Right: &chplan.LitInt{V: 1}},
				&chplan.InlineString{V: chplan.InfoConflictingLabelMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}

	agg := &chplan.Aggregate{
		Input:          node,
		GroupBy:        groupBy,
		GroupByAliases: groupByAliases,
		AggFuncs: []chplan.AggFunc{
			{
				Name:  "argMax",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}, timeUnix},
				Alias: infoWinnerAttrsAlias,
			},
			{
				Name:  "max",
				Args:  []chplan.Expr{timeUnix},
				Alias: infoWinnerTimeAlias,
			},
			{
				Name:  "argMax",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}, timeUnix},
				Alias: infoWinnerValueAlias,
			},
		},
		Having: tieCheck,
	}

	// Drop the synthetic signature-key + tie-check columns, restoring the
	// canonical four-column Sample shape lowerInfoJoin's Info field
	// requires.
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: infoWinnerAttrsAlias}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: infoWinnerTimeAlias}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: infoWinnerValueAlias}, Alias: s.ValueColumn},
		},
	}
}

// infoDataLabelNames returns the de-duplicated set of label names the
// second-argument matchers reference (excluding `__name__`). Non-empty
// → the InfoJoin copies only these info labels onto the output; empty →
// every non-identity info label copies (the default `info(v)` case).
func infoDataLabelNames(dataMatchers []*labels.Matcher) []string {
	if len(dataMatchers) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	names := make([]string, 0, len(dataMatchers))
	for _, m := range dataMatchers {
		if _, ok := seen[m.Name]; ok {
			continue
		}
		seen[m.Name] = struct{}{}
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names
}
