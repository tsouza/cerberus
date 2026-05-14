//go:build chdb

// Property test for the LogQL pipeline.
//
// On every iteration:
//
//  1. The dataset generator (gen.LogsDataset) draws a random
//     in-memory LogsModel plus a parallel DDL script.
//  2. The framework seeds the DDL into an ephemeral chDB session
//     (shared across iterations; each iteration's CREATE OR REPLACE
//     TABLE statement keeps replays idempotent).
//  3. The LogQL generator (gen.LogQLQuery) draws a random query
//     targeted at the dataset's stream-label / body-token pool.
//  4. The from-scratch oracle (oracle/logql.Evaluate) evaluates the
//     query against an in-memory mirror of the dataset, implementing
//     LogQL log-stream semantics directly off the Loki docs (no
//     delegation to Loki's engine).
//  5. Cerberus evaluates the query via its real HTTP handler — a
//     httptest.Server in front of the chDB-backed loki.Handler. The
//     handler runs the full parse → lower → optimize → emit →
//     execute → post-process pipeline.
//  6. The framework's CompareOutcomes diffs the two result sets and
//     fails the property if they drift.
//
// rapid's shrinker minimises the failing dataset + query before this
// test reports — the failure log shows the smallest reproducer.
//
// # CI lanes
//
// Like TestPromQL_Property_FromScratch, this test runs in two lanes:
//
//   - Locally and on any explicit `go test -tags chdb ./test/property/...`
//     invocation, rapid uses its default of 100 iterations.
//   - The nightly `property` workflow (`.github/workflows/property.yml`)
//     overrides to `-rapid.checks=500` for a deeper sweep.
//
// To reproduce a failing CI run locally, copy the rapid seed from the
// workflow log and re-run:
//
//	go test -tags chdb -run TestLogQL_Property \
//	    -rapid.seed=<N> ./test/property/...
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
	"sort"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oraclelogql "github.com/tsouza/cerberus/test/property/oracle/logql"
)

