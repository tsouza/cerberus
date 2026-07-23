package prom

import (
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// NewExplainLang builds an engine.Lang for offline instant explain — the
// migration preview's read-only "what SQL would cerberus run for this PromQL"
// path. It reuses the exact unexported lang adapter the HTTP handler drives, so
// ProjectSamples and Parse (parser options, dotted-name rewrite, lowering) are
// byte-identical to production.
//
// evalTime is pinned to both Start and End with Step left at 0, so the query
// lowers as an instant evaluation at a single anchor. Lowerers is left at its
// zero value (the all-fan-out default), matching an instant query on the
// handler where the native timeSeries*ToGrid table is not threaded.
func NewExplainLang(s schema.Metrics, evalTime time.Time) engine.Lang {
	return &lang{
		Parser: promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true}),
		Schema: s,
		Start:  evalTime,
		End:    evalTime, // Step stays 0 => instant evaluation
	}
}

// NewExplainLangRange builds an engine.Lang for offline RANGE explain — the
// preview path for dashboard-panel queries, which the server runs as a
// query_range (a non-zero Step lowers the outer step grid), not as the instant
// evaluation NewExplainLang models for rules. It reuses the same lang adapter, so
// Parse and ProjectSamples stay byte-identical to production; only the [start,
// end, step] window differs. Callers pin a fixed, representative window so the
// emitted SQL — and any goldens over it — stay deterministic.
func NewExplainLangRange(s schema.Metrics, start, end time.Time, step time.Duration) engine.Lang {
	return &lang{
		Parser: promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true}),
		Schema: s,
		Start:  start,
		End:    end,
		Step:   step,
	}
}
