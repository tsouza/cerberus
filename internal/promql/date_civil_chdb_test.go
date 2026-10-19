//go:build chdb

package promql_test

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/promql"
)

// civilEvalTS is the instant the value-argument queries evaluate at. The
// answer does not depend on it; it only has to sit inside the evaluation
// domain.
var civilEvalTS = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

// civilDateFunctions are the PromQL functions whose answer is a calendar
// component of a Unix-seconds argument.
var civilDateFunctions = []string{
	"year", "month", "day_of_month", "day_of_week", "day_of_year", "days_in_month", "hour", "minute",
}

// civilBoundaryYears are the years whose month, leap-day and year-end
// boundaries the value grid probes: negative and pre-1900 years, the
// 1970/2106 edges of ClickHouse DateTime, the 1900/2262 edges of
// DateTime64(9), centuries that are and are not leap years, and years far
// past any ClickHouse date type.
var civilBoundaryYears = []int{
	-1_000_000_000, -4_713, -401, -400, -100, -1, 0, 1, 1_600, 1_700, 1_899, 1_900, 1_969, 1_970,
	2_000, 2_024, 2_100, 2_105, 2_106, 2_262, 2_263, 2_400, 9_999, 10_000, 1_000_000_000, 200_000_000_000,
}

// goInt64Wrap is Go's `unixToAbsolute`: an int64 second count below its
// negation wraps to the top of Go's absolute calendar.
const goInt64Wrap = 9223372028741760000

// civilFloatSpacing is the distance between adjacent float64 values just
// below 2^63, so goInt64Wrap and its float neighbours are exact samples.
const civilFloatSpacing = 1024

// civilValueGrid returns the sample values every date function is checked
// at: calendar boundaries from [civilBoundaryYears] and one second either
// side, fractions that truncate toward zero, and the values Go's
// `int64(v)` does not convert by truncation.
func civilValueGrid() []float64 {
	values := []float64{
		0, 1, -1, 0.5, -0.5, 1.5, -1.5, 0.999, -0.999, 59, 60, 3599, 3600, -3600, 86399, 86400, -86400, -86401,
		5e9, -5e9, 1e12, -1e12, 1e15, -1e15, 1 << 53, -(1 << 53),
		math.NaN(), math.Inf(1), math.Inf(-1), 1e19, -1e19, 1e300, -1e300,
		math.MaxInt64, math.MinInt64, math.Nextafter(math.MaxInt64, 0), math.Nextafter(math.MinInt64, 0),
	}
	for _, steps := range []float64{-3000, -2700, -100, -1, 0, 1, 100} {
		values = append(values, -goInt64Wrap+steps*civilFloatSpacing)
	}
	for _, year := range civilBoundaryYears {
		// Each instant and the second after it: the last second of February
		// 28, of February, of April and of the year.
		for _, at := range []time.Time{
			time.Date(year, time.February, 28, 23, 59, 59, 0, time.UTC),
			time.Date(year, time.March, 1, 0, 0, 0, 0, time.UTC).Add(-time.Second),
			time.Date(year, time.April, 30, 23, 59, 59, 0, time.UTC),
			time.Date(year, time.December, 31, 23, 59, 59, 0, time.UTC),
		} {
			sec := float64(at.Unix())
			values = append(values, sec, sec+1)
		}
	}
	return values
}

// civilPromQLNumber renders v as a PromQL number literal.
func civilPromQLNumber(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// TestDateFunctionsOfValueMatchReference_ChDB checks every date function of
// a `vector(v)` argument against the reference engine across the full
// float64 range: no value may come back clamped to a ClickHouse date
// type's range.
func TestDateFunctionsOfValueMatchReference_ChDB(t *testing.T) {
	db := spec.OpenChDB(t)
	values := civilValueGrid()
	for _, fn := range civilDateFunctions {
		t.Run(fn, func(t *testing.T) {
			for _, v := range values {
				query := fn + "(vector(" + civilPromQLNumber(v) + "))"
				want := civilReference(t, query, civilEvalTS, civilEvalTS, 0)
				got := civilCerberus(t, db, query, civilEvalTS, civilEvalTS, 0)
				civilAssertEqual(t, query, got, want)
			}
		})
	}
}

// TestDateFunctionsOfEvaluationTimeMatchReference_ChDB checks the
// no-argument form, which reads the evaluation instant, across the
// evaluation domain — before the Unix epoch included — in instant and
// range mode.
func TestDateFunctionsOfEvaluationTimeMatchReference_ChDB(t *testing.T) {
	const rangeStep = 37 * time.Hour
	const rangeSteps = 3
	db := spec.OpenChDB(t)
	instants := []time.Time{
		time.Date(1900, time.March, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1950, time.June, 15, 12, 34, 56, 0, time.UTC),
		time.Date(1969, time.December, 31, 23, 59, 59, 500_000_000, time.UTC),
		time.Date(2000, time.February, 29, 23, 59, 59, 0, time.UTC),
		time.Date(2106, time.February, 7, 6, 28, 16, 0, time.UTC),
		time.Date(2262, time.April, 9, 23, 59, 59, 0, time.UTC),
	}
	for _, fn := range civilDateFunctions {
		t.Run(fn, func(t *testing.T) {
			query := fn + "()"
			for _, at := range instants {
				civilAssertEqual(t, query+" @instant "+at.String(),
					civilCerberus(t, db, query, at, at, 0), civilReference(t, query, at, at, 0))
				end := at.Add(-rangeStep * rangeSteps)
				civilAssertEqual(t, query+" @range ending "+at.String(),
					civilCerberus(t, db, query, end, at, rangeStep), civilReference(t, query, end, at, rangeStep))
			}
		})
	}
}

// civilSample is one output sample: its timestamp in Unix milliseconds
// and its value.
type civilSample struct {
	millis int64
	value  float64
}

func civilReference(t *testing.T, query string, start, end time.Time, step time.Duration) []civilSample {
	t.Helper()
	results, err := oracle.Evaluate(t, nil, oracle.Query{Expr: query, Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	samples := make([]civilSample, 0, len(results))
	for _, r := range results {
		samples = append(samples, civilSample{millis: r.TMillis, value: r.Value})
	}
	return samples
}

func civilCerberus(t *testing.T, db *sql.DB, query string, start, end time.Time, step time.Duration) []civilSample {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(query)
	if err != nil {
		t.Fatal(err)
	}
	s := schema.DefaultOTelMetrics()
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	projection := &chplan.Project{Input: plan, Projections: []chplan.Projection{
		{Expr: &chplan.FuncCall{Fn: chplan.FnToUnixMillis, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}}}, Alias: "millis"},
		{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: "value"},
	}}
	sqlText, args, err := chsql.Emit(context.Background(), projection)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(sqlText, args...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", query, err, sqlText)
	}
	defer func() { _ = rows.Close() }()
	var samples []civilSample
	for rows.Next() {
		var sample civilSample
		if err := rows.Scan(&sample.millis, &sample.value); err != nil {
			t.Fatal(err)
		}
		if step == 0 {
			// The instant row's timestamp is the server clock; the HTTP
			// layer stamps the evaluation instant over it.
			sample.millis = end.UnixMilli()
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return samples
}

func civilAssertEqual(t *testing.T, query string, got, want []civilSample) {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("%s: the reference answered no sample, so the comparison would be vacuous", query)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: cerberus %v, reference %v", query, got, want)
	}
}
