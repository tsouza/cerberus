package logql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// The `mapFilter` lambda binds each map entry to these two parameters.
// They are lambda-local, so the names only have to avoid colliding with
// a column identifier in the same scope.
const (
	projectionKeyParam   = "k"
	projectionValueParam = "v"
)

// narrowIdentityByProjection applies a pipeline's `| drop` / `| keep`
// stages to a SERIES-IDENTITY map expression, in pipeline order.
//
// For a log query the projection is post-fetch: the API handler's
// [newLabelProjectionStep] rewrites the label map of each returned row
// and the SQL is unaffected. For a METRIC query that is not enough —
// the identity map is also the RangeWindow's grouping key, so a `| drop`
// that erases the only label distinguishing two streams has to erase it
// BEFORE the group-by, or ClickHouse aggregates the streams separately
// and the caller gets two series where Loki returns one (issue #1491).
//
// The semantics mirror [newLabelProjectionStep] exactly — same rules,
// expressed as a ClickHouse `mapFilter` predicate instead of a Go map
// mutation:
//
//   - `| drop`: a bare entry removes the named key; a matcher entry
//     (`| drop x="v"`) removes it only when the value also matches.
//   - `| keep`: an empty list keeps everything; otherwise a key survives
//     only if it matches an entry, with the `__error__` family always
//     retained.
//
// A selector with no projection stage returns identity unchanged, so
// every existing plan is byte-identical.
func narrowIdentityByProjection(identity chplan.Expr, sel syntax.LogSelectorExpr) chplan.Expr {
	pipe, ok := sel.(*syntax.PipelineExpr)
	if !ok {
		return identity
	}
	for _, stage := range pipe.MultiStages {
		switch st := stage.(type) {
		case *syntax.DropLabelsExpr:
			if pred := anyProjectionEntryMatches(st.Matchers()); pred != nil {
				identity = mapFilterExpr(identity, &chplan.FuncCall{
					Name: "not",
					Args: []chplan.Expr{pred},
				})
			}
		case *syntax.KeepLabelsExpr:
			// An empty keep list is upstream's "keep everything", not
			// "keep nothing" — leaving identity untouched is the whole
			// behaviour.
			if pred := anyProjectionEntryMatches(st.Matchers()); pred != nil {
				identity = mapFilterExpr(identity, &chplan.Binary{
					Op:    chplan.OpOr,
					Left:  specialLabelKeyPredicate(),
					Right: pred,
				})
			}
		}
	}
	return identity
}

// projectSyntheticLabelValue applies a pipeline's `| drop` / `| keep`
// stages, in pipeline order, to ONE SYNTHETIC label — a key cerberus
// splices into the series identity after [narrowIdentityByProjection]
// has already filtered the real map entries, so the `mapFilter`
// predicate never sees it. Today the only such key is the synthesized
// `detected_level` (see [detectedLevelIdentityExpr]).
//
// The projection is expressed on the synthetic key's VALUE instead of
// on the map: callers splice the result into a `map(<key>, <value>)`
// that an enclosing `mapFilter((k, v) -> v != ”)` already prunes, so a
// value of `”` removes the entry exactly like a filtered-out map key.
// `newValue` is called once per use site rather than taking a single
// expression, so the predicate and the retained value are independent
// trees (no shared node aliased into two positions of the plan).
//
// The three outcomes mirror [newLabelProjectionStep]'s Go-side rules:
//
//   - nil — the pipeline removes the key on EVERY row: a bare
//     `| drop <key>`, or a non-empty `| keep` list no entry of which
//     can ever match the key.
//   - `newValue()` — no stage touches the key.
//   - `if(<survives>, newValue(), ”)` — value-matcher entries
//     (`| drop detected_level="info"`, `| keep detected_level=~"e.*"`)
//     decide per row, so the retention rides as a row predicate.
//
// A non-pipeline selector has no projection stages and returns the
// value untouched.
func projectSyntheticLabelValue(expr syntax.Expr, key string, newValue func() chplan.Expr) chplan.Expr {
	pipe, ok := expr.(*syntax.PipelineExpr)
	if !ok {
		return newValue()
	}
	var survives chplan.Expr
	for _, stage := range pipe.MultiStages {
		switch st := stage.(type) {
		case *syntax.DropLabelsExpr:
			for _, e := range st.Matchers() {
				if e.Matcher == nil {
					if e.Name == key {
						return nil
					}
					continue
				}
				if e.Matcher.Name != key {
					continue
				}
				survives = andExpr(survives, &chplan.FuncCall{
					Name: "not",
					Args: []chplan.Expr{syntheticValueMatches(e, newValue())},
				})
			}
		case *syntax.KeepLabelsExpr:
			entries := st.Matchers()
			// An empty keep list is upstream's "keep everything".
			if len(entries) == 0 {
				continue
			}
			kept, unconditional := keepPredicateForKey(entries, key, newValue)
			if unconditional {
				continue
			}
			if kept == nil {
				return nil
			}
			survives = andExpr(survives, kept)
		}
	}
	if survives == nil {
		return newValue()
	}
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{survives, newValue(), &chplan.LitString{V: ""}},
	}
}

