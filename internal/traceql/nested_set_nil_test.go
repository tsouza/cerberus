package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/test/spec"
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
		// The four v=1 boundary rejections below pin the exact `v < 1`
		// guards in nonRootCmpConstant: position 1 is the smallest
		// real non-root position, so each of these predicates is true
		// for some non-root spans and false for others — un-lowerable
		// without materialised positions. A CONDITIONALS_BOUNDARY
		// mutant (`v < 1` → `v <= 1`) silently lowers each of them to
		// a wrong constant / root-ness predicate instead of erroring.
		{
			name:    "position_eq_one_needs_real_positions",
			query:   `{ nestedSetParent = 1 }`,
			wantSub: "nested-set positions",
		},
		{
			name:    "position_ne_one_needs_real_positions",
			query:   `{ nestedSetParent != 1 }`,
			wantSub: "nested-set positions",
		},
		{
			name:    "position_le_one_needs_real_positions",
			query:   `{ nestedSetParent <= 1 }`,
			wantSub: "nested-set positions",
		},
		{
			name:    "position_gt_one_needs_real_positions",
			query:   `{ nestedSetParent > 1 }`,
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

// TestLower_RootIdiomLiteralVariants pins the exact predicate every
// supported (op, literal) spelling of a nested-set parent comparison
// lowers to. Tempo evaluates root-ness as nestedSetParent == -1; spans
// of an unbuilt tree carry 0, which never matches a root test —
// cerberus's domain model is {-1} ∪ {>= 1}. Asserting the lowered
// chplan predicate (not just "lowers without error") is what
// distinguishes each comparison operator from its neighbours:
//   - evalIntCmp(-1, op, v) decides whether the predicate holds for
//     root spans — a CONDITIONALS_BOUNDARY / _NEGATION mutant there
//     flips `= -1` between (ParentSpanId = "") and constant false,
//     `<= -1` between (ParentSpanId = "") and constant false,
//     `> -1` between (ParentSpanId != "") and constant true, etc.
//     Only the v = -1 literals sit exactly on the root position, so
//     they are the cases where strict vs non-strict differ.
//   - nonRootCmpConstant's `v <= 1` guards for OpLt/OpGe make the
//     v = 1 literals (`< 1`, `>= 1`) lowerable root-ness tests; a
//     boundary mutant (`v <= 1` → `v < 1`) turns them into errors.
func TestLower_RootIdiomLiteralVariants(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	cases := []struct {
		query string
		// wantPredicate is the printed chplan Filter predicate:
		// (ParentSpanId = "") selects root spans, (ParentSpanId != "")
		// selects non-root spans, true/false are whole-table constants.
		wantPredicate string
	}{
		{`{ nestedSetParent < 0 }`, `(ParentSpanId = "")`},
		{`{ nestedSetParent <= -1 }`, `(ParentSpanId = "")`},
		{`{ nestedSetParent = -1 }`, `(ParentSpanId = "")`},
		{`{ nestedSetParent < 1 }`, `(ParentSpanId = "")`},
		{`{ nestedSetParent <= 0 }`, `(ParentSpanId = "")`},
		{`{ nestedSetParent >= 0 }`, `(ParentSpanId != "")`},
		{`{ nestedSetParent >= 1 }`, `(ParentSpanId != "")`},
		{`{ nestedSetParent > -1 }`, `(ParentSpanId != "")`},
		{`{ nestedSetParent > 0 }`, `(ParentSpanId != "")`},
		{`{ nestedSetParent != -1 }`, `(ParentSpanId != "")`},
		// Strictly below the root position: false for root (-1) and
		// for every non-root position (>= 1) — constant false.
		{`{ nestedSetParent < -1 }`, `false`},
		// At or above the root position: true for root and for every
		// non-root position — constant true.
		{`{ nestedSetParent >= -1 }`, `true`},
		// Position 0 never occurs in the domain {-1} ∪ {>= 1}.
		{`{ nestedSetParent = 0 }`, `false`},
		{`{ nestedSetParent != 0 }`, `true`},
		// Literal-on-the-left spelling flips the comparison.
		{`{ 0 > nestedSetParent }`, `(ParentSpanId = "")`},
		{`{ -1 >= nestedSetParent }`, `(ParentSpanId = "")`},
		{`{ -1 < nestedSetParent }`, `(ParentSpanId != "")`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v — root-ness idiom must lower", tc.query, err)
			}
			printed := spec.PrintChplan(plan)
			want := "Filter predicate=" + tc.wantPredicate + "\n"
			if !strings.Contains(printed, want) {
				t.Errorf("Lower(%q) plan:\n%s\nwant a filter with %s", tc.query, printed, want)
			}
		})
	}
}

