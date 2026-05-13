package local

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// seedDataset populates a SampleStore with three series of http_requests_total
// counters and one `up` series, spanning a 5-minute window at 15s resolution.
//
// Layout (counter values are cumulative, starting at 0):
//
//	http_requests_total{job="api",   method="get"}  → +1 every 15s
//	http_requests_total{job="api",   method="post"} → +2 every 15s
//	http_requests_total{job="batch", method="get"}  → +5 every 15s
//	up{job="api"}                                   ≡ 1
//	up{job="batch"}                                 ≡ 1
//
// baseTS is the inclusive start of the 5-minute window.
func seedDataset(t *testing.T, store *SampleStore, baseTS time.Time) {
	t.Helper()
	step := 15 * time.Second
	const samples = 21 // 5 minutes inclusive of both endpoints at 15s

	series := []struct {
		lset      labels.Labels
		increment float64
	}{
		{labels.FromStrings("__name__", "http_requests_total", "job", "api", "method", "get"), 1},
		{labels.FromStrings("__name__", "http_requests_total", "job", "api", "method", "post"), 2},
		{labels.FromStrings("__name__", "http_requests_total", "job", "batch", "method", "get"), 5},
	}
	for _, s := range series {
		var v float64
		for i := 0; i < samples; i++ {
			ts := baseTS.Add(time.Duration(i) * step)
			store.Append(s.lset, ts.UnixMilli(), v)
			v += s.increment
		}
	}
	upSeries := []labels.Labels{
		labels.FromStrings("__name__", "up", "job", "api"),
		labels.FromStrings("__name__", "up", "job", "batch"),
	}
	for _, lset := range upSeries {
		for i := 0; i < samples; i++ {
			ts := baseTS.Add(time.Duration(i) * step)
			store.Append(lset, ts.UnixMilli(), 1)
		}
	}
}

func TestEngine_Instant(t *testing.T) {
	t.Parallel()
	baseTS := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	store := NewSampleStore()
	seedDataset(t, store, baseTS)
	eng := NewEngine(Options{})
	ctx := context.Background()

	tests := []struct {
		name      string
		query     string
		evalAt    time.Time
		wantKind  ResultKind
		wantCount int // expected number of series (0 means "don't check")
		// check is an optional extra assertion on the result.
		check func(t *testing.T, res Result)
	}{
		{
			name:      "rate_5m_yields_three_series",
			query:     `rate(http_requests_total[5m])`,
			evalAt:    baseTS.Add(5 * time.Minute),
			wantKind:  ResultKindVector,
			wantCount: 3,
			check: func(t *testing.T, res Result) {
				// At evalAt, each series has been incrementing for 5 minutes
				// (300s). rate() over [5m] for the api/get series should equal
				// (20 increments * 1) / 300s ≈ 0.0666...; api/post ≈ 0.1333...;
				// batch/get ≈ 0.3333...
				for _, s := range res.Vector {
					method := s.Metric.Get("method")
					job := s.Metric.Get("job")
					switch {
					case job == "api" && method == "get":
						assertApprox(t, s.V, 20.0/300.0, 1e-6, "api/get rate")
					case job == "api" && method == "post":
						assertApprox(t, s.V, 40.0/300.0, 1e-6, "api/post rate")
					case job == "batch" && method == "get":
						assertApprox(t, s.V, 100.0/300.0, 1e-6, "batch/get rate")
					default:
						t.Fatalf("unexpected series in rate result: %s", s.Metric)
					}
				}
			},
		},
		{
			name:      "sum_by_job_collapses_methods",
			query:     `sum by (job) (rate(http_requests_total[5m]))`,
			evalAt:    baseTS.Add(5 * time.Minute),
			wantKind:  ResultKindVector,
			wantCount: 2,
			check: func(t *testing.T, res Result) {
				got := map[string]float64{}
				for _, s := range res.Vector {
					got[s.Metric.Get("job")] = s.V
				}
				// api: (20 + 40) / 300 = 0.2
				assertApprox(t, got["api"], 0.2, 1e-6, "sum by job=api")
				// batch: 100 / 300 ≈ 0.3333
				assertApprox(t, got["batch"], 100.0/300.0, 1e-6, "sum by job=batch")
			},
		},
		{
			name:      "scalar_literal",
			query:     `scalar(vector(42))`,
			evalAt:    baseTS.Add(5 * time.Minute),
			wantKind:  ResultKindScalar,
			wantCount: 0,
			check: func(t *testing.T, res Result) {
				if res.Scalar == nil {
					t.Fatalf("expected scalar result, got nil")
				}
				assertApprox(t, res.Scalar.V, 42, 1e-9, "scalar value")
			},
		},
		{
			name:      "up_matcher_filters_to_one_series",
			query:     `up{job="api"}`,
			evalAt:    baseTS.Add(2 * time.Minute),
			wantKind:  ResultKindVector,
			wantCount: 1,
			check: func(t *testing.T, res Result) {
				if got := res.Vector[0].Metric.Get("job"); got != "api" {
					t.Fatalf("expected job=api, got %q", got)
				}
				assertApprox(t, res.Vector[0].V, 1, 1e-9, "up value")
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := eng.Instant(ctx, store, tc.query, tc.evalAt)
			if err != nil {
				t.Fatalf("Instant(%q): %v", tc.query, err)
			}
			if res.Kind != tc.wantKind {
				t.Fatalf("Instant(%q) kind: got %v, want %v", tc.query, res.Kind, tc.wantKind)
			}
			if tc.wantCount > 0 && len(res.Vector) != tc.wantCount {
				t.Fatalf("Instant(%q): got %d vector samples, want %d (vector=%v)",
					tc.query, len(res.Vector), tc.wantCount, res.Vector)
			}
			if tc.check != nil {
				tc.check(t, res)
			}
		})
	}
}

