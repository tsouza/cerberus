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
