package tempo_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/tempopb"

	"github.com/tsouza/cerberus/internal/chclient"
)

// These tests are the regression gate for the Grafana 12 trace-by-id
// outage: cerberus's `/api/v2/traces/{id}` used to return the SAME
// bytes as the v1 endpoint (a bare proto-encoded *tempopb.Trace), but
// reference Tempo's v2 endpoint wraps the trace in a
// tempopb.TraceByIDResponse envelope (`trace` field 1 + `metrics`
// field 2 + partial-trace `status`/`message`; see upstream
// modules/frontend/combiner/trace_by_id_v2.go and pkg/tempopb/
// tempo.proto). Grafana 12.x's Tempo plugin unmarshals the v2 body as
// TraceByIDResponse before converting to OTLP, so the un-enveloped
// bytes misaligned one message level deep and died inside a KeyValue
// (`proto: KeyValue: wiretype end group for non-group` → "An error
// occurred within the plugin" in Explore).
//
// The contract pinned here:
//
//   - v2 proto body unmarshals as tempopb.TraceByIDResponse, with a
//     non-nil Trace and a non-nil (zero) Metrics block.
//   - v1 proto body unmarshals as a bare tempopb.Trace.
//   - the v2 envelope's inner trace is byte-identical to the v1 body
//     (the envelope is the ONLY divergence between the endpoints).
//   - v2 JSON is the jsonpb envelope `{"trace":…,"metrics":{}}`, not
//     the flattened v1 `{"batches":…}` shape.

// traceByIDFixtureSamples builds a small deterministic two-service
// trace through the reserved-key label contract the engine's
// wrap-projection emits (see splitTraceByIDLabels).
func traceByIDFixtureSamples() []chclient.Sample {
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	span := func(svc, name, spanID, parentID string, at time.Time) chclient.Sample {
		return chclient.Sample{
			MetricName: name,
			Labels: map[string]string{
				"service.name":             svc,
				"__cerberus_traceID":       "f48694fee9f78da6f98ec5a8cd7d3274",
				"__cerberus_spanID":        spanID,
				"__cerberus_parentSpanID":  parentID,
				"__cerberus_spanKind":      "Server",
				"__cerberus_statusCode":    "Ok",
				"__cerberus_spanAttrsJSON": `{"http.method":"GET"}`,
			},
			Timestamp: at,
			Value:     1_500_000,
		}
	}
	return []chclient.Sample{
		span("frontend", "GET /", "aaaa000000000001", "", ts),
		span("backend", "SELECT users", "bbbb000000000002", "aaaa000000000001", ts.Add(2*time.Millisecond)),
	}
}

func getTraceByID(t *testing.T, path, accept string) (*http.Response, []byte) {
	t.Helper()
	q := &stubQuerier{samples: traceByIDFixtureSamples()}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp, body
}

