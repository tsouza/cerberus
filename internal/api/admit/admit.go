// Package admit provides per-handler concurrency caps for cerberus's
// HTTP and gRPC listeners. Each cerberus replica accepts unlimited
// inbound requests by default; under sustained load that fans out into
// hundreds of slow ClickHouse queries running in parallel, which
// exhausts CH's thread pool and drags every concurrent request's
// latency down with the saturated ones.
//
// A Limiter caps the number of in-flight handler invocations for a
// given API head (Prom / Loki / Tempo). When a new request arrives at
// the cap, the limiter rejects it immediately — HTTP 503 with a
// `Retry-After: 1` header for HTTP callers (Middleware), gRPC
// `codes.ResourceExhausted` for streaming RPC callers
// (StreamInterceptor) — so well-behaved clients back off and retry,
// failing fast on the slow few rather than degrading service for
// everyone.
//
// The limiter is opt-out (`CERBERUS_ADMIT_DISABLED=true`) for local /
// dev workflows where artificial caps get in the way; in production
// it should always be on.
package admit

import (
	"context"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// meterName is the instrumentation-scope identifier stamped on the
// admit.rejected_total counter. Distinct from internal/telemetry's
// scope so dashboards can pivot on the admit-specific scope when
// drilling into rejection events.
const meterName = "github.com/tsouza/cerberus/internal/api/admit"

// attrQL labels the rejection counter with the query language the
// limiter is fronting — "promql" / "logql" / "traceql". Mirrors the
// cerberus.ql attribute set by internal/telemetry on the
// per-query counters, so the cerberus dashboard's
// `sum by (cerberus_ql, reason) (rate(cerberus_admit_rejected_total[5m]))`
// panel resolves consistently across both metric sources.
const attrQL = attribute.Key("cerberus.ql")

// attrReason labels the rejection counter with the rejection cause.
// The limiter currently has exactly one rejection path: the weighted
// semaphore was at its cap when Acquire ran. Future paths (e.g.,
// queue-timeout, route-disabled) should add to this vocabulary
// rather than overload an existing value.
const attrReason = attribute.Key("reason")

// ReasonCapExceeded is emitted on the reason attribute when Acquire
// is called while the limiter's semaphore is saturated. It's the only
// rejection path the limiter has today, and newWithProvider
// pre-registers its (cerberus.ql, reason) stream at 0 when the
// Limiter is constructed — see the zero-init note there. Future
// reason values must be pre-registered the same way so dashboards
// see a 0-valued series instead of "No data" on healthy replicas.
const ReasonCapExceeded = "cap_exceeded"

// headToQL maps the API-head identifier ("prom" / "loki" / "tempo")
// used to construct a Limiter onto the query-language string
// ("promql" / "logql" / "traceql") cerberus uses everywhere else in
// its telemetry. Unknown heads fall through to the raw value so a
// future head ("otlp"?) still produces a usable metric label.
func headToQL(head string) string {
	switch head {
	case "prom":
		return "promql"
	case "loki":
		return "logql"
	case "tempo":
		return "traceql"
	default:
		return head
	}
}

// Limiter caps concurrent in-flight requests for one API head. The
// zero value is unusable; build via New. A nil *Limiter is a sentinel
// meaning "admission control disabled" — every call to Acquire
// succeeds with a no-op release closure, and the HTTP wrapper passes
// every request through without bookkeeping.
//
// Acquire takes a non-blocking weighted-semaphore slot. When the
// semaphore is full the function returns false; the caller then maps
// that into a 503 response. The semaphore choice (over a buffered
// channel) keeps the door open for future weighted admission — heavy
// `query_range` queries could cost more than label-list calls — but
// every callsite today uses weight 1.
type Limiter struct {
	head     string
	ql       string
	sem      *semaphore.Weighted
	rejected metric.Int64Counter
}

// New constructs a Limiter for head with the given cap. head is the
// API identifier ("prom" / "loki" / "tempo") used to label the
// rejection counter; cap is the maximum number of concurrent
// in-flight requests. A cap of 0 or less returns nil — the caller
// treats that as "admission control disabled for this head", which
// makes config wiring symmetric across the enabled / disabled cases.
//
// The rejection counter is built off the OTel global MeterProvider at
// the moment of construction; install the cerberus telemetry provider
// (via cmd/cerberus/main.go) before building limiters so the counter
// flows to the configured OTLP exporter.
func New(head string, cap int) *Limiter {
	return newWithProvider(head, cap, otel.GetMeterProvider())
}

// newWithProvider is the test seam New() funnels through. Lets unit
// tests construct a Limiter whose rejected counter targets a manual
// reader without racing with parallel tests that use the global
// provider.
func newWithProvider(head string, cap int, mp metric.MeterProvider) *Limiter {
	if cap <= 0 {
		return nil
	}
	meter := mp.Meter(meterName)
	rejected, err := meter.Int64Counter(
		"cerberus_admit_rejected_total",
		metric.WithDescription(
			"Requests rejected by the per-handler concurrency cap. "+
				"Labels: cerberus.ql (promql / logql / traceql), "+
				"reason (cap_exceeded).",
		),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		// Instrument validation only fails on a misconfigured
		// MeterProvider; surface loudly rather than silently dropping
		// the counter.
		panic("admit: build rejected counter: " + err.Error())
	}
	ql := headToQL(head)
	// Zero-initialize the rejection stream for the one label set
	// Acquire records to. OTel synchronous counters export no data
	// until their first Add, so without this a healthy replica that
	// never saturates its cap exports no
	// cerberus_admit_rejected_total stream at all and the dashboard's
	// `sum by (cerberus_ql, reason) (rate(...))` panel renders
	// "No data" instead of a flat 0. Pre-registering at construction
	// follows the standard Prometheus practice for counters whose
	// label sets are known in advance: the cumulative stream exists
	// from process start, so rate() resolves to 0 per head.
	rejected.Add(context.Background(), 0, metric.WithAttributes(
		attrQL.String(ql),
		attrReason.String(ReasonCapExceeded),
	))
	return &Limiter{
		head:     head,
		ql:       ql,
		sem:      semaphore.NewWeighted(int64(cap)),
		rejected: rejected,
	}
}

// Acquire tries to take a slot without blocking. Returns a release
// closure (always non-nil) and a boolean — true when the slot was
// acquired, false when the limiter is saturated. The release closure
// is idempotent and safe to call even after a rejection (it's a
// no-op then). A nil receiver returns (no-op, true) so the disabled
// path is allocation-free.
func (l *Limiter) Acquire(ctx context.Context) (release func(), ok bool) {
	if l == nil {
		return func() {}, true
	}
	if !l.sem.TryAcquire(1) {
		l.rejected.Add(ctx, 1, metric.WithAttributes(
			attrQL.String(l.ql),
			attrReason.String(ReasonCapExceeded),
		))
		return func() {}, false
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		l.sem.Release(1)
	}, true
}

// Middleware wraps next so every request first tries to acquire a
// slot from l. On rejection the wrapper writes a 503 with
// `Retry-After: 1` and drops the request without invoking next. On
// success the wrapper invokes next inside an Acquire/release pair so
// the slot is returned even if next panics (defer ordering with the
// release closure).
//
// A nil *Limiter falls through to next directly — handy for the
// `CERBERUS_ADMIT_DISABLED=true` case where main.go passes nil into
// every register call. retryAfterSeconds is the value cerberus
// stamps into the `Retry-After` header; pass 0 to suppress the
// header entirely.
func (l *Limiter) Middleware(retryAfterSeconds int, next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := l.Acquire(r.Context())
		if !ok {
			if retryAfterSeconds > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
			}
			http.Error(w, "admission control: server saturated", http.StatusServiceUnavailable)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// Head returns the API-head identifier this Limiter labels its
// rejection counter with. Useful for log lines that need to identify
// the limiter on rejection.
func (l *Limiter) Head() string {
	if l == nil {
		return ""
	}
	return l.head
}

// StreamInterceptor returns a grpc.StreamServerInterceptor that
// enforces the same admission cap as Middleware does for HTTP. On a
// saturated limiter the interceptor short-circuits with
// `codes.ResourceExhausted`; the gRPC equivalent of the HTTP 503 +
// `Retry-After: 1` pair the Middleware writes. The status code is the
// canonical signal for "back off and retry" in gRPC clients (Grafana's
// Go gRPC client honours it via the standard retry policy).
//
// A nil *Limiter returns a pass-through interceptor — symmetrical with
// Middleware so the `CERBERUS_ADMIT_DISABLED=true` path stays
// allocation-free. The interceptor uses the stream's context for the
// rejection counter attribution so the per-RPC trace context flows
// into the recorded metric.
func (l *Limiter) StreamInterceptor() grpc.StreamServerInterceptor {
	if l == nil {
		return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, ss)
		}
	}
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		release, ok := l.Acquire(ss.Context())
		if !ok {
			return status.Errorf(codes.ResourceExhausted, "admission control: server saturated")
		}
		defer release()
		return handler(srv, ss)
	}
}
