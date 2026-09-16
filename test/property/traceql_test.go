//go:build chdb

// Property test for the TraceQL pipeline.
//
// On every iteration:
//
//  1. The dataset generator (gen.TraceQLDataset) draws a random
//     in-memory TracesModel of OTel-CH span rows plus a parallel
//     DDL script.
//  2. The framework seeds the DDL into an ephemeral chDB session
//     (shared across iterations; each iteration's
//     CREATE OR REPLACE TABLE statement keeps replays idempotent).
//  3. The TraceQL generator (gen.TraceQLQuery) draws a random query
//     from 17 stable shapes across 14 weighted families — attribute
//     matchers (resource + span scope,
//     equality/negation/regex), duration/status/name intrinsics,
//     multi-condition filters, structural relations (`>`/`>>`),
//     count()/avg()/min()/max()/sum() scalar-filter pipelines, and
//     select() — see gen.TraceQLQuery's doc for the full list.
//  4. The from-scratch oracle (oracle/traceql.Evaluate) evaluates the
//     query against an in-memory mirror of the dataset, implementing
//     spanset filter + count() semantics directly from the TraceQL
//     spec (no Tempo engine dependency).
//  5. Cerberus evaluates the query via its real HTTP handler — a
//     httptest.Server in front of the chDB-backed tempo.Handler. The
//     handler runs the full parse → lower → optimize → emit → execute
//     pipeline.
//  6. The framework's CompareTraceIdentityOutcomes diffs the two result
//     sets by TraceID multiset and fails the property if they drift.
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
// Each TraceSummary carries a real TraceID (internal/api/tempo/handler.go's
// toTraceSummaries) plus, for a selector/structural/select() shape, a
// SpanSet whose Matched field is the trace's TRUE matched-span count
// (uncapped by the spss display limit). runCerberusTraceQL reshapes the
// response into one property.OutcomeRow per matched span — TraceID
// repeated Matched times per trace — mirroring the oracle's own per-span
// row shape (oracle/traceql.Evaluate's doc). A trace-scoped aggregate
// pipeline (count()/avg|min|max|sum(duration)) collapses to one summary
// row per trace with no SpanSet at all, so those traces contribute exactly
// one row each — again mirroring the oracle's per-trace SET projection for
// that shape. property.CompareTraceIdentityOutcomes then multiset-compares
// rows by TraceID: a substituted, missing, duplicated, or scope-swapped
// trace identity is caught even when the total row count agrees, which
// the previous inspected-span-count-only comparison could not
// distinguish.
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
//   - Locally and on any explicit composite-tag property invocation
//     invocation, rapid uses its default of 100 iterations.
//   - The nightly `property` workflow (`.github/workflows/property.yml`)
//     overrides to `-rapid.checks=500` for a deeper sweep.
//
// To reproduce a failing CI run locally, copy the rapid seed from the
// workflow log and re-run:
//
//	go test -tags chdb,agpl_oracle,chdb_agpl_oracle -run TestTraceQL_Property -rapid.seed=<N> \
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
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oracletraceql "github.com/tsouza/cerberus/test/property/oracle/traceql"
	"github.com/tsouza/cerberus/test/spec/wire"
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

	property.Run(t, property.Config{Compare: property.CompareTraceIdentityOutcomes}, dgen, qgen, oracleFn, cerberusFn)
}

