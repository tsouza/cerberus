package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// prewhereByServerPlan is a Filter(Scan) the emitter's own split promotes:
// the projection reads the wide Body column and both conjuncts are cheap and
// narrow, so under the zero strategies the first lands in PREWHERE.
func prewhereByServerPlan() chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_logs", Columns: []string{"Body"}},
		Predicate: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.Binary{Op: chplan.OpGt, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: time.Unix(0, 0).UnixNano()}},
			Right: &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: "SeverityNumber"}, Right: &chplan.LitInt{V: 9}},
		},
	}
}

func emitUnder(t *testing.T, s chsql.SchemaStrategies, plan chplan.Node) string {
	t.Helper()
	sql, _, err := chsql.Emit(chsql.WithSchemaStrategies(context.Background(), s), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sql
}

// TestEmit_PrewhereByServer_LeavesTheSplitToTheServer pins both sides of
// SchemaStrategies.PrewhereByServer on a plain Filter(Scan): the zero value
// keeps the emitter's PREWHERE, and PrewhereByServer writes no PREWHERE and
// keeps every conjunct, in the emitter's order, in WHERE.
func TestEmit_PrewhereByServer_LeavesTheSplitToTheServer(t *testing.T) {
	t.Parallel()

	own := emitUnder(t, chsql.SchemaStrategies{}, prewhereByServerPlan())
	if !strings.Contains(own, "PREWHERE (`Timestamp` > ?) WHERE (`SeverityNumber` >= ?)") {
		t.Fatalf("zero strategies: want the emitter's own PREWHERE split in:\n%s", own)
	}

	byServer := emitUnder(t, chsql.SchemaStrategies{PrewhereByServer: true}, prewhereByServerPlan())
	if strings.Contains(byServer, "PREWHERE") {
		t.Fatalf("PrewhereByServer: want no PREWHERE, got:\n%s", byServer)
	}
	if !strings.Contains(byServer, "WHERE (`Timestamp` > ?) AND (`SeverityNumber` >= ?)") {
		t.Fatalf("PrewhereByServer: want both conjuncts in WHERE in order, got:\n%s", byServer)
	}
}

// TestEmit_PrewhereByServer_FusedIntersect pins the `&&` single-pass gate,
// which assembles its scan through the same split: under PrewhereByServer the
// arms' disjunction joins the shared window in WHERE, and the trace gate is
// unchanged.
func TestEmit_PrewhereByServer_FusedIntersect(t *testing.T) {
	t.Parallel()

	// The arms read the wide SpanAttributes map, which is what makes the
	// emitter's own split promote the window.
	arm := func(name string) chplan.Node {
		scan := setOpSchemaScan("otel_traces", "TraceId", "SpanId")
		scan.Columns = append(scan.Columns, "SpanAttributes")
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input:     scan,
				Predicate: &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: "Timestamp"}, Right: &chplan.LitInt{V: setOpWindowEpoch}},
			},
			Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanName"}, Right: &chplan.LitString{V: name}},
		}
	}
	plan := fusedIntersect(arm("a"), arm("b"))
	own := emitUnder(t, chsql.SchemaStrategies{}, plan)
	if !strings.Contains(own, "PREWHERE ((`SpanName` = ?) OR (`SpanName` = ?)) WHERE (`Timestamp` >= ?)") {
		t.Fatalf("zero strategies: want the emitter's own PREWHERE split in:\n%s", own)
	}

	byServer := emitUnder(t, chsql.SchemaStrategies{PrewhereByServer: true}, plan)
	if strings.Contains(byServer, "PREWHERE") {
		t.Fatalf("PrewhereByServer: want no PREWHERE in the fused scan, got:\n%s", byServer)
	}
	if !strings.Contains(byServer, "WHERE ((`SpanName` = ?) OR (`SpanName` = ?)) AND (`Timestamp` >= ?)") {
		t.Fatalf("PrewhereByServer: want the disjunction and the window together in WHERE, got:\n%s", byServer)
	}
	const gate = "QUALIFY max((`SpanName` = ?)) OVER (PARTITION BY `TraceId`)"
	if !strings.Contains(byServer, gate) {
		t.Fatalf("PrewhereByServer: want the trace gate %q unchanged in:\n%s", gate, byServer)
	}
}

// TestSchemaStrategiesCell_PrewhereByServer pins that the cell stamps its
// PrewhereByServer on every signal at Load, whatever it holds, including
// strategies a re-probe stores after it was set.
func TestSchemaStrategiesCell_PrewhereByServer(t *testing.T) {
	t.Parallel()

	c := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	got := c.Load()
	if got.Metrics.PrewhereByServer || got.Logs.PrewhereByServer || got.Traces.PrewhereByServer {
		t.Fatalf("unset cell loads PrewhereByServer on some signal: %+v", got)
	}

	c.SetPrewhereByServer(true)
	c.Store(chsql.ResolvedSchemaStrategies{Logs: chsql.SchemaStrategies{SumTimestampTimezone: "UTC"}})
	got = c.Load()
	if !got.Metrics.PrewhereByServer || !got.Logs.PrewhereByServer || !got.Traces.PrewhereByServer {
		t.Fatalf("set cell after a Store loads PrewhereByServer=false on some signal: %+v", got)
	}
	if got.Logs.SumTimestampTimezone != "UTC" {
		t.Fatalf("Load lost the stored strategies: %+v", got)
	}
	for _, signal := range []chsql.Signal{chsql.SignalMetrics, chsql.SignalLogs, chsql.SignalTraces} {
		s, _ := chsql.SchemaStrategiesFromContext(c.WithRequestStrategies(context.Background(), signal))
		if !s.PrewhereByServer {
			t.Errorf("signal %d: the request snapshot dropped PrewhereByServer", signal)
		}
	}

	c.SetPrewhereByServer(false)
	if got := c.Load(); got.Metrics.PrewhereByServer || got.Logs.PrewhereByServer || got.Traces.PrewhereByServer {
		t.Fatalf("cleared cell still loads PrewhereByServer: %+v", got)
	}
}
