package tempo_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/tempopb"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// fixtureTraceSamples returns the canonical 3-sample input both the
// JSON and proto path tests below run against. The reserved-key labels
// (`__cerberus_traceID`, `__cerberus_spanID`, …) mirror what
// traceByIDProjections emits via CH so the handler's reserved-key
// split paths exercise the same input both paths see in production.
//
// Two of the samples share a ResourceAttributes set (service.name =
// "frontend") so groupBatches / groupBatchesProto bucket them into the
// same ResourceSpans; the third uses a different service.name so a
// second bucket is created. This pins both the bucketing logic and
// the (Resource, Span) projection across the two helpers.
func fixtureTraceSamples() []chclient.Sample {
	ts := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	return []chclient.Sample{
		{
			MetricName: "GET /api/users",
			Labels: map[string]string{
				"service.name":             "frontend",
				"k8s.namespace":            "prod",
				"__cerberus_traceID":       "f48694fee9f78da6f98ec5a8cd7d3274",
				"__cerberus_spanID":        "abc1234567890def",
				"__cerberus_parentSpanID":  "1111222233334444",
				"__cerberus_spanKind":      "Server",
				"__cerberus_statusCode":    "Ok",
				"__cerberus_spanAttrsJSON": `{"http.method":"GET","http.status_code":"200"}`,
			},
			Timestamp: ts,
			Value:     150_000_000,
		},
		{
			MetricName: "db.query",
			Labels: map[string]string{
				"service.name":             "frontend",
				"k8s.namespace":            "prod",
				"__cerberus_traceID":       "f48694fee9f78da6f98ec5a8cd7d3274",
				"__cerberus_spanID":        "ddddeeeeffff0000",
				"__cerberus_parentSpanID":  "abc1234567890def",
				"__cerberus_spanKind":      "Client",
				"__cerberus_statusCode":    "Error",
				"__cerberus_spanAttrsJSON": `{"db.system":"postgres"}`,
			},
			Timestamp: ts.Add(10 * time.Millisecond),
			Value:     45_000_000,
		},
		{
			MetricName: "HTTP GET",
			Labels: map[string]string{
				"service.name":             "backend",
				"__cerberus_traceID":       "f48694fee9f78da6f98ec5a8cd7d3274",
				"__cerberus_spanID":        "9999888877776666",
				"__cerberus_parentSpanID":  "",
				"__cerberus_spanKind":      "Internal",
				"__cerberus_statusCode":    "Unset",
				"__cerberus_spanAttrsJSON": "{}",
			},
			Timestamp: ts.Add(5 * time.Millisecond),
			Value:     200_000_000,
		},
	}
}

// TestHandleTraceByID_AcceptProto_MarshalsTempopbTrace exercises the
// Accept: application/protobuf branch end-to-end: the handler must
// stamp Content-Type: application/protobuf and the body must
// proto.Unmarshal into a *tempopb.Trace whose ResourceSpans / Spans
// reflect the stubbed input.
func TestHandleTraceByID_AcceptProto_MarshalsTempopbTrace(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: fixtureTraceSamples()}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	req, err := http.NewRequest("GET", srv.URL+"/api/traces/f48694fee9f78da6f98ec5a8cd7d3274", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", "application/protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
		t.Fatalf("Content-Type=%q, want application/protobuf", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var trace tempopb.Trace
	if err := proto.Unmarshal(body, &trace); err != nil {
		t.Fatalf("proto.Unmarshal: %v (body=%x)", err, body)
	}

	// 2 resource buckets (frontend × 2 spans, backend × 1 span).
	if got, want := len(trace.ResourceSpans), 2; got != want {
		t.Fatalf("ResourceSpans=%d, want %d", got, want)
	}

	// Total span count across all ResourceSpans / ScopeSpans must be 3.
	totalSpans := 0
	wantTraceIDHex := "f48694fee9f78da6f98ec5a8cd7d3274"
	wantTraceIDBytes, _ := hex.DecodeString(wantTraceIDHex)
	for _, rs := range trace.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				totalSpans++
				if !bytes.Equal(s.TraceId, wantTraceIDBytes) {
					t.Errorf("Span.TraceId=%x, want %x", s.TraceId, wantTraceIDBytes)
				}
				if len(s.SpanId) != 8 {
					t.Errorf("Span.SpanId length=%d, want 8", len(s.SpanId))
				}
			}
		}
	}
	if totalSpans != 3 {
		t.Fatalf("total spans=%d, want 3", totalSpans)
	}

	// Resource attributes round-trip: the "frontend" bucket carries
	// service.name + k8s.namespace, the "backend" bucket carries only
	// service.name. We don't assume an order, so collect into a set.
	serviceByBucket := map[string]bool{}
	for _, rs := range trace.ResourceSpans {
		for _, kv := range rs.Resource.Attributes {
			if kv.Key == "service.name" {
				serviceByBucket[kv.Value.GetStringValue()] = true
			}
		}
	}
	if !serviceByBucket["frontend"] || !serviceByBucket["backend"] {
		t.Fatalf("service.name attributes=%v, want frontend+backend", serviceByBucket)
	}
}

