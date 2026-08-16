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
