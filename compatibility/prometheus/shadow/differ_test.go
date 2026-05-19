package shadow

import (
	"math"
	"strings"
	"testing"
)

func TestDiff_Identical(t *testing.T) {
	t.Parallel()
	a := VectorResult{Series: []Series{
		{
			Labels:  map[string]string{"__name__": "up", "job": "api"},
			Samples: []Sample{{TimestampMs: 1000, Value: 1.0}, {TimestampMs: 2000, Value: 1.0}},
		},
	}}
	b := a // identical contents are diff-equal

	d := Compare(a, b, DefaultDiffOptions())
	if !d.Equal {
		t.Fatalf("expected Equal=true, got %+v", d)
	}
	if len(d.Reasons)+len(d.ExtraInA)+len(d.ExtraInB) != 0 {
		t.Fatalf("expected no diffs, got %+v", d)
	}
}

func TestDiff_MissingSeries(t *testing.T) {
	t.Parallel()
	a := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1}}},
		{Labels: map[string]string{"job": "web"}, Samples: []Sample{{TimestampMs: 1000, Value: 1}}},
	}}
	b := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1}}},
	}}

	d := Compare(a, b, DefaultDiffOptions())
	if d.Equal {
		t.Fatalf("expected Equal=false, got Equal=true")
	}
	if len(d.ExtraInA) != 1 || !strings.Contains(d.ExtraInA[0], "web") {
		t.Fatalf("expected ExtraInA to contain 'web' series, got %+v", d.ExtraInA)
	}
	if len(d.ExtraInB) != 0 {
		t.Fatalf("expected ExtraInB empty, got %+v", d.ExtraInB)
	}
}

func TestDiff_ValueDrift(t *testing.T) {
	t.Parallel()
	a := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1.0}}},
	}}
	b := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1.5}}},
	}}

	d := Compare(a, b, DefaultDiffOptions())
	if d.Equal {
		t.Fatalf("expected Equal=false, got Equal=true")
	}
	if len(d.Reasons) == 0 || !strings.Contains(d.Reasons[0], "value[0]") {
		t.Fatalf("expected value-drift reason, got %+v", d.Reasons)
	}
}

func TestDiff_LabelSetDifference(t *testing.T) {
	t.Parallel()
	a := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1.0}}},
	}}
	b := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api", "env": "prod"}, Samples: []Sample{{TimestampMs: 1000, Value: 1.0}}},
	}}

	d := Compare(a, b, DefaultDiffOptions())
	if d.Equal {
		t.Fatalf("expected Equal=false on different label sets")
	}
	if len(d.ExtraInA) != 1 || len(d.ExtraInB) != 1 {
		t.Fatalf("expected one extra on each side, got A=%v B=%v", d.ExtraInA, d.ExtraInB)
	}
}

func TestDiff_EpsilonTolerance(t *testing.T) {
	t.Parallel()
	a := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1.0}}},
	}}
	b := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1.0 + 1e-12}}},
	}}

	d := Compare(a, b, DefaultDiffOptions())
	if !d.Equal {
		t.Fatalf("expected Equal=true within default epsilon, got %+v", d)
	}
}

func TestDiff_SampleCountMismatch(t *testing.T) {
	t.Parallel()
	a := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{
			{TimestampMs: 1000, Value: 1}, {TimestampMs: 2000, Value: 2},
		}},
	}}
	b := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: 1}}},
	}}

	d := Compare(a, b, DefaultDiffOptions())
	if d.Equal {
		t.Fatalf("expected Equal=false")
	}
	found := false
	for _, r := range d.Reasons {
		if strings.Contains(r, "sample count") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected 'sample count' reason, got %+v", d.Reasons)
	}
}

func TestDiff_NaNEquality(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	a := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: nan}}},
	}}
	b := VectorResult{Series: []Series{
		{Labels: map[string]string{"job": "api"}, Samples: []Sample{{TimestampMs: 1000, Value: nan}}},
	}}

	d := Compare(a, b, DefaultDiffOptions())
	if !d.Equal {
		t.Fatalf("expected NaN == NaN under shadow-mode diff, got %+v", d)
	}
}
