package chsql

import (
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Builder.err is a FIRST-ERROR-WINS latch (see the field's doc comment): a Frag
// cannot return an error, so the three sites that write the latch —
// Builder.Expr's defer, Subquery and Spliced — all guard with the identical
// `err != nil && b.err == nil`. Each conjunct carries its own obligation, and
// the tests below pin one obligation apiece:
//
//   - `err != nil` — record ONLY real errors, and record them when they happen.
//     Negated to `err == nil` the latch only ever stores nil, so a failure is
//     swallowed and the caller renders truncated SQL as if it had succeeded.
//   - `b.err == nil` — record only into an EMPTY latch. Negated to
//     `b.err != nil` the latch can never take its first value, which swallows
//     the failure exactly as above.
//   - `&&` — both must hold. Inverted to `||` the guard fires on every error,
//     so a later failure OVERWRITES the first one and the caller is told about
//     the wrong (downstream, usually derivative) failure.
//
// errExpr (builder_expr_mutation_test.go) supplies the first error shape; the
// helpers below supply a SECOND, textually distinguishable one so the
// overwrite tests can name which error survived.

// otherErrExpr returns a chplan.Expr that Builder.Expr rejects with a message
// distinct from errExpr's, so a test can tell the two apart by text.
func otherErrExpr() chplan.Expr { return &chplan.ScalarSubquery{} }

// exprErrorText is the substring unique to errExpr's rejection.
const exprErrorText = "chplan.Lambda"

// otherErrorText is the substring unique to otherErrExpr's rejection.
const otherErrorText = "chplan.ScalarSubquery"

// failingSubquery returns a QueryBuilder whose render fails with
// otherErrExpr's error, so Subquery / Spliced have a nested error to
// propagate.
func failingSubquery() *QueryBuilder {
	return NewQuery().Select(func(b *Builder) { _ = b.Expr(otherErrExpr()) })
}

// TestMutation_Expr_LatchKeepsFirstError pins that Builder.Expr's latch is
// first-error-wins: the FIRST rejected expression is the one the caller is
// told about, even though a later Expr on the same Builder also fails. That
// ordering is the contract — the first failure is the root cause; everything
// after it is rendered against a Builder already known to be broken.
//
// Kills builder.go:298:17 INVERT_LOGICAL (`&&` -> `||`): the mutated guard is
// `err != nil || b.err == nil`, which fires on EVERY non-nil err, so the
// second rejection overwrites the first and Build reports the ScalarSubquery
// error instead of the Lambda one.
func TestMutation_Expr_LatchKeepsFirstError(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(errExpr()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("first Expr must reject, got %v", err)
	}
	if err := b.Expr(otherErrExpr()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("second Expr must reject, got %v", err)
	}
	_, _, err := b.Build()
	if err == nil {
		t.Fatalf("Build must surface the latched error")
	}
	if !strings.Contains(err.Error(), exprErrorText) {
		t.Fatalf("the latch must keep the FIRST error (%s), got %v", exprErrorText, err)
	}
	if strings.Contains(err.Error(), otherErrorText) {
		t.Fatalf("the latch must not be overwritten by the second error, got %v", err)
	}
}

// TestMutation_Subquery_PropagatesNestedError pins that a nested
// QueryBuilder's render failure reaches the OUTER Builder. Subquery has
// already written the nested SQL — a truncated `SELECT ` — into the outer
// stream by the time it returns, so dropping the error hands the caller a
// query that cannot parse and no indication why.
//
// Kills builder.go:2001:10 CONDITIONALS_NEGATION (`err != nil` -> `err == nil`)
// and 2001:26 CONDITIONALS_NEGATION (`b.err == nil` -> `b.err != nil`): under
// either the outer latch is never given its first value, and Build reports
// success.
func TestMutation_Subquery_PropagatesNestedError(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Subquery(failingSubquery())(b)
	_, _, err := b.Build()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a nested QueryBuilder's render error must reach the outer Builder, got %v", err)
	}
}

// TestMutation_Subquery_KeepsOuterFirstError pins the other half of the same
// guard: an outer Builder that has ALREADY failed keeps its own error when a
// spliced subquery fails too.
//
// Kills builder.go:2001:17 INVERT_LOGICAL (`&&` -> `||`), which fires on every
// non-nil nested error and so replaces the outer Builder's first error with
// the subquery's.
func TestMutation_Subquery_KeepsOuterFirstError(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(errExpr()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("seeding Expr must reject, got %v", err)
	}
	Subquery(failingSubquery())(b)
	_, _, err := b.Build()
	if err == nil {
		t.Fatalf("Build must surface the latched error")
	}
	if !strings.Contains(err.Error(), exprErrorText) || strings.Contains(err.Error(), otherErrorText) {
		t.Fatalf("Subquery must not overwrite the outer Builder's first error (%s), got %v", exprErrorText, err)
	}
}

// TestMutation_Spliced_PropagatesNestedError is
// TestMutation_Subquery_PropagatesNestedError for Spliced, whose guard is a
// verbatim copy carrying its own mutants.
//
// Kills builder.go:2023:10 CONDITIONALS_NEGATION (`err != nil` -> `err == nil`)
// and 2023:26 CONDITIONALS_NEGATION (`b.err == nil` -> `b.err != nil`).
func TestMutation_Spliced_PropagatesNestedError(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	Spliced(failingSubquery())(b)
	_, _, err := b.Build()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a spliced QueryBuilder's render error must reach the outer Builder, got %v", err)
	}
}

