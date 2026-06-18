//go:build chdb

package promql

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/property"
	oraclepromql "github.com/tsouza/cerberus/test/property/oracle/promql"
)

// TestExoticPromQL is the chDB-backed exotic-PromQL integration suite.
//
// It seeds ONE rich, fixed fixture (RichSeed) into an ephemeral chDB
// session, mounts the real prom.Handler behind an httptest server, then for
// every query in ExoticMatrix runs BOTH cerberus (parse -> lower ->
// optimize -> emit -> execute via the HTTP handler) AND the from-scratch
// oracle (test/property/oracle/promql.Evaluate), asserting they agree via
// property.CompareOutcomes (multiset, 1e-9 tol, NaN-equal, __name__-
// stripped).
//
// There is NO golden file and NO GOLDEN_UPDATE: expected results are
// computed at run time by the SUT-independent oracle, so a pass proves
// cerberus matches PromQL SEMANTICS, not that it reproduces a recording.
//
// CAT 1 (binary-op-over-rate) is the durable regression net for the prod
// code-47 break; it only passes once the vector-join code-47 fix
// (fix/vector-join-rate-metricname) is present, which this branch stacks
// on.
func TestExoticPromQL(t *testing.T) {
	ddl, model := RichSeed()

	cli := chclienttest.NewChDB(t)
	cli.Seed(t, ddl) // seed ONCE — every matrix query reads the same tables.

	h := prom.New(cli, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ds := property.Dataset{DDL: ddl, Metrics: model}

	// Seed-correctness self-check (design risk #2): before trusting the
	// matrix, prove the histogram BucketCounts prefix-sum + le synthesis is
	// right by running histogram_quantile against BOTH sides. If the seed's
	// cumulative _bucket{le} model diverges from cerberus's array fan-out,
	// this fails loudly here rather than masquerading as an engine bug
	// downstream.
	t.Run("seed-selfcheck/histogram_quantile", func(t *testing.T) {
		q := property.Query{
			String: "histogram_quantile(0.9, sum by(le)(rate(demo_api_request_duration_seconds_bucket[5m])))",
			EvalTs: EvalTs,
		}
		oracleOut := oraclepromql.Evaluate(ds, q, oraclepromql.Options{})
		cerberusOut := runCerberusInstant(t.Context(), srv.URL, q)
		if oracleOut.Err != nil {
			t.Fatalf("seed self-check: oracle errored: %v", oracleOut.Err)
		}
		if len(oracleOut.Rows) == 0 {
			t.Fatalf("seed self-check: oracle produced no histogram_quantile rows — seed is wrong")
		}
		zeroTimestamps(&oracleOut)
		zeroTimestamps(&cerberusOut)
		if diff := property.CompareOutcomes(oracleOut, cerberusOut); diff != "" {
			t.Fatalf("seed self-check histogram mismatch:\nquery=%s\n--- diff ---\n%s\n--- dataset ---\n%s",
				q.String, diff, dumpModel(model))
		}
	})

	for _, tc := range ExoticMatrix {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			q := property.Query{String: tc.promql, EvalTs: tc.ts()}
			oracleOut := oraclepromql.Evaluate(ds, q, oraclepromql.Options{})
			cerberusOut := runCerberusInstant(t.Context(), srv.URL, q)
			// Instant queries stamp every row at the single eval instant,
			// so the timestamp carries no information to assert on. The
			// oracle stamps a top-level SCALAR at ts=0 while cerberus
			// surfaces `scalar(...)`-style scalars as an eval-ts-stamped
			// label-less vector row — zero both sides so the comparison is
			// about VALUES + LABELS, not the wire timestamp.
			zeroTimestamps(&oracleOut)
			zeroTimestamps(&cerberusOut)
			if diff := property.CompareOutcomes(oracleOut, cerberusOut); diff != "" {
				t.Fatalf("exotic drift\n--- query ---\n%s\nevalTs=%d\n--- diff (want=oracle got=cerberus) ---\n%s",
					q.String, q.EvalTs, diff)
			}
		})
	}
}

