package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// The reason override exists because upstream wire parity pins a query
// wall-clock timeout to 503 and a per-query budget refusal to 422. Both
// tests below assert the OBSERVED label, not the helper's return value:
// a test that only called SetReason and read it back would pass even if
// the middleware never consulted the cell.

// serveWithMiddleware runs handler behind QueryMiddleware against a fresh
// ManualReader-backed provider and returns the cerberus_error_reason label
// the query counter recorded.
func serveWithMiddleware(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	reader := installManualReader(t)

	// A real PanicRenderer: the middleware calls it on the recovered-panic
	// path, and a nil one would turn that test into a nil deref.
	renderPanic := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	h := telemetry.QueryMiddleware("promql", renderPanic, handler)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/query", nil))

	m := findMetric(t, collect(t, reader), "cerberus_queries_total")
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("cerberus_queries_total is %T, want Sum[int64]", m.Data)
	}
	if len(sum.DataPoints) != 1 {
		t.Fatalf("cerberus_queries_total has %d data points, want 1", len(sum.DataPoints))
	}
	v, ok := sum.DataPoints[0].Attributes.Value(telemetry.AttrErrorReason)
	if !ok {
		t.Fatalf("data point carries no %s attribute: %v", telemetry.AttrErrorReason, sum.DataPoints[0].Attributes)
	}
	return v.AsString()
}

func TestQueryMiddleware_HandlerReasonOverridesStatusDerivedOne(t *testing.T) {
	// 503 alone derives backend_unavailable. A timeout is not an outage,
	// and the handler is the only thing that knows the difference.
	got := serveWithMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
		telemetry.SetReason(r.Context(), telemetry.ReasonTimeout)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if got != telemetry.ReasonTimeout {
		t.Fatalf("reason = %q, want %q — the handler's SetReason did not reach the counter", got, telemetry.ReasonTimeout)
	}

	// The same status with no override must still classify as before, or
	// the test above would pass for the wrong reason.
	got = serveWithMiddleware(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if got != telemetry.ReasonBackendUnavailable {
		t.Fatalf("reason without an override = %q, want %q", got, telemetry.ReasonBackendUnavailable)
	}
}

func TestQueryMiddleware_HandlerReasonSeparatesBudgetFromMalformed(t *testing.T) {
	got := serveWithMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
		telemetry.SetReason(r.Context(), telemetry.ReasonResourceExhausted)
		w.WriteHeader(http.StatusUnprocessableEntity)
	})
	if got != telemetry.ReasonResourceExhausted {
		t.Fatalf("reason = %q, want %q", got, telemetry.ReasonResourceExhausted)
	}

	got = serveWithMiddleware(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	})
	if got != telemetry.ReasonBadRequest {
		t.Fatalf("reason without an override = %q, want %q", got, telemetry.ReasonBadRequest)
	}
}

func TestQueryMiddleware_ReasonOverrideCannotTurnAnErrorIntoASuccess(t *testing.T) {
	// ReasonNone is outside SetReason's accepted set, and a 200 is not an
	// error to relabel anyway. Both guards are exercised here.
	got := serveWithMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
		telemetry.SetReason(r.Context(), telemetry.ReasonNone)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if got != telemetry.ReasonBackendUnavailable {
		t.Fatalf("reason = %q, want the status-derived %q — ReasonNone must not be settable", got, telemetry.ReasonBackendUnavailable)
	}

	got = serveWithMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
		telemetry.SetReason(r.Context(), telemetry.ReasonTimeout)
		w.WriteHeader(http.StatusOK)
	})
	if got != telemetry.ReasonNone {
		t.Fatalf("reason on a 200 = %q, want %q — an override must not relabel a success", got, telemetry.ReasonNone)
	}
}

func TestQueryMiddleware_PanicStaysInternalDespiteAHandlerReason(t *testing.T) {
	got := serveWithMiddleware(t, func(_ http.ResponseWriter, r *http.Request) {
		telemetry.SetReason(r.Context(), telemetry.ReasonTimeout)
		panic("boom")
	})
	if got != telemetry.ReasonInternal {
		t.Fatalf("reason after a recovered panic = %q, want %q", got, telemetry.ReasonInternal)
	}
}

func TestSetReason_IsANoOpWithoutACell(t *testing.T) {
	// Callers outside QueryMiddleware (tests, non-HTTP entrypoints) must
	// not panic or allocate a cell of their own.
	telemetry.SetReason(context.Background(), telemetry.ReasonTimeout)
}

func TestSetReason_RejectsAValueOutsideTheClosedEnum(t *testing.T) {
	got := serveWithMiddleware(t, func(w http.ResponseWriter, r *http.Request) {
		telemetry.SetReason(r.Context(), "database_on_fire")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if got != telemetry.ReasonBackendUnavailable {
		t.Fatalf("reason = %q, want the status-derived %q — an unknown value must not widen the label", got, telemetry.ReasonBackendUnavailable)
	}
}
