package grpc_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"

	"github.com/tsouza/cerberus/internal/api/tempo"
	tempogrpc "github.com/tsouza/cerberus/internal/api/tempo/grpc"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// installManualReader swaps the global MeterProvider with one backed by
// a ManualReader so a test can pull a deterministic metrics snapshot
// after driving RPCs through a real dialServer-built gRPC server.
// Mirrors internal/telemetry/metrics_test.go's helper of the same name
// — duplicated here rather than exported from internal/telemetry
// because it is test-only scaffolding, not a package API.
func installManualReader(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })
	return reader
}

// collectQueriesTotal drains the manual reader and returns the
// cerberus_queries_total sum's data points.
func collectQueriesTotal(t *testing.T, reader *metric.ManualReader) []metricdata.DataPoint[int64] {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name == "cerberus_queries_total" {
				return m.Data.(metricdata.Sum[int64]).DataPoints
			}
		}
	}
	t.Fatalf("cerberus_queries_total not found; scopes=%v", rm.ScopeMetrics)
	return nil
}

// attrString reads a string-valued attribute off a data point, failing
// the test if it is absent.
func attrString(t *testing.T, dp metricdata.DataPoint[int64], key attribute.Key) string {
	t.Helper()
	v, ok := dp.Attributes.Value(key)
	if !ok {
		t.Fatalf("attribute %q missing; have %v", key, dp.Attributes)
	}
	return v.AsString()
}

// TestQueryTelemetryInterceptor_Search_RecordsOK pins the success path
// wired in NewServer (#1452): a gRPC Search RPC that completes cleanly
// records exactly one cerberus_queries_total data point tagged with the
// TraceQL language, the FullMethod route, and result=ok — the same
// triple the HTTP /api/search handler records via QueryMiddleware. Before
// the interceptor existed, this RPC recorded nothing at all, so the
// dashboards and alerts built on cerberus_queries_total silently
// excluded every client using Grafana's gRPC/h2c streaming toggle.
func TestQueryTelemetryInterceptor_Search_RecordsOK(t *testing.T) {
	// Not t.Parallel(): installManualReader swaps the process-global
	// OTel MeterProvider (telemetry.Install), which two tests doing so
	// concurrently would race — mirrors internal/telemetry/metrics_test.go,
	// none of whose Install-using tests run in parallel either.
	reader := installManualReader(t)

	q := &stubCursorQuerier{}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{}"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := drainSearch(t, stream); err != nil {
		t.Fatalf("drain: %v", err)
	}

	dps := collectQueriesTotal(t, reader)
	if len(dps) != 1 {
		t.Fatalf("queries.total DPs: got %d want 1: %+v", len(dps), dps)
	}
	dp := dps[0]
	if got := attrString(t, dp, telemetry.AttrQL); got != telemetry.QLTraceQL {
		t.Errorf("ql: got %q want %q", got, telemetry.QLTraceQL)
	}
	if got := attrString(t, dp, telemetry.AttrRoute); got != "/tempopb.StreamingQuerier/Search" {
		t.Errorf("route: got %q want /tempopb.StreamingQuerier/Search", got)
	}
	if got := attrString(t, dp, telemetry.AttrResult); got != telemetry.ResultOK {
		t.Errorf("result: got %q want %q", got, telemetry.ResultOK)
	}
}

// TestQueryTelemetryInterceptor_Search_RecordsError mirrors the OK test
// on the parse-error path: an unparseable TraceQL query still reaches
// codes.InvalidArgument (see TestSearch_ParseErrorMapsToInvalidArgument),
// and the interceptor must record it as a failed query — a
// cerberus_queries_total{result="error"} data point classified through
// the same telemetry.ClassifyStatus the HTTP path uses for its
// equivalent 400 Bad Request.
func TestQueryTelemetryInterceptor_Search_RecordsError(t *testing.T) {
	// Not t.Parallel() — see TestQueryTelemetryInterceptor_Search_RecordsOK.
	reader := installManualReader(t)

	q := &stubCursorQuerier{}
	client, cleanup := dialServer(t, q)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Search(ctx, &tempopb.SearchRequest{Query: "{ unclosed"})
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if _, err := drainSearch(t, stream); err == nil {
		t.Fatalf("want a parse error, got nil")
	}

	dps := collectQueriesTotal(t, reader)
	if len(dps) != 1 {
		t.Fatalf("queries.total DPs: got %d want 1: %+v", len(dps), dps)
	}
	dp := dps[0]
	if got := attrString(t, dp, telemetry.AttrResult); got != telemetry.ResultError {
		t.Errorf("result: got %q want %q", got, telemetry.ResultError)
	}
	if got := attrString(t, dp, telemetry.AttrErrorReason); got != telemetry.ReasonBadRequest {
		t.Errorf("error_reason: got %q want %q", got, telemetry.ReasonBadRequest)
	}
}