// runCerberusInstant POSTs to /api/v1/query and decodes the Prom-shaped
// response into a property.Outcome. It handles both the "vector"
// resultType (the common case) and "scalar" (top-level scalar expressions
// like scalar(...) and -2^2), reshaping the latter into the same
// label-less single-row form the oracle's outcomeFromValue emits.
//
// Replicated from test/property/promql_test.go::runCerberusInstant (that
// helper lives in package property_test and isn't importable) with the
// scalar branch added for the exotic matrix.
func runCerberusInstant(ctx context.Context, baseURL string, q property.Query) property.Outcome {
	u := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d", baseURL, urlEscape(q.String), q.EvalTs)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: build request: %w", err)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: query roundtrip: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: read body: %w", err)}
	}

	var parsed struct {
		Status    string `json:"status"`
		ErrorType string `json:"errorType"`
		Error     string `json:"error"`
		Data      struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: decode body: %w; status=%d body=%s", err, resp.StatusCode, body)}
	}
	if parsed.Status != "success" {
		// A failed status is a legitimate outcome — the oracle may fail on
		// the same query too; CompareOutcomes treats both-erroring as
		// agreement.
		return property.Outcome{Err: fmt.Errorf("cerberus status=%q errorType=%q err=%q",
			parsed.Status, parsed.ErrorType, parsed.Error)}
	}

	switch parsed.Data.ResultType {
	case "vector":
		return decodeVector(parsed.Data.Result)
	case "scalar":
		return decodeScalar(parsed.Data.Result)
	default:
		return property.Outcome{Err: fmt.Errorf("cerberus resultType=%q, want vector or scalar", parsed.Data.ResultType)}
	}
}

func decodeVector(raw json.RawMessage) property.Outcome {
	var result []prom.VectorSample
	if err := json.Unmarshal(raw, &result); err != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: decode vector: %w", err)}
	}
	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(result))}
	for _, s := range result {
		stripped := make(map[string]string, len(s.Metric))
		for k, v := range s.Metric {
			if k == "__name__" {
				continue
			}
			stripped[k] = v
		}
		ts, val, perr := parseSample(s.Value)
		if perr != nil {
			return property.Outcome{Err: fmt.Errorf("exotic: parse sample: %w", perr)}
		}
		out.Rows = append(out.Rows, property.OutcomeRow{Labels: stripped, TimestampMs: ts, Value: val})
	}
	return out
}

// decodeScalar reshapes a "scalar" resultType ([ts, "value"]) into the same
// single label-less row at TimestampMs=0 the oracle emits for a top-level
// scalar (see oracle/promql.outcomeFromValue).
func decodeScalar(raw json.RawMessage) property.Outcome {
	var s prom.Sample
	if err := json.Unmarshal(raw, &s); err != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: decode scalar: %w", err)}
	}
	_, val, perr := parseSample(s)
	if perr != nil {
		return property.Outcome{Err: fmt.Errorf("exotic: parse scalar: %w", perr)}
	}
	return property.Outcome{Rows: []property.OutcomeRow{
		{Labels: map[string]string{}, TimestampMs: 0, Value: val},
	}}
}

// parseSample turns Prom's [seconds_float, value_string] shape into
// (unix_milliseconds, float64). The value string may be "NaN" / "+Inf" /
// "-Inf" — strconv.ParseFloat handles all three.
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

// urlEscape escapes the punctuation PromQL queries carry. Replicated from
// the property test's helper (same character set, plus `@`, `:`, `/`, `*`,
// `%`, `^`, `-`, `>`, `<`, `!`, `'` for the exotic matrix's richer
// expressions).
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
	case '{', '}', '"', '=', ',', '(', ')', '[', ']', ' ', '\n',
		'+', '&', '@', ':', '/', '*', '%', '^', '>', '<', '!', '\'':
		return true
	}
	return false
}

// zeroTimestamps sets every row's TimestampMs to 0 in place. Used to make
// instant-query comparisons timestamp-insensitive (see the call sites).
func zeroTimestamps(o *property.Outcome) {
	for i := range o.Rows {
		o.Rows[i].TimestampMs = 0
	}
}

// dumpModel renders the model's series for a failure log.
func dumpModel(m *property.MetricsModel) string {
	var out []byte
	for _, s := range m.Series {
		out = append(out, fmt.Sprintf("  %s%v points=%d\n", s.MetricName, s.Labels, len(s.Points))...)
	}
	return string(out)
}
