package promql

import (
	"math"
	"testing"
)

func TestTryFoldScalarDivisionIEEE754(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  float64
	}{
		{"1 / -0", math.Inf(-1)},
		{"-1 / -0", math.Inf(1)},
		{"Inf / -0", math.Inf(-1)},
		{"-Inf / -0", math.Inf(1)},
		{"NaN / 0", math.NaN()},
		{"NaN / -0", math.NaN()},
		{"0 / -0", math.NaN()},
		{"-0 / 0", math.NaN()},
		{"1 / 0", math.Inf(1)},
		{"-1 / 0", math.Inf(-1)},
		{"-0 / 1", math.Copysign(0, -1)},
		{"0 / -1", math.Copysign(0, -1)},
		{"1 / Inf", 0},
		{"1 / -Inf", math.Copysign(0, -1)},
		{"Inf / Inf", math.NaN()},
		{"2 / 1", 2},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			got, ok := TryFoldScalar(mustParse(t, tc.query))
			if !ok {
				t.Fatal("constant division was not folded")
			}
			if math.IsNaN(tc.want) {
				if !math.IsNaN(got) {
					t.Fatalf("got %v, want NaN", got)
				}
				return
			}
			if got != tc.want || math.Signbit(got) != math.Signbit(tc.want) {
				t.Fatalf("got %v (negative=%t), want %v (negative=%t)", got, math.Signbit(got), tc.want, math.Signbit(tc.want))
			}
		})
	}
}
