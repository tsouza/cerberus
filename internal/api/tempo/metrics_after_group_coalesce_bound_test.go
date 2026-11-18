package tempo

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMetricsAfterGroupOrCoalesceEmitsBounded pins that a metrics function
// after coalesce(), or after by() and another stage, emits through the
// metrics range and instant paths with every spans scan bounded by the
// metrics window: the emit chokepoint's RequireSpansScansBounded accepts the
// statement, and every otel_traces scan in it sits beside a bound. Each query
// once failed to emit, with the timestamp-role error or the unbounded-scan
// error.
func TestMetricsAfterGroupOrCoalesceEmitsBounded(t *testing.T) {
	t.Parallel()
	s := tracesSchema()
	lang := &traceqlLang{schema: s}
	start := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	paths := []struct {
		name string
		wrap func(*testing.T, chplan.Node, time.Time, time.Time) chplan.Node
	}{
		{"range", wrapMetricsMatrix},
		{"instant", wrapMetricsInstant},
	}
	queries := []string{
		`{ } | coalesce() | rate()`,
		`{ } | by(name) | count() > 1 | rate()`,
		`{ } | count() > 1 | coalesce() | rate()`,
		`{ } | by(name) | select(name) | rate()`,
		`{ } | by(span.a + 1) | count() > 2 | rate()`,
	}
	for _, query := range queries {
		for _, path := range paths {
			t.Run(query+"/"+path.name, func(t *testing.T) {
				t.Parallel()
				wrapped := path.wrap(t, lowerMetricsWithWindow(t, query, start, end), start, end)
				if err := chplan.RequireSpansScansBounded(s.SpansTable, chplan.BoundMetricsPerTraceSubqueries(wrapped)); err != nil {
					t.Fatalf("RequireSpansScansBounded: %v", err)
				}
				sql, err := emitScoped(t, lang, wrapped)
				if err != nil {
					t.Fatalf("windowed metrics emit: %v", err)
				}
				assertEverySpansFromBounded(t, sql)
			})
		}
	}
}
