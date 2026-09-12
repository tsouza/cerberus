package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestNativeTSGridMatrixProductionRecollapseHasClosedChild(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	s.AggregationTemporalityColumn = ""
	expr, err := parser.NewParser(parser.Options{}).ParseExpr("rate(foo[5m])")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_700_000_000, 0).UTC()
	plan, err := LowerAtRangeOpts(context.Background(), expr, s, start, start.Add(5*time.Minute), time.Minute, LowerOpts{Lowerers: RangeLowerers{Rate: NativeRateLowerer{Fallback: FanoutRateLowerer{}, Recollapse: true}}})
	if err != nil {
		t.Fatal(err)
	}
	native, ok := plan.(*chplan.RangeWindowGridNative)
	if !ok {
		t.Fatalf("plan = %T", plan)
	}
	if len(native.Recollapse) == 0 {
		t.Fatal("recollapse did not activate")
	}
	if _, _, err := chsql.Emit(context.Background(), native); err != nil {
		t.Fatal(err)
	}
}
