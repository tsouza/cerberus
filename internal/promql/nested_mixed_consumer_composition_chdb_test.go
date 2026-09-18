//go:build chdb

// chDB-backed proof that a mixed float/histogram plan produced ONE LEVEL DOWN
// — by a payload-preserving wrapper over a mixed `or` — answers every scale,
// unary, vector-arithmetic and vector-comparison consumer IDENTICALLY to the
// direct root form of the same consumer, whose answers the oracle-enrolled
// test/spec/promql fixtures already pin. The wrappers are chosen to be
// identities over this seed (a label sort, a limitk above the row count, a
// one-sample subquery select, an `and` against a partner that matches every
// series), so any difference between the nested
// and the direct answer is the consumer reading the nested plan wrongly:
// dropping its histogram rows or fabricating a float from a placeholder.
package promql_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
	"github.com/tsouza/cerberus/test/spec"
)

const (
	ncHistMetric        = "nc_hist_side_exp_hist"
	ncFloatMetric       = "nc_float_side_gauge"
	ncPartnerMetric     = "nc_partner_gauge"
	ncPartnerHistMetric = "nc_partner_exp_hist"
	// ncRowCount is the number of series the mixed `or` answers; limitk at
	// or above it is the identity.
	ncRowCount = 4
)

// ncSeed keys two histogram series ("h1", "h2") and two float series ("f1"=3,
// "f2"=9) on disjoint label sets, plus a partner gauge carrying one row for
// EVERY one of those four label sets so a vector-vector operator matches
// each mixed row exactly once and `and` forwards all of them, and a partner
// histogram on "h1" and "f1" so a histogram-valued partner meets one
// histogram row and one float row.
var ncSeed = "" +
	"CREATE OR REPLACE TABLE otel_metrics_exponential_histogram (" +
	"`MetricName` String, `Attributes` Map(String, String), " +
	"`ResourceAttributes` Map(String, String) DEFAULT map(), `ServiceName` LowCardinality(String) DEFAULT '', " +
	"`TimeUnix` DateTime64(9), " +
	"`Count` UInt64, `Sum` Float64, `Scale` Int32, `ZeroCount` UInt64, " +
	"`PositiveOffset` Int32, `PositiveBucketCounts` Array(UInt64), " +
	"`NegativeOffset` Int32, `NegativeBucketCounts` Array(UInt64)" +
	") ENGINE = MergeTree ORDER BY (`MetricName`, `Attributes`, `TimeUnix`);\n" +
	"INSERT INTO otel_metrics_exponential_histogram " +
	"(MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n" +
	"    ('" + ncHistMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []),\n" +
	"    ('" + ncHistMetric + "', map('series', 'h2'), toDateTime64('2026-01-01 00:00:00', 9), 3, 9.0, 0, 0, 0, [7], 0, []),\n" +
	"    ('" + ncPartnerHistMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), 5, 10.0, 0, 0, 0, [8], 0, []),\n" +
	"    ('" + ncPartnerHistMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:00:00', 9), 7, 14.0, 0, 0, 0, [9], 0, []);\n" +
	swapGaugeSeedDDL +
	"INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES\n" +
	"    ('" + ncFloatMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:00:00', 9), 3.0),\n" +
	"    ('" + ncFloatMetric + "', map('series', 'f2'), toDateTime64('2026-01-01 00:00:00', 9), 9.0),\n" +
	"    ('" + ncPartnerMetric + "', map('series', 'h1'), toDateTime64('2026-01-01 00:00:00', 9), 10.0),\n" +
	"    ('" + ncPartnerMetric + "', map('series', 'h2'), toDateTime64('2026-01-01 00:00:00', 9), 20.0),\n" +
	"    ('" + ncPartnerMetric + "', map('series', 'f1'), toDateTime64('2026-01-01 00:00:00', 9), 30.0),\n" +
	"    ('" + ncPartnerMetric + "', map('series', 'f2'), toDateTime64('2026-01-01 00:00:00', 9), 40.0);\n"

// ncRow is one answer row with everything the two data models compare on:
// the series identity, the payload kind, and the payload itself (float
// Value on a float row; Count/Sum/first positive bucket on a histogram
// row). Timestamps are left out: an instant answer's sample time is not
// part of the PromQL result, and the wrappers legitimately re-stamp it.
type ncRow struct {
	Name    string
	Series  string
	Disc    int32
	Value   float64
	Count   float64
	Sum     float64
	Bucket1 float64
}