// TestHandleTraceByID_ProtoJSONParity pins that the JSON path and the
// proto path describe the same trace: identical span count, identical
// trace-id, identical attribute key universe across the two outputs.
// The shape diverges (string-keyed map vs []*KeyValue; hex string vs
// raw bytes) but the *semantic* content must match — otherwise the
// "click trace" UX would render different data depending on which
// Accept header Grafana negotiated.
func TestHandleTraceByID_ProtoJSONParity(t *testing.T) {
	t.Parallel()

	samples := fixtureTraceSamples()
	q := &stubQuerier{samples: samples}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// JSON path.
	jsonResp, err := http.Get(srv.URL + "/api/traces/f48694fee9f78da6f98ec5a8cd7d3274")
	if err != nil {
		t.Fatalf("GET json: %v", err)
	}
	defer jsonResp.Body.Close()
	var jsonTrace tempo.TraceByIDResponse
	if err := json.NewDecoder(jsonResp.Body).Decode(&jsonTrace); err != nil {
		t.Fatalf("decode json: %v", err)
	}

	// Proto path.
	req, err := http.NewRequest("GET", srv.URL+"/api/traces/f48694fee9f78da6f98ec5a8cd7d3274", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", "application/protobuf")
	protoResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET proto: %v", err)
	}
	defer protoResp.Body.Close()
	body, err := io.ReadAll(protoResp.Body)
	if err != nil {
		t.Fatalf("read proto body: %v", err)
	}
	var pbTrace tempopb.Trace
	if err := proto.Unmarshal(body, &pbTrace); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}

	// 1. Span-count parity.
	jsonSpans := 0
	for _, b := range jsonTrace.Batches {
		jsonSpans += len(b.Spans)
	}
	protoSpans := 0
	for _, rs := range pbTrace.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			protoSpans += len(ss.Spans)
		}
	}
	if jsonSpans != protoSpans {
		t.Fatalf("span count: json=%d proto=%d", jsonSpans, protoSpans)
	}

	// 2. Bucket-count parity (one ResourceSpans per distinct
	//    ResourceAttributes set in both shapes).
	if got, want := len(jsonTrace.Batches), len(pbTrace.ResourceSpans); got != want {
		t.Fatalf("bucket count: json=%d proto=%d", got, want)
	}

	// 3. Resource attribute *key universe* parity across all buckets.
	jsonRAKeys := map[string]bool{}
	for _, b := range jsonTrace.Batches {
		for k := range b.Resource.Attributes {
			jsonRAKeys[k] = true
		}
	}
	protoRAKeys := map[string]bool{}
	for _, rs := range pbTrace.ResourceSpans {
		for _, kv := range rs.Resource.Attributes {
			protoRAKeys[kv.Key] = true
		}
	}
	if !stringSetsEqual(jsonRAKeys, protoRAKeys) {
		t.Fatalf("resource attr key universe diverged:\n  json=%v\n  proto=%v",
			keys(jsonRAKeys), keys(protoRAKeys))
	}

	// 4. Span attribute *key universe* parity.
	jsonSAKeys := map[string]bool{}
	for _, b := range jsonTrace.Batches {
		for _, sp := range b.Spans {
			for k := range sp.Attributes {
				jsonSAKeys[k] = true
			}
		}
	}
	protoSAKeys := map[string]bool{}
	for _, rs := range pbTrace.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				for _, kv := range sp.Attributes {
					protoSAKeys[kv.Key] = true
				}
			}
		}
	}
	if !stringSetsEqual(jsonSAKeys, protoSAKeys) {
		t.Fatalf("span attr key universe diverged:\n  json=%v\n  proto=%v",
			keys(jsonSAKeys), keys(protoSAKeys))
	}

	// 5. Trace ID parity — every span on both sides carries the same
	//    trace-id literal (JSON hex form, proto bytes form).
	wantHex := "f48694fee9f78da6f98ec5a8cd7d3274"
	wantBytes, _ := hex.DecodeString(wantHex)
	for _, b := range jsonTrace.Batches {
		for _, sp := range b.Spans {
			// The JSON path emits the leading-zero-stripped form (it's
			// what stripLeadingHexZeros produces on CH); the literal
			// fixture trace-id happens to have no leading zeros so
			// stripped == padded == the constant above.
			if sp.TraceID != wantHex {
				t.Errorf("json span TraceID=%q, want %q", sp.TraceID, wantHex)
			}
		}
	}
	for _, rs := range pbTrace.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				if !bytes.Equal(sp.TraceId, wantBytes) {
					t.Errorf("proto span TraceId=%x, want %x", sp.TraceId, wantBytes)
				}
			}
		}
	}
}