// TestGRPCCodeToHTTPStatus pins grpcCodeToHTTPStatus's table — the
// reverse of grpcCodeFor's HTTP↔gRPC correspondence (errclass.go) that
// the query-telemetry interceptor uses to reuse telemetry.ClassifyStatus
// instead of maintaining a second closed-enum mapping. Table-driven in
// the same spirit as TestGRPCStatusFor (errclass_test.go), which pins
// the forward direction.
func TestGRPCCodeToHTTPStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code codes.Code
		want int
	}{
		{"ok", codes.OK, http.StatusOK},
		{"invalid_argument", codes.InvalidArgument, http.StatusBadRequest},
		{"resource_exhausted", codes.ResourceExhausted, http.StatusUnprocessableEntity},
		{"unavailable", codes.Unavailable, http.StatusServiceUnavailable},
		{"canceled", codes.Canceled, tempo.StatusClientClosedRequest},
		{"unclassified_defaults_internal", codes.Unknown, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tempogrpc.GRPCCodeToHTTPStatusTest(tc.code); got != tc.want {
				t.Errorf("grpcCodeToHTTPStatus(%v) = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}

// TestOutcomeForCode_CancellationAgreesWithTheOtherHeads is the cross-transport
// half of cerberus issue #3197's fix.
//
// A client cancellation reaches cerberus three ways and used to be recorded
// three different ways: this gRPC stream and Tempo's HTTP handler answer 499,
// which reasonForStatus reads as bad_request, while Prometheus and Loki answer
// 503, which it reads as backend_unavailable. Same event, opposite verdicts,
// and neither true — the request was not malformed and the backend was not
// unavailable.
//
// The assertion is deliberately made AGAINST the reason the HTTP heads record
// rather than against a hard-coded literal: what #3197 asks for is agreement,
// so a fix that renamed the value on one transport only would still fail here.
func TestOutcomeForCode_CancellationAgreesWithTheOtherHeads(t *testing.T) {
	t.Parallel()

	gotGRPC := tempogrpc.OutcomeForCodeTest(codes.Canceled)

	// What the HTTP QueryMiddleware records for the same event: Tempo's own
	// 499 and prom/loki's 503, each re-labelled by the cancellation the
	// request context carries.
	for _, httpStatus := range []int{tempo.StatusClientClosedRequest, http.StatusServiceUnavailable} {
		wantHTTP := telemetry.ClassifyStatus(httpStatus).AsCanceled()
		if gotGRPC.Reason != wantHTTP.Reason {
			t.Errorf("gRPC cancellation reason = %q, but the HTTP head answering %d records %q — "+
				"one event must not be three reasons", gotGRPC.Reason, httpStatus, wantHTTP.Reason)
		}
	}
	if gotGRPC.Reason != telemetry.ReasonCanceled {
		t.Errorf("gRPC cancellation reason = %q, want %q", gotGRPC.Reason, telemetry.ReasonCanceled)
	}
	if gotGRPC.Result != telemetry.ResultError {
		t.Errorf("result = %q, want error — the query was not answered", gotGRPC.Result)
	}
	// status_class is deliberately NOT unified: 499 is genuinely the status
	// this transport reports, and the label reports what was sent.
	if gotGRPC.StatusClass != telemetry.StatusClass4xx {
		t.Errorf("status_class = %q, want %q — only the REASON is re-labelled", gotGRPC.StatusClass, telemetry.StatusClass4xx)
	}
	// Non-vacuity: an ordinary error code must NOT pick up the cancellation
	// reason, or the assertions above would pass for a blanket re-label.
	if other := tempogrpc.OutcomeForCodeTest(codes.Unavailable); other.Reason == telemetry.ReasonCanceled {
		t.Error("codes.Unavailable classified as a cancellation; only codes.Canceled may be")
	}
}
