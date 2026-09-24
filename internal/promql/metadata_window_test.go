package promql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerMetadataRange_AppliesTheWindow pins wrapMetadataFullRange's
// closed [start, end] window: with both bounds set, the enumeration scan
// is filtered on both, so a series with no sample inside the window is not
// listed.
func TestLowerMetadataRange_AppliesTheWindow(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(`{__name__=~"up|down"}`)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := LowerMetadataRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, bound := range []string{"`TimeUnix` >= ", "`TimeUnix` <= "} {
		if !strings.Contains(sql, bound) {
			t.Errorf("metadata lowering over [start, end] has no %q bound:\n%s", bound, sql)
		}
	}
}
