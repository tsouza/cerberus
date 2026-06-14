package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_OvertimeTsMadFamily_LowersAndEmits pins that the experimental
// `mad_over_time` + `ts_of_{first,last,max,min}_over_time` family — which
// the reference engine implements but cerberus previously 422'd at the
// generic "function X is not yet supported" fall-through — now lowers to a
// RangeWindow and emits ClickHouse SQL end-to-end. Each function's emitted
// SQL must carry the CH idiom the reducer maps to:
//
//   - mad_over_time     → arraySort over window_vals + abs deviation (median of |x - median|)
//   - ts_of_first/last  → toUnixTimestamp64Milli of the boundary pair's timestamp
//   - ts_of_max/min     → argMax/argMin(timestamps, values) over window_pairs
//
// This is the always-green tripwire guarding the unmasked showcase panels:
// the moment any of these regresses back to a fall-through error, this
// test fails before the compose sweep does.
func TestLower_OvertimeTsMadFamily_LowersAndEmits(t *testing.T) {
	t.Parallel()

	cases := []struct {
		fn      string
		query   string
		wantSQL []string
	}{
		{
			fn:      "mad_over_time",
			query:   `mad_over_time(temperature[10m])`,
			wantSQL: []string{"arraySort", "abs", "window_vals"},
		},
		{
			fn:      "ts_of_first_over_time",
			query:   `ts_of_first_over_time(temperature[10m])`,
			wantSQL: []string{"toUnixTimestamp64Milli", "window_pairs", "/ 1000"},
		},
		{
			fn:      "ts_of_last_over_time",
			query:   `ts_of_last_over_time(temperature[10m])`,
			wantSQL: []string{"toUnixTimestamp64Milli", "length(`window_pairs`)"},
		},
		{
			fn:      "ts_of_max_over_time",
			query:   `ts_of_max_over_time(temperature[10m])`,
			wantSQL: []string{"argMax", "toUnixTimestamp64Nano", "toUnixTimestamp64Milli", "window_pairs"},
		},
		{
			// ts_of_min maximises a NEGATED value key (so the latest
			// equal-min wins, matching Prom's `cur <= minVal`), so the
			// emitted aggregate is also argMax over a `-value` key.
			fn:      "ts_of_min_over_time",
			query:   `ts_of_min_over_time(temperature[10m])`,
			wantSQL: []string{"argMax", "-tupleElement(p, 2)", "toUnixTimestamp64Milli", "window_pairs"},
		},
	}

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	for _, tc := range cases {
		tc := tc
		t.Run(tc.fn, func(t *testing.T) {
			t.Parallel()

			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%s): %v", tc.fn, err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("%s should lower cleanly, got: %v", tc.fn, err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("%s should emit SQL cleanly, got: %v", tc.fn, err)
			}
			for _, want := range tc.wantSQL {
				if !strings.Contains(sql, want) {
					t.Fatalf("%s emitted SQL missing %q\nSQL: %s", tc.fn, want, sql)
				}
			}
		})
	}
}
