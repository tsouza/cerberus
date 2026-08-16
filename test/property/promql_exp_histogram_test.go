//go:build chdb

// Property test for the PromQL native (exponential) histogram
// functions.
//
// This is the histogram-valued sibling of
// TestPromQL_Property_FromScratch (promql_test.go), which draws gauge
// series only. The wiring is identical — random dataset, random query,
// from-scratch oracle versus the real HTTP handler over chDB — but both
// generators draw the native-histogram shapes:
//
//  1. gen.ExpHistogramDataset draws OTel exponential-histogram series
//     (scale, zero bucket, positive and negative bucket arrays) and
//     renders them into an otel_metrics_exponential_histogram seed
//     script alongside the in-memory mirror the oracle reads.
//  2. gen.ExpHistogramQuery draws selector-only value functions plus
//     histogram-valued bare/sum/rate/increase and wrapped quantile shapes.
//  3. The from-scratch oracle (test/property/oracle/promql's
//     histogram_native.go) walks the bucket layout in Go, off the
//     documented exponential-schema semantics — it never sees
//     cerberus's lowering, its SQL, or its results.
//  4. Cerberus answers the same query through parse → lower → optimize
//     → emit → execute, where the whole bucket walk is ClickHouse
//     array arithmetic emitted by internal/chsql.
//
// The two implementations share no code: a Go loop over []uint64 on one
// side, an arrayCumSum / arrayFirstIndex expression tree on the other.
// Their INDEPENDENCE is real but not total, and the limit is worth
// stating plainly. The oracle's bucket walk was written against the
// same algorithm cerberus emits, so where both are wrong in the same
// way this test agrees rather than reports. One such class is known and
// tracked in #2066 (cerberus and this oracle both walk the cumulative
// array forward for every phi; reference Prometheus walks backward for
// phi >= 0.5, which differs at an exact rank tie). Agreement here is
// therefore strong evidence against transcription and arithmetic
// errors, and no evidence at all about a shared misreading of the spec
// — that is the parity layer's job, below.
//
// What this catches that the layers around it do not:
//
//   - The `-- parity --` fixtures under test/spec/promql/ are checked
//     against Prometheus's OWN engine (test/spec/parity_native_histogram_chdb.go),
//     so they arbitrate the spec — but only on the handful of seeds
//     someone thought to write down. Two of them,
//     edge_hq_native_positive_only_p0 and edge_hq_native_trailing_empty_p1,
//     are what established the empty-bucket saturation rule this
//     oracle's phi == 0 / phi == 1 edges follow.
//   - The non-parity txtar fixtures pin emitted SQL and a chDB
//     roundtrip, with expected rows regenerated from cerberus, so they
//     catch drift rather than a wrong-but-stable answer.
//   - test/integration/promql's cat13 runs the same oracle over two
//     FIXED seeds.
//
// This test's contribution is breadth: it sweeps the input space those
// fixed points sit in, which is where transcription errors in the
// bucket walk actually live.
//
// Reproduce a CI failure with the seed from the log:
//
//	go test -tags chdb,agpl_oracle,chdb_agpl_oracle -run TestPromQL_Property_NativeHistogram \
//	    -rapid.seed=<N> ./test/property/...
package property_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oraclepromql "github.com/tsouza/cerberus/test/property/oracle/promql"
	"github.com/tsouza/cerberus/test/spec/wire"
)

// TestPromQL_Property_NativeHistogram differentially checks every
// native-histogram function against the from-scratch oracle over
// randomly drawn exponential histograms.
func TestPromQL_Property_NativeHistogram(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dgen := func(rt *rapid.T) property.Dataset {
		return gen.ExpHistogramDataset().Draw(rt, "expHistDataset")
	}
	qgen := func(rt *rapid.T, d property.Dataset) property.Query {
		return gen.ExpHistogramQuery(d).Draw(rt, "expHistQuery")
	}

	cerberusFn := func(d property.Dataset, q property.Query) property.Outcome {
		cli.Seed(t, d.DDL)
		return wire.RunInstant(t.Context(), srv.URL, q, wire.InstantOptions{})
	}

	oracleFn := func(d property.Dataset, q property.Query) property.Outcome {
		return oraclepromql.Evaluate(d, q, oraclepromql.Options{})
	}

	property.Run(t, property.Config{}, dgen, qgen, oracleFn, cerberusFn)
}

// TestPromQL_NativeHistogramShapeRoster executes one deterministic live
// differential for every enrolled native-histogram semantic shape.
func TestPromQL_NativeHistogramShapeRoster(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	property.RunShapeExamples(
		t,
		gen.ExpHistogramShapeIDs(),
		func(shapeID gen.ShapeID, seed int) (property.Dataset, property.Query) {
			dataset := gen.ExpHistogramDataset().Example(seed)
			return dataset, gen.ExpHistogramQueryForShape(dataset, shapeID).Example(seed)
		},
		func(_ *testing.T, dataset property.Dataset, query property.Query) property.Outcome {
			return oraclepromql.Evaluate(dataset, query, oraclepromql.Options{})
		},
		func(t *testing.T, dataset property.Dataset, query property.Query) property.Outcome {
			cli.Seed(t, dataset.DDL)
			return wire.RunInstant(t.Context(), srv.URL, query, wire.InstantOptions{})
		},
	)
}
