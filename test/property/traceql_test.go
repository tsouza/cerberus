//go:build chdb

// Property test for the TraceQL pipeline.
//
// On every iteration:
//
//  1. The dataset generator (gen.TraceQLDataset) draws a random
//     in-memory MetricsModel of OTel-CH span rows plus a parallel
//     DDL script.
//  2. The framework seeds the DDL into an ephemeral chDB session
//     (shared across iterations; each iteration's
//     CREATE OR REPLACE TABLE statement keeps replays idempotent).
//  3. The TraceQL generator (gen.TraceQLQuery) draws a random query
//     targeting the dataset's service pool — either a bare
//     `{ resource.service.name = "<value>" }` selector or a
//     selector + `| count() OP N` scalar filter.
//  4. The from-scratch oracle (oracle/traceql.Evaluate) evaluates the
//     query against an in-memory mirror of the dataset, implementing
//     spanset filter + count() semantics directly from the TraceQL
//     spec (no Tempo engine dependency).
//  5. Cerberus evaluates the query via its real HTTP handler — a
//     httptest.Server in front of the chDB-backed tempo.Handler. The
//     handler runs the full parse → lower → optimize → emit → execute
//     pipeline.
//  6. The framework's CompareOutcomes diffs the two result sets and
//     fails the property if they drift.
//
// rapid's shrinker minimises the failing dataset + query before this
// test reports — the failure log shows the smallest reproducer.
//
// # Wire-shape comparison
//
// Tempo's /api/search response shape:
//
//	{
//	  "traces":  [<TraceSummary>, ...],
//	  "metrics": {"inspectedTraces": <N>}
//	}
//
// `inspectedTraces` is populated as `len(res.Samples)`. For both
// selector and aggregate shapes the comparator uses that count
// directly — the oracle emits one outcome row per matching span (or
// one row when `| count() OP N` is satisfied, zero when it isn't),
// and we compare row counts.
//
// The TraceSummary collapse rule (Tempo keys by SpanName+Timestamp,
// merging spans that share that tuple) is avoided by the generator:
// each span gets a unique (SpanName, Timestamp) pair via a per-span
// index suffix on the span name. See gen/traceql.go.
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
//	go test -tags chdb -run TestTraceQL_Property -rapid.seed=<N> \
//	    ./test/property/...
//
// rapid persists the shrunk failing draw under `testdata/rapid/`; the
// nightly workflow archives that directory as an artifact on failure.
package property_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oracletraceql "github.com/tsouza/cerberus/test/property/oracle/traceql"
)

// TestTraceQL_Property wires every layer together for the TraceQL
// selector / count-filter shapes. rapid's default iteration count is
// 100 (no per-test override here); the nightly `property` workflow
// overrides to `-rapid.checks=500`. Locally, pass `-rapid.checks=N`
// to widen or narrow the sweep on demand.
//
// The oracle is the from-scratch [oracletraceql.Evaluate] — spanset +
// count() semantics implemented in-tree, not the Tempo engine.
//
// Failure logs include both the rapid seed (so the failing draw
// reproduces with `-rapid.seed=<N>`) and the minimised dataset / query
// rapid shrunk to.
func TestTraceQL_Property(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := tempo.New(cli, schema.DefaultOTelTraces(), "v1.0.0-property", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dgen := func(rt *rapid.T) property.Dataset {
		return gen.TraceQLDataset().Draw(rt, "dataset")
	}
	qgen := func(rt *rapid.T, d property.Dataset) property.Query {
		return gen.TraceQLQuery(d).Draw(rt, "query")
	}

	// cerberusFn closes over the chDB client + http server: every
	// iteration first re-seeds the DDL (CREATE OR REPLACE TABLE makes
	// this idempotent against the prior iteration's rows) and then
	// runs the query via the real Tempo HTTP handler.
	cerberusFn := func(d property.Dataset, q property.Query) property.Outcome {
		cli.Seed(t, d.DDL)
		return runCerberusTraceQL(t.Context(), srv.URL, q)
	}

	oracleFn := func(d property.Dataset, q property.Query) property.Outcome {
		return oracletraceql.Evaluate(d, q)
	}

	property.Run(t, property.Config{}, dgen, qgen, oracleFn, cerberusFn)
}

// runCerberusTraceQL GETs /api/search?q=<query> and decodes the
// Tempo-shaped response into the framework's property.Outcome.
//
// Tempo's /api/search wire shape:
//
//	{
//	  "traces":  [<TraceSummary>, ...],
//	  "metrics": {"inspectedTraces": N, ...}
//	}
//
// `inspectedTraces` equals `len(res.Samples)` from cerberus's handler
// (see internal/api/tempo/handler.go's SearchMetrics population). The
// oracle emits one row per matching span / one row for a satisfied
// `count() OP N`, so the row-count comparison is exact when we
// reshape the cerberus response into "inspectedTraces empty-label
// rows".
//
// We use InspectedTraces rather than len(traces) because the Tempo
// search response collapses TraceSummary entries that share
// (SpanName, Timestamp) — the generator already avoids that collapse
// by stamping unique suffixes, but reading from InspectedTraces makes
// the comparator robust against a future generator widening.
func runCerberusTraceQL(ctx context.Context, baseURL string, q property.Query) property.Outcome {
	u := fmt.Sprintf("%s/api/search?q=%s", baseURL, urlEscape(q.String))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("property: build request: %w", err)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("property: query roundtrip: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("property: read body: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		// Non-2xx is a legitimate cerberus outcome — the oracle may
		// also error on the same query. The framework's
		// CompareOutcomes treats both-erroring queries as agreement.
		return property.Outcome{
			Err: fmt.Errorf("cerberus returned status=%d body=%s", resp.StatusCode, body),
		}
	}

	var parsed tempo.SearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return property.Outcome{
			Err: fmt.Errorf("property: decode body: %w; status=%d body=%s",
				err, resp.StatusCode, body),
		}
	}

	// Reshape InspectedTraces (== len(res.Samples)) into
	// InspectedTraces empty-label OutcomeRows. The framework's
	// CompareOutcomes counts rows per label-key, so this gives the
	// per-iteration row-count equality check the oracle is set up to
	// support.
	rows := make([]property.OutcomeRow, 0, parsed.Metrics.InspectedTraces)
	for i := 0; i < parsed.Metrics.InspectedTraces; i++ {
		rows = append(rows, property.OutcomeRow{
			Labels:      map[string]string{},
			TimestampMs: 0,
			Value:       0,
		})
	}
	return property.Outcome{Rows: rows}
}
