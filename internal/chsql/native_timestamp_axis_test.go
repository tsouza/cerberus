package chsql

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestNativeTimeSeriesAggregates_ReadTimestampPerPrecision pins every site
// that feeds a metrics timestamp column to a native timeSeries* aggregate.
// The aggregates compute in their timestamp argument's scale, so over a
// column coarser than nanoseconds each must read it widened through
// toDateTime64(…, 9) to answer what it answers over DateTime64(9) — and read
// the column directly under the zero value.
func TestNativeTimeSeriesAggregates_ReadTimestampPerPrecision(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	staleResample := &chplan.RangeWindowStaleResample{
		Input: &chplan.Scan{Table: "samples", Columns: []string{"MetricName", "Attributes", "TimeUnix", "Value"}, Roles: []chplan.Column{
			{Name: "MetricName", Role: chplan.RoleMetricName},
			{Name: "Attributes", Role: chplan.RoleAttributes},
			{Name: "TimeUnix", Role: chplan.RoleTimestamp},
			{Name: "Value", Role: chplan.RoleValue},
		}},
		Start: at, End: at.Add(time.Minute), Step: time.Minute, Lookback: 5 * time.Minute,
	}
	bucketGrid := &chplan.RangeBucketGridNative{
		Input: &chplan.Scan{Table: "samples", Columns: []string{"series", "TimeUnix", "BucketCounts", "ExplicitBounds"}, Roles: []chplan.Column{
			{Name: "series", Role: chplan.RoleAttributes},
			{Name: "TimeUnix", Role: chplan.RoleTimestamp},
			{Name: "BucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldBucketCounts},
			{Name: "ExplicitBounds", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldExplicitBounds},
		}},
		Start: at, End: at.Add(time.Minute), Step: time.Minute, Range: 5 * time.Minute,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "series"}}, GroupByAliases: []string{"series"},
		AnchorAlias: "anchor_ts", TimestampCol: "TimeUnix",
		BucketCountsCol: "BucketCounts", ExplicitBoundsCol: "ExplicitBounds",
	}
	emit := func(plan chplan.Node) func(SchemaStrategies) (string, error) {
		return func(s SchemaStrategies) (string, error) {
			sql, _, err := Emit(WithSchemaStrategies(context.Background(), s), plan)
			return sql, err
		}
	}
	render := func(f Frag) func(SchemaStrategies) (string, error) {
		return func(s SchemaStrategies) (string, error) {
			sql, _ := Render(UnderSchemaStrategies(s, f))
			return sql, nil
		}
	}

	cases := []struct {
		name string
		sql  func(SchemaStrategies) (string, error)
		// read is the aggregate call up to and including its timestamp
		// argument, with %s standing for that argument.
		read string
		col  string
	}{
		{"timeSeriesGroupArray", render(nativeGroupArrayPairFrag("ts", "v")), "timeSeriesGroupArray(%s, `v`)", "ts"},
		{"timeSeriesGroupArrayIf", render(nativeGroupArrayPairIfFrag("ts", "v", Col("c"))), "timeSeriesGroupArrayIf(%s, `v`, `c`)", "ts"},
		{"grid axis", render(nativeGridTsAxisFrag("rate", "ts")), "%s", "ts"},
		{"stale resample", emit(staleResample), "(%s, `Value`)", "TimeUnix"},
		{"bucket grid rate", emit(bucketGrid), bucketGridRateFn + "(?)(%s, `" + bucketGridCumAlias + "`)", "TimeUnix"},
		{"bucket grid resets", emit(bucketGrid), bucketGridSeenFn + "(?)(%s, `" + bucketGridCumAlias + "`)", "TimeUnix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			bare, widened := "`"+c.col+"`", "toDateTime64(`"+c.col+"`, 9)"
			for _, w := range []struct {
				s    SchemaStrategies
				want string
				not  string
			}{
				{nanosStrategies, bare, widened},
				{secondsStrategies, widened, ""},
			} {
				sql, err := c.sql(w.s)
				if err != nil {
					t.Fatalf("render: %v", err)
				}
				if !containsRead(sql, c.read, w.want) || w.not != "" && containsRead(sql, c.read, w.not) {
					t.Errorf("%v: SQL does not read %s through %s:\n%s", w.s.MetricsTimestamp, c.col, w.want, sql)
				}
			}
		})
	}
}

// parametricParams matches one parenthesised parameter list whose
// parameters nest at most one level of parentheses.
const parametricParams = `\((?:[^()]|\([^()]*\))*\)`

// containsRead reports whether sql holds read with its %s argument arg;
// a `(?)` in read stands for one parenthesised parameter list.
func containsRead(sql, read, arg string) bool {
	head, tail, _ := strings.Cut(strings.Replace(read, "%s", arg, 1), "(?)")
	pattern := regexp.QuoteMeta(head)
	if strings.Contains(read, "(?)") {
		pattern += parametricParams + regexp.QuoteMeta(tail)
	}
	return regexp.MustCompile(pattern).MatchString(sql)
}
