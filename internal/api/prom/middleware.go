package prom

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// ctxKey is the unexported key type for request-scoped values in this
// package — avoids string-key collisions across packages.
type ctxKey int

const (
	ctxKeyChMillis ctxKey = iota
)

// chMillisCounter accumulates time spent in ClickHouse calls for a single
// request. Stored in context.Value(ctxKeyChMillis) as *atomic.Int64.
// Atomic because handlers may fan out to multiple CH calls concurrently
// (e.g. /api/v1/metadata queries gauge / sum / histogram tables).
type chMillisCounter struct{ ms atomic.Int64 }

func (c *chMillisCounter) add(d time.Duration) {
	c.ms.Add(d.Milliseconds())
}

func (c *chMillisCounter) load() int64 {
	return c.ms.Load()
}

// withChCounter attaches a fresh ms counter to ctx so wrapped Querier
// calls can accumulate their durations into it.
func withChCounter(ctx context.Context) (context.Context, *chMillisCounter) {
	c := &chMillisCounter{}
	return context.WithValue(ctx, ctxKeyChMillis, c), c
}

// ctxCounter returns the request's chMillisCounter or nil if none.
func ctxCounter(ctx context.Context) *chMillisCounter {
	c, _ := ctx.Value(ctxKeyChMillis).(*chMillisCounter)
	return c
}

// timeCH wraps a CH call, recording its duration into the request's
// chMillisCounter (if attached). Returns the bare result so callers can
// chain without changing signatures.
func timeCH[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	start := time.Now()
	v, err := fn()
	if c := ctxCounter(ctx); c != nil {
		c.add(time.Since(start))
	}
	return v, err
}

// promHeadersMiddleware sets the static Prom-compatibility header on
// every response. Per-request headers (CH timing, strategy) are set by
// the handlers themselves before WriteHeader fires.
func promHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Prometheus-API-Version", "v1")
		// Attach a per-request CH-timing counter; handlers that go
		// through `h.timedClient(ctx)` will populate it.
		ctx, counter := withChCounter(r.Context())
		// Wrap w so we can stamp X-Cerberus-CH-Millis just before the
		// status is written (response headers freeze at that point).
		ww := &headerStampingWriter{ResponseWriter: w, counter: counter}
		next.ServeHTTP(ww, r.WithContext(ctx))
		// Idempotent — if WriteHeader was never called the deferred
		// stamp still runs once on flush.
		ww.stamp()
	})
}

// headerStampingWriter intercepts WriteHeader (and the first Write)
// to inject X-Cerberus-CH-Millis just before the headers freeze.
type headerStampingWriter struct {
	http.ResponseWriter
	counter *chMillisCounter
	done    bool
}

func (w *headerStampingWriter) stamp() {
	if w.done || w.counter == nil {
		return
	}
	w.done = true
	w.Header().Set("X-Cerberus-CH-Millis", strconv.FormatInt(w.counter.load(), 10))
}

func (w *headerStampingWriter) WriteHeader(status int) {
	w.stamp()
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerStampingWriter) Write(b []byte) (int, error) {
	w.stamp()
	return w.ResponseWriter.Write(b)
}
