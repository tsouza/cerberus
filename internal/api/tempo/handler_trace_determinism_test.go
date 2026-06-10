package tempo_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// shuffleProneTraceSamples builds a multi-resource span set engineered
// to expose any map-iteration / input-order dependence in the
// trace-by-ID assemblers:
//
//   - 8 distinct resource buckets (7 service.names; "dup" appears twice
//     with different extra resource attrs), so an assembler that emits
//     batches in Go map-iteration order has a 1-in-8! chance of
//     matching the sorted order on any given call.
//   - Input order is deliberately NOT the contract order: services are
//     interleaved, the earliest "frontend" span arrives last, and the
//     two same-timestamp "frontend" spans arrive with their SpanIDs in
//     descending order.
//
// This is the fixture the k3d e2e flake reduced to: run 27284868985
// saw /api/traces and /api/v2/traces return the SAME trace with the
// resource batches in DIFFERENT order (v1 led with "frontend", v2 with
// "api"), because groupBatches emitted its bucket map in iteration
// order.
func shuffleProneTraceSamples() []chclient.Sample {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	span := func(svc, name, spanID, parentID string, at time.Time, extraResource map[string]string) chclient.Sample {
		labels := map[string]string{
			"service.name":             svc,
			"__cerberus_traceID":       "f48694fee9f78da6f98ec5a8cd7d3274",
			"__cerberus_spanID":        spanID,
			"__cerberus_parentSpanID":  parentID,
			"__cerberus_spanKind":      "Server",
			"__cerberus_statusCode":    "Ok",
			"__cerberus_spanAttrsJSON": `{"http.method":"GET"}`,
		}
		for k, v := range extraResource {
			labels[k] = v
		}
		return chclient.Sample{
			MetricName: name,
			Labels:     labels,
			Timestamp:  at,
			Value:      1_000_000,
		}
	}
	return []chclient.Sample{
		// Same-timestamp pair, SpanIDs supplied in DESCENDING order so
		// the SpanID tie-break has to actually reorder them.
		span("frontend", "GET /b", "bbbb000000000002", "cccc000000000003", ts.Add(10*time.Millisecond), nil),
		span("queue", "publish", "1111000000000001", "", ts.Add(2*time.Millisecond), nil),
		span("frontend", "GET /a", "aaaa000000000001", "cccc000000000003", ts.Add(10*time.Millisecond), nil),
		span("api", "POST /v1", "2222000000000002", "", ts.Add(3*time.Millisecond), nil),
		span("dup", "dup-b", "3333000000000003", "", ts.Add(4*time.Millisecond), map[string]string{"zone": "b"}),
		span("backend", "work", "4444000000000004", "", ts.Add(5*time.Millisecond), nil),
		span("dup", "dup-a", "5555000000000005", "", ts.Add(6*time.Millisecond), map[string]string{"zone": "a"}),
		span("cache", "lookup", "6666000000000006", "", ts.Add(7*time.Millisecond), nil),
		span("db", "select", "7777000000000007", "", ts.Add(8*time.Millisecond), nil),
		// Earliest frontend span arrives LAST in input order, so the
		// StartTimeUnixNano sort has to move it to the front of its batch.
		span("frontend", "GET /c", "cccc000000000003", "", ts, nil),
	}
}

// TestGroupBatches_OrderContract pins the documented sort contract on
// the JSON assembler:
//
//   - batches sort by resource service.name, then by the canonical
//     resource-attribute string (so two "dup" services tie-break on
//     their remaining attrs);
//   - spans within a batch sort by StartTimeUnixNano, then SpanID.
func TestGroupBatches_OrderContract(t *testing.T) {
	t.Parallel()

	batches := tempo.GroupBatchesForTest(shuffleProneTraceSamples())

	wantServices := []string{"api", "backend", "cache", "db", "dup", "dup", "frontend", "queue"}
	if got, want := len(batches), len(wantServices); got != want {
		t.Fatalf("batch count=%d, want %d", got, want)
	}
	for i, b := range batches {
		if got := b.Resource.Attributes["service.name"]; got != wantServices[i] {
			t.Errorf("batch[%d] service.name=%q, want %q", i, got, wantServices[i])
		}
	}

	// The two "dup" batches tie-break on the canonical resource-attr
	// string: zone=a sorts before zone=b.
	if got := batches[4].Resource.Attributes["zone"]; got != "a" {
		t.Errorf("batch[4] (first dup) zone=%q, want %q", got, "a")
	}
	if got := batches[5].Resource.Attributes["zone"]; got != "b" {
		t.Errorf("batch[5] (second dup) zone=%q, want %q", got, "b")
	}

	// Frontend batch: earliest span first (despite arriving last in
	// input order), then the same-timestamp pair ordered by SpanID.
	frontend := batches[6]
	wantSpanIDs := []string{"cccc000000000003", "aaaa000000000001", "bbbb000000000002"}
	if got, want := len(frontend.Spans), len(wantSpanIDs); got != want {
		t.Fatalf("frontend span count=%d, want %d", got, want)
	}
	for i, sp := range frontend.Spans {
		if sp.SpanID != wantSpanIDs[i] {
			t.Errorf("frontend span[%d] SpanID=%q, want %q", i, sp.SpanID, wantSpanIDs[i])
		}
	}
}