// keepPredicateForKey reduces a `| keep` stage's entries to the
// condition under which `key` survives it. The second return reports an
// UNCONDITIONAL survival — a bare entry naming the key — in which case
// the predicate is meaningless and the stage is a no-op for this key.
// A nil predicate with `false` means no entry can ever match, i.e. the
// stage drops the key on every row.
//
// Reserved error labels are not consulted: [specialLabelKeyPredicate]'s
// always-kept set is real map keys, and no synthetic identity key is a
// member of it.
func keepPredicateForKey(entries []syntax.NamedLabelMatcher, key string, newValue func() chplan.Expr) (chplan.Expr, bool) {
	var pred chplan.Expr
	for _, e := range entries {
		if e.Matcher == nil {
			if e.Name == key {
				return nil, true
			}
			continue
		}
		if e.Matcher.Name != key {
			continue
		}
		next := syntheticValueMatches(e, newValue())
		if pred == nil {
			pred = next
			continue
		}
		pred = &chplan.Binary{Op: chplan.OpOr, Left: pred, Right: next}
	}
	return pred, false
}

// syntheticValueMatches builds the `<value> <op> '<literal>'`
// comparison a drop/keep VALUE matcher tests. The synthetic key's value
// is an expression rather than a `mapFilter` lambda parameter, so this
// is [projectionEntryMatches]'s value half with `v` substituted.
func syntheticValueMatches(e syntax.NamedLabelMatcher, value chplan.Expr) chplan.Expr {
	return &chplan.Binary{
		Op:    matchOp(e.Matcher.Type),
		Left:  value,
		Right: &chplan.LitString{V: e.Matcher.Value},
	}
}

// andExpr AND-folds `next` onto `pred`, treating a nil `pred` as "no
// condition yet".
func andExpr(pred, next chplan.Expr) chplan.Expr {
	if pred == nil {
		return next
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: next}
}

// mapFilterExpr builds `mapFilter((k, v) -> <pred>, <m>)`.
func mapFilterExpr(m, pred chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "mapFilter",
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{projectionKeyParam, projectionValueParam},
				Body:   pred,
			},
			m,
		},
	}
}

// anyProjectionEntryMatches OR-folds one predicate per drop/keep entry,
// or returns nil when there are no entries.
func anyProjectionEntryMatches(entries []syntax.NamedLabelMatcher) chplan.Expr {
	var pred chplan.Expr
	for _, e := range entries {
		next := projectionEntryMatches(e)
		if next == nil {
			continue
		}
		if pred == nil {
			pred = next
			continue
		}
		pred = &chplan.Binary{Op: chplan.OpOr, Left: pred, Right: next}
	}
	return pred
}

// projectionEntryMatches builds the predicate identifying the map entry
// a single drop/keep entry targets.
//
// The two entry forms are mutually exclusive — the parser leaves `Name`
// empty for a matcher entry and `Matcher` nil for a bare one (see
// [syntax.DropLabelsExpr.Names]) — so the key comes from whichever is
// populated, and only the matcher form constrains the value.
func projectionEntryMatches(e syntax.NamedLabelMatcher) chplan.Expr {
	if e.Matcher == nil {
		if e.Name == "" {
			return nil
		}
		return keyEquals(e.Name)
	}
	return &chplan.Binary{
		Op:   chplan.OpAnd,
		Left: keyEquals(e.Matcher.Name),
		Right: &chplan.Binary{
			Op:    matchOp(e.Matcher.Type),
			Left:  &chplan.BareIdent{Name: projectionValueParam},
			Right: &chplan.LitString{V: e.Matcher.Value},
		},
	}
}

func keyEquals(name string) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.BareIdent{Name: projectionKeyParam},
		Right: &chplan.LitString{V: name},
	}
}

// specialLabelKeyPredicate matches the reserved error labels `| keep`
// always retains, mirroring the Go-side `isSpecialLabel`.
func specialLabelKeyPredicate() chplan.Expr {
	return &chplan.InList{
		Left: &chplan.BareIdent{Name: projectionKeyParam},
		List: []chplan.Expr{
			&chplan.LitString{V: syntax.ErrorLabel},
			&chplan.LitString{V: syntax.ErrorDetailsLabel},
			&chplan.LitString{V: syntax.PreserveErrorLabel},
		},
	}
}
