package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// The happy paths for the nested-set root idiom and nil comparisons are
// pinned by TXTAR fixtures (nested_set_parent_root / _nonroot,
// attr_not_nil / attr_eq_nil). This file pins the REJECTION surface:
// every comparison that would need real nested-set positions (which the
// OTel-CH schema does not materialise) must fail at lower time with a
// descriptive error rather than mis-lower to a SpanAttributes map
// lookup (the pre-fix behaviour, which produced a ClickHouse
// `Cannot parse Float64 from String` execution error on every Traces
// Drilldown query — Grafana 12.x stamps `nestedSetParent<0` on each).
func TestLower_NestedSetUnsupportedShapes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	cases := []struct {
		name    string
		query   string
		wantSub string
	}{
		{
			name:    "position_equality_needs_real_positions",
			query:   `{ nestedSetParent = 5 }`,
			wantSub: "nested-set positions",
		},
		{
			name:    "position_range_needs_real_positions",
			query:   `{ nestedSetParent > 2 }`,
			wantSub: "nested-set positions",
		},
		{
			name:    "nested_set_left_unsupported",
			query:   `{ nestedSetLeft > 0 }`,
			wantSub: "unsupported",
		},
		{
			name:    "nested_set_right_unsupported",
			query:   `{ nestedSetRight > 0 }`,
			wantSub: "unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			_, err = traceql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q) succeeded; want error containing %q", tc.query, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Lower(%q) error %q does not contain %q", tc.query, err, tc.wantSub)
			}
		})
	}
}

// TestLower_RootIdiomLiteralVariants pins that every literal spelling
// of the root-span test lowers (Tempo evaluates root-ness as
// nestedSetParent == -1; spans of an unbuilt tree carry 0, which never
// matches a root test — cerberus's domain model is {-1} ∪ {>= 1}).
func TestLower_RootIdiomLiteralVariants(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	for _, query := range []string{
		`{ nestedSetParent < 0 }`,
		`{ nestedSetParent <= -1 }`,
		`{ nestedSetParent = -1 }`,
		`{ nestedSetParent >= 0 }`,
		`{ nestedSetParent != -1 }`,
		// Literal-on-the-left spelling flips the comparison.
		`{ 0 > nestedSetParent }`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", query, err)
			}
			if _, err := traceql.Lower(context.Background(), expr, s); err != nil {
				t.Errorf("Lower(%q): %v — root-ness idiom must lower", query, err)
			}
		})
	}
}

// TestLower_NilComparisonRejections pins the unsupported nil-comparison
// operands: intrinsics (always materialised in OTel-CH) and link/event
// scopes (Nested columns need the arrayExists shape, not mapContains).
func TestLower_NilComparisonRejections(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	cases := []struct {
		name    string
		query   string
		wantSub string
	}{
		{
			name:    "intrinsic_always_present",
			query:   `{ name != nil }`,
			wantSub: "intrinsic",
		},
		{
			name:    "event_scope_unsupported",
			query:   `{ event.message != nil }`,
			wantSub: "unsupported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			_, err = traceql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q) succeeded; want error containing %q", tc.query, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Lower(%q) error %q does not contain %q", tc.query, err, tc.wantSub)
			}
		})
	}
}
