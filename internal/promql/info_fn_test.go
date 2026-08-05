package promql_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Info covers PromQL's `info(v[, {matchers}])` label-enrichment
// join. `info` is experimental, so the parser must enable experimental
// functions. The lowering builds a chplan.InfoJoin keyed on
// {instance, job}; the chsql emitter renders a join whose output keeps
// the base side's value/timestamp and grows its Attributes with the info
// series' data labels via mapConcat.
func TestLower_Info(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		query        string
		wantInSQL    []string
		notWantInSQL []string
		wantArgs     []string
	}{
		{
			name:  "default target_info enrichment",
			query: `info(up)`,
			wantInSQL: []string{
				"LEFT JOIN",
				"L.`Attributes`[?] = R.`Attributes`[?]",
				"mapConcat(",
				"mapFilter((k, v) -> NOT (k IN (?, ?, ?))",
				"L.`Value` AS `Value`",
				"L.`MetricName` AS `MetricName`",
			},
			notWantInSQL: []string{"INNER JOIN", "groupArrayArray("},
		},
		{
			name:  "second-arg name matcher selects info metric",
			query: `info(up, {__name__="build_info"})`,
			wantInSQL: []string{
				"LEFT JOIN",
				"mapConcat(",
			},
			notWantInSQL: []string{"INNER JOIN", "groupArrayArray("},
			wantArgs:     []string{"build_info"},
		},
		{
			name:  "second-arg data-label matcher restricts copied labels",
			query: `info(up, {version=~".+"})`,
			wantInSQL: []string{
				"INNER JOIN",
				"mapFilter((k, v) -> k IN (?)",
			},
			notWantInSQL: []string{"LEFT JOIN", "groupArrayArray("},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, args := emitInfoQuery(t, tc.query)
			for _, want := range tc.wantInSQL {
				if !strings.Contains(sql, want) {
					t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
				}
			}
			for _, unwanted := range tc.notWantInSQL {
				if strings.Contains(sql, unwanted) {
					t.Errorf("expected SQL NOT to contain %q; full SQL:\n%s", unwanted, sql)
				}
			}
			for _, want := range tc.wantArgs {
				if !slices.Contains(args, want) {
					t.Errorf("expected args to contain %q; got %v", want, args)
				}
			}
		})
	}
}

// TestLower_Info_DropsUnmatchedBaseSamples pins the reference engine's
// drop branch (promql/info.go::combineWithInfoVector,
// `allMatchersMatchEmpty`): a base sample that matched no info series
// survives only when every data-label matcher also matches the empty
// string. A matcher that cannot match "" therefore turns the enrichment
// into an INNER JOIN — an unmatched base sample is not in the result.
func TestLower_Info_DropsUnmatchedBaseSamples(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		// drop is true when the reference engine would omit an
		// info-less base sample from the result.
		drop bool
	}{
		{name: "no second argument keeps unmatched", query: `info(up)`, drop: false},
		{name: "name-only selector keeps unmatched", query: `info(up, {__name__="build_info"})`, drop: false},
		{name: "match-all regex keeps unmatched", query: `info(up, {version=~".*"})`, drop: false},
		{name: "negated matcher keeps unmatched", query: `info(up, {version!="1.2.3"})`, drop: false},
		{name: "empty-matching negated regex keeps unmatched", query: `info(up, {version!~".+"})`, drop: false},
		{name: "equality matcher drops unmatched", query: `info(up, {version="1.2.3"})`, drop: true},
		{name: "non-empty regex drops unmatched", query: `info(up, {version=~".+"})`, drop: true},
		{name: "negated empty matcher drops unmatched", query: `info(up, {version!=""})`, drop: true},
		{name: "one non-empty matcher among several drops unmatched", query: `info(up, {version=~".*", build=~".+"})`, drop: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, _ := emitInfoQuery(t, tc.query)
			wantJoin, unwantedJoin := "LEFT JOIN", "INNER JOIN"
			if tc.drop {
				wantJoin, unwantedJoin = "INNER JOIN", "LEFT JOIN"
			}
			if !strings.Contains(sql, wantJoin) {
				t.Errorf("expected %s for %s; full SQL:\n%s", wantJoin, tc.query, sql)
			}
			if strings.Contains(sql, unwantedJoin) {
				t.Errorf("expected no %s for %s; full SQL:\n%s", unwantedJoin, tc.query, sql)
			}
		})
	}
}

