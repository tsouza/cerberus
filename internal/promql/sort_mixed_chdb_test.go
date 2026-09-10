//go:build chdb

package promql_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"golang.org/x/tools/txtar"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
	"github.com/tsouza/cerberus/test/spec"
)

type sortFloatTuple struct {
	metric, series string
	timestamp      int64
	value          float64
}

func TestSortNestedMixedFloatOnly_ChDB(t *testing.T) {
	// A histogram-only row keeps float-first union from hiding the bug by
	// shadowing its sole histogram. The duplicate still proves shadow-then-drop.
	seed := strings.Replace(tkShadowSeed, "[6], 0, []);", "[6], 0, []), ('"+tkShadowHistMetric+"', map('series', 'hist_only'), toDateTime64('2026-01-01 00:00:00', 9), 2, 4.0, 0, 0, 0, [6], 0, []);", 1)
	if seed == tkShadowSeed {
		t.Fatal("histogram-only seed insertion did not match")
	}
	sampleTime := foEvalTS.Add(-time.Second).UnixNano()
	solo := sortFloatTuple{tkShadowFloatMetric, "solo", sampleTime, 7}
	duplicate := sortFloatTuple{tkShadowFloatMetric, "dup", sampleTime, 42}
	for _, fn := range []string{"sort", "sort_desc"} {
		bothFloats := []sortFloatTuple{solo, duplicate}
		if fn == "sort_desc" {
			bothFloats = []sortFloatTuple{duplicate, solo}
		}
		for _, histFirst := range []bool{false, true} {
			operand := tkShadowFloatMetric + " or " + tkShadowHistMetric
			want := bothFloats
			if histFirst {
				operand = tkShadowHistMetric + " or " + tkShadowFloatMetric
				want = []sortFloatTuple{solo}
			}
			direct := fn + "(" + operand + ")"
			for _, wrapper := range []string{"", "sort_by_label", "sort_by_label_desc"} {
				query := direct
				if wrapper != "" {
					query = fn + "(" + wrapper + "(" + operand + `, "series"))`
				}
				t.Run(fmt.Sprintf("%s/%v/%s", fn, histFirst, wrapper), func(t *testing.T) {
					checkSortFloatTuples(t, query, direct, seed, want, 0)
				})
			}
		}
		for index, control := range []struct {
			operand string
			want    []sortFloatTuple
		}{
			{tkShadowFloatMetric, bothFloats},
			{tkShadowFloatMetric + `{series="solo"}`, []sortFloatTuple{solo}},
			{tkShadowHistMetric, nil},
			{tkShadowHistMetric + " + 0", nil},
		} {
			t.Run(fmt.Sprintf("%s/control/%d", fn, index), func(t *testing.T) {
				query := fn + "(" + control.operand + ")"
				checkSortFloatTuples(t, query, query, seed, control.want, 0)
			})
		}
	}
	t.Run("range-membership", func(t *testing.T) {
		const step = time.Minute
		operand := tkShadowFloatMetric + " or " + tkShadowHistMetric
		query := "sort_desc(sort_by_label(" + operand + `, "series"))`
		direct := "sort_desc(" + operand + ")"
		var want []sortFloatTuple
		for _, at := range []time.Time{foEvalTS, foEvalTS.Add(step)} {
			want = append(want,
				sortFloatTuple{tkShadowFloatMetric, "dup", at.UnixNano(), 42},
				sortFloatTuple{tkShadowFloatMetric, "solo", at.UnixNano(), 7})
		}
		checkSortFloatTuples(t, query, direct, seed, want, step)
	})
}

func checkSortFloatTuples(t *testing.T, query, direct, seed string, want []sortFloatTuple, step time.Duration) {
	t.Helper()
	const permissions = 0o600
	expected := make([][]any, 0, len(want))
	for _, tuple := range want {
		expected = append(expected, []any{tuple.metric, map[string]string{"series": tuple.series}, time.Unix(0, tuple.timestamp).UTC().Format(time.RFC3339), tuple.value})
	}
	expectedBytes, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "/api/v1/query"
	if step > 0 {
		endpoint = "/api/v1/query_range"
	}
	archive := &txtar.Archive{Files: []txtar.File{
		{Name: "query.promql", Data: []byte(query + "\n")},
		{Name: "parity", Data: []byte("oracle: prometheus\nendpoint: " + endpoint + "\nscope: full\n")},
		{Name: "seed", Data: []byte(seed)},
		{Name: "expected_rows", Data: expectedBytes},
	}}
	path := filepath.Join(t.TempDir(), "sort.txtar")
	if err := os.WriteFile(path, txtar.Format(archive), permissions); err != nil {
		t.Fatal(err)
	}
	fixture, err := spec.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	emit := func(query string) (string, []any) {
		p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), foEvalTS, foEvalTS.Add(step), step)
		if err != nil {
			t.Fatal(err)
		}
		optimized := spec.AssertScanTimeBoundAccepts(t, plan)
		sql, args, err := chsql.Emit(context.Background(), optimized)
		if err != nil {
			t.Fatal(err)
		}
		return sql, args
	}
	// First establish the direct control and independently evaluate the actual
	// query in Prometheus over seeded rows. Its comparison normalizes order.
	directSQL, directArgs := emit(direct)
	result := spec.RunRoundTripSQL(t, fixture, directSQL, directArgs)
	spec.RunParity(t, fixture, spec.ParityEval{Start: foEvalTS, End: foEvalTS.Add(step), Step: step}, result)

	// Then assert actual optimized SQL's ordered tuples, including exact row
	// count, labels, metric name and source timestamp. A map would hide duplicate
	// rows, and the canonical parity comparison alone cannot prove sort order.
	sql, args := emit(query)
	db := newChDBFixture(t, seed)
	rows := db.queryOverEmitted(t, "`MetricName` AS metric, `Attributes`['series'] AS series, toUnixTimestamp64Nano(`TimeUnix`) AS timestamp, `Value` AS value", sql, args)
	defer func() { _ = rows.Close() }()
	var got []sortFloatTuple
	for rows.Next() {
		var tuple sortFloatTuple
		if err := rows.Scan(&tuple.metric, &tuple.series, &tuple.timestamp, &tuple.value); err != nil {
			t.Fatal(err)
		}
		got = append(got, tuple)
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatal(err)
	}
	if step > 0 {
		// query_range compares series membership per step, not vector order.
		compare := func(a, b sortFloatTuple) int {
			return cmp.Or(cmp.Compare(a.timestamp, b.timestamp), cmp.Compare(a.metric, b.metric), cmp.Compare(a.series, b.series))
		}
		slices.SortFunc(got, compare)
		slices.SortFunc(want, compare)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query=%s\ngot=%v\nwant=%v", query, got, want)
	}
}