// TestTraceByIDV2_ProtoEnvelope pins the v2 proto wire shape end to
// end: the body must decode as the TraceByIDResponse envelope (the
// exact decode Grafana 12 performs), the envelope must carry the
// always-present zero Metrics block reference Tempo emits, and the
// inner trace must be byte-identical to the bare trace the v1
// endpoint serves.
func TestTraceByIDV2_ProtoEnvelope(t *testing.T) {
	t.Parallel()

	const id = "f48694fee9f78da6f98ec5a8cd7d3274"
	v2Resp, v2Body := getTraceByID(t, "/api/v2/traces/"+id, "application/protobuf")
	if v2Resp.StatusCode != http.StatusOK {
		t.Fatalf("v2 status=%d body=%q", v2Resp.StatusCode, v2Body)
	}
	if ct := v2Resp.Header.Get("Content-Type"); ct != "application/protobuf" {
		t.Fatalf("v2 Content-Type=%q, want application/protobuf", ct)
	}

	// The Grafana-12 decode: TraceByIDResponse, not Trace.
	envelope := &tempopb.TraceByIDResponse{}
	if err := proto.Unmarshal(v2Body, envelope); err != nil {
		t.Fatalf("v2 body must unmarshal as tempopb.TraceByIDResponse (the Grafana 12 decode): %v", err)
	}
	if envelope.Trace == nil {
		t.Fatalf("v2 envelope.Trace nil; the trace must be carried in field 1 of the envelope")
	}
	if got := len(envelope.Trace.ResourceSpans); got != 2 {
		t.Errorf("v2 envelope trace ResourceSpans=%d, want 2", got)
	}
	if envelope.Metrics == nil {
		t.Errorf("v2 envelope.Metrics nil; reference Tempo always emits a non-nil metrics block (modules/frontend/combiner/trace_by_id_v2.go finalize)")
	}
	if envelope.Status != tempopb.PartialStatus_COMPLETE {
		t.Errorf("v2 envelope.Status=%v, want COMPLETE (cerberus never truncates traces)", envelope.Status)
	}
	if envelope.Message != "" {
		t.Errorf("v2 envelope.Message=%q, want empty on a complete trace", envelope.Message)
	}

	// v1 stays the bare Trace…
	v1Resp, v1Body := getTraceByID(t, "/api/traces/"+id, "application/protobuf")
	if v1Resp.StatusCode != http.StatusOK {
		t.Fatalf("v1 status=%d body=%q", v1Resp.StatusCode, v1Body)
	}
	bare := &tempopb.Trace{}
	if err := proto.Unmarshal(v1Body, bare); err != nil {
		t.Fatalf("v1 body must unmarshal as bare tempopb.Trace: %v", err)
	}
	if got := len(bare.ResourceSpans); got != 2 {
		t.Errorf("v1 trace ResourceSpans=%d, want 2", got)
	}

	// …and the inner v2 trace is byte-identical to it: the envelope is
	// the only divergence between the endpoints; the trace content is
	// shared and deterministic (groupTraceBatches ordering contract).
	innerBytes, err := proto.Marshal(envelope.Trace)
	if err != nil {
		t.Fatalf("marshal v2 inner trace: %v", err)
	}
	if !bytes.Equal(innerBytes, v1Body) {
		t.Errorf("v2 envelope inner trace diverged from v1 bare trace:\n v1=%x\n v2.trace=%x", v1Body, innerBytes)
	}

	// The bug shape itself: the v1 and v2 bodies must NOT be
	// byte-identical for a non-empty trace — equality means the
	// envelope was dropped (the exact regression that broke Grafana
	// 12's trace view).
	if bytes.Equal(v1Body, v2Body) {
		t.Errorf("v1 and v2 proto bodies are byte-identical — the v2 TraceByIDResponse envelope is missing")
	}
}

// TestTraceByIDV2_JSONEnvelope pins the v2 JSON wire shape: the
// jsonpb-marshaled TraceByIDResponse (`{"trace":…,"metrics":{}}`),
// distinct from v1's flattened `{"batches":…}` shape.
func TestTraceByIDV2_JSONEnvelope(t *testing.T) {
	t.Parallel()

	const id = "f48694fee9f78da6f98ec5a8cd7d3274"
	resp, body := getTraceByID(t, "/api/v2/traces/"+id, "application/json")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", ct)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := raw["trace"]; !ok {
		t.Fatalf("missing top-level \"trace\" key; body=%s", body)
	}
	if _, ok := raw["metrics"]; !ok {
		t.Errorf("missing top-level \"metrics\" key; body=%s", body)
	}
	if _, ok := raw["batches"]; ok {
		t.Errorf("stray top-level \"batches\" key — v2 JSON must be the envelope, not the v1 shape; body=%s", body)
	}

	// The envelope round-trips through the same decoder Tempo clients
	// use for v2 JSON (jsonpb); spot-check the inner trace.
	var inner struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []json.RawMessage `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(raw["trace"], &inner); err != nil {
		t.Fatalf("decode trace field: %v", err)
	}
	spans := 0
	for _, rs := range inner.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			spans += len(ss.Spans)
		}
	}
	if spans != 2 {
		t.Errorf("v2 JSON envelope carries %d spans, want 2; body=%s", spans, body)
	}
}

// TestTraceByIDV2_NotFound pins the v2 not-found semantics: reference
// Tempo's frontend relays the downstream 404 on v2 exactly like v1
// (modules/frontend/combiner/common.go erroredResponse), so the status
// must be 404 on both encodings. The proto body is an empty message
// (an empty TraceByIDResponse marshals to zero bytes — identical to an
// empty Trace, so v1/v2 coincide here); JSON keeps Tempo's error
// envelope so Grafana renders the "trace not found" UI.
func TestTraceByIDV2_NotFound(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{samples: nil}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v2/traces/0123456789abcdef", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", "application/protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read body: %v", readErr)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
	// Whatever the body is, it must remain decodable as the envelope.
	envelope := &tempopb.TraceByIDResponse{}
	if err := proto.Unmarshal(body, envelope); err != nil {
		t.Errorf("404 body must stay decodable as TraceByIDResponse: %v", err)
	}
}
