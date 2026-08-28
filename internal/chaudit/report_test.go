package chaudit

import "testing"

// TestOptions_Validate_RejectsUnusableInput pins that an audit refuses to run
// rather than emit a confident report from meaningless inputs. Every field
// here changes a number the report presents as fact, so a zero in any of them
// would produce output that reads as measured and is not.
func TestOptions_Validate_RejectsUnusableInput(t *testing.T) {
	t.Parallel()

	valid := Options{Table: "t", WindowSeconds: 60, Anchors: 10, DensityUnitBudget: 1000, Top: 5}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a fully-specified Options must validate: %v", err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*Options)
	}{
		{"no table", func(o *Options) { o.Table = "" }},
		{"zero window", func(o *Options) { o.WindowSeconds = 0 }},
		{"negative window", func(o *Options) { o.WindowSeconds = -1 }},
		{"zero anchors", func(o *Options) { o.Anchors = 0 }},
		{"zero budget", func(o *Options) { o.DensityUnitBudget = 0 }},
		{"zero top", func(o *Options) { o.Top = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := valid
			tc.mut(&o)
			if err := o.Validate(); err == nil {
				t.Errorf("Validate accepted %s; the report would present a derived number as a\n"+
					"measurement without the input that derives it", tc.name)
			}
		})
	}
}

// TestReport_OverBudget_SelectsExactlyTheRejected pins the accessor an alerting
// caller would key on against the per-metric verdict, so the summary and the
// detail cannot disagree about who is failing.
func TestReport_OverBudget_SelectsExactlyTheRejected(t *testing.T) {
	t.Parallel()

	r := Report{Metrics: []MetricAudit{
		{Metric: "under", CostUnits: 10, Budget: 100},
		{Metric: "exactly_at", CostUnits: 100, Budget: 100},
		{Metric: "over", CostUnits: 101, Budget: 100},
	}}
	got := r.OverBudget()
	if len(got) != 1 || got[0].Metric != "over" {
		t.Fatalf("OverBudget() = %+v, want only \"over\" — the boundary is strictly greater-than,\n"+
			"matching the guard's own `cost > bound` predicate", got)
	}
}

// TestRankByHeadroom_WorstFirst pins the ordering the report is read in. An
// operator scanning a long audit acts on the first rows; burying the metric
// that is already failing below healthy ones defeats the report.
func TestRankByHeadroom_WorstFirst(t *testing.T) {
	t.Parallel()

	m := []MetricAudit{
		{Metric: "healthy", HeadroomPct: 90},
		{Metric: "failing", HeadroomPct: -50},
		{Metric: "tight", HeadroomPct: 5},
	}
	rankByHeadroom(m)
	for i, want := range []string{"failing", "tight", "healthy"} {
		if m[i].Metric != want {
			t.Errorf("position %d is %q, want %q", i, m[i].Metric, want)
		}
	}
}
