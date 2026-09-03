package logql

import (
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Tests in this file kill LIVED gremlins mutants reported on the
// phase4-logql-lower leg (cerberus issue #2949), whose mutated file is
// lower.go.

// TestPipelineLineExprScansPastNonUnpackStages kills the INVERT_LOOPCTRL
// mutant on the `continue` that skips a non-`| unpack` stage in
// [PipelineLineExpr]'s scan, guarded by
// lower.go:PipelineLineExpr:`!ok || lp.Op != syntax.OpParserTypeUnpack`.
//
// The citation names that neighbouring `if` rather than the mutated
// statement: the mutant rewrites a bare `continue`, which no substring
// singles out.
//
// The loop looks for an `| unpack` stage ANYWHERE in the pipeline, so a
// stage that is not one has to be stepped over rather than treated as the
// end of the search. `| json | unpack` puts a non-unpack parser stage in
// front of the unpack:
//
//   - ORIGINAL `continue`: the `| json` stage is skipped, the `| unpack`
//     stage behind it is found, and the line expression is rewritten to
//     read the packed payload's `_entry` member.
//   - MUTANT `break`: the scan stops at `| json` and returns nil, so
//     [lang.go]'s log-row projection falls back to the bare body column
//     and hands callers the packed JSON under labels describing its
//     contents — the exact regression PipelineLineExpr exists to prevent.
func TestPipelineLineExprScansPastNonUnpackStages(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `{app="api"} | json | unpack`

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	got, err := PipelineLineExpr(expr, s)
	if err != nil {
		t.Fatalf("PipelineLineExpr(%q): %v", query, err)
	}
	if got == nil {
		t.Fatalf("PipelineLineExpr(%q) = nil, want the unpack line rewrite — the scan stopped at the first non-unpack stage instead of stepping over it", query)
	}
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnIf {
		t.Fatalf("PipelineLineExpr(%q) = %T (%q), want *chplan.FuncCall(%q)", query, got, funcName(got), chplan.FnIf)
	}
}

// TestPipelineLineExprSeedsFirstUnpackWithBodyColumn kills the
// CONDITIONALS_NEGATION mutant on lower.go:PipelineLineExpr:`prev == nil`
// (`== nil` -> `!= nil`).
//
// That guard seeds the FIRST `| unpack` in a pipeline with the stored body
// column, because there is no earlier line rewrite to read from; a second
// unpack instead chains onto the first one's result.
//
//   - ORIGINAL: on the single unpack of `{app="api"} | unpack`, prev is
//     nil, so it is seeded with the body column and the emitted
//     `if(isPacked, JSONExtractString(Body, '_entry'), Body)` has the body
//     column as its else-branch.
//   - MUTANT `!= nil`: prev stays nil, the seed is skipped, and the
//     else-branch of the emitted `if` is a nil expression — an unpacked
//     log line whose non-packed fallback reads nothing at all.
func TestPipelineLineExprSeedsFirstUnpackWithBodyColumn(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const query = `{app="api"} | unpack`

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	got, err := PipelineLineExpr(expr, s)
	if err != nil {
		t.Fatalf("PipelineLineExpr(%q): %v", query, err)
	}
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnIf || len(fc.Args) != 3 {
		t.Fatalf("PipelineLineExpr(%q) = %T (%q), want a 3-arg *chplan.FuncCall(%q)", query, got, funcName(got), chplan.FnIf)
	}
	col, ok := fc.Args[2].(*chplan.ColumnRef)
	if !ok || col.Name != s.BodyColumn {
		t.Fatalf("PipelineLineExpr(%q) not-packed branch = %#v, want *chplan.ColumnRef{Name: %q} — the first unpack was not seeded with the stored body column", query, fc.Args[2], s.BodyColumn)
	}
}

// TestPipelineSkipsGoSideErrorFilterWithoutStopping kills the
// INVERT_LOOPCTRL mutant on the `continue` that skips a post-dynamic-stage
// `__error__` label filter in [lowerPipelineWithLabels]'s stage loop,
// guarded by
// lower.go:lowerPipelineWithLabels:`ok && dynamicLabels && FiltersErrorLabel(lf.LabelFilterer)`.
//
// The citation names that neighbouring `if` rather than the mutated
// statement: the mutant rewrites a bare `continue`, which no substring
// singles out.
//
// An `__error__` filter that follows a dynamic label stage (`| pattern`,
// per [isDynamicLabelStage]) is deliberately NOT lowered into SQL —
// internal/api/loki's newLabelFilterStep re-applies it in Go once the
// stage's transform has computed the row's real `__error__` label.
// Skipping it must not abandon the stages BEHIND it, which still lower
// normally.
//
//   - ORIGINAL `continue`: the `__error__=""` stage is skipped and the
//     `level="ZZ-KILL-MARKER"` stage behind it is lowered, so the marker
//     appears in the plan's filter predicate.
//   - MUTANT `break`: the loop abandons the pipeline at the `__error__`
//     stage, silently dropping every later filter — the plan returns rows
//     the query excluded, and the marker is absent.
func TestPipelineSkipsGoSideErrorFilterWithoutStopping(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	const marker = "ZZ-KILL-MARKER"
	const query = `{app="api"} | pattern "<ip> - <_>" | __error__="" | level="` + marker + `"`

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower(%q): %v", query, err)
	}
	if !planPredicateHasLiteral(plan, marker) {
		t.Fatalf("lower(%q): the %q label filter is missing from the plan's predicates — the stage loop stopped at the Go-side `__error__` filter instead of stepping over it, dropping every stage behind it", query, marker)
	}
}