// TestLower_Info_NameMatcherTypes pins the reference engine's
// `__name__`-matcher handling (promql/info.go::effectiveInfoNameMatchers).
// Equality, regex, negated and multi-metric selectors all lower; a
// selector carrying only negated name matchers gains the synthetic
// `.+_info` regex so the info scan stays anchored to the info-metric
// family. Selectors that can match several info metrics additionally fold
// the per-metric join rows back to one row per base sample, because the
// reference engine unions their data labels onto a single output sample.
func TestLower_Info_NameMatcherTypes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// query is the info() call under test.
		query string
		// merge is true when the selector may match several info
		// metrics, so the emitter must fold their rows together.
		merge bool
		// wantArgs are bound values the emitted SQL must carry — the
		// name-matcher values that reached the info-side scan.
		wantArgs []string
	}{
		{
			name:     "equality name matcher pins one metric",
			query:    `info(up, {__name__="build_info"})`,
			merge:    false,
			wantArgs: []string{"build_info"},
		},
		{
			// Regex matcher values are bound anchored (`^(?:...)$`,
			// issue #1741 — ClickHouse's match() is a substring search
			// but Prometheus `__name__` regex matchers are always
			// fully anchored); equality matcher values (build_info /
			// target_info below) are untouched by that wrap.
			name:     "regex name matcher may match several metrics",
			query:    `info(up, {__name__=~".*_info"})`,
			merge:    true,
			wantArgs: []string{"^(?:.*_info)$"},
		},
		{
			name:     "negated-only name matcher gains the .+_info fallback",
			query:    `info(up, {__name__!="target_info"})`,
			merge:    true,
			wantArgs: []string{"^(?:.+_info)$", "target_info"},
		},
		{
			name:     "negated regex name matcher gains the .+_info fallback",
			query:    `info(up, {__name__!~"target.*"})`,
			merge:    true,
			wantArgs: []string{"^(?:.+_info)$", "^(?:target.*)$"},
		},
		{
			name:     "positive plus negated name matchers keep the positive anchor",
			query:    `info(up, {__name__=~".*_info", __name__!="build_info"})`,
			merge:    true,
			wantArgs: []string{"^(?:.*_info)$", "build_info"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, args := emitInfoQuery(t, tc.query)
			for _, want := range tc.wantArgs {
				if !slices.Contains(args, want) {
					t.Errorf("expected args to contain %q; got %v\nfull SQL:\n%s", want, args, sql)
				}
			}
			// The fold is the outer GROUP BY on the four base-side
			// columns plus the groupArrayArray map union; the inner
			// per-series-latest aggregate carries its own GROUP BY, so
			// the base-side key list is what discriminates.
			const foldGroupBy = "GROUP BY L.`MetricName`, L.`Attributes`, L.`TimeUnix`, L.`Value`"
			hasFold := strings.Contains(sql, "groupArrayArray(") && strings.Contains(sql, foldGroupBy)
			if hasFold != tc.merge {
				t.Errorf("multi-metric fold present=%v, want %v; full SQL:\n%s", hasFold, tc.merge, sql)
			}
		})
	}
}

// emitInfoQuery parses, lowers and emits an `info(...)` query, returning
// the SQL text and the string-typed bound arguments.
func emitInfoQuery(t *testing.T, query string) (string, []string) {
	t.Helper()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower(%q): %v", query, err)
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	strs := make([]string, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok {
			strs = append(strs, s)
		}
	}
	return sql, strs
}
