//go:build chdb

// Property test for the PromQL pipeline.
//
// On every iteration:
//
//  1. The dataset generator (gen.MetricsDataset) draws a random
//     in-memory MetricsModel plus a parallel DDL script.
//  2. The framework seeds the DDL into an ephemeral chDB session
//     (shared across iterations; each iteration's CREATE OR REPLACE
//     TABLE statement keeps replays idempotent).
//  3. The PromQL generator (gen.PromQLQuery) draws a random query
//     targeted at the dataset's metric / label / value pool.
//  4. The from-scratch oracle (oracle/promql.Evaluate) evaluates the
//     query against an in-memory mirror of the dataset, implementing
//     PromQL semantics directly off the spec (no delegation to
//     Prometheus's engine).
//  5. Cerberus evaluates the query via its real HTTP handler — a
//     httptest.Server in front of the chDB-backed prom.Handler. The
//     handler runs the full parse → lower → optimize → emit → execute
//     pipeline.
//  6. The framework's CompareOutcomes diffs the two result sets and
//     fails the property if they drift.
//
// rapid's shrinker minimises the failing dataset + query before this
// test reports — the failure log shows the smallest reproducer.
//
// # CI lanes
//
// The test runs in two CI lanes:
//
//   - Locally and on any explicit `go test -tags chdb ./test/property/...`
//     invocation, rapid uses its default of 100 iterations.
//   - The nightly `property` workflow (`.github/workflows/property.yml`)
//     overrides to `-rapid.checks=500` for a deeper sweep.
//
// To reproduce a failing CI run locally, copy the rapid seed from the
// workflow log and re-run:
//
//	go test -tags chdb -run TestPromQL_Property_FromScratch \
//	    -rapid.seed=<N> ./test/property/...
//
// rapid persists the shrunk failing draw under `testdata/rapid/`; the
// nightly workflow archives that directory as an artifact on failure.
//
// # History of past divergences (resolved)
//
// The from-scratch oracle surfaced four cerberus-vs-Prom divergences:
//
//   - sum-LWR (#275) — FIXED
//   - instant-selector / VectorJoin eval-ts (#277) — FIXED
//   - rate / increase / delta / *_over_time empty-window zero rows
//     (#287 — `WHERE length(window_vals) >= N` on the outer SELECT of
//     `internal/chsql/range_window.go::emitWindowedArray`) — FIXED
//   - RangeWindow value alias case mismatch: the outer SELECT of
//     `emitWindowedArrayPairs` / `emitWindowedArray` /
//     `emitWindowedArrayMatrix` projected the windowed value as
//     lowercase `value`, while parent `Aggregate` referenced the
//     schema-cased `Value` column — FIXED by emitting `r.ValueColumn`
//     (PascalCase canonical) at all three emit sites; the
//     `projectValueOverInner` rename-workaround in
//     `internal/promql/instant_fns.go` is removed in the same change.
//
// # Divergence outside the generated eval-ts window
//
// One divergence sits outside the shape this generator draws:
// `sum(metric{...}[r])` over an evalTs that lies outside the data range
// makes cerberus emit a spurious `{} = 0` row, while PromQL specifies an
// empty result. Root cause: `chplan.Aggregate` with empty GroupBy emits
// `SELECT sum(Value) FROM (...)` without a HAVING/COUNT guard, so CH's
// "1 row per aggregate-only query" semantics produce a 0 even on empty
// input.
//
// `gen.PromQLQuery` anchors EvalTs after every dataset sample but inside
// Prometheus's 5-minute LookbackDelta, so the property test never draws
// that shape and runs unconditionally in the chdb lane. Widening the
// generator's EvalTs range is gated on the aggregate fix, which is one
// of:
//
//	a) Wrap with a `count()` subquery + outer `WHERE _cnt > 0`.
//	b) Add a `chsql.SelectBuilder.Having` slot + an `Aggregate`-level
//	   `HAVING count() > 0` for the GroupBy=[] case.
//	c) Lower `Aggregate(GroupBy=[], …)` into
//	   `Filter(_cnt > 0, Aggregate(…, count AS _cnt))` at the PromQL
//	   lowering layer + a downstream `Project` to drop `_cnt`.
//
// The shape reproduces off-grid at rapid seed 11512813954976776230
// (rapid v0.4.8).
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

// TestPromQL_Property_FromScratch wires every layer together for the
// instant-query / gauge MVP. rapid's default iteration count is 100
// (no per-test override here); the nightly `property` workflow overrides
// to `-rapid.checks=500`. Locally, pass `-rapid.checks=N` to widen or
// narrow the sweep on demand.
//
// The oracle is the from-scratch [oraclepromql.Evaluate] — PromQL
// semantics implemented in-tree, not the Prom engine.
//
// Failure logs include both the rapid seed (so the failing draw
// reproduces with `-rapid.seed=<N>`) and the minimised dataset / query
// rapid shrunk to.
func TestPromQL_Property_FromScratch(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dgen := func(rt *rapid.T) property.Dataset {
		return gen.MetricsDataset().Draw(rt, "dataset")
	}
	qgen := func(rt *rapid.T, d property.Dataset) property.Query {
		return gen.PromQLQuery(d).Draw(rt, "query")
	}

	// cerberusFn closes over the chDB client + http server: every
	// iteration first re-seeds the DDL (CREATE OR REPLACE TABLE makes
	// this idempotent against the prior iteration's rows) and then
	// runs the query via the real Prom HTTP handler.
	cerberusFn := func(d property.Dataset, q property.Query) property.Outcome {
		cli.Seed(t, d.DDL)
		return wire.RunInstant(t.Context(), srv.URL, q, wire.InstantOptions{})
	}

	oracleFn := func(d property.Dataset, q property.Query) property.Outcome {
		return oraclepromql.Evaluate(d, q, oraclepromql.Options{})
	}

	property.Run(t, property.Config{}, dgen, qgen, oracleFn, cerberusFn)
}
