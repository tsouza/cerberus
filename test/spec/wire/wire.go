// Package wire hosts the shared "issue an instant PromQL query against a
// cerberus test server and decode the response into a property.Outcome"
// helper.
//
// test/property/promql_test.go and test/integration/promql/exotic_test.go
// both drive the real HTTP handler (parse -> lower -> optimize -> emit ->
// execute) via an httptest.Server and need the same request-build /
// response-decode logic; this package is the single copy both call.
package wire

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/test/property"
)

// InstantOptions configures RunInstant's request encoding and response
// decoding. The two call sites need genuinely different behaviour along two
// axes; both are explicit knobs here rather than folded into one behaviour,
// so hoisting the duplicated code doesn't change what either suite asserts.
type InstantOptions struct {
	// ExtraEscapeChars are additional punctuation bytes to percent-encode in
	// the query string, on top of the base set (`{}"=,()[] `+"\n"+`+&`
	// every generated PromQL query might carry). Percent-encoding an extra
	// byte never changes the decoded query string the server sees (Go's
	// query-string decoder unescapes it back to the exact original byte),
	// so widening this set is always safe; it exists purely so each call
	// site documents which punctuation its own query shapes can carry.
	// test/integration/promql/exotic_test.go's richer expressions need
	// `@:/*%^><!'` in addition to the base set.
	ExtraEscapeChars string
	// AllowScalar, when true, also accepts a "scalar" resultType response
	// (decoded into a single label-less row at TimestampMs=0), in addition
	// to "vector". When false, any non-"vector" resultType is a decode
	// error.
	//
	// test/property/promql_test.go's generator (gen.PromQLQuery) only ever
	// emits vector-shaped queries — bare selector / sum / sum-by / rate /
	// sum-rate — never a top-level scalar expression, so it leaves this
	// false: a future generator widening that starts producing a
	// scalar-evaluating query fails loudly here rather than being silently
	// decoded. test/integration/promql/exotic_test.go's matrix deliberately
	// includes top-level scalar expressions (e.g. `-2^2`, `scalar(...)`),
	// so it sets this true.
	AllowScalar bool
}

// RunInstant GETs /api/v1/query and decodes the Prom-shaped response into
// the framework's property.Outcome.
//
// Cerberus's instant-query response surfaces every series at the requested
// eval timestamp (Prom convention — see prom/handler.go's toVector); we
// extract that pair per series and reshape into the canonical OutcomeRow
// shape.
func RunInstant(ctx context.Context, baseURL string, q property.Query, opts InstantOptions) property.Outcome {
	u := fmt.Sprintf(
		"%s/api/v1/query?query=%s&time=%d",
		baseURL,
		EscapeQuery(q.String, opts.ExtraEscapeChars),
		q.EvalTs,
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("wire: build request: %w", err)}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("wire: query roundtrip: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return property.Outcome{Err: fmt.Errorf("wire: read body: %w", err)}
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
		return property.Outcome{
			Err: fmt.Errorf("wire: decode body: %w; status=%d body=%s",
				err, resp.StatusCode, body),
		}
	}
	if parsed.Status != "success" {
		// A failed-status response is a legitimate outcome — the oracle may
		// also fail on the same query. Surface the error to the
		// comparator; both sides erroring still counts as agreement.
		return property.Outcome{
			Err: fmt.Errorf("wire: cerberus returned status=%q errorType=%q err=%q",
				parsed.Status, parsed.ErrorType, parsed.Error),
		}
	}

	if parsed.Data.ResultType == "vector" {
		return decodeVector(parsed.Data.Result)
	}
	if opts.AllowScalar && parsed.Data.ResultType == "scalar" {
		return decodeScalar(parsed.Data.Result)
	}
	want := "vector"
	if opts.AllowScalar {
		want = "vector or scalar"
	}
	return property.Outcome{
		Err: fmt.Errorf("wire: cerberus returned resultType=%q, want %s",
			parsed.Data.ResultType, want),
	}
}

// decodeVector reshapes a "vector" resultType payload into a
// property.Outcome, one row per series.
func decodeVector(raw json.RawMessage) property.Outcome {
	var result []prom.VectorSample
	if err := json.Unmarshal(raw, &result); err != nil {
		return property.Outcome{Err: fmt.Errorf("wire: decode vector: %w", err)}
	}
	out := property.Outcome{Rows: make([]property.OutcomeRow, 0, len(result))}
	for _, s := range result {
		// Strip __name__ so the comparator's labelKey() compares only the
		// user-defined labels (the oracle strips it too).
		stripped := make(map[string]string, len(s.Metric))
		for k, v := range s.Metric {
			if k == "__name__" {
				continue
			}
			stripped[k] = v
		}

		if s.Value == nil {
			// A histogram-valued sample (s.Histogram set instead) has no
			// float Value; this harness only exercises float-valued PromQL
			// shapes today, so treat it as a decode error rather than a
			// nil-pointer panic.
			return property.Outcome{Err: fmt.Errorf("wire: vector sample %v has no float value (histogram-valued?)", s.Metric)}
		}
		ts, val, perr := ParseSample(*s.Value)
		if perr != nil {
			return property.Outcome{Err: fmt.Errorf("wire: parse sample: %w", perr)}
		}
		out.Rows = append(out.Rows, property.OutcomeRow{
			Labels:      stripped,
			TimestampMs: ts,
			Value:       val,
		})
	}
	return out
}

// decodeScalar reshapes a "scalar" resultType ([ts, "value"]) into a single
// label-less row at TimestampMs=0 — the same shape the from-scratch oracle
// emits for a top-level scalar (see oracle/promql.outcomeFromValue).
func decodeScalar(raw json.RawMessage) property.Outcome {
	var s prom.Sample
	if err := json.Unmarshal(raw, &s); err != nil {
		return property.Outcome{Err: fmt.Errorf("wire: decode scalar: %w", err)}
	}
	_, val, perr := ParseSample(s)
	if perr != nil {
		return property.Outcome{Err: fmt.Errorf("wire: parse scalar: %w", perr)}
	}
	return property.Outcome{Rows: []property.OutcomeRow{
		{Labels: map[string]string{}, TimestampMs: 0, Value: val},
	}}
}

// ParseSample turns Prom's [seconds_float, value_string] wire shape into
// (unix_milliseconds, float64). The value string may be "NaN" / "+Inf" /
// "-Inf" — strconv.ParseFloat handles all three.
//
// Exported so other test/property call sites decoding a different Prom
// response shape (e.g. a query_range matrix's per-point samples) can reuse
// the same parse without redeclaring it.
func ParseSample(s prom.Sample) (int64, float64, error) {
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

// baseEscapeChars are punctuation marks every generated PromQL query might
// carry that need percent-encoding to survive the request URL: `{`, `}`,
// `"`, `=`, `,`, parens, brackets, spaces, `+`, `&`.
const baseEscapeChars = "{}\"=,()[] \n+&"

// EscapeQuery is a minimal URL escape covering baseEscapeChars plus any
// caller-supplied extra bytes. The full net/url package would do the same
// but pulling it in to escape a handful of punctuation marks would be
// overkill.
func EscapeQuery(s, extra string) string {
	const hex = "0123456789ABCDEF"
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if shouldEscape(c, extra) {
			out = append(out, '%', hex[c>>4], hex[c&0xF])
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

func shouldEscape(c byte, extra string) bool {
	if strings.IndexByte(baseEscapeChars, c) >= 0 {
		return true
	}
	return extra != "" && strings.IndexByte(extra, c) >= 0
}
