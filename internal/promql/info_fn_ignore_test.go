package promql_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
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
// This guard is only meaningful where several info metrics can merge
// onto one base sample; a `__name__` equality pins a single info metric,
// so there is nothing to disagree with and no merge guard is emitted. It
// is distinct from — and always coexists with — the per-signature
// timestamp-tie throwIf collapseInfoSeriesBySignature always plants on
// the info arm as its own HAVING clause (see
// TestLower_Info_SignatureCollapse), so both guards render as `HAVING
// (throwIf(...) = ?)`. The merge guard's own marker is
// `groupArrayArray(arrayZip(` — the per-info-metric label-pair fold only
// the merge path builds — rather than the bare "HAVING" any info() query
// now emits.
func TestLower_Info_ConflictGuard(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		query string
		// guard is true when several info metrics may merge, so a
		// conflict is representable and the merge guard must abort the
		// query.
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
			// The pairs are zipped BEFORE the fold, so a key can never be
			// paired with another info metric's value; its presence is
			// what's unique to the merge-conflict guard.
			hasMergeGuard := strings.Contains(sql, "groupArrayArray(arrayZip(")
			if hasMergeGuard != tc.guard {
				t.Fatalf("merge guard present = %v, want %v for %s; full SQL:\n%s",
					hasMergeGuard, tc.guard, tc.query, sql)
			}
			// The signature-tie HAVING guard is unconditional, so
			// "HAVING" and the shared message always appear regardless
			// of merge multiplicity.
			for _, want := range []string{
				"HAVING",
				"'" + chplan.InfoConflictingLabelMessage + "'",
			} {
				if !strings.Contains(sql, want) {
					t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
				}
			}
		})
	}
}

// TestLower_Info_SignatureCollapse pins collapseInfoSeriesBySignature's
// always-on shape: every info() lowering — merged or not — groups the
// info arm by (metric name, identity-label values) and plants a
// timestamp-tie throwIf in a real HAVING clause, so ClickHouse's
// column-pruning analyzer can never optimise the guard away as an
// unreferenced SELECT-list expression (see chplan.Aggregate.Having's
// doc and issue #1630).
func TestLower_Info_SignatureCollapse(t *testing.T) {
	t.Parallel()

	for _, query := range []string{
		`info(up)`,
		`info(up, {__name__="build_info"})`,
		`info(up, {__name__=~".+_info"})`,
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			sql, _ := emitInfoQuery(t, query)
			for _, want := range []string{
				"argMax(",
				"HAVING",
				"countEqual(groupArray(",
				"'" + chplan.InfoConflictingLabelMessage + "'",
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