// TestMutation_Spliced_KeepsOuterFirstError is
// TestMutation_Subquery_KeepsOuterFirstError for Spliced.
//
// Kills builder.go:2023:17 INVERT_LOGICAL (`&&` -> `||`).
func TestMutation_Spliced_KeepsOuterFirstError(t *testing.T) {
	t.Parallel()

	b := NewBuilder()
	if err := b.Expr(errExpr()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("seeding Expr must reject, got %v", err)
	}
	Spliced(failingSubquery())(b)
	_, _, err := b.Build()
	if err == nil {
		t.Fatalf("Build must surface the latched error")
	}
	if !strings.Contains(err.Error(), exprErrorText) || strings.Contains(err.Error(), otherErrorText) {
		t.Fatalf("Spliced must not overwrite the outer Builder's first error (%s), got %v", exprErrorText, err)
	}
}

// TestMutation_LabelReplaceSegment_SrcErrorPropagates pins the same latch in
// its local, non-Builder form: labelReplaceSegment renders the source value
// inside a Frag closure (which cannot return) and stashes the failure in
// srcErr, returning it once the Frag has run. A dropped error here means a
// label_replace whose source expression cannot render emits
// `extractGroups(<nothing>, '…')[1]` and reports success.
//
// Kills builder.go:1116:33 CONDITIONALS_NEGATION (`err != nil` -> `err == nil`)
// and 1116:50 CONDITIONALS_NEGATION (`srcErr == nil` -> `srcErr != nil`):
// under either, srcErr never takes a non-nil value and the method returns nil.
func TestMutation_LabelReplaceSegment_SrcErrorPropagates(t *testing.T) {
	t.Parallel()

	l := &chplan.LabelReplace{
		Map:         errExpr(),
		Dst:         "dst",
		Src:         "src",
		Regex:       "(.*)",
		Replacement: "$1",
	}
	err := NewBuilder().labelReplaceSegment(l, chplan.LabelReplaceSegment{Group: 1}, "^(?:(.*))$")
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("an unrenderable source expression must surface as an error, got %v", err)
	}
}

// TestMutation_LabelReplaceSegment_RendersCaptureGroup pins the success shape
// so the assertion above cannot pass by rejecting every segment.
func TestMutation_LabelReplaceSegment_RendersCaptureGroup(t *testing.T) {
	t.Parallel()

	l := &chplan.LabelReplace{
		Map:         &chplan.ColumnRef{Name: "Attributes"},
		Dst:         "dst",
		Src:         "src",
		Regex:       "(.*)",
		Replacement: "$1",
	}
	b := NewBuilder()
	if err := b.labelReplaceSegment(l, chplan.LabelReplaceSegment{Group: 1}, "^(?:(.*))$"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sql, _, _ := b.Build()
	if !strings.HasPrefix(sql, "extractGroups(`Attributes`[?], ?)[?]") {
		t.Fatalf("a capture-group segment must render `extractGroups(<src>, <re>)[<n>]`, got %q", sql)
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// builder.go:1116:40 (INVERT_LOGICAL, `&&` -> `||` in labelReplaceSegment's
// srcErr latch) is EQUIVALENT. The mutated guard is
// `err != nil || srcErr == nil`, which differs from the original in exactly
// one state: srcErr already non-nil AND err non-nil, where the mutant
// overwrites srcErr with the later error instead of keeping the first. Every
// invocation of that closure calls `fb.srcValue(l)` on the SAME *LabelReplace
// value, and srcValue's only failure mode is `b.Expr(l.Map)`, whose verdict
// and message are a pure function of l.Map's concrete type — Builder.Expr
// neither reads Builder state when deciding nor mutates the expression it
// renders. So within one labelReplaceSegment call every non-nil error the
// closure can produce is a fresh value with an IDENTICAL message and an
// identical wrapped sentinel, and first vs. last is indistinguishable through
// the only channel the method exposes (its returned error). The remaining
// state pair, err == nil with srcErr == nil, assigns nil over nil under the
// mutant and is a no-op.
//
// Contrast Subquery / Spliced / Builder.Expr above, whose latch CAN be handed
// two errors from different sources — those `&&`s are killable and are killed.