func ncQueryRows(t *testing.T, fixture *chdbFixture, s schema.Metrics, p parser.Parser, query string) []ncRow {
	t.Helper()
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, foEvalTS, foEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	plan = spec.AssertScanTimeBoundAccepts(t, plan)
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	// A float-only answer (the `bool` comparison) publishes no payload
	// columns; read the payload only where the plan carries it, and
	// zero it otherwise so a float answer compares as such.
	projection := "`MetricName`, `Attributes`['series'], `Value`, toInt32(0), toFloat64(0), toFloat64(0), toFloat64(0)"
	if chplan.LiveSampleKind(plan) == chplan.SampleKindMixed {
		projection = "`MetricName`, `Attributes`['series'], `Value`, `_setop_is_histogram`, " +
			"`HistogramCount`, `HistogramSum`, `HistogramPositiveBucketCounts`[1]"
	}
	rows := fixture.queryOverEmitted(t, projection, sqlText, args)
	defer func() { _ = rows.Close() }()
	var out []ncRow
	for rows.Next() {
		var r ncRow
		if err := rows.Scan(&r.Name, &r.Series, &r.Value, &r.Disc, &r.Count, &r.Sum, &r.Bucket1); err != nil {
			t.Fatalf("scan(%q): %v", query, err)
		}
		if r.Disc != 0 {
			// A histogram row's Value is a placeholder no consumer reads.
			r.Value = 0
		}
		out = append(out, r)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows(%q): %v", query, err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Series != out[j].Series {
			return out[i].Series < out[j].Series
		}
		return out[i].Disc < out[j].Disc
	})
	return out
}

