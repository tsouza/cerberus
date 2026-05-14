// Package oracle holds the property-test oracles — the independent
// specs that compute the expected output for a (dataset, query) pair.
//
// As of Phase 1 PR 2 the canonical oracle is the from-scratch
// PromQL evaluator in [oracle/promql] (invoked via
// [promql.Evaluate]). It implements PromQL semantics directly off
// the in-tree spec rather than delegating to Prometheus's
// promql.Engine — that's the whole point: catch bugs cerberus
// shares with upstream Prometheus.
//
// The PR 1 bridge oracle below (BridgePromQLOracle) is retained
// for two reasons:
//
//  1. It's still a useful sanity check during evaluator development
//     — if a query disagrees between the bridge oracle and the
//     from-scratch oracle, one of them has a bug.
//  2. Other consumers (the shadow harness, future regression
//     tests) may want a bridge to Prom's engine without pulling in
//     this oracle's evaluator. Keep the bridge wiring intact.
//
// The property test (test/property/promql_test.go) wires
// [promql.Evaluate] as the oracle going forward.
package oracle

import (
	"context"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/promshim/local"
	"github.com/tsouza/cerberus/test/property"
)

// BridgePromQLOracle evaluates q against d using Prometheus's own
// promql.Engine (wrapped by internal/promshim/local). It returns the
// result in the framework's property.Outcome shape.
//
// As of Phase 1 PR 2 the property test wires the from-scratch
// [oracle/promql] evaluator as the canonical oracle. This bridge is
// retained as a secondary sanity check — if a query disagrees
// between the bridge (delegating to Prom's engine) and the
// from-scratch oracle, one of them has a bug. It's also a useful
// helper for the shadow harness or any consumer that wants Prom-
// engine semantics without pulling in the from-scratch evaluator.
//
// The bridge:
//
//  1. Builds a local.SampleStore from the dataset's MetricsModel,
//     prepending the `__name__` label per series.
//  2. Runs an instant query at q.EvalTs against the SampleStore.
//  3. Converts each VectorSample / MatrixSeries into the
//     property.OutcomeRow shape the framework's comparator consumes.
func BridgePromQLOracle(d property.Dataset, q property.Query) property.Outcome {
	store := local.NewSampleStore()
	for _, s := range d.Metrics.Series {
		// Combine the user-defined labels with __name__ so the
		// resulting labelset matches what a Prometheus exporter
		// would have produced. SampleStore.Append takes a labels.
		// Labels value; we build it via labels.FromMap after
		// stamping __name__.
		lblMap := make(map[string]string, len(s.Labels)+1)
		for k, v := range s.Labels {
			lblMap[k] = v
		}
		lblMap["__name__"] = s.MetricName
		lset := labels.FromMap(lblMap)
		for _, p := range s.Points {
			store.Append(lset, p.TimestampMs, p.Value)
		}
	}

	eng := local.NewEngine(local.Options{})
	res, err := eng.Instant(context.Background(), store, q.String, time.Unix(q.EvalTs, 0).UTC())
	if err != nil {
		return property.Outcome{Err: err}
	}

	// PR 1 only generates instant queries, so the only kind worth
	// branching on is Vector. Matrix / Scalar are bridges to PR 2's
	// range-vector + scalar shapes — fall through to an empty result
	// (rather than panic) so an unexpected shape leaks through as a
	// "got 0 rows" mismatch that's easy to diagnose.
	out := property.Outcome{}
	if res.Kind != local.ResultKindVector {
		return out
	}

	// The evaluation timestamp surfaces on every VectorSample as
	// `T` in unix milliseconds. PromQL evaluators stamp every
	// sample at the eval ts (not the sample's source ts) — we
	// mirror that so cerberus's output (which also stamps at eval
	// ts via the /api/v1/query handler) compares against the
	// oracle's.
	evalMs := time.Unix(q.EvalTs, 0).UTC().UnixMilli()
	out.Rows = make([]property.OutcomeRow, 0, len(res.Vector))
	for _, s := range res.Vector {
		lblMap := make(map[string]string, s.Metric.Len())
		s.Metric.Range(func(l labels.Label) {
			// Strip __name__ from the per-row label map; cerberus's
			// HTTP response surfaces __name__ as a real label too,
			// so symmetric stripping keeps the comparator's
			// labelKey() canonical-form aligned.
			if l.Name == model.MetricNameLabel {
				return
			}
			lblMap[l.Name] = l.Value
		})
		out.Rows = append(out.Rows, property.OutcomeRow{
			Labels:      lblMap,
			TimestampMs: evalMs,
			Value:       s.V,
		})
	}
	return out
}
