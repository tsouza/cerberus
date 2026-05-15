package logql

import (
	"reflect"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
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
