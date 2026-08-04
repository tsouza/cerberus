//go:build chdb

// Property test for the PromQL RANGE path (/api/v1/query_range).
//
// # Why this test exists (the gap it closes — #1499)
//
// TestPromQL_Property_FromScratch (promql_test.go) only ever drives
// /api/v1/query — a single eval instant. The range path
// (/api/v1/query_range, a [start,end,step] grid) has its own lowering
// (internal/promql's range-query plan, internal/chsql's RangeWindow /
// ts_grid emitters) and is exactly where the repo's recurring
// window-anchor bug class lives: an endpoint anchoring to now64()
// (server wall-clock at execution) instead of the request's actual
// [start,end] window. That bug is invisible to an instant-only oracle
// sweep — it only manifests when start/end/step actually vary and the
// grid crosses (or starts before, or ends after) the seeded data.
//
// This file closes that gap with the minimal architecture the problem
// actually needs: PromQL range-query semantics ARE repeated instant
// evaluation at each grid timestamp (Prometheus's own definition), so
// no new evaluator logic is required — gen.PromQLRangeQuery draws a
// [start,end,step] grid (deliberately including offsets before and
// past the dataset's data window, see its doc comment), the existing
// from-scratch instant oracle is evaluated once per anchor via
// oracleRangeOutcome (already defined in rate_dup_timestamp_test.go),
// and cerberus is driven through its real query_range HTTP handler via
// runCerberusRange (ditto). The framework's CompareOutcomes already
// supports multi-row-per-series (matrix) comparison, so no framework
// changes were needed either.
//
// # Aggregate-empty-GroupBy landmine
//
// gen.PromQLRangeQuery's expression pool deliberately excludes the
// ungrouped `sum(...)` / `sum(rate(...))` shapes the instant generator
// draws, to avoid re-triggering the separately-tracked
// aggregate-empty-GroupBy bug documented in promql_test.go's doc
// comment (chplan.Aggregate with empty GroupBy spuriously emits a
// `{}=0` row on empty input instead of an empty result) — see
// gen/promql_range.go's doc comment for the full reasoning. That bug
// is orthogonal to the window-anchor property this test targets, and
// letting it fire on every off-window anchor would drown the signal
// this test exists to catch.
//
// # CI lane
//
// Build-tagged `chdb`; runs in the same `property` workflow as the
// other property tests (`go test -tags chdb ./test/property/...`).
package property_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
)

// TestPromQL_RangeProperty_FromScratch sweeps random [start,end,step]
// grids (including offsets before/past the seeded data window) against
// random datasets and queries, asserting cerberus's real
// /api/v1/query_range matrix agrees with the from-scratch instant
// oracle evaluated independently at every grid anchor.
//
// rapid's default iteration count applies (100 locally; the nightly
// `property` workflow overrides to -rapid.checks=500). To reproduce a
// failing CI run locally:
//
//	go test -tags chdb -run TestPromQL_RangeProperty_FromScratch \
//	    -rapid.seed=<N> ./test/property/...
func TestPromQL_RangeProperty_FromScratch(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rapid.Check(t, func(rt *rapid.T) {
		ds := gen.MetricsDataset().Draw(rt, "dataset")
		if len(ds.Metrics.NamesPresent()) == 0 {
			return
		}
		rq := gen.PromQLRangeQuery(ds).Draw(rt, "rangeQuery")
		if rq.String == "" {
			return
		}

		cli.Seed(t, ds.DDL)

		anchors := gridAnchors(rq.StartSec, rq.EndSec, rq.StepSec)
		oracleOut := oracleRangeOutcome(ds, rq.String, anchors)
		if oracleOut.Err != nil {
			// The oracle rejecting a shape this generator draws is a
			// test-harness bug, not a property finding — every shape
			// PromQLRangeQuery emits is one the oracle is supposed to
			// support (bare selector / grouped sum / bare rate).
			rt.Fatalf("range property: oracle error on generated query\nquery=%s start=%d end=%d step=%d\nerr=%v",
				rq.String, rq.StartSec, rq.EndSec, rq.StepSec, oracleOut.Err)
		}

		cerberusOut := runCerberusRange(
			context.Background(), srv.URL, rq.String,
			rq.StartSec, rq.EndSec, rq.StepSec,
		)
		if cerberusOut.Err != nil {
			rt.Fatalf("range property: cerberus error\nquery=%s start=%d end=%d step=%d\nerr=%v",
				rq.String, rq.StartSec, rq.EndSec, rq.StepSec, cerberusOut.Err)
		}

		if diff := property.CompareOutcomes(oracleOut, cerberusOut); diff != "" {
			rt.Fatalf("range property: drift vs oracle across grid %d..%d/%ds\nquery=%s\n--- diff ---\n%s",
				rq.StartSec, rq.EndSec, rq.StepSec, rq.String, diff)
		}
	})
}
