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
//  2. gen.ExpHistogramQuery draws one of the seven PromQL functions
//     that read a native histogram, over a bare selector.
//  3. The from-scratch oracle (test/property/oracle/promql's
//     histogram_native.go) walks the bucket layout in Go, off the
//     documented exponential-schema semantics — it never sees
//     cerberus's lowering, its SQL, or its results.
//  4. Cerberus answers the same query through parse → lower → optimize
//     → emit → execute, where the whole bucket walk is ClickHouse
//     array arithmetic emitted by internal/chsql.
//
// The two implementations therefore share nothing but the seed rows and
// the PromQL spec, which is what makes an agreement between them
// evidence. The arithmetic is genuinely independent: a Go loop over
// []uint64 on one side, an arrayCumSum / arrayFirstIndex expression
// tree on the other.
//
// What this catches that the layers around it do not:
//
//   - test/spec/promql/histogram_*_exp.txtar pins the emitted SQL and a
//     chDB roundtrip for a handful of hand-written seeds. It cannot
//     tell a wrong-but-stable answer from a right one, because the
//     expected rows are regenerated from cerberus itself.
//   - test/integration/promql's cat13 runs the same oracle over two
//     FIXED seeds. It pins the algorithm at two points; this test
//     sweeps the input space around them.
//
// Reproduce a CI failure with the seed from the log:
//
//	go test -tags chdb -run TestPromQL_Property_NativeHistogram \
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
