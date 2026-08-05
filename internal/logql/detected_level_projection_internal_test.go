package logql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
	"github.com/tsouza/cerberus/internal/schema"
)

// parseProjectionExpr parses a LogQL selector for the projection tests
// and fails the test if the source doesn't parse.
func parseProjectionExpr(t *testing.T, q string) syntax.Expr {
	t.Helper()
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	return expr
}

// TestDetectedLevelIdentityExpr_ProjectionOutcomes pins issue #1607:
// the synthesized `detected_level` is ordinary structured metadata in
// reference Loki, so a `| drop` / `| keep` stage projects it exactly
// like an indexed label. Cerberus splices it into the identity map
// AFTER [narrowIdentityByProjection] has filtered the real entries, so
// the projection has to ride on the VALUE — the enclosing
// `mapFilter((k, v) -> v != ”)` turns an empty value into an absent
// key.
//
// The three outcomes are structurally distinguishable, which is what
// this test asserts:
//
//   - nil          — removed on every row
//   - multiIf(...) — untouched (the raw [detectedLevelExpr] cascade)
//   - if(...)      — retained per row by a value matcher
//
// Pre-fix, every `| keep` / value-matcher case below returned the
// unconditional cascade: the old static gate only recognised a BARE
// `| drop detected_level` and a keep list that named the key, and it
// had no way to express a per-row decision at all.
func TestDetectedLevelIdentityExpr_ProjectionOutcomes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	// outcome discriminates the three shapes without depending on the
	// (large) internals of the multiIf severity cascade.
	const (
		removed       = "removed"
		unconditional = "unconditional"
		conditional   = "conditional"
	)
	classify := func(t *testing.T, e chplan.Expr) string {
		t.Helper()
		if e == nil {
			return removed
		}
		fc, ok := e.(*chplan.FuncCall)
		if !ok {
			t.Fatalf("detectedLevelIdentityExpr -> %T, want *chplan.FuncCall or nil", e)
		}
		switch fc.Name {
		case "if":
			if len(fc.Args) != 3 {
				t.Fatalf("if(...) has %d args, want 3", len(fc.Args))
			}
			empty, ok := fc.Args[2].(*chplan.LitString)
			if !ok || empty.V != "" {
				t.Errorf("if(...) else-branch = %#v, want LitString{\"\"} so the "+
					"enclosing mapFilter prunes the key", fc.Args[2])
			}
			return conditional
		case "multiIf":
			return unconditional
		default:
			t.Fatalf("detectedLevelIdentityExpr -> FuncCall %q, want if / multiIf", fc.Name)
			return ""
		}
	}

	for _, tc := range []struct {
		query string
		want  string
		why   string
	}{
		{`{job="api"}`, unconditional, "no pipeline touches the label"},
		{`{job="api"} | logfmt`, unconditional, "a parser stage is not a projection stage"},
		{`{job="api"} | drop env`, unconditional, "an unrelated drop leaves the label alone"},
		{`{job="api"} | drop detected_level`, removed, "bare drop removes it on every row"},
		{`{job="api"} | drop env, detected_level`, removed, "a multi-name drop naming it removes it"},
		{`{job="api"} | drop env | drop detected_level`, removed, "a later stage still removes it"},
		{`{job="api"} | drop detected_level="info"`, conditional, "a value matcher decides per row"},
		{`{job="api"} | drop detected_level=~"e.+"`, conditional, "a regex value matcher decides per row"},
		{`{job="api"} | keep job`, removed, "a keep list that omits it drops it"},
		{`{job="api"} | keep job="api"`, removed, "a value-matched keep entry naming another label still omits it"},
		{`{job="api"} | keep job, detected_level`, unconditional, "a bare keep entry naming it retains it whole"},
		{`{job="api"} | keep detected_level="info"`, conditional, "a value-matched keep entry naming it decides per row"},
		{`{job="api"} | keep detected_level | drop detected_level`, removed, "the drop still runs after the keep"},
		{`{job="api"} | drop detected_level | keep detected_level`, removed, "an earlier bare drop wins outright"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			got := classify(t, detectedLevelIdentityExpr(s, parseProjectionExpr(t, tc.query)))
			if got != tc.want {
				t.Errorf("detectedLevelIdentityExpr(%q) = %s, want %s — %s", tc.query, got, tc.want, tc.why)
			}
		})
	}
}

// TestDetectedLevelIdentityExpr_MultipleKeepEntriesOr pins that two
// keep entries naming the level with different values compose as OR,
// not AND: `| keep detected_level="info", detected_level="warn"` keeps
// a row whose level is EITHER, and an AND fold would keep neither.
func TestDetectedLevelIdentityExpr_MultipleKeepEntriesOr(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr := parseProjectionExpr(t, `{job="api"} | keep detected_level="info", detected_level="warn"`)

	got, ok := detectedLevelIdentityExpr(s, expr).(*chplan.FuncCall)
	if !ok || got.Name != "if" {
		t.Fatalf("detectedLevelIdentityExpr -> %#v, want an if(...) FuncCall", got)
	}
	pred, ok := got.Args[0].(*chplan.Binary)
	if !ok {
		t.Fatalf("if(...) predicate = %T, want *chplan.Binary", got.Args[0])
	}
	if pred.Op != chplan.OpOr {
		t.Errorf("if(...) predicate op = %v, want OpOr — two keep entries are alternatives, "+
			"and an AND fold would drop the label on every row", pred.Op)
	}
}