// TestHandleTraceByID_AcceptProto_NotFound — the not-found branch
// honours the same negotiation: protobuf clients get an empty
// *tempopb.Trace under 404 (reference Tempo's behaviour), JSON
// clients keep getting the documented ErrorResponse envelope.
func TestHandleTraceByID_AcceptProto_NotFound(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// Valid 32-hex grammar so the 16-/32-hex gate passes; the
	// stubQuerier returns no rows, exercising the proto-encoded
	// not-found branch.
	req, err := http.NewRequest("GET", srv.URL+"/api/traces/00000000000000000000000000000000", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept", "application/protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/protobuf" {
		t.Fatalf("Content-Type=%q, want application/protobuf", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var trace tempopb.Trace
	if err := proto.Unmarshal(body, &trace); err != nil {
		t.Fatalf("proto.Unmarshal: %v (body=%x)", err, body)
	}
	if len(trace.ResourceSpans) != 0 {
		t.Fatalf("expected empty trace on 404, got %d ResourceSpans", len(trace.ResourceSpans))
	}
}

// TestHandleTraceByID_AcceptJSON_KeepsJSONShape — the existing JSON
// path is untouched. A bare GET (no Accept header) and an explicit
// `Accept: application/json` both keep getting the documented JSON
// shape so callers like curl + the conformance suite + the
// dashboard-driven /api/ds/query loop are not silently re-routed
// to the proto branch.
func TestHandleTraceByID_AcceptJSON_KeepsJSONShape(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: fixtureTraceSamples()}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	for _, accept := range []string{"", "application/json", "*/*"} {
		t.Run("accept="+accept, func(t *testing.T) {
			req, err := http.NewRequest("GET", srv.URL+"/api/traces/f48694fee9f78da6f98ec5a8cd7d3274", nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			if accept != "" {
				req.Header.Set("Accept", accept)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			ct := resp.Header.Get("Content-Type")
			if ct == "application/protobuf" {
				t.Fatalf("Content-Type=%q under Accept=%q; expected JSON branch", ct, accept)
			}
			var tr tempo.TraceByIDResponse
			if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
				t.Fatalf("json decode: %v", err)
			}
			if len(tr.Batches) == 0 {
				t.Fatalf("expected non-empty batches")
			}
		})
	}
}

func stringSetsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
