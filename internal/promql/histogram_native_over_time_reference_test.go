package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/promqltest"
)

// TestExpHistogramOverTime_ReferenceOnlyStorageBoundaries pins the upstream
// semantics for three native-histogram combinations the stock OTel
// exponential-histogram table cannot encode. Float samples live in separate
// Gauge/Sum tables, custom-bucket native histograms have no OTel exponential
// row shape, and the table has no counter-reset-hint column. These are oracle
// obligations, not license to invent a cross-table series join in lowering.
func TestExpHistogramOverTime_ReferenceOnlyStorageBoundaries(t *testing.T) {
	t.Parallel()

	storage := promqltest.LoadedStorage(t, `
load 1m
  mixed 1 {{schema:0 sum:2 count:2 buckets:[2]}}
  incompatible {{schema:0 sum:1 count:1 buckets:[1]}} {{schema:-53 sum:2 count:2 custom_values:[1] buckets:[2 0]}}
  reset_late {{schema:0 sum:1 count:0 buckets:[0]}} {{schema:0 sum:1 count:2 buckets:[2]}} {{schema:0 sum:1 count:3 buckets:[3]}} {{schema:0 sum:1 count:2 buckets:[2]}}
`)
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Errorf("close upstream test storage: %v", err)
		}
	})
	engine := promqltest.NewTestEngine(t, false, 0, promqltest.DefaultMaxSamplesPerQuery)
	at := time.Unix(int64(3*time.Minute/time.Second), 0).UTC()

	for _, tc := range []struct {
		name        string
		query       string
		wantSamples int
		wantFloat   float64
		wantWarning string
	}{
		{
			name:        "sum mixed float and histogram",
			query:       "sum_over_time(mixed[5m])",
			wantWarning: "encountered a mix of histograms and floats",
		},
		{
			name:        "avg mixed float and histogram",
			query:       "avg_over_time(mixed[5m])",
			wantWarning: "encountered a mix of histograms and floats",
		},
		{
			name:        "sum exponential and custom schemas",
			query:       "sum_over_time(incompatible[5m])",
			wantWarning: "mix of histograms with exponential and custom buckets schemas",
		},
		{
			name:        "avg exponential and custom schemas",
			query:       "avg_over_time(incompatible[5m])",
			wantWarning: "mix of histograms with exponential and custom buckets schemas",
		},
		{
			name:        "sum reset hint collision keeps payload",
			query:       "histogram_count(sum_over_time(reset_late[5m]))",
			wantSamples: 1,
			wantFloat:   7,
			wantWarning: "conflicting counter resets during histogram aggregation",
		},
		{
			name:        "avg reset hint collision keeps payload",
			query:       "histogram_count(avg_over_time(reset_late[5m]))",
			wantSamples: 1,
			wantFloat:   1.75,
			wantWarning: "conflicting counter resets during histogram aggregation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, err := engine.NewInstantQuery(context.Background(), storage, nil, tc.query, at)
			if err != nil {
				t.Fatalf("NewInstantQuery(%q): %v", tc.query, err)
			}
			defer query.Close()

			result := query.Exec(context.Background())
			vector, err := result.Vector()
			if err != nil {
				t.Fatalf("Exec(%q): %v", tc.query, err)
			}
			if len(vector) != tc.wantSamples {
				t.Fatalf("Exec(%q) returned %d samples, want %d: %v", tc.query, len(vector), tc.wantSamples, vector)
			}
			warnings := fmt.Sprint(result.Warnings.AsErrors())
			if !strings.Contains(warnings, tc.wantWarning) {
				t.Errorf("Exec(%q) warnings %q do not contain %q", tc.query, warnings, tc.wantWarning)
			}
			if tc.wantSamples == 0 {
				return
			}
			if vector[0].H != nil {
				t.Fatalf("Exec(%q) returned a histogram after histogram_count: %v", tc.query, vector[0])
			}
			if vector[0].F != tc.wantFloat {
				t.Errorf("Exec(%q) histogram_count = %v, want %v", tc.query, vector[0].F, tc.wantFloat)
			}
		})
	}
}