// TestDetectedLevelIdentityExpr_TwoDropMatchersAnd is the complement:
// two `| drop` value matchers each REMOVE a row's label, so the
// survival predicate is the conjunction of both negations. An OR fold
// would keep the label on rows the first matcher already removed it
// from.
func TestDetectedLevelIdentityExpr_TwoDropMatchersAnd(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr := parseProjectionExpr(t, `{job="api"} | drop detected_level="info", detected_level="warn"`)

	got, ok := detectedLevelIdentityExpr(s, expr).(*chplan.FuncCall)
	if !ok || got.Name != "if" {
		t.Fatalf("detectedLevelIdentityExpr -> %#v, want an if(...) FuncCall", got)
	}
	pred, ok := got.Args[0].(*chplan.Binary)
	if !ok {
		t.Fatalf("if(...) predicate = %T, want *chplan.Binary", got.Args[0])
	}
	if pred.Op != chplan.OpAnd {
		t.Errorf("if(...) predicate op = %v, want OpAnd — a row survives only if NEITHER "+
			"matcher removed the label", pred.Op)
	}
}

// TestProjectSyntheticLabelValue_EmptyKeepIsIdentity mirrors
// [TestNarrowIdentityByProjection_EmptyKeepIsIdentity] for the
// synthetic-key half: a zero-entry `| keep` keeps EVERYTHING. The
// grammar has no source spelling for it, so the stage is built
// directly. The inverted reading would delete the level from every
// series.
func TestProjectSyntheticLabelValue_EmptyKeepIsIdentity(t *testing.T) {
	t.Parallel()

	value := &chplan.LitString{V: "sentinel"}
	got := projectSyntheticLabelValue(
		&syntax.PipelineExpr{MultiStages: []syntax.StageExpr{&syntax.KeepLabelsExpr{}}},
		detectedLevelLabel,
		func() chplan.Expr { return value },
	)
	if got != chplan.Expr(value) {
		t.Errorf("empty keep list rewrote the value to %#v; want it untouched", got)
	}
}

// TestProjectSyntheticLabelValue_FreshValuePerUse pins that the
// factory is called once per USE SITE rather than once overall: a
// conditional outcome puts the value in both the predicate and the
// retained branch, and aliasing one plan node into two live positions
// is exactly what the chplan tree forbids.
func TestProjectSyntheticLabelValue_FreshValuePerUse(t *testing.T) {
	t.Parallel()

	calls := 0
	got := projectSyntheticLabelValue(
		parseProjectionExpr(t, `{job="api"} | drop detected_level="info"`).(*syntax.PipelineExpr),
		detectedLevelLabel,
		func() chplan.Expr {
			calls++
			return &chplan.LitString{V: "v"}
		},
	)
	fc, ok := got.(*chplan.FuncCall)
	if !ok || fc.Name != "if" {
		t.Fatalf("projectSyntheticLabelValue -> %#v, want an if(...) FuncCall", got)
	}
	if calls != 2 {
		t.Errorf("value factory called %d times, want 2 (once for the predicate, once for the "+
			"retained branch) — a single shared node would alias into two live positions", calls)
	}
	not, ok := fc.Args[0].(*chplan.FuncCall)
	if !ok || not.Name != "not" {
		t.Fatalf("if(...) predicate = %#v, want not(...)", fc.Args[0])
	}
	cmp, ok := not.Args[0].(*chplan.Binary)
	if !ok {
		t.Fatalf("not(...) operand = %T, want *chplan.Binary", not.Args[0])
	}
	if cmp.Left == fc.Args[1] {
		t.Error("the predicate's value node is the SAME pointer as the retained branch's")
	}
}

// TestLevelAwareRangeGroupKey_RemovedLevelCollapsesToEmpty pins the
// grouping half of #1607: `by (detected_level)` after a stage that
// removed the label groups on the empty string, the same collapse
// reference Loki produces once its LabelsBuilder no longer carries the
// label — NOT on the raw severity cascade, which would resurrect a
// dimension the pipeline deleted.
func TestLevelAwareRangeGroupKey_RemovedLevelCollapsesToEmpty(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	for _, label := range []string{"detected_level", "level"} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			got := levelAwareRangeGroupKey(label, s, func() chplan.Expr { return nil })
			lit, ok := got.(*chplan.LitString)
			if !ok {
				t.Fatalf("levelAwareRangeGroupKey(%q, nil-level) -> %T, want *chplan.LitString", label, got)
			}
			if lit.V != "" {
				t.Errorf("group key = %q, want the empty string", lit.V)
			}
		})
	}
}

// TestLevelAwareRangeGroupKey_UsesProjectedValue pins that the grouping
// key is the PROJECTED level value, not a freshly derived cascade: a
// `| drop detected_level="info"` before `by (level)` must group the
// "info" rows under the empty string like every other removed label.
func TestLevelAwareRangeGroupKey_UsesProjectedValue(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	sentinel := &chplan.LitString{V: "projected"}
	got := levelAwareRangeGroupKey("level", s, func() chplan.Expr { return sentinel })
	if got != chplan.Expr(sentinel) {
		t.Errorf("levelAwareRangeGroupKey -> %#v, want the projected value verbatim", got)
	}
}
