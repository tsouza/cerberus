package prom_test

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// The PromQL functions whose scalar parameters have an evaluation-time
// domain reject the same values whether the parameter was written as a
// literal or computed from a vector. Reference Prometheus reaches that
// verdict by evaluating the parameter first and inspecting the values;
// these tests pin the computed spelling against reference's exact
// message and status, because the literal spelling was already correct
// and it is the computed one that used to answer 200 with a substituted
// value.
//
// Reference wording, verified in the pinned engine
// (github.com/tsouza/prometheus@v0.0.1-cerberus-parser):
//
//   - promql/engine.go rangeEvalAgg → `ev.errorf("Parameter value is NaN")`
//   - promql/functions.go funcDoubleExponentialSmoothing →
//     `panic(fmt.Errorf("invalid smoothing factor. Expected: 0 < sf < 1, got: %f", sf))`
//
// Reference surfaces both as an evaluation error: HTTP 422 with
// errorType "execution".

// scalarDomainCase is one computed-parameter query, the value its
// parameter evaluates to, and what reference Prometheus answers.
type scalarDomainCase struct {
	name string
	// query carries a computed scalar parameter — a `scalar(<vector>)`
	// call, so the value is unknowable until the query runs.
	query string
	// paramValues is the value series the stubbed backend reports for
	// every query, including the parameter's own evaluation — so it is
	// what the computed parameter evaluates to, one entry per step.
	paramValues []float64
	// wantError is reference Prometheus's message, verbatim.
	wantError string
}

func TestQuery_ComputedScalarParameterDomain(t *testing.T) {
	cases := []scalarDomainCase{
		{
			name:        "topk K is NaN",
			query:       `topk(scalar(k_metric), up)`,
			paramValues: []float64{math.NaN()},
			wantError:   "Parameter value is NaN",
		},
		{
			name:        "bottomk K is NaN",
			query:       `bottomk(scalar(k_metric), up)`,
			paramValues: []float64{math.NaN()},
			wantError:   "Parameter value is NaN",
		},
		{
			name:        "limitk K is NaN",
			query:       `limitk(scalar(k_metric), up)`,
			paramValues: []float64{math.NaN()},
			wantError:   "Parameter value is NaN",
		},
		{
			name:        "topk K overflows int64",
			query:       `topk(scalar(k_metric), up)`,
			paramValues: []float64{1e300},
			wantError:   "Scalar value 1e+300 overflows int64",
		},
		{
			// Reference checks underflow against params.Min() and
			// overflow against params.Max(), so only a series carrying
			// BOTH an in-range step and an underflowing one reaches the
			// underflow branch: a series that underflows at every step
			// has max < 1 and answers empty instead.
			name:        "topk K underflows int64",
			query:       `topk(scalar(k_metric), up)`,
			paramValues: []float64{math.Inf(-1), 5},
			wantError:   "Scalar value -Inf underflows int64",
		},
		{
			name:        "limit_ratio ratio is NaN",
			query:       `limit_ratio(scalar(r_metric), up)`,
			paramValues: []float64{math.NaN()},
			wantError:   "Ratio value is NaN",
		},
		{
			name:        "smoothing factor above the open interval",
			query:       `double_exponential_smoothing(up[5m], scalar(f_metric), 0.3)`,
			paramValues: []float64{1.5},
			wantError:   "invalid smoothing factor. Expected: 0 < sf < 1, got: 1.500000",
		},
		{
			name:        "smoothing factor at the closed lower bound",
			query:       `double_exponential_smoothing(up[5m], scalar(f_metric), 0.3)`,
			paramValues: []float64{0},
			wantError:   "invalid smoothing factor. Expected: 0 < sf < 1, got: 0.000000",
		},
		{
			name:        "trend factor above the open interval",
			query:       `double_exponential_smoothing(up[5m], 0.3, scalar(f_metric))`,
			paramValues: []float64{2},
			wantError:   "invalid trend factor. Expected: 0 < tf < 1, got: 2.000000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := runPromQuery(t, tc.query, tc.paramValues)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d (reference rejects this query at eval time); body = %s",
					status, http.StatusUnprocessableEntity, body.raw)
			}
			if body.Status != "error" {
				t.Errorf("status field = %q, want %q", body.Status, "error")
			}
			if body.ErrorType != "execution" {
				t.Errorf("errorType = %q, want %q", body.ErrorType, "execution")
			}
			if body.Error != tc.wantError {
				t.Errorf("error = %q, want %q (reference Prometheus's own wording)", body.Error, tc.wantError)
			}
		})
	}
}

// TestQuery_ComputedScalarParameterInDomain pins the other half of the
// contract: a computed parameter reference ACCEPTS must still answer.
// Without it the domain checks could pass by rejecting everything.
func TestQuery_ComputedScalarParameterInDomain(t *testing.T) {
	for _, tc := range []struct {
		name        string
		query       string
		paramValues []float64
	}{
		{"topk K of 2", `topk(scalar(k_metric), up)`, []float64{2}},
		{"topk K below 1 is empty, not an error", `topk(scalar(k_metric), up)`, []float64{0}},
		{
			"smoothing factor inside the open interval",
			`double_exponential_smoothing(up[5m], scalar(f_metric), 0.3)`,
			[]float64{0.5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := runPromQuery(t, tc.query, tc.paramValues)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (reference accepts this query); body = %s", status, body.raw)
			}
		})
	}
}

// promErrorBody is the shape the Prometheus HTTP API answers an error
// with, plus the raw payload for failure messages.
type promErrorBody struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	raw       string
}

// runPromQuery drives one instant query against a handler whose backend
// reports paramValues as its result rows — so that series is what the
// query's computed scalar parameter evaluates to when the guard runs the
// parameter's own statement.
func runPromQuery(t *testing.T, query string, paramValues []float64) (int, promErrorBody) {
	t.Helper()
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	samples := make([]chclient.Sample, len(paramValues))
	for i, v := range paramValues {
		samples[i] = chclient.Sample{
			MetricName: "up",
			Labels:     map[string]string{"job": "api"},
			Timestamp:  ts.Add(time.Duration(i) * time.Minute),
			Value:      v,
		}
	}
	q := &stubQuerier{samples: samples}
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		t.Fatalf("query request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var body promErrorBody
	dec := json.NewDecoder(resp.Body)
	raw := map[string]any{}
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode response: %v", err)
	}
	body.raw = string(encoded)
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return resp.StatusCode, body
}