// TestGroupBatchesProto_OrderContract pins the same contract on the
// proto assembler — both siblings consume groupTraceBatches, so the
// orders must be identical between the two wire shapes.
func TestGroupBatchesProto_OrderContract(t *testing.T) {
	t.Parallel()

	trace := tempo.GroupBatchesProtoForTest(shuffleProneTraceSamples())

	wantServices := []string{"api", "backend", "cache", "db", "dup", "dup", "frontend", "queue"}
	if got, want := len(trace.ResourceSpans), len(wantServices); got != want {
		t.Fatalf("ResourceSpans count=%d, want %d", got, want)
	}
	for i, rs := range trace.ResourceSpans {
		var svc string
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == "service.name" {
				svc = kv.Value.GetStringValue()
			}
		}
		if svc != wantServices[i] {
			t.Errorf("ResourceSpans[%d] service.name=%q, want %q", i, svc, wantServices[i])
		}
	}

	// Frontend bucket span order matches the JSON path: start time
	// ascending, SpanID tie-break. Proto carries raw bytes; the hex
	// fixture IDs have no leading zeros so the byte order mirrors the
	// hex-string order.
	frontend := trace.ResourceSpans[6].ScopeSpans[0].Spans
	wantSpanIDs := [][]byte{
		{0xcc, 0xcc, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03},
		{0xaa, 0xaa, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
		{0xbb, 0xbb, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02},
	}
	if got, want := len(frontend), len(wantSpanIDs); got != want {
		t.Fatalf("frontend span count=%d, want %d", got, want)
	}
	for i, sp := range frontend {
		if !bytes.Equal(sp.SpanId, wantSpanIDs[i]) {
			t.Errorf("frontend span[%d] SpanId=%x, want %x", i, sp.SpanId, wantSpanIDs[i])
		}
	}
}

// TestGroupBatches_RepeatedAssemblyIsByteIdentical assembles the same
// multi-resource span set 50 times through BOTH wire paths and asserts
// every serialized output is byte-identical to the first. Before the
// groupTraceBatches sort landed, the JSON path emitted batches in Go
// map-iteration order, so two sequential fetches of the same trace
// intermittently differed — the retry-masked "v2 is a byte-for-byte
// alias of v1" e2e flake in k3d run 27284868985.
func TestGroupBatches_RepeatedAssemblyIsByteIdentical(t *testing.T) {
	t.Parallel()

	samples := shuffleProneTraceSamples()

	firstJSON, err := json.Marshal(tempo.TraceByIDResponse{Batches: tempo.GroupBatchesForTest(samples)})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	firstProto, err := proto.Marshal(tempo.GroupBatchesProtoForTest(samples))
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}

	for i := 1; i < 50; i++ {
		gotJSON, err := json.Marshal(tempo.TraceByIDResponse{Batches: tempo.GroupBatchesForTest(samples)})
		if err != nil {
			t.Fatalf("iteration %d: json.Marshal: %v", i, err)
		}
		if !bytes.Equal(gotJSON, firstJSON) {
			t.Fatalf("iteration %d: JSON body diverged from iteration 0:\n first=%s\n   got=%s", i, firstJSON, gotJSON)
		}
		gotProto, err := proto.Marshal(tempo.GroupBatchesProtoForTest(samples))
		if err != nil {
			t.Fatalf("iteration %d: proto.Marshal: %v", i, err)
		}
		if !bytes.Equal(gotProto, firstProto) {
			t.Fatalf("iteration %d: proto body diverged from iteration 0 (first=%x got=%x)", i, firstProto, gotProto)
		}
	}
}

// TestGroupBatches_InputOrderIndependent reverses the sample slice and
// asserts both wire outputs are byte-identical to the forward-order
// assembly. The CH result-row order is an implementation detail of the
// scan; the assembled trace must not depend on it.
func TestGroupBatches_InputOrderIndependent(t *testing.T) {
	t.Parallel()

	forward := shuffleProneTraceSamples()
	reversed := make([]chclient.Sample, len(forward))
	for i, s := range forward {
		reversed[len(forward)-1-i] = s
	}

	fwdJSON, err := json.Marshal(tempo.TraceByIDResponse{Batches: tempo.GroupBatchesForTest(forward)})
	if err != nil {
		t.Fatalf("json.Marshal forward: %v", err)
	}
	revJSON, err := json.Marshal(tempo.TraceByIDResponse{Batches: tempo.GroupBatchesForTest(reversed)})
	if err != nil {
		t.Fatalf("json.Marshal reversed: %v", err)
	}
	if !bytes.Equal(fwdJSON, revJSON) {
		t.Fatalf("JSON body depends on input row order:\n forward=%s\nreversed=%s", fwdJSON, revJSON)
	}

	fwdProto, err := proto.Marshal(tempo.GroupBatchesProtoForTest(forward))
	if err != nil {
		t.Fatalf("proto.Marshal forward: %v", err)
	}
	revProto, err := proto.Marshal(tempo.GroupBatchesProtoForTest(reversed))
	if err != nil {
		t.Fatalf("proto.Marshal reversed: %v", err)
	}
	if !bytes.Equal(fwdProto, revProto) {
		t.Fatalf("proto body depends on input row order (forward=%x reversed=%x)", fwdProto, revProto)
	}
}
