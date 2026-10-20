package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerStartTimestamp_PlansTheArgumentFilteredEmpty pins that
// start_timestamp(v) is answered by ClickHouse: the argument is planned
// and read, and a constant-false filter empties it, for instant and range
// queries alike.
func TestLowerStartTimestamp_PlansTheArgumentFilteredEmpty(t *testing.T) {
	t.Parallel()
	end := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	for _, step := range []time.Duration{0, time.Minute} {
		start := end
		if step > 0 {
			start = end.Add(-5 * time.Minute)
		}
		plan, err := LowerAtRange(context.Background(), parseExperimental(t, "start_timestamp(cpu)"), schema.DefaultOTelMetrics(), start, end, step)
		if err != nil {
			t.Fatalf("LowerAtRange(step=%s): %v", step, err)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(step=%s): %v", step, err)
		}
		// The outermost clause is the filter, its predicate bound as the
		// last parameter.
		if !strings.HasSuffix(sql, " WHERE ?") || len(args) == 0 || args[len(args)-1] != false {
			t.Fatalf("step=%s: start_timestamp SQL does not end in a constant-false filter (last arg %v):\n%s", step, args[len(args)-1], sql)
		}
		if !containsArg(args, "cpu") {
			t.Fatalf("step=%s: start_timestamp SQL does not read its argument cpu: args %v", step, args)
		}
	}
}

// TestLowerStartTimestamp_RangeVectorArgumentDoesNotParse pins the
// reference's rejection of a range-vector argument, which the parser
// enforces before lowering.
func TestLowerStartTimestamp_RangeVectorArgumentDoesNotParse(t *testing.T) {
	t.Parallel()
	_, err := promParserForTest().ParseExpr("start_timestamp(cpu[5m])")
	if err == nil || !strings.Contains(err.Error(), "expected type instant vector") {
		t.Fatalf("start_timestamp(cpu[5m]) parse error = %v, want the reference's instant-vector type error", err)
	}
}

func containsArg(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}
