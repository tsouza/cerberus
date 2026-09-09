//go:build chdb

// Differential coverage for the MIXED sample shape — the fourteen-column
// result a `VectorSetOr` between a float-valued and a histogram-valued
// operand emits (internal/chsql's emitMixedVectorSetOp).
//
// The shape is the only one production's rowsCursor probes for that no
// chDB-backed test reached. Both halves of that gap were real and had to
// be closed together:
//
//  1. internal/chclienttest's chDB double dispatches on the result's
//     column shape and had four branches, none of them mixed. A mixed
//     result does not end in HistogramNegativeBucketCounts — the
//     trailing `_setop_is_histogram` discriminator displaces it — so it
//     fell to the default arm, which scans four destinations against
//     fourteen columns.
//  2. Nothing under test/property, the only consumer of
//     chclienttest.NewChDB, generated a float-vs-histogram `or`, so the
//     missing branch was never reached and the double's failure was
//     latent rather than observed.
//
// Closing either alone would have left untested code guarding untested
// code. This file is the second half: it drives the real query through
// the whole stack — HTTP handler, lowering, emitter, and the double's
// Query — and differentially checks the answer against the from-scratch
// oracle, exactly as the neighbouring property tests do.
//
// The dataset is composed from the two existing generators rather than a
// third one: gen.MetricsDataset draws float gauge series into
// otel_metrics_gauge and gen.ExpHistogramDataset draws exponential
// histograms into otel_metrics_exponential_histogram, and their metric
// name pools are disjoint, so concatenating the seeds and merging the
// oracle mirrors yields a dataset where one operand of the `or` is
// float-valued and the other histogram-valued. Deriving it that way
// keeps the mixed dataset honest as those generators evolve, instead of
// pinning a hand-written seed that would drift away from them.
package property_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oraclepromql "github.com/tsouza/cerberus/test/property/oracle/promql"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// mixedOrExampleSeeds are the generator seeds this test draws its
// datasets from. Several are used because a single draw can produce a
// dataset in which every histogram series' label set collides with a
// float series' — `or` then drops the whole right-hand side and the
// result is float-only, which does not exercise the mixed shape. The
// assertions below reject a run in which no seed produced a genuinely
// mixed answer, so a future generator change that made the mixture
// impossible fails loudly instead of passing vacuously.
var mixedOrExampleSeeds = []int{1, 2, 3, 5, 8, 13}

// TestPromQL_MixedOr_FloatOrHistogram differentially checks
// `<float selector> or <histogram selector>` against the from-scratch
// oracle, and asserts the answer really was mixed.
func TestPromQL_MixedOr_FloatOrHistogram(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sawFloat := false
	sawHistogram := false

	for _, seed := range mixedOrExampleSeeds {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			dataset, query := mixedOrExample(seed)

			cli.Seed(t, dataset.DDL)
			got := wire.RunInstant(t.Context(), srv.URL, query, wire.InstantOptions{})
			want := oraclepromql.Evaluate(dataset, query, oraclepromql.Options{})

			if diff := property.CompareOutcomes(want, got); diff != "" {
				t.Fatalf("mixed `or` diverged from the oracle\nquery: %s\n%s", query.String, diff)
			}

			for _, row := range got.Rows {
				if row.Histogram != nil {
					sawHistogram = true
				} else {
					sawFloat = true
				}
			}
		})
	}

	// Without this the test could pass on a float-only answer, which
	// never produces a fourteen-column result and so never reaches the
	// branch this file exists to exercise.
	if !sawHistogram || !sawFloat {
		t.Fatalf("no seed produced a genuinely mixed answer (sawFloat=%v sawHistogram=%v), "+
			"so the fourteen-column mixed shape was never emitted and this test proved nothing",
			sawFloat, sawHistogram)
	}
}

// mixedOrExample composes one float dataset and one histogram dataset
// into a single seed plus oracle mirror, and returns the `or` query over
// the two metric names they carry.
func mixedOrExample(seed int) (property.Dataset, property.Query) {
	floats := gen.MetricsDataset().Example(seed)
	histograms := gen.ExpHistogramDataset().Example(seed)

	merged := property.Dataset{
		DDL: floats.DDL + histograms.DDL,
		Metrics: &property.MetricsModel{
			Series: append(
				append([]property.SeriesData{}, floats.Metrics.Series...),
				histograms.Metrics.Series...,
			),
		},
	}

	query := property.Query{
		ShapeID: mixedOrShapeID,
		String: fmt.Sprintf("%s or %s",
			floats.Metrics.Series[0].MetricName,
			histograms.Metrics.Series[0].MetricName),
		EvalTs: mixedOrEvalTs(merged),
	}
	return merged, query
}

// mixedOrShapeID labels the semantic shape this file covers, matching
// the ShapeID convention the other PromQL property generators follow.
const mixedOrShapeID property.ShapeID = "mixed_or_float_or_histogram"

// mixedOrEvalTs returns an instant, in unix seconds, at which both
// operands have a sample: the latest timestamp that is at or before
// every series' last point. Evaluating past a series' end would let the
// PromQL staleness window drop it and quietly turn the answer
// single-sided.
func mixedOrEvalTs(d property.Dataset) int64 {
	const millisPerSecond = 1000
	earliestLast := int64(0)
	for _, s := range d.Metrics.Series {
		if len(s.Points) == 0 {
			continue
		}
		last := s.Points[len(s.Points)-1].TimestampMs
		if earliestLast == 0 || last < earliestLast {
			earliestLast = last
		}
	}
	return earliestLast / millisPerSecond
}
