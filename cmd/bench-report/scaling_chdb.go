//go:build chdb

package main

import (
	"fmt"
	"strings"
	"time"
)

// scalingPoint is one swept parameter value's measurement.
type scalingPoint struct {
	Param    int64
	Wall     time.Duration
	PeakRows int64
	FanRatio float64 // PeakRows / scanRows
}

// scalingCurve is a registered construct's sweep: wall + fan_factor vs the
// real fan-out multiplier. Re-measures the same query shapes the
// test/perf/scaling constructs register, so the curve matches the gated
// harness.
type scalingCurve struct {
	Name     string
	Param    string // human label for the swept parameter
	ScanRows int64
	Points   []scalingPoint

	ParamGrowth float64 // last/first param
	WallGrowth  float64 // last/first wall — the sub-linearity headline
}

func measureScalingCurves(s *session, iters int) ([]scalingCurve, error) {
	var curves []scalingCurve

	c, err := curveRangeLWR(s, iters)
	if err != nil {
		return nil, fmt.Errorf("range_lwr curve: %w", err)
	}
	curves = append(curves, c)

	c, err = curveSetOpChain(s, iters)
	if err != nil {
		return nil, fmt.Errorf("setop_chain curve: %w", err)
	}
	curves = append(curves, c)

	return curves, nil
}

func finishCurve(c *scalingCurve) {
	if len(c.Points) == 0 {
		return
	}
	first, last := c.Points[0], c.Points[len(c.Points)-1]
	if first.Param > 0 {
		c.ParamGrowth = float64(last.Param) / float64(first.Param)
	}
	if first.Wall > 0 {
		c.WallGrowth = float64(last.Wall) / float64(first.Wall)
	}
	for i := range c.Points {
		if c.ScanRows > 0 {
			c.Points[i].FanRatio = float64(c.Points[i].PeakRows) / float64(c.ScanRows)
		}
	}
}

// curveRangeLWR sweeps the range/step anchor count N over a fixed row set
// — the real fan-out multiplier for the bare query_range LWR path. The
// production single-pass shape's wall stays ~flat and its peak
// intermediate stays a small multiple of scan_rows, independent of N.
func curveRangeLWR(s *session, iters int) (scalingCurve, error) {
	const lookback = 5 * time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	// Reuse the table seeded by the headline measurement if present;
	// otherwise seed it. CREATE OR REPLACE keeps this idempotent.
	seed := `CREATE TABLE IF NOT EXISTS bench_lwr_gauge (
  MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);`
	if err := s.execAll(seed); err != nil {
		return scalingCurve{}, err
	}
	scan, err := s.scalarCount("SELECT * FROM bench_lwr_gauge")
	if err != nil {
		return scalingCurve{}, err
	}

	c := scalingCurve{Name: "range_lwr", Param: "range/step anchors N", ScanRows: scan}
	for _, n := range []int64{61, 121, 241} {
		step := end.Sub(start) / time.Duration(n-1)
		sqlText, err := emitRangeOverTable(start, end, step, "bench_lwr_gauge")
		if err != nil {
			return scalingCurve{}, err
		}
		wall, err := s.bestWall(sqlText, iters)
		if err != nil {
			return scalingCurve{}, err
		}
		peak, err := s.scalarCount(rangeLWRFanoutInner(end, step, lookback, n))
		if err != nil {
			return scalingCurve{}, err
		}
		c.Points = append(c.Points, scalingPoint{Param: n, Wall: wall, PeakRows: peak})
	}
	finishCurve(&c)
	return c, nil
}

// curveSetOpChain sweeps the chain depth K. Post-#814 the intermediate
// stays a tiny bounded constant (the disjoint-arm row count); the wall is
// the residual super-linear finding (~2.6×/level) tracked for the N-ary
// flatten (#90). The curve shows both: fan_factor flat, wall tracking K.
func curveSetOpChain(s *session, iters int) (scalingCurve, error) {
	evalTime := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	const maxK = 8

	var b strings.Builder
	b.WriteString("DROP TABLE IF EXISTS bench_setop_curve;")
	b.WriteString(`CREATE TABLE bench_setop_curve (
  MetricName String, Attributes Map(String,String),
  TimeUnix DateTime64(9), Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, Attributes, TimeUnix);`)
	ts := evalTime.Add(-time.Second).UTC().Format("2006-01-02 15:04:05.000000000")
	b.WriteString("\nINSERT INTO bench_setop_curve VALUES\n")
	for i := 0; i <= maxK; i++ {
		if i > 0 {
			b.WriteString(",\n")
		}
		fmt.Fprintf(&b, "  ('setop.chain.metric.%d', map('arm','%d'), toDateTime64('%s',9), %d.0)", i, i, ts, i+1)
	}
	b.WriteString(";")
	if err := s.execAll(b.String()); err != nil {
		return scalingCurve{}, err
	}
	scan, err := s.scalarCount("SELECT * FROM bench_setop_curve")
	if err != nil {
		return scalingCurve{}, err
	}

	c := scalingCurve{Name: "setop_chain", Param: "chain depth K", ScanRows: scan}
	for _, k := range []int64{2, 4, 8} {
		sqlText, err := emitSetOpChain("or", int(k), evalTime)
		if err != nil {
			return scalingCurve{}, err
		}
		sqlText = strings.ReplaceAll(sqlText, "otel_metrics_sum", "bench_setop_curve")
		wall, err := s.bestWall(sqlText, iters)
		if err != nil {
			return scalingCurve{}, err
		}
		peak, err := s.scalarCount(sqlText)
		if err != nil {
			return scalingCurve{}, err
		}
		c.Points = append(c.Points, scalingPoint{Param: k, Wall: wall, PeakRows: peak})
	}
	finishCurve(&c)
	return c, nil
}
