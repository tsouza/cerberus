package chclient

import (
	"context"
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

// TestQueryIDFromContext_TraceID — a valid active trace id becomes the
// CH query_id; an un-instrumented ctx yields "" (driver generates its own,
// never an error).
func TestQueryIDFromContext_TraceID(t *testing.T) {
	t.Parallel()

	if got := queryIDFromContext(context.Background()); got != "" {
		t.Errorf("queryIDFromContext(plain) = %q; want empty", got)
	}

	tid := trace.TraceID{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}
	sid := trace.SpanID{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	got := queryIDFromContext(ctx)
	if want := tid.String(); got != want {
		t.Errorf("queryIDFromContext(traced) = %q; want %q", got, want)
	}
}

// TestQueryContext_StampsQueryID — queryContext stamps the trace-derived
// query_id onto the dispatch context's ClickHouse QueryOptions even when no
// settings are configured, so the join-to-query_log id always rides.
func TestQueryContext_StampsQueryID(t *testing.T) {
	t.Parallel()

	tid := trace.TraceID{
		0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11,
		0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99,
	}
	sid := trace.SpanID{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00, 0x11}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)

	// A bare client with no caps: queryContext must STILL derive a new ctx
	// (carrying the query_id) rather than returning the input unchanged.
	c := &Client{}
	out := c.queryContext(ctx)
	if out == ctx {
		t.Fatal("queryContext returned the input ctx unchanged; want a query_id-stamped ctx")
	}
}
