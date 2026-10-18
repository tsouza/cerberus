package promql

import (
	"strings"
	"testing"
)

// The int64 bounds are inclusive in reference Prometheus: a K equal to
// either bound is already out of range.
func TestAggregationParamDomain_BoundsAreInclusive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  []float64
		wantErr string
	}{
		{"exactly the lower bound", []float64{aggParamMinInt64, aggregationParamMinK}, "underflows int64"},
		{"exactly the upper bound", []float64{aggParamMaxInt64}, "overflows int64"},
		{"in range", []float64{aggregationParamMinK}, ""},
	} {
		empty, err := aggregationParamDomain(tc.values)
		if empty {
			t.Errorf("%s: aggregationParamDomain(%v) reported empty", tc.name, tc.values)
		}
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%s: aggregationParamDomain(%v) = %v; want no error", tc.name, tc.values, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: aggregationParamDomain(%v) = %v; want an error containing %q", tc.name, tc.values, err, tc.wantErr)
		}
	}
}

// limit_ratio is empty only when every step is zero; one zero end of the
// series is not enough.
func TestLimitRatioParamDomain_EmptyNeedsBothExtremaZero(t *testing.T) {
	for _, tc := range []struct {
		name      string
		values    []float64
		wantEmpty bool
	}{
		{"all zero", []float64{0, 0}, true},
		{"no values", nil, true},
		{"zero minimum only", []float64{0, 0.5}, false},
		{"zero maximum only", []float64{-0.5, 0}, false},
	} {
		empty, err := limitRatioParamDomain(tc.values)
		if err != nil || empty != tc.wantEmpty {
			t.Errorf("%s: limitRatioParamDomain(%v) = %v, %v; want %v, nil", tc.name, tc.values, empty, err, tc.wantEmpty)
		}
	}
}
