//go:build chdb

// chDB proof, against the Prometheus reference engine, that a grid or a
// window spanning more than a time.Duration holds, about 292 years, is
// answered in SQL with Prometheus's own answer while every instant it names
// lies inside ClickHouse's DateTime64(9) range.
package promql_test

import (
	"testing"
	"time"
)

// gridSpanCase is one query asked over a grid spanning more than 292 years,
// as a range query over gridSpanStart..gridSpanEnd by gridSpanStep and,
// unless rangeOnly, as an instant query at gridSpanEnd.
type gridSpanCase struct {
	name      string
	query     string
	rows      []instantDomainRow
	rangeOnly bool
}

// gridSpanSeed adds to instantDomainSeed a sample on 1700-01-01 (job
// "colonial"), which the windows of the first steps of a range query from
// gridSpanStart hold, more than 292 years before gridSpanEnd.
const gridSpanSeed = instantDomainSeed +
	"INSERT INTO otel_metrics_gauge VALUES\n" +
	"    ('" + instantDomainMetric + "', map('job', 'colonial'), fromUnixTimestamp64Nano(toInt64(-8520336000000000000)), 1.0);\n"

var (
	// gridSpanColonialSample is the job "colonial" sample's timestamp.
	gridSpanColonialSample = time.Unix(-8520336000, 0).UTC()
	gridSpanStart          = time.Date(1990, 2, 6, 0, 0, 0, 0, time.UTC)
	gridSpanEnd            = time.Date(2025, 11, 8, 0, 0, 0, 0, time.UTC)
)

// gridSpanStep is 100 days: 130 steps from gridSpanStart land on
// gridSpanEnd.
const gridSpanStep = 100 * 24 * time.Hour

// TestGridSpanParity_ChDB pins queries whose subquery grid or whose
// windows, laid across the steps of a range query, span about 328 years.
func TestGridSpanParity_ChDB(t *testing.T) {
	noLabels := map[string]string{}
	old := map[string]string{"job": "old"}
	none := map[string]string{"job": "none"}
	// A 292y window holds the 1700 sample while it ends before cutoff.
	cutoff := gridSpanColonialSample.Add(292 * 365 * 24 * time.Hour)
	holdsColonial := func(step time.Time) float64 {
		if step.Before(cutoff) {
			return 1
		}
		return 0
	}
	cases := []gridSpanCase{
		// A 292y subquery grid by 100d holds 1066 instants at every step.
		{"subquery_grid", `count_over_time(vector(1)[292y:100d])`, []instantDomainRow{idRow(noLabels, idConst(1066))}, false},
		// A 100y grid of 200y grids by 100d, each exactly 365 and 730
		// steps, reaching 300 years back from each step.
		{"nested_subquery_grid", `sum_over_time(count_over_time(vector(1)[200y:100d])[100y:100d])`, []instantDomainRow{idRow(noLabels, idConst(365*730))}, false},
		// Every 292y window from 1990 to 2025 holds the 1811 sample.
		{"window_reads_old_sample", `count_over_time(` + instantDomainMetric + `{job="old"}[292y])`, []instantDomainRow{idRow(old, idConst(1))}, false},
		// The windows of the first steps hold the 1700 sample, which lies
		// more than 292 years before the last step. Range only: one
		// instant's window spans no more than 292 years.
		{"window_reads_colonial_sample", `sum(count_over_time(` + instantDomainMetric + `{job="colonial"}[292y])) or vector(0)`, []instantDomainRow{idRow(noLabels, holdsColonial)}, true},
		{"absent_window", `absent_over_time(` + instantDomainMetric + `{job="none"}[292y])`, []instantDomainRow{idRow(none, idConst(1))}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/range", func(t *testing.T) {
			runGridParity(t, gridSpanSeed, tc.query, gridSpanStart, gridSpanEnd, gridSpanStep, tc.rows)
		})
		if tc.rangeOnly {
			continue
		}
		t.Run(tc.name+"/instant", func(t *testing.T) {
			runGridParity(t, gridSpanSeed, tc.query, gridSpanEnd, gridSpanEnd, 0, tc.rows)
		})
	}
}