// TestLogQL_Property wires every layer together for the log-stream
// MVP. rapid's default iteration count is 100 (no per-test override
// here); the nightly `property` workflow overrides to
// `-rapid.checks=500`. Locally, pass `-rapid.checks=N` to widen or
// narrow the sweep on demand.
//
// The oracle is the from-scratch [oraclelogql.Evaluate] — LogQL
// semantics implemented in-tree, not the Loki engine.
//
// Failure logs include both the rapid seed (so the failing draw
// reproduces with `-rapid.seed=<N>`) and the minimised dataset /
// query rapid shrunk to.
func TestLogQL_Property(t *testing.T) {
	cli := chclienttest.NewChDB(t)
	h := loki.New(cli, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dgen := func(rt *rapid.T) property.Dataset {
		return gen.LogsDataset().Draw(rt, "dataset")
	}
	qgen := func(rt *rapid.T, d property.Dataset) property.Query {
		return gen.LogQLQuery(d).Draw(rt, "query")
	}

	// cerberusFn closes over the chDB client + HTTP server: every
	// iteration first re-seeds the DDL (CREATE OR REPLACE TABLE
	// makes this idempotent against the prior iteration's rows) and
	// then runs the query via the real Loki HTTP handler.
	cerberusFn := func(d property.Dataset, q property.Query) property.Outcome {
		cli.Seed(t, d.DDL)
		return runCerberusLogQLInstant(t.Context(), srv.URL, q, d.Logs)
	}

	oracleFn := func(d property.Dataset, q property.Query) property.Outcome {
		return oraclelogql.Evaluate(d, q)
	}

	property.RunLogs(t, property.Config{}, dgen, qgen, oracleFn, cerberusFn)
}

// runCerberusLogQLInstant POSTs to /loki/api/v1/query and decodes
// the Loki-shaped response into the framework's property.Outcome.
//
// The Loki instant-query response shape is {status, data: {
// resultType, result }}. For log-stream queries the result is an
// array of Streams; each Stream has a labelset (the post-pipeline
// stream identity) and a Values array of [unix_nanos_string,
// line_text] pairs. We pivot each (label set, value pair) into one
// OutcomeRow so the framework's comparator can pair-match against
// the oracle's row stream.
//
// The d.Logs handle is used to recover the dataset's
// LogAnchorTime — the cerberus handler clips records to its
// instant-lookback window so the query parameters must thread a
// [start, end] window that covers every record. The generator
// anchors records 15s apart starting at LogAnchorTime, so a
// window from LogAnchorTime - 1m → LogAnchorTime + 10m covers
// every record without bringing in adjacent runs.
func runCerberusLogQLInstant(ctx context.Context, baseURL string, q property.Query, logs *property.LogsModel) property.Outcome {
	// Use /query_range instead of /query: instant has a 5min
	// lookback only and the property test's records live at
	// anchor + (15s * i) for i in 0..4 — that fits in the default
	// instant lookback, but the explicit range query lets us pin
	// the window so a future generator widening (more records, or
	// wider spacing) doesn't silently start dropping records on
	// the cerberus side.
	startTs := gen.LogAnchorTime().Add(-1 * time.Minute).Unix()
	endTs := gen.LogAnchorTime().Add(10 * time.Minute).Unix()
	u := fmt.Sprintf(
		"%s/loki/api/v1/query_range?query=%s&start=%d&end=%d&step=60",
		baseURL,
		urlEscape(q.String),
		startTs,
		endTs,
	)
	_ = logs // reserved for future windowing decisions tied to the dataset

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

	var parsed struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string         `json:"resultType"`
			Result     []loki.Stream  `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return property.Outcome{
			Err: fmt.Errorf("property: decode body: %w; status=%d body=%s",
				err, resp.StatusCode, body),
		}
	}
	if parsed.Status != "success" {
		// A failed-status response is a legitimate outcome — the
		// oracle may also fail on the same query. Surface the
		// error so the comparator can pair both-error outcomes.
		return property.Outcome{
			Err: fmt.Errorf("cerberus returned status=%q errorType=%q err=%q",
				parsed.Status, parsed.ErrorType, parsed.Error),
		}
	}
	if parsed.Data.ResultType != "streams" {
		return property.Outcome{
			Err: fmt.Errorf("cerberus returned resultType=%q, want streams",
				parsed.Data.ResultType),
		}
	}

	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, 4)}
	for _, s := range parsed.Data.Result {
		stripped := copyLabels(s.Stream)
		for _, v := range s.Values {
			ts, line, perr := parseStreamSample(v)
			if perr != nil {
				return property.Outcome{Err: fmt.Errorf("property: parse stream sample: %w", perr)}
			}
			out.Rows = append(out.Rows, property.OutcomeRow{
				Labels:      copyLabels(stripped),
				TimestampMs: ts / int64(1e6),
				Line:        line,
			})
		}
	}
	// Deterministic ordering eases failure-log readability: the
	// framework's indexOutcomeRows already pairs by (label set, ts,
	// line), but a stable sort here keeps a successful-iteration's
	// diff-log shape predictable.
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].TimestampMs != out.Rows[j].TimestampMs {
			return out.Rows[i].TimestampMs < out.Rows[j].TimestampMs
		}
		return out.Rows[i].Line < out.Rows[j].Line
	})
	return out
}

// parseStreamSample decodes a [unix_nanos_string, line_text] tuple
// from Loki's streams response shape.
func parseStreamSample(v [2]string) (int64, string, error) {
	ts, err := strconv.ParseInt(v[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("parse timestamp %q: %w", v[0], err)
	}
	return ts, v[1], nil
}

// copyLabels returns a fresh map[string]string identical to in. The
// stream Values array shares the parent Stream.Stream reference
// across pairs, so the test must clone before stamping per-row
// labels to avoid aliasing.
func copyLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