// TestLower_NilComparisonRejections pins the nil-comparison forms
// reference Tempo itself rejects — and ONLY those. Reference
// validation (pkg/traceql/ast_validate.go UnaryOperation.validate)
// rejects `= nil` on every intrinsic and on resource.service.name;
// vparquet4's checkConditions errors on any childCount condition.
// Everything else — including `!= nil` on intrinsics, which the
// Traces Drilldown app stamps on every intrinsic breakdown groupBy —
// must lower (see TestLower_NilComparisonSemantics).
func TestLower_NilComparisonRejections(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	cases := []struct {
		name    string
		query   string
		wantSub string
	}{
		{
			name:    "intrinsic_eq_nil_kind",
			query:   `{ kind = nil }`,
			wantSub: "intrinsics cannot be nil",
		},
		{
			name:    "intrinsic_eq_nil_name",
			query:   `{ name = nil }`,
			wantSub: "intrinsics cannot be nil",
		},
		{
			name:    "intrinsic_eq_nil_literal_first",
			query:   `{ nil = status }`,
			wantSub: "intrinsics cannot be nil",
		},
		{
			name:    "resource_service_name_eq_nil",
			query:   `{ resource.service.name = nil }`,
			wantSub: "resource.service.name cannot be nil",
		},
		{
			name:    "child_count_not_nil",
			query:   `{ span:childCount != nil }`,
			wantSub: "child counts",
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

// TestLower_NilComparisonSemantics pins the lowered predicate for every
// accepted nil-comparison shape, derived from reference Tempo:
//
//   - `<intrinsic> != nil` is TRUE for every span (vparquet4 stores
//     intrinsics as required parquet columns and the span collector
//     adds the static unconditionally — kind=SPAN_KIND_UNSPECIFIED,
//     empty name, and unset status all still satisfy `!= nil`).
//   - attribute `!= nil` / `= nil` probe the scope's attribute map.
//   - event./link. attribute probes quantify over the Nested elements
//     (≥1 element carrying / lacking the key).
//   - nested intrinsics (event:name / link:traceID) probe element
//     presence (≥1 event / ≥1 link).
func TestLower_NilComparisonSemantics(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()
	cases := []struct {
		query         string
		wantPredicate string
	}{
		// Intrinsics: constant TRUE — pinned for each intrinsic the
		// Traces Drilldown breakdown tab group-bys on.
		{`{ kind != nil }`, `true`},
		{`{ status != nil }`, `true`},
		{`{ name != nil }`, `true`},
		{`{ duration != nil }`, `true`},
		{`{ statusMessage != nil }`, `true`},
		{`{ nil != kind }`, `true`},
		{`{ nestedSetParent != nil }`, `true`},
		// Attribute scopes: map-key existence on the scope carrier.
		{`{ span.http.method != nil }`, `mapContains(SpanAttributes, "http.method")`},
		{`{ resource.service.name != nil }`, `mapContains(ResourceAttributes, "service.name")`},
		{`{ span.http.method = nil }`, `not(mapContains(SpanAttributes, "http.method"))`},
		// Event / link attributes: per-element key probes.
		{`{ event.message != nil }`, `nestedArrayExists(Events.Attributes hasKey "message")`},
		{`{ event.message = nil }`, `nestedArrayExists(Events.Attributes lacksKey "message")`},
		{`{ link.opentracing.ref_type != nil }`, `nestedArrayExists(Links.Attributes hasKey "opentracing.ref_type")`},
		// Nested intrinsics: element presence.
		{`{ event:name != nil }`, `nestedArrayNotEmpty(Events.Name)`},
		{`{ link:traceID != nil }`, `nestedArrayNotEmpty(Links.TraceId)`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v — reference Tempo accepts this nil comparison", tc.query, err)
			}
			printed := spec.PrintChplan(plan)
			want := "Filter predicate=" + tc.wantPredicate + "\n"
			if !strings.Contains(printed, want) {
				t.Errorf("Lower(%q) plan:\n%s\nwant a filter with %s", tc.query, printed, want)
			}
		})
	}
}
