package logql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestIncludeLabelsFromBinop pins the surface of [includeLabelsFromBinop]:
// the helper returns the labels declared inside `group_left(...)` /
// `group_right(...)` of a binop's VectorMatching, and returns an empty
// (non-nil) slice when no Include list is present.
//
// Pre-threading work for #393 (LogQL include-labels through aggregation
// lowering).
func TestIncludeLabelsFromBinop(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "bare op has no include labels",
			query: `sum(rate({app="api"}[1m])) + sum(rate({app="db"}[1m]))`,
			want:  []string{},
		},
		{
			name:  "group_left single label",
			query: `sum by (svc) (rate({app="api"}[1m])) * on (svc) group_left(env) sum by (svc, env) (rate({app="meta"}[1m]))`,
			want:  []string{"env"},
		},
		{
			name:  "group_left multiple labels",
			query: `sum by (svc) (rate({app="api"}[1m])) * on (svc) group_left(env, region) sum by (svc, env, region) (rate({app="meta"}[1m]))`,
			want:  []string{"env", "region"},
		},
		{
			name:  "group_right with ignoring",
			query: `sum by (svc, level) (rate({app="meta"}[1m])) * ignoring(level) group_right(svc) sum by (svc) (rate({app="api"}[1m]))`,
			want:  []string{"svc"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			binop, ok := expr.(*syntax.BinOpExpr)
			if !ok {
				t.Fatalf("ParseExpr(%q): expected *syntax.BinOpExpr, got %T", tc.query, expr)
			}

			got := includeLabelsFromBinop(binop)
			if got == nil {
				t.Fatalf("includeLabelsFromBinop returned nil slice; want non-nil")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("includeLabelsFromBinop = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIncludeLabelsFromBinopNil guards the defensive nil-input path so
// future call sites can pass a possibly-nil binop without panicking.
func TestIncludeLabelsFromBinopNil(t *testing.T) {
	t.Parallel()

	got := includeLabelsFromBinop(nil)
	if got == nil {
		t.Fatalf("includeLabelsFromBinop(nil) returned nil; want empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("includeLabelsFromBinop(nil) = %v, want empty slice", got)
	}
}

// TestLowerBinaryRejectsNilLeg pins the defensive guard at the top of
// [lowerBinary]: when *either* the LHS (SampleExpr) or the RHS leg of a
// BinOpExpr is nil — a shape the upstream parser shouldn't normally
// hand us — the helper returns a clear error instead of dereferencing
// the nil leg and panicking.
//
// The guard reads `if b.SampleExpr == nil || b.RHS == nil`; a single-leg
// nil must trip it. Parse a real binop first so the rest of the struct
// (Op, Opts) stays parser-valid, then drop one leg to nil and confirm
// the helper rejects it.
func TestLowerBinaryRejectsNilLeg(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*syntax.BinOpExpr)
	}{
		{
			name: "nil RHS",
			mut:  func(b *syntax.BinOpExpr) { b.RHS = nil },
		},
		{
			name: "nil LHS (SampleExpr)",
			mut:  func(b *syntax.BinOpExpr) { b.SampleExpr = nil },
		},
	}

	s := schema.DefaultOTelLogs()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := syntax.ParseExpr(`rate({app="api"}[1m]) + rate({app="db"}[1m])`)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			binop, ok := expr.(*syntax.BinOpExpr)
			if !ok {
				t.Fatalf("expected *syntax.BinOpExpr, got %T", expr)
			}
			tc.mut(binop)

			_, err = lowerBinary(binop, s, lowerCtx{})
			if err == nil {
				t.Fatalf("lowerBinary with %s: want error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), "nil leg") {
				t.Fatalf("lowerBinary with %s: error %q does not mention 'nil leg'", tc.name, err.Error())
			}
		})
	}
}
