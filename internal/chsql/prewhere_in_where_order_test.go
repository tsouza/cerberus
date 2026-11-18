package chsql

import "testing"

// The cell stamps its PrewhereInWhereOrder on every signal at Load, whatever
// it holds, including strategies a re-probe stores after it was set.
func TestSchemaStrategiesCell_PrewhereInWhereOrder(t *testing.T) {
	t.Parallel()

	c := NewSchemaStrategiesCell(ResolvedSchemaStrategies{})
	if got := c.Load(); got.Metrics.PrewhereInWhereOrder || got.Logs.PrewhereInWhereOrder || got.Traces.PrewhereInWhereOrder {
		t.Fatalf("unset cell loads PrewhereInWhereOrder on some signal: %+v", got)
	}
	c.SetPrewhereInWhereOrder(true)
	c.Store(ResolvedSchemaStrategies{Logs: SchemaStrategies{SumTimestampTimezone: "UTC"}})
	got := c.Load()
	if !got.Metrics.PrewhereInWhereOrder || !got.Logs.PrewhereInWhereOrder || !got.Traces.PrewhereInWhereOrder {
		t.Fatalf("set cell after a Store loads PrewhereInWhereOrder=false on some signal: %+v", got)
	}
	if got.Metrics.PrewhereByServer || got.Metrics.CanonicalMapTypeCast || got.Logs.SumTimestampTimezone != "UTC" {
		t.Fatalf("Load changed fields other than PrewhereInWhereOrder: %+v", got)
	}
	c.SetPrewhereInWhereOrder(false)
	if got := c.Load(); got.Metrics.PrewhereInWhereOrder || got.Logs.PrewhereInWhereOrder || got.Traces.PrewhereInWhereOrder {
		t.Fatalf("cleared cell still loads PrewhereInWhereOrder: %+v", got)
	}
}
