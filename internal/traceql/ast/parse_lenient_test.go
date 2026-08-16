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
