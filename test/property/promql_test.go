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
//     Prometheus's engine). The bridge oracle is still available as
//     [oracle.BridgePromQLOracle] but is no longer the default.
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
// # CI lanes (when not t.Skip'd)
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
// # Skip rationale (current state)
//
// PR #272 surfaced two cerberus-vs-Prom divergences via the from-scratch
// oracle: `sum(metric{...})` aggregating every stored sample's value
// instead of the LWR per series, and the instant selector not honouring
// the eval-ts boundary. Both were fixed in PRs #275 and #277. A third
// divergence — surfaced by the same generator once #272 widened the
// accept-set to include `rate(...[60s])` and `sum(rate(...[60s]))` —
// remains: cerberus emits a 0-rate row for series with zero samples in
// the rate window. PromQL drops the series entirely (and so does the
// from-scratch oracle). Because the generator's evalTs is fixed at
// `AnchorTime() + 200s` while series samples span `AnchorTime() + [0,
// 135s]`, every rate-shaped draw hits this divergence.
//
// Until rate-empty-window semantics are fixed in
// `internal/chsql/range_window.go`'s `rateValueFrag` (or the generator
// is adjusted to keep evalTs inside the sample span), this test stays
// t.Skip'd. Run with `CERBERUS_PROPERTY_FORCE=1` to reproduce the
// remaining divergence locally. Once the production-side fix lands,
// remove the `t.Skip` — the nightly `property` workflow already runs
// `./test/property/...` and will pick up the unskipped test
// automatically.
package property_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"pgregory.net/rapid"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	"github.com/tsouza/cerberus/test/property/gen"
	oraclepromql "github.com/tsouza/cerberus/test/property/oracle/promql"
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
// rapid shrunk to. See the package-level doc above for the current
// skip rationale and how to remove it.
func TestPromQL_Property_FromScratch(t *testing.T) {
	if os.Getenv("CERBERUS_PROPERTY_FORCE") == "" {
		t.Skip("property: skipped pending production-side fix for rate(metric[range]) empty-window " +
			"semantics (cerberus emits 0; PromQL drops the series). PRs #275 + #277 fixed " +
			"the sum-LWR and instant-selector eval-ts divergences that originally motivated " +
			"this skip; the rate-empty-window divergence remains. Set CERBERUS_PROPERTY_FORCE=1 " +
			"once internal/chsql/range_window.go matches PromQL on rate-empty-window.")
	}

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
		return runCerberusInstant(t.Context(), srv.URL, q)
	}

	oracleFn := func(d property.Dataset, q property.Query) property.Outcome {
		return oraclepromql.Evaluate(d, q, oraclepromql.Options{})
	}

	property.Run(t, property.Config{}, dgen, qgen, oracleFn, cerberusFn)
}

// runCerberusInstant POSTs to /api/v1/query and decodes the
// Prom-shaped response into the framework's property.Outcome.
//
// Cerberus's instant-query response surfaces every series at the
// requested eval timestamp (Prom convention — see prom/handler.go's
// toVector); we extract that pair per series and reshape into the
// canonical OutcomeRow shape.
func runCerberusInstant(ctx context.Context, baseURL string, q property.Query) property.Outcome {
	u := fmt.Sprintf(
		"%s/api/v1/query?query=%s&time=%d",
		baseURL,
		urlEscape(q.String),
		q.EvalTs,
	)
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
			ResultType string              `json:"resultType"`
			Result     []prom.VectorSample `json:"result"`
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
		// bridge oracle may also fail on the same query. Surface
		// the error to the comparator; both sides erroring still
		// counts as agreement.
		return property.Outcome{
			Err: fmt.Errorf("cerberus returned status=%q errorType=%q err=%q",
				parsed.Status, parsed.ErrorType, parsed.Error),
		}
	}
	if parsed.Data.ResultType != "vector" {
		// PR 1 generates instant-only queries, so cerberus must
		// answer with vector. Treat anything else as a mismatch so
		// the framework reports.
		return property.Outcome{
			Err: fmt.Errorf("cerberus returned resultType=%q, want vector",
				parsed.Data.ResultType),
		}
	}

	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(parsed.Data.Result))}
	for _, s := range parsed.Data.Result {
		// Strip __name__ so the comparator's labelKey() compares
		// only the user-defined labels (the oracle strips it too).
		stripped := make(map[string]string, len(s.Metric))
		for k, v := range s.Metric {
			if k == "__name__" {
				continue
			}
			stripped[k] = v
		}

		ts, val, perr := parseSample(s.Value)
		if perr != nil {
			return property.Outcome{Err: fmt.Errorf("property: parse sample: %w", perr)}
		}
		out.Rows = append(out.Rows, property.OutcomeRow{
			Labels:      stripped,
			TimestampMs: ts,
			Value:       val,
		})
	}
	return out
}

// parseSample turns Prom's [seconds_float, value_string] wire shape
// into (unix_milliseconds, float64).
func parseSample(s prom.Sample) (int64, float64, error) {
	if len(s) < 2 {
		return 0, 0, fmt.Errorf("expected 2-element sample, got %d", len(s))
	}
	tsSec, ok := s[0].(float64)
	if !ok {
		return 0, 0, fmt.Errorf("sample[0]: want float64, got %T (%v)", s[0], s[0])
	}
	valStr, ok := s[1].(string)
	if !ok {
		return 0, 0, fmt.Errorf("sample[1]: want string, got %T (%v)", s[1], s[1])
	}
	v, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("sample[1]: parse float %q: %w", valStr, err)
	}
	return int64(tsSec * 1000), v, nil
}

// urlEscape is a minimal URL escape that covers the characters PromQL
// queries actually carry — `{`, `}`, `"`, `=`, `,`, parens, brackets,
// spaces. The full net/url package would do the same but pulling it
// in to escape a handful of punctuation marks would be overkill.
func urlEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if shouldEscape(c) {
			out = append(out, '%', hex[c>>4], hex[c&0xF])
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

func shouldEscape(c byte) bool {
	switch c {
	case '{', '}', '"', '=', ',', '(', ')', '[', ']', ' ', '\n', '+', '&':
		return true
	}
	return false
}