// TestTraceQLDescendantPropertyMatch is the stable two-span form of the
// descendant relation exercised by TestTraceQL_Property. The root uses the
// canonical OTel empty ParentSpanId and the child references it directly, so
// the real parse -> lower -> emit -> chDB -> HTTP path must return one
// trace-level empty-label result for the matching descendant.
func TestTraceQLDescendantPropertyMatch(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	cli.Seed(t, `CREATE TABLE otel_traces (
    Timestamp DateTime64(9),
    TraceId String,
    SpanId String,
    ParentSpanId String,
    SpanName String,
    SpanKind LowCardinality(String),
    ServiceName LowCardinality(String),
    ResourceAttributes Map(String, String),
    SpanAttributes Map(String, String),
    Duration Int64,
    StatusCode LowCardinality(String),
    StatusMessage String,
    ScopeName String,
    ScopeVersion String
) ENGINE = MergeTree ORDER BY (Timestamp, TraceId);
INSERT INTO otel_traces VALUES
    (toDateTime64('2026-05-13 12:00:00', 9), '0102030405060708090a0b0c0d0e0f10', '0102030405060708', '', 'root', 'Internal', 'web', map('service.name', 'web'), map(), 1, 'Unset', '', '', ''),
    (toDateTime64('2026-05-13 12:00:01', 9), '0102030405060708090a0b0c0d0e0f10', '1112131415161718', '0102030405060708', 'child', 'Internal', 'batch', map('service.name', 'batch'), map(), 1, 'Unset', '', '', '');`)

	h := tempo.New(cli, schema.DefaultOTelTraces(), "v1.0.0-property", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	query := property.Query{
		String: `{ resource.service.name = "web" } >> { resource.service.name = "batch" }`,
		EvalTs: gen.TraceQLAnchorTime().Add(time.Hour).Unix(),
	}
	want := property.Outcome{Rows: []property.OutcomeRow{{Labels: map[string]string{}}}}
	got := runCerberusTraceQL(t.Context(), srv.URL, query)
	if diff := property.CompareOutcomes(want, got); diff != "" {
		t.Fatalf("descendant property drift\n%s", diff)
	}
}

// TestTraceQL_PropertyShapeRoster executes one deterministic live
// differential per enrolled selector, intrinsic, structural, and pipeline
// shape under the same composite tags as the random sweep.
func TestTraceQL_PropertyShapeRoster(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := tempo.New(cli, schema.DefaultOTelTraces(), "v1.0.0-property-shapes", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	property.RunShapeExamplesWithComparator(
		t,
		gen.TraceQLShapeIDs(),
		func(shapeID gen.ShapeID, seed int) (property.Dataset, property.Query) {
			dataset := gen.TraceQLDataset().Example(seed)
			return dataset, gen.TraceQLQueryForShape(dataset, shapeID).Example(seed)
		},
		func(_ *testing.T, dataset property.Dataset, query property.Query) property.Outcome {
			return oracletraceql.Evaluate(dataset, query)
		},
		func(t *testing.T, dataset property.Dataset, query property.Query) property.Outcome {
			cli.Seed(t, dataset.DDL)
			return runCerberusTraceQL(t.Context(), srv.URL, query)
		},
		property.CompareTraceIdentityOutcomes,
	)
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
// traceIdentityRows reshapes parsed.Traces into one OutcomeRow per matched
// span (TraceID repeated by that trace's SpanSet.Matched count) for a
// selector/structural/select() shape, or exactly one row per trace for a
// trace-scoped aggregate shape whose summaries carry no SpanSet at all —
// see its own doc for the full projection this mirrors on the oracle
// side (oracle/traceql.Evaluate's doc).
//
// The X-Cerberus-Inspected-Spans header — the drained span-ROW count,
// independent of how toTraceSummaries grouped them — is cross-checked
// against the reshaped rows' own total below as a secondary, complementary
// signal: for the selector/structural/select() shapes (the only shapes
// whose summaries carry a SpanSet, so the only shapes this check applies
// to) the two must agree, or the wire reply is internally inconsistent
// (a malformed-reply case that must stay red rather than silently produce
// a green row-count coincidence).
// propertyWindowMarginSec brackets the dataset anchor by ~a year on each
// side — wide enough that the /api/search window can never clip a generated
// span, while still being a real (non-windowless) request that skips the
// DefaultSearchLookback clamp.
const propertyWindowMarginSec int64 = 366 * 24 * 60 * 60

func runCerberusTraceQL(ctx context.Context, baseURL string, q property.Query) property.Outcome {
	// Thread an explicit time window bracketing the dataset anchor. A
	// windowless /api/search clamps to [now-1h, now] (DefaultSearchLookback,
	// added with the trace-limit/window pushdown); this dataset is stamped at
	// a fixed historical anchor (gen.TraceQLAnchorTime), so a windowless
	// request would filter every span out and drift from the window-less
	// oracle. Both bounds present skips the clamp; the wide margin can't clip
	// any generated span (the oracle has no window concept, so over-wide is
	// safe). Tests what Grafana's Traces Drilldown actually sends — a window.
	anchorSec := gen.TraceQLAnchorTime().Unix()
	startSec, endSec := anchorSec-propertyWindowMarginSec, anchorSec+propertyWindowMarginSec
	u := fmt.Sprintf("%s/api/search?q=%s&start=%d&end=%d", baseURL, wire.EscapeQuery(q.String, ""), startSec, endSec)
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
		// Surface non-2xx as a system error. The fail-closed verdict rejects
		// it even when the oracle also errors.
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

	inspectedSpans, err := strconv.Atoi(resp.Header.Get(tempo.HeaderInspectedSpans))
	if err != nil {
		return property.Outcome{
			Err: fmt.Errorf("property: %s header %q: %w", tempo.HeaderInspectedSpans,
				resp.Header.Get(tempo.HeaderInspectedSpans), err),
		}
	}

	rows, matchedTotal, anySpanSet := traceIdentityRows(parsed.Traces)
	if anySpanSet && matchedTotal != inspectedSpans {
		return property.Outcome{
			Err: fmt.Errorf("property: trace summaries' matched-span total=%d disagrees with %s header=%d",
				matchedTotal, tempo.HeaderInspectedSpans, inspectedSpans),
		}
	}
	return property.Outcome{Rows: rows}
}

// traceIdentityRows reshapes a /api/search response's TraceSummary array
// into the OutcomeRow shape property.CompareTraceIdentityOutcomes expects:
// one row per matched SPAN (TraceID repeated by SpanSet.Matched — the
// trace's true matched-span count, uncapped by the spss display limit;
// see internal/api/tempo/handler.go's observeSpan) for a
// selector/structural/select() shape, or exactly one row per trace for a
// trace-scoped aggregate (count()/avg|min|max|sum(duration)) shape, whose
// collapsed-to-one-row-per-trace summaries carry no SpanSet at all
// (isSpansetAggregateShape's branch never populates the reserved
// __cerberus_spanID slot, so buildSpanSet returns nil). Distinguishing the
// two shapes from the wire response alone — rather than from the query
// text — means this helper never needs to duplicate the oracle's own
// parser: a summary's SpanSet presence already tells us which projection
// its endpoint promises.
//
// Returns the rows, the summed per-trace matched-span count (0 when no
// summary carried a SpanSet), and whether any summary carried one at all
// (so the caller's inspected-span cross-check can skip the aggregate shape,
// which has no wire-observable per-trace count to check it against).
func traceIdentityRows(traces []tempo.TraceSummary) (rows []property.OutcomeRow, matchedTotal int, anySpanSet bool) {
	for _, tr := range traces {
		if len(tr.SpanSets) == 0 {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}, TraceID: tr.TraceID})
			continue
		}
		anySpanSet = true
		matched := tr.SpanSets[0].Matched
		matchedTotal += matched
		for i := 0; i < matched; i++ {
			rows = append(rows, property.OutcomeRow{Labels: map[string]string{}, TraceID: tr.TraceID})
		}
	}
	return rows, matchedTotal, anySpanSet
}
