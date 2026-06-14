package tempo

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerTraceByID_WindowGate pins the trace_id_ts window pre-filter's
// emit under both gate states:
//
//   - OFF (default): today's plain `TraceId = ?` filter, byte-for-byte
//     unchanged from before the lever landed. No reference to the lookup
//     table, no Timestamp window.
//   - ON: the exact `TraceId = ?` Eq is KEPT and ANDed with a
//     Timestamp-window SUPERSET read from the `<spans>_trace_id_ts`
//     lookup MV via two scalar subqueries (min(Start) lower bound, padded
//     max(End) upper bound). The window can only widen the matched set, so
//     the result rows are identical to the OFF emit.
func TestLowerTraceByID_WindowGate(t *testing.T) {
	t.Parallel()

	emit := func(t *testing.T, enabled bool) string {
		t.Helper()
		sc := schema.DefaultOTelTraces()
		sc.TraceIDTsEnabled = enabled
		h := &Handler{Schema: sc}
		plan, err := h.lowerTraceByID("abc")
		if err != nil {
			t.Fatalf("lowerTraceByID: %v", err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		return sql
	}

	off := emit(t, false)
	if strings.Contains(off, "trace_id_ts") {
		t.Errorf("gate OFF must not reference the lookup table; got %s", off)
	}
	if strings.Contains(off, "Start") || strings.Contains(off, "addSeconds") {
		t.Errorf("gate OFF must not emit a Timestamp window; got %s", off)
	}
	if !strings.Contains(off, "`TraceId` = ?") {
		t.Errorf("gate OFF must keep the plain TraceId filter; got %s", off)
	}

	on := emit(t, true)
	// The exact TraceId Eq is still present (possibly promoted to
	// PREWHERE by the emitter — still ANDed, so rows must pass it).
	if !strings.Contains(on, "`TraceId` = ?") {
		t.Errorf("gate ON must keep the exact TraceId Eq ANDed; got %s", on)
	}
	// Lower bound reads min(Start) from the lookup table.
	if !strings.Contains(on, "otel_traces_trace_id_ts") {
		t.Errorf("gate ON must read the trace_id_ts lookup table; got %s", on)
	}
	if !strings.Contains(on, "min(`Start`)") {
		t.Errorf("gate ON must read min(Start) as the lower bound; got %s", on)
	}
	// Upper bound reads max(End), padded one second to compensate the
	// MV's DateTime second-flooring of the DateTime64(9) Timestamp.
	if !strings.Contains(on, "max(`End`)") {
		t.Errorf("gate ON must read max(End) as the upper bound; got %s", on)
	}
	if !strings.Contains(on, "addSeconds(") {
		t.Errorf("gate ON must pad the upper bound via addSeconds; got %s", on)
	}
	// Both bounds compare against the Timestamp column.
	if !strings.Contains(on, "`Timestamp` >=") || !strings.Contains(on, "`Timestamp` <=") {
		t.Errorf("gate ON must window the Timestamp column on both sides; got %s", on)
	}
}
