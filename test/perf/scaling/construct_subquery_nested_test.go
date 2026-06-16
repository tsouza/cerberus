//go:build chdb

// Construct: subquery_nested — outer reducer over an inner subquery.
//
// A NEW registered shape the Phase-1 audit flagged but which had no
// standalone guard: `max_over_time(sum_over_time(m[1m])[So_range:Si_step])`.
// The inner subquery resolves the inner range-fn at So = outer_range /
// inner_step anchors per series; the outer reducer collapses them. THE REAL
// MULTIPLIER is the inner resolution So — shrink the inner step and the
// per-series anchor fan-out grows. The audit's concern: if the inner
// fan-out materialised rows x So WITHOUT a bounded GROUP-BY collapse, the
// intermediate would explode in So even though the scanned rows are
// constant.
//
// On current main the subquery lowering emits a SINGLE bounded arrayJoin
// fan-out (each sample fans to only the inner anchors within the outer
// window, then GROUP BY (series, anchor) collapses), so the intermediate is
// rows x (outer_range/inner_step) — bounded — and the wall stays sub-linear
// in So. Param = So = outer_range/inner_step, swept by shrinking the inner
// step at a FIXED outer window + row set.
package scaling

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const subqueryMetric = "demo_memory_usage_bytes"

func init() {
	// Fixed outer window; the inner step is the swept knob (So = outer/inner).
	outerRange := 60 * time.Minute
	end := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)

	register(Construct{
		Name:        "subquery_nested",
		Param:       "inner resolution So",
		Why:         "nested subquery inner-anchor fan-out (rows x So) without bounded collapse",
		ScanRowsSQL: "SELECT count() FROM otel_metrics_gauge",
		// rows x (outer_range/inner_step): at the densest swept inner step
		// (1m over a 60m window) this is rows x ~60 worth of MEMBERSHIP, but
		// the bounded arrayJoin only fans each sample to the anchors whose
		// window covers it (inner range 1m, inner step >=1m -> ~1-2 anchors
		// per sample). A bound of 4x scan_rows is generous headroom while a
		// rows x So blow-up (60x) trips it.
		CardinalityBound: 4.0,
		SubLinearSlack:   0.9,
		Seed: func() string {
			// ResourceAttributes mirrors the OTel-CH default schema: the rc.5
			// read path projects mapUpdate(sanitize(ResourceAttributes), …),
			// so the seed table must carry the column (left empty via DEFAULT)
			// or the chDB round-trip 502s with UNKNOWN_IDENTIFIER.
			ddl := `DROP TABLE IF EXISTS otel_metrics_gauge;
			CREATE TABLE otel_metrics_gauge (
			  ServiceName String, MetricName String, Attributes Map(String,String),
			  ResourceAttributes Map(String,String) DEFAULT map(),
			  TimeUnix DateTime64(9), Value Float64
			) ENGINE = MergeTree()
			ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
			// 100 series x ~720 samples (one every 10s over 2h) = 72k rows;
			// FIXED across every inner-step variant so So is the only var.
			ins := `INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value) SELECT
			  'svc', '` + subqueryMetric + `',
			  map('instance', concat('i', toString(number % 100))),
			  toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond((intDiv(number, 100)) * 10),
			  toFloat64(number)
			FROM numbers(72000);`
			return ddl + ins
		},
		Points: func(t *testing.T) []Point {
			// inner steps 4m / 2m / 1m -> So = 60/4=15, 60/2=30, 60/1=60.
			innerSteps := []time.Duration{4 * time.Minute, 2 * time.Minute, time.Minute}
			pts := make([]Point, 0, len(innerSteps))
			for _, is := range innerSteps {
				so := int64(outerRange / is)
				sqlText, args := emitSubqueryNestedSQL(t, end, outerRange, is)
				pts = append(pts, Point{
					Param:     so,
					SQL:       sqlText,
					Args:      args,
					LevelSQLs: []string{subqueryInnerFanout(end, outerRange, time.Minute, is)},
				})
			}
			return pts
		},
	})
}

// emitSubqueryNestedSQL lowers the outer-reducer-over-inner-subquery at the
// fixed instant anchor through the real chain. innerRange is held at 1m;
// innerStep is the swept knob (So = outerRange/innerStep).
func emitSubqueryNestedSQL(t *testing.T, end time.Time, outerRange, innerStep time.Duration) (string, []any) {
	t.Helper()
	q := fmt.Sprintf("max_over_time(sum_over_time(%s[1m])[%s:%s])",
		subqueryMetric, durLit(outerRange), durLit(innerStep))
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, schema.DefaultOTelMetrics(), end, end)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", q, err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", q, err)
	}
	return sqlText, args
}

// subqueryInnerFanout is the bounded inner-anchor fan-out the subquery
// lowering produces BEFORE the per-(series, anchor) GROUP BY collapse — one
// row per (sample, covered inner anchor). It mirrors the emitter's bounded
// `range(lo, hi)` arrayJoin: each sample fans to only the inner anchors
// within `innerRange` of it. Counting its rows measures the intermediate
// cardinality the bounded collapse keeps at rows x (overlap factor), NOT
// rows x So.
func subqueryInnerFanout(end time.Time, outerRange, innerRange, innerStep time.Duration) string {
	stepNS := innerStep.Nanoseconds()
	numAnchors := int64(outerRange/innerStep) + 1
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	dist := fmt.Sprintf("dateDiff('nanosecond', TimeUnix, toDateTime64('%s', 9))", endLit)
	floorIdx := func(addNS int64) string {
		num := dist
		if addNS < 0 {
			num = fmt.Sprintf("%s - %d", dist, -addNS)
		}
		return fmt.Sprintf("intDiv(%s, toInt64(%d)) - (modulo(%s, toInt64(%d)) < 0) + 1",
			num, stepNS, num, stepNS)
	}
	return fmt.Sprintf(`SELECT TimeUnix, Value,
	  arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
	    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
	FROM otel_metrics_gauge WHERE MetricName = '`+subqueryMetric+`'`,
		endLit, stepNS, floorIdx(-innerRange.Nanoseconds()), numAnchors, floorIdx(0))
}

// durLit renders a duration as a PromQL range literal (e.g. 60m, 30s).
func durLit(d time.Duration) string {
	if d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int64(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int64(d/time.Second))
}
