package ast

import (
	"strings"
	"testing"
)

func TestParseLenientRemovesIncompleteMatchers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		query      string
		want       string
		wantAbsent string
	}{
		{
			name:       "only incomplete matcher",
			query:      `{ resource.cluster = }`,
			want:       "true",
			wantAbsent: "cluster",
		},
		{
			name:       "valid conjunct survives",
			query:      `{ span.http.method = "GET" && resource.cluster = }`,
			want:       "http.method",
			wantAbsent: "cluster",
		},
		{
			name:       "multiple incomplete matchers",
			query:      `{ span.partial = || resource.cluster = }`,
			want:       "true",
			wantAbsent: "partial",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tc.query); err == nil {
				t.Fatalf("strict Parse(%q) succeeded; autocomplete leniency leaked into the language parser", tc.query)
			}
			expr, err := ParseLenient(tc.query)
			if err != nil {
				t.Fatalf("ParseLenient(%q): %v", tc.query, err)
			}
			rendered := expr.String()
			if !strings.Contains(rendered, tc.want) {
				t.Fatalf("ParseLenient(%q) = %q, want fragment %q", tc.query, rendered, tc.want)
			}
			if strings.Contains(rendered, tc.wantAbsent) {
				t.Fatalf("ParseLenient(%q) = %q, incomplete matcher %q survived", tc.query, rendered, tc.wantAbsent)
			}
		})
	}
}

func TestParseLenientKeepsUnrelatedSyntaxErrors(t *testing.T) {
	t.Parallel()
	if _, err := ParseLenient("{{{"); err == nil {
		t.Fatal("ParseLenient accepted an unrelated malformed query")
	}
}

func TestReplaceIncompleteMatchersRequiresOperandAndRightBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		tokens      []token
		wantChanged bool
		wantKinds   []tokenKind
	}{
		{
			name:      "comparison cannot be its own operand",
			tokens:    []token{{kind: tokOpenBrace}, {kind: tokEq}, {kind: tokCloseBrace}},
			wantKinds: []tokenKind{tokOpenBrace, tokEq, tokCloseBrace},
		},
		{
			name:      "right operand is not incomplete",
			tokens:    []token{{kind: tokName}, {kind: tokEq}, {kind: tokInteger}},
			wantKinds: []tokenKind{tokName, tokEq, tokInteger},
		},
		{
			name:      "trailing comparison has no boundary token",
			tokens:    []token{{kind: tokName}, {kind: tokEq}},
			wantKinds: []tokenKind{tokName, tokEq},
		},
		{
			name:        "operand may start the token slice",
			tokens:      []token{{kind: tokName}, {kind: tokEq}, {kind: tokCloseBrace}},
			wantChanged: true,
			wantKinds:   []tokenKind{tokTrue, tokCloseBrace},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, changed := replaceIncompleteMatchers(tc.tokens)
			if changed != tc.wantChanged {
				t.Fatalf("replaceIncompleteMatchers changed = %v, want %v", changed, tc.wantChanged)
			}
			if len(got) != len(tc.wantKinds) {
				t.Fatalf("replaceIncompleteMatchers returned %d tokens, want %d: %+v", len(got), len(tc.wantKinds), got)
			}
			for i, want := range tc.wantKinds {
				if got[i].kind != want {
					t.Fatalf("replaceIncompleteMatchers token %d = %v, want %v", i, got[i].kind, want)
				}
			}
		})
	}
}

// TestReplaceIncompleteMatchersScansPastAnUnrepairableComparison pins that a
// comparison with no operand to its left — one that sits directly on a matcher
// start boundary, so the backward scan lands on the comparison itself — is
// SKIPPED rather than ending the scan. Turning that `continue` into a `break`
// abandons every later token, so the genuinely incomplete matcher after it is
// left in place and the lenient parse reports no change at all.
func TestReplaceIncompleteMatchersScansPastAnUnrepairableComparison(t *testing.T) {
	t.Parallel()
	// `{ = && .x = }`: the first `=` has no left operand; the second one has.
	toks := []token{
		{kind: tokOpenBrace},
		{kind: tokEq},
		{kind: tokAnd},
		{kind: tokName},
		{kind: tokEq},
		{kind: tokCloseBrace},
	}
	want := []tokenKind{tokOpenBrace, tokEq, tokAnd, tokTrue, tokCloseBrace}

	got, changed := replaceIncompleteMatchers(toks)
	if !changed {
		t.Fatalf("replaceIncompleteMatchers changed = false, want true: the second matcher is repairable")
	}
	if len(got) != len(want) {
		t.Fatalf("replaceIncompleteMatchers returned %d tokens, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].kind != w {
			t.Fatalf("replaceIncompleteMatchers token %d = %v, want %v", i, got[i].kind, w)
		}
	}
}

// NOT KILLABLE — documented, not defended by a test.
//
// parse.go:`expr == nil || len(expr.Pipeline.Elements) == 0`
// (INVERT_LOGICAL, `||` -> `&&`). Both operands are always false
// together, so neither form ever enters the branch. parseTokens returns a nil
// expr only alongside a non-nil error, which ParseIdentifier has already
// returned on; on the success path parseRoot returns the result of
// newRootExpr, newRootExprWithMetrics or newRootExprWithMetricsTwoStage,
// each of which is non-nil and sets Pipeline through asPipeline. asPipeline
// either wraps a bare element in a one-element Pipeline or passes through a
// Pipeline built from a slice seeded with a parsed first stage — non-empty in
// both cases, and inductively so for nested parenthesised pipelines. The
// guard is a post-condition check on that internal contract.
