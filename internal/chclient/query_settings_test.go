package chclient

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// TestWithQuerySetting_CarrierMergeAndCopyOnWrite — the generalised
// per-request settings carrier accumulates multiple (name, value) settings,
// and each WithQuerySetting derives a fresh map so a child ctx never mutates
// a parent ctx's settings.
func TestWithQuerySetting_CarrierMergeAndCopyOnWrite(t *testing.T) {
	t.Parallel()

	base := context.Background()
	ctx1 := WithQuerySetting(base, "a", 1)
	ctx2 := WithQuerySetting(ctx1, "b", 2)

	// The parent ctx must NOT see the child's later addition (copy-on-write).
	parent := querySettingsFromContext(ctx1)
	if len(parent) != 1 || parent["a"] != 1 {
		t.Errorf("parent settings = %v; want exactly {a:1}", parent)
	}
	if _, leaked := parent["b"]; leaked {
		t.Errorf("parent settings leaked child key b: %v", parent)
	}

	child := querySettingsFromContext(ctx2)
	if child["a"] != 1 || child["b"] != 2 || len(child) != 2 {
		t.Errorf("child settings = %v; want {a:1, b:2}", child)
	}
}

// TestQuerySettings_GeneralisedCarrierCoexists — ts-grid (now one writer into
// the carrier) and an arbitrary second plan-shape-gated setting ride the same
// per-query settings map alongside the memory cap, none clobbering another.
func TestQuerySettings_GeneralisedCarrierCoexists(t *testing.T) {
	t.Parallel()

	c := &Client{maxMemory: 1 << 30}
	ctx := WithTSGridSetting(context.Background())
	ctx = WithQuerySetting(ctx, "optimize_aggregation_in_order", 1)

	s := c.querySettings(ctx)
	if s[SettingExperimentalTSGridAggregate] != 1 {
		t.Errorf("%s = %v; want 1", SettingExperimentalTSGridAggregate, s[SettingExperimentalTSGridAggregate])
	}
	if s["optimize_aggregation_in_order"] != 1 {
		t.Errorf("optimize_aggregation_in_order = %v; want 1", s["optimize_aggregation_in_order"])
	}
	if s["max_memory_usage"] != int64(1<<30) {
		t.Errorf("max_memory_usage = %v; want the cap (no clobber)", s["max_memory_usage"])
	}
	if len(s) != 3 {
		t.Errorf("settings carries %d entries (%v); want the three knobs", len(s), s)
	}
}

// tracedCtx returns a context carrying a valid active span context built from
// the supplied trace/span id bytes — the seam the otelhttp server span gives
// every real request.
func tracedCtx(tid trace.TraceID, sid trace.SpanID) context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	return trace.ContextWithSpanContext(context.Background(), sc)
}

// TestEnsureQueryID_NoTrace — an un-instrumented ctx yields "" (the driver
// generates its own id) and nothing is cached on the returned ctx.
func TestEnsureQueryID_NoTrace(t *testing.T) {
	t.Parallel()

	id, out := ensureQueryID(context.Background())
	if id != "" {
		t.Errorf("ensureQueryID(plain) = %q; want empty", id)
	}
	if got := queryIDFromContext(out); got != "" {
		t.Errorf("queryIDFromContext after no-trace ensure = %q; want empty", got)
	}
}

// TestEnsureQueryID_TracePrefix — a valid trace yields a non-empty id whose
// trace id is a greppable prefix (operators join query_log on `LIKE
// '<traceID>%'`), and the id is cached so queryIDFromContext returns the
// SAME value (consistency for any reader joining query_log).
func TestEnsureQueryID_TracePrefix(t *testing.T) {
	t.Parallel()

	tid := trace.TraceID{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	sid := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	ctx := tracedCtx(tid, sid)

	id, out := ensureQueryID(ctx)
	if id == "" {
		t.Fatal("ensureQueryID(traced) = empty; want a per-dispatch id")
	}
	if prefix := tid.String() + "-"; !strings.HasPrefix(id, prefix) {
		t.Errorf("query_id %q is not prefixed by %q (trace id must stay greppable)", id, prefix)
	}
	if got := queryIDFromContext(out); got != id {
		t.Errorf("queryIDFromContext = %q; want the cached %q (reader must see the stamped id)", got, id)
	}
}

// TestEnsureQueryID_UniquePerDispatch — many dispatches under the SAME trace,
// including from concurrent goroutines, each get a DISTINCT query_id (so
// concurrent CH queries never collide on ClickHouse code 216), and every id
// still carries the trace-id prefix.
func TestEnsureQueryID_UniquePerDispatch(t *testing.T) {
	t.Parallel()

	tid := trace.TraceID{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	sid := trace.SpanID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11}
	prefix := tid.String() + "-"

	const (
		workers      = 16
		perWorker    = 64
		wantDistinct = workers * perWorker
	)

	var (
		mu  sync.Mutex
		ids = make(map[string]struct{}, wantDistinct)
		wg  sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				// A fresh traced ctx per dispatch: the SAME trace/span, the
				// shape a fan-out produces (one trace, many concurrent CH
				// dispatches).
				id, _ := ensureQueryID(tracedCtx(tid, sid))
				if !strings.HasPrefix(id, prefix) {
					t.Errorf("query_id %q lost the trace prefix %q", id, prefix)
				}
				mu.Lock()
				ids[id] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(ids) != wantDistinct {
		t.Errorf("got %d distinct query_ids; want %d (concurrent dispatches must never collide)", len(ids), wantDistinct)
	}
}

// TestQueryContext_StampsAndCachesQueryID — queryContext derives a new ctx
// carrying a per-dispatch query_id even with no settings, and that id is the
// SAME value queryIDFromContext returns (the stamp and the reader agree).
func TestQueryContext_StampsAndCachesQueryID(t *testing.T) {
	t.Parallel()

	tid := trace.TraceID{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	sid := trace.SpanID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11}
	ctx := tracedCtx(tid, sid)

	// A bare client with no caps: queryContext must STILL derive a new ctx
	// (carrying the query_id) rather than returning the input unchanged.
	c := &Client{}
	out := c.queryContext(ctx)
	if out == ctx {
		t.Fatal("queryContext returned the input ctx unchanged; want a query_id-stamped ctx")
	}
	if got := queryIDFromContext(out); got == "" {
		t.Fatal("queryContext did not cache a query_id on the returned ctx")
	} else if !strings.HasPrefix(got, tid.String()+"-") {
		t.Errorf("cached query_id %q is not prefixed by the trace id", got)
	}
}
