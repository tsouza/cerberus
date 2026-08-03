package promql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestLower_Info_IgnoresBaseInfoSeries pins the reference engine's
// `ignoreSeries` carve-out (promql/info.go::evalInfo builds the set,
// combineWithInfoVector appends its members to the output verbatim): a
// base series whose `__name__` matches EVERY effective name matcher is
// never enriched, and never dropped either.
//
// When the base vector pins its own metric name the answer is a
// compile-time one, so the lowering collapses to a single arm — either a
// plain join (the name is not an info metric) or a bare pass-through with
// no join at all (it is). When the base can yield several names the split
// survives into the plan as a UNION ALL of the two arms.
func TestLower_Info_IgnoresBaseInfoSeries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		query        string
		wantInSQL    []string
		notWantInSQL []string
	}{
		{
			// Control: an ordinary base metric is not in the ignore set,
			// so nothing about the existing join shape changes.
			name:         "ordinary base metric still joins",
			query:        `info(up)`,
			wantInSQL:    []string{"LEFT JOIN"},
			notWantInSQL: []string{"UNION ALL"},
		},
		{
			// The base IS the info metric the default matcher selects, so
			// every base series is ignored and info() is a no-op.
			name:         "base is the default info metric",
			query:        `info(target_info)`,
			notWantInSQL: []string{"LEFT JOIN", "INNER JOIN", "UNION ALL"},
		},
		{
			// The wrongly-DROPPED divergence: `{version=~".+"}` cannot
			// match the empty string, which would make the join INNER and
			// drop a `target_info` series carrying no `version`. Ignored
			// series bypass the drop logic entirely.
			name:         "ignored base survives a dropping data matcher",
			query:        `info(target_info, {version=~".+"})`,
			notWantInSQL: []string{"LEFT JOIN", "INNER JOIN", "UNION ALL"},
		},
		{
			// The wrongly-ENRICHED divergence: `build_info` is itself a
			// member of the `.+_info` family it is being enriched from.
			name:         "base inside the selected info-metric family",
			query:        `info(build_info, {__name__=~".+_info"})`,
			notWantInSQL: []string{"LEFT JOIN", "INNER JOIN", "UNION ALL"},
		},
		{
			// A regex-named base yields several metric names, so ignore-set
			// membership is a per-row question: the plan carries both arms.
			name:  "unpinned base splits into two arms",
			query: `info({__name__=~"up|target_info"})`,
			wantInSQL: []string{
				"UNION ALL",
				"LEFT JOIN",
				// The join arm sees only the non-ignored names ...
				"`MetricName` NOT IN (?, ?, ?)",
				// ... and the pass-through arm only the ignored ones.
				"`MetricName` IN (?, ?, ?)",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, _ := emitInfoQuery(t, tc.query)
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
		})
	}
}

// TestLower_Info_ConflictGuard pins the multi-info-metric merge's
// conflicting-label abort (promql/info.go::combineWithInfoVector returns
// `conflicting label: %s` when a second info metric contributes a data
// label the first already recorded with a different value).
//
// The guard is only meaningful where several info metrics can merge onto
// one base sample; a `__name__` equality pins a single info metric, so
// there is nothing to disagree with and no guard is emitted.
func TestLower_Info_ConflictGuard(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		// guard is true when several info metrics may merge, so a
		// conflict is representable and must abort the query.
		guard bool
	}{
		{name: "regex name matcher merges several info metrics", query: `info(up, {__name__=~".+_info"})`, guard: true},
		{name: "negated name matcher merges the .+_info family", query: `info(up, {__name__!="build_info"})`, guard: true},
		{name: "default target_info pins one info metric", query: `info(up)`, guard: false},
		{name: "name equality pins one info metric", query: `info(up, {__name__="build_info"})`, guard: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, _ := emitInfoQuery(t, tc.query)
			hasGuard := strings.Contains(sql, "throwIf(")
			if hasGuard != tc.guard {
				t.Fatalf("conflict guard present = %v, want %v for %s; full SQL:\n%s",
					hasGuard, tc.guard, tc.query, sql)
			}
			if !tc.guard {
				return
			}
			for _, want := range []string{
				"HAVING",
				"'" + chsql.InfoConflictingLabelMessage + "'",
				// The pairs are zipped BEFORE the fold, so a key can never
				// be paired with another info metric's value.
				"groupArrayArray(arrayZip(",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
				}
			}
		})
	}
}

// TestLower_Info_ExtrasSkipBaseCarriedAndEmptyLabels pins the two
// exclusions the extras filter applies on top of the name selection, both
// of them mirroring the reference engine's per-label loop: a label the
// base series already carries is skipped (so the base wins structurally,
// and the skipped label takes no part in the conflict test), and an
// empty-valued label counts as absent.
func TestLower_Info_ExtrasSkipBaseCarriedAndEmptyLabels(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		`info(up)`,
		`info(up, {version=~".+"})`,
		`info(up, {__name__=~".+_info"})`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := emitInfoQuery(t, query)
			for _, want := range []string{
				"NOT mapContains(L.`Attributes`, k)",
				"v != ?",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
				}
			}
		})
	}
}