func TestNestedMixedConsumersAnswerLikeTheirRoots_ChDB(t *testing.T) {
	fixture := newChDBFixture(t, ncSeed)
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	direct := "(" + ncHistMetric + " or " + ncFloatMetric + ")"
	wrappers := []struct{ name, query string }{
		{"sort_by_label", `sort_by_label(` + direct + `, "series")`},
		{"limitk", fmt.Sprintf(`limitk(%d, %s)`, ncRowCount, direct)},
		{"last_over_time_subquery", `last_over_time(` + direct + `[5m:1m])`},
		{"and_partner", `(` + direct + ` and ` + ncPartnerMetric + `)`},
	}
	consumers := []struct {
		name string
		wrap func(operand string) string
		// root is the direct-root reference the nested answer must equal;
		// nil means the same consumer over the direct `or`.
		root func(direct string) string
		// wantRows guards against a vacuous "both empty" agreement.
		wantRows int
	}{
		{"scale_right", func(o string) string { return o + ` * 2` }, nil, 4},
		{"scale_left", func(o string) string { return `2 * ` + o }, nil, 4},
		{"scale_div", func(o string) string { return o + ` / 2` }, nil, 4},
		// A computed scalar is the same scaling as the literal it folds to.
		{"scale_computed", func(o string) string { return o + ` * scalar(vector(2))` }, func(d string) string { return d + ` * 2` }, 4},
		{"unary_minus", func(o string) string { return `-` + o }, nil, 4},
		{"unary_plus", func(o string) string { return `+` + o }, nil, 4},
		{"vector_add", func(o string) string { return o + ` + ` + ncPartnerMetric }, nil, 2},
		{"vector_sub_left", func(o string) string { return ncPartnerMetric + ` - ` + o }, nil, 2},
		{"vector_mul", func(o string) string { return o + ` * ` + ncPartnerMetric }, nil, 4},
		{"vector_div_left", func(o string) string { return ncPartnerMetric + ` / ` + o }, nil, 2},
		{"vector_pow", func(o string) string { return o + ` ^ ` + ncPartnerMetric }, nil, 2},
		{"vector_add_self", func(o string) string { return o + ` + ` + o }, nil, 4},
		{"compare_bool", func(o string) string { return o + ` > bool ` + ncPartnerMetric }, nil, 2},
		{"compare_filter", func(o string) string { return o + ` < ` + ncPartnerMetric }, nil, 2},
		{"compare_left", func(o string) string { return ncPartnerMetric + ` > ` + o }, nil, 2},
		{"compare_self_eq", func(o string) string { return o + ` == ` + o }, nil, 4},
	}
	// Each root answer is executed once and shared by every wrapper that
	// must reproduce it.
	rootRows := map[string][]ncRow{}
	for _, c := range consumers {
		rootQuery := c.wrap(direct)
		if c.root != nil {
			rootQuery = c.root(direct)
		}
		want := ncQueryRows(t, fixture, s, p, rootQuery)
		if len(want) != c.wantRows {
			t.Fatalf("root %q answered %d rows, want %d: %+v", rootQuery, len(want), c.wantRows, want)
		}
		rootRows[c.name] = want
	}
	// A histogram-valued partner has no direct-root twin to compare
	// against (the root `(h or f) OP <histogram>` is refused), so the
	// answer is pinned outright from reference's per-row rule: a
	// histogram row merges with (`+`/`-`) or drops against (`*`, `==`) a
	// matching histogram partner row, a float row scales a histogram
	// partner under `*` and drops under everything else. The forwarded
	// partner (`and`) is the shape lowerRoot's histogram recognisers
	// decline; the bare partner is the shape they would otherwise claim,
	// reading the nested plan as a float vector.
	forwardedPartner := "(" + ncHistMetric + " and " + ncPartnerMetric + ")"
	histogramPartnerConsumers := []struct {
		name string
		wrap func(operand string) string
		want []ncRow
	}{
		{"vector_add_forwarded_histogram", func(o string) string { return o + ` + ` + forwardedPartner }, []ncRow{
			{Series: "h1", Disc: 1, Count: 4, Sum: 8, Bucket1: 12},
			{Series: "h2", Disc: 1, Count: 6, Sum: 18, Bucket1: 14},
		}},
		{"vector_sub_forwarded_histogram_left", func(o string) string { return forwardedPartner + ` - ` + o }, []ncRow{
			{Series: "h1", Disc: 1},
			{Series: "h2", Disc: 1},
		}},
		// histogram,histogram drops under `*`, and the float rows have no
		// matching partner row: nothing survives.
		{"vector_mul_forwarded_histogram", func(o string) string { return o + ` * ` + forwardedPartner }, nil},
		{"vector_add_histogram", func(o string) string { return o + ` + ` + ncPartnerHistMetric }, []ncRow{
			{Series: "h1", Disc: 1, Count: 7, Sum: 14, Bucket1: 14},
		}},
		{"vector_sub_histogram_left", func(o string) string { return ncPartnerHistMetric + ` - ` + o }, []ncRow{
			{Series: "h1", Disc: 1, Count: 3, Sum: 6, Bucket1: 2},
		}},
		// f1=3 scales the partner histogram; h1,h1 drops.
		{"vector_mul_histogram", func(o string) string { return o + ` * ` + ncPartnerHistMetric }, []ncRow{
			{Series: "f1", Disc: 1, Count: 21, Sum: 42, Bucket1: 27},
		}},
		{"vector_mul_histogram_left", func(o string) string { return ncPartnerHistMetric + ` * ` + o }, []ncRow{
			{Series: "f1", Disc: 1, Count: 21, Sum: 42, Bucket1: 27},
		}},
		{"compare_histogram_filter", func(o string) string { return o + ` == ` + ncPartnerHistMetric }, nil},
		{"compare_histogram_bool", func(o string) string { return o + ` != bool ` + ncPartnerHistMetric }, []ncRow{
			{Series: "h1", Value: 1},
		}},
		{"sum_over_vector_add_histogram", func(o string) string { return `sum(` + o + ` + ` + ncPartnerHistMetric + `)` }, []ncRow{
			{Series: "", Disc: 1, Count: 7, Sum: 14, Bucket1: 14},
		}},
	}
	for _, w := range wrappers {
		for _, c := range consumers {
			t.Run(w.name+"/"+c.name, func(t *testing.T) {
				nestedQuery := c.wrap(w.query)
				got := ncQueryRows(t, fixture, s, p, nestedQuery)
				if want := rootRows[c.name]; !reflect.DeepEqual(got, want) {
					t.Fatalf("nested %q answers\n  %+v\nbut its root answers\n  %+v", nestedQuery, got, want)
				}
			})
		}
		for _, c := range histogramPartnerConsumers {
			t.Run(w.name+"/"+c.name, func(t *testing.T) {
				nestedQuery := c.wrap(w.query)
				got := ncQueryRows(t, fixture, s, p, nestedQuery)
				if !reflect.DeepEqual(got, c.want) {
					t.Fatalf("nested %q answers\n  %+v\nwant\n  %+v", nestedQuery, got, c.want)
				}
			})
		}
	}
}
