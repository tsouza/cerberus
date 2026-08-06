//go:build chdb

// chDB-backed VALUE oracle for the native-histogram zero bucket's
// one-sided clamp (#1836).
//
// The clamp only has an observable effect when the deployment persists a
// zero threshold: schema.DefaultOTelMetrics leaves ZeroThresholdColumn
// empty because the upstream exporter's exp-histogram DDL does not write
// the OTLP zero_threshold field, and with no threshold both edges of the
// zero band collapse onto the constant 0 — a point, whatever the
// distribution's shape. Every TXTAR fixture runs on that default DDL, so
// none of them can tell a clamped band from an unclamped one.
//
// This test configures the override that a deployment with the column set
// uses, and asserts the property the clamp exists for: a distribution with
// observations on ONE side of zero can never report a quantile on the
// other side. Reference Prometheus (promql/quantile.go) clamps the
// impossible edge to zero — positive-only gives the zero bucket
// [0, zeroThreshold], negative-only gives [-zeroThreshold, 0]. The
// unclamped band [-zeroThreshold, +zeroThreshold] straddles zero, so
// interpolating in it hands a positive-only histogram a NEGATIVE answer
// (and the mirror for negative-only) — which is the bug.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// zeroBandExpHistDDL mirrors the exporter's exp-histogram table plus the
// ZeroThreshold column an OTLP-complete deployment persists.
const zeroBandExpHistDDL = `CREATE TABLE otel_metrics_exponential_histogram (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64,
    Scale Int32,
    ZeroCount UInt64,
    ZeroThreshold Float64,
    PositiveOffset Int32,
    PositiveBucketCounts Array(UInt64),
    NegativeOffset Int32,
    NegativeBucketCounts Array(UInt64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// zeroBandThreshold is the seeded zero-bucket half-width. It is wide
// enough that a mis-signed interpolation inside the band lands far from
// zero rather than on a rounding boundary.
const zeroBandThreshold = 4.0

// newChDBServerWithZeroThreshold builds a handler whose schema names the
// ZeroThreshold column, then seeds it.
func newChDBServerWithZeroThreshold(t *testing.T, seed string) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	s := schema.DefaultOTelMetrics()
	s.ZeroThresholdColumn = "ZeroThreshold"
	h := prom.New(c, s, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestQuery_NativeZeroBandClamp_ChDB seeds a positive-only and a
// negative-only exponential histogram, each with half its observations in
// the zero bucket, and picks a phi that lands inside that bucket. The
// answer must stay on the populated side of zero.
func TestQuery_NativeZeroBandClamp_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")

	// Ten observations in the zero bucket and ten in a single ordinary
	// bucket on one side. A phi inside the zero bucket's rank span is what
	// exercises the band; the ordinary bucket exists so the zero bucket is
	// not the whole distribution, which would make every phi degenerate.
	seed := zeroBandExpHistDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_exponential_histogram VALUES
    ('positive_only_exp_hist', map('side', 'positive'), toDateTime64('%s', 9), 1.0, 0, 10, %v, 0, [10], 0, []),
    ('negative_only_exp_hist', map('side', 'negative'), toDateTime64('%s', 9), 1.0, 0, 10, %v, 0, [], 0, [10]);`,
		ts, zeroBandThreshold, ts, zeroBandThreshold)

	srv := newChDBServerWithZeroThreshold(t, seed)

	cases := []struct {
		name string
		// query's phi lands inside the zero bucket in both seeds: the
		// positive-only distribution counts the zero bucket first, the
		// negative-only one counts it after the negative bucket.
		query string
		// lo / hi are the reference's clamped zero bucket for that
		// distribution. The unclamped band is [-zeroBandThreshold,
		// +zeroBandThreshold], so a pre-fix answer falls outside.
		lo, hi float64
	}{
		{
			name:  "positive-only",
			query: "histogram_quantile(0.1, positive_only_exp_hist)",
			lo:    0,
			hi:    zeroBandThreshold,
		},
		{
			name:  "negative-only",
			query: "histogram_quantile(0.9, negative_only_exp_hist)",
			lo:    -zeroBandThreshold,
			hi:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=%s&time=%d",
				srv.URL, escape(tc.query), seedTime.Unix()))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}

			var parsed queryResponse
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v\nbody=%s", err, body)
			}
			if parsed.Status != "success" {
				t.Fatalf("status=%q err=%s", parsed.Status, parsed.Error)
			}

			rawResult, _ := json.Marshal(parsed.Data.Result)
			var vec []prom.VectorSample
			if err := json.Unmarshal(rawResult, &vec); err != nil {
				t.Fatalf("unmarshal vector: %v\nresult=%s", err, rawResult)
			}
			if len(vec) != 1 {
				t.Fatalf("got %d samples, want 1: %s", len(vec), rawResult)
			}
			got, err := strconv.ParseFloat(fmt.Sprint(vec[0].Value[1]), 64)
			if err != nil {
				t.Fatalf("parse value %v: %v", vec[0].Value[1], err)
			}
			if got < tc.lo || got > tc.hi {
				t.Fatalf("quantile = %v, want within the clamped zero bucket [%v, %v]; "+
					"the unclamped band [%v, %v] straddles zero and puts the answer on "+
					"the side of zero where this distribution has no observations (#1836)",
					got, tc.lo, tc.hi, -zeroBandThreshold, zeroBandThreshold)
			}
		})
	}
}