func TestEngine_Range(t *testing.T) {
	t.Parallel()
	baseTS := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	store := NewSampleStore()
	seedDataset(t, store, baseTS)
	eng := NewEngine(Options{})
	ctx := context.Background()

	res, err := eng.Range(ctx, store, `rate(http_requests_total{job="batch"}[1m])`,
		baseTS.Add(1*time.Minute), baseTS.Add(5*time.Minute), 30*time.Second)
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if res.Kind != ResultKindMatrix {
		t.Fatalf("Range kind: got %v, want Matrix", res.Kind)
	}
	if len(res.Matrix) != 1 {
		t.Fatalf("Range matrix: got %d series, want 1", len(res.Matrix))
	}
	// step = 30s over [1m, 5m] → 9 evaluation points (1:00, 1:30, …, 5:00).
	if got := len(res.Matrix[0].Points); got != 9 {
		t.Fatalf("Range matrix points: got %d, want 9", got)
	}
	// Every point should report rate ≈ 5/15 ≈ 0.3333 (one increment of 5 per
	// 15s sample). Tolerance is generous because rate() uses extrapolation at
	// window edges.
	for _, p := range res.Matrix[0].Points {
		if p.V <= 0 || math.IsNaN(p.V) {
			t.Fatalf("Range point: non-positive or NaN value at t=%d: %v", p.T, p.V)
		}
		assertApprox(t, p.V, 5.0/15.0, 0.05, "batch/get range rate")
	}
}

func TestEngine_Range_RejectsZeroStep(t *testing.T) {
	t.Parallel()
	store := NewSampleStore()
	eng := NewEngine(Options{})
	ts := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	if _, err := eng.Range(context.Background(), store, `up`, ts, ts.Add(time.Minute), 0); err == nil {
		t.Fatalf("Range with step=0 should error")
	}
}

func assertApprox(t *testing.T, got, want, tol float64, label string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %v, want %v (±%v)", label, got, want, tol)
	}
}