// planPredicateHasLiteral reports whether any [chplan.Filter] predicate in
// the plan's spine carries the string literal `want`.
func planPredicateHasLiteral(plan chplan.Node, want string) bool {
	found := false
	var walk func(chplan.Node)
	walk = func(n chplan.Node) {
		switch v := n.(type) {
		case *chplan.Filter:
			walkExprTree(v.Predicate, func(e chplan.Expr) {
				if ls, ok := e.(*chplan.LitString); ok && ls.V == want {
					found = true
				}
			})
			walk(v.Input)
		case *chplan.Project:
			walk(v.Input)
		case *chplan.RangeWindow:
			walk(v.Input)
		case *chplan.Aggregate:
			walk(v.Input)
		}
	}
	walk(plan)
	return found
}

// NOT KILLABLE — documented, not defended by a test. These are the LIVED
// mutants phase4-logql-lower reports that no input can distinguish.
//
// lower.go:`extension <= 0` — CONDITIONALS_BOUNDARY (`<=` -> `<`).
// The two forms differ only at extension == 0, and there the guarded body
// is the identity: it copies the context and rewrites Start as
// `c.Start.Add(-0)`. `time.Time.Add(0)` adds zero to both the wall reading
// and the monotonic reading and returns the receiver's exact value, so the
// mutant's "extended" context is field-for-field the context the original
// returns early. Nothing downstream can see a difference because there is
// none to see.
//
// The ARITHMETIC_BASE mutants on mergeParsedFields's and
// jsonExtractStringExpr's capacity hints were adjudicated EQUIVALENT here,
// and that was wrong (cerberus issue #2984). The argument ran: both slices
// are built with append from length 0, so contents and ordering are
// identical whatever the pre-sized cap, and a capacity is not reachable
// from the returned plan. The first half is true and the second is false —
// one slice becomes the map literal's exported Args inside the mapMerge the
// first function returns, and the other IS the JSONExtractString FuncCall's
// Args. Their verdicts now live with the kills, in
// [TestMergeParsedFields_CapHintMutantKilled] and
// [TestJSONExtractStringExpr_CapHintMutantKilled], which is where the
// citations went too: a mutant carries one verdict, so leaving them under a
// footer here would leave them carrying two.
//
// lower.go:`len(candidates) <= 1` — CONDITIONALS_BOUNDARY (`<=` -> `<`).
// The forms differ only at len(candidates) == 1, and there they build the
// same node. The original returns `MapAccess{labelsMap, key}`; the mutant
// calls qlcommon.OTelDottedFallbackChain with the one candidate, whose
// loop over the remaining candidates does not run, so it returns
// `MapAccess{labelsMap, candidates[0]}`. A one-element candidate list is
// produced only for a key with no `_` separator to re-spell, and
// format.PromLabelToOTelCandidates always leads with the key verbatim, so
// candidates[0] IS key and the two MapAccess nodes are field-identical.
// len(candidates) == 0 does not distinguish them either: both forms take
// the early return.
