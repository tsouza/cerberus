package main

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// mutationTargetForEngine is the whole correctness argument of the
// datashard-lane DELETE fix (issue #3105): every engine string ClickHouse
// could plausibly report for one of this seeder's target tables must map
// to a table that actually accepts a mutation. Exercised directly against
// fabricated engine strings — no live ClickHouse needed, unlike
// resolveMutationTarget's own system.tables round-trip (covered instead by
// the datashard e2e lane itself, which is the one place a Distributed
// wrapper actually exists).
func TestMutationTargetForEngine(t *testing.T) {
	t.Parallel()

	const table = "otel_metrics_gauge"

	for _, tc := range []struct {
		name          string
		engine        string
		wantTable     string
		wantOnCluster string
	}{
		{"Distributed wrapper redirects to the local table, ON CLUSTER", "Distributed", table + ddl.DataShardLocalSuffix, onClusterMutationClause},
		{"MergeTree is already the storage table, no ON CLUSTER", "MergeTree", table, ""},
		{"ReplicatedMergeTree is already the storage table, no ON CLUSTER", "ReplicatedMergeTree", table, ""},
		{"ReplacingMergeTree is already the storage table, no ON CLUSTER", "ReplacingMergeTree", table, ""},
		{"ReplicatedReplacingMergeTree is already the storage table, no ON CLUSTER", "ReplicatedReplacingMergeTree", table, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mutationTargetForEngine(table, tc.engine)
			if got.table != tc.wantTable {
				t.Fatalf("mutationTargetForEngine(%q, %q).table = %q, want %q", table, tc.engine, got.table, tc.wantTable)
			}
			if got.onCluster != tc.wantOnCluster {
				t.Fatalf("mutationTargetForEngine(%q, %q).onCluster = %q, want %q", table, tc.engine, got.onCluster, tc.wantOnCluster)
			}
		})
	}
}

// A table name is never mistaken for a Distributed wrapper unless its
// engine string is an EXACT match for "Distributed" — a substring or
// case-insensitive match would misclassify names ClickHouse never reports
// but that a naive check might accept.
func TestMutationTargetForEngineRequiresExactMatch(t *testing.T) {
	t.Parallel()

	const table = "otel_logs"
	for _, engine := range []string{"distributed", "DISTRIBUTED", "DistributedMergeTree", " Distributed", "Distributed "} {
		got := mutationTargetForEngine(table, engine)
		if got.table != table {
			t.Fatalf("mutationTargetForEngine(%q, %q).table = %q, want the unredirected table name %q", table, engine, got.table, table)
		}
		if got.onCluster != "" {
			t.Fatalf("mutationTargetForEngine(%q, %q).onCluster = %q, want empty", table, engine, got.onCluster)
		}
	}
}

// mutationTableSQL is the substitution half of the fix: every DELETE
// template must carry the placeholder EXACTLY twice (the outer
// ALTER/DELETE and the inner max(...) subquery's FROM, both pinned to the
// same resolved table), and mutationTableSQL must replace every
// occurrence, leaving none of the placeholder text behind. stale.go and
// showcase_traceql.go type "@@MUTATION_TABLE@@" directly into each
// template rather than referencing mutationTablePlaceholder by name (see
// those files' doc comments for why — keeping each const a single
// unbroken backtick literal for test/regression/seed_test.go's
// extractBacktickConst), so this test is what actually pins the two
// literals against each other: it fails the moment either side drifts.
func TestMutationTableSQLReplacesEveryTemplatePlaceholder(t *testing.T) {
	t.Parallel()

	const (
		target      = "otel_metrics_gauge_local"
		publicTable = "otel_metrics_gauge"
		onCluster   = onClusterMutationClause
	)

	for name, tpl := range map[string]string{
		"deleteStaleMetricsGaugeSQLTemplate":     deleteStaleMetricsGaugeSQLTemplate,
		"deleteStaleMetricsSumSQLTemplate":       deleteStaleMetricsSumSQLTemplate,
		"deleteStaleMetricsHistogramSQLTemplate": deleteStaleMetricsHistogramSQLTemplate,
		"deleteStaleMetricsExpHistSQLTemplate":   deleteStaleMetricsExpHistSQLTemplate,
		"deleteStaleLogsSQLTemplate":             deleteStaleLogsSQLTemplate,
		"deleteStaleBaseTracesSQLTemplate":       deleteStaleBaseTracesSQLTemplate,
		"deleteStaleShowcaseTracesSQLTemplate":   deleteStaleShowcaseTracesSQLTemplate,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const wantTableOccurrences = 2
			if got := strings.Count(tpl, mutationTablePlaceholder); got != wantTableOccurrences {
				t.Fatalf("%s contains %d occurrences of the table placeholder, want %d (one for the outer statement, one for the inner max(...) subquery's FROM)",
					name, got, wantTableOccurrences)
			}

			const wantOnClusterOccurrences = 1
			if got := strings.Count(tpl, mutationOnClusterPlaceholder); got != wantOnClusterOccurrences {
				t.Fatalf("%s contains %d occurrences of the ON CLUSTER placeholder, want %d (the outer statement only — the inner max(...) subquery is a plain per-node SELECT)",
					name, got, wantOnClusterOccurrences)
			}

			got := mutationTableSQL(tpl, target, publicTable, onCluster)
			if strings.Contains(got, mutationTablePlaceholder) {
				t.Fatalf("%s: mutationTableSQL left an unsubstituted table placeholder behind: %q", name, got)
			}
			if strings.Contains(got, mutationOnClusterPlaceholder) {
				t.Fatalf("%s: mutationTableSQL left an unsubstituted ON CLUSTER placeholder behind: %q", name, got)
			}
			if got2 := strings.Count(got, onCluster); got2 != wantOnClusterOccurrences {
				t.Fatalf("%s: mutationTableSQL produced %d occurrences of %q, want %d", name, got2, onCluster, wantOnClusterOccurrences)
			}

			// The outer statement gets `target` (the resolved local table),
			// the inner max(...) subquery's FROM gets `publicTable` (the
			// Distributed name, which aggregates correctly across every
			// shard) — see sharded_mutation.go's package doc comment for
			// why they must differ. `target` here is `publicTable + "_local"`
			// (matching real usage), so `publicTable` is a PREFIX of
			// `target`; search for it strictly after target's own span, not
			// with a plain strings.Index that would just re-find target's
			// own leading substring.
			outerIdx := strings.Index(got, target)
			if outerIdx == -1 {
				t.Fatalf("%s: mutationTableSQL did not place the resolved target %q anywhere: %q", name, target, got)
			}
			rest := got[outerIdx+len(target):]
			innerIdx := strings.Index(rest, publicTable)
			if innerIdx == -1 {
				t.Fatalf("%s: mutationTableSQL did not place the public table name %q AFTER the resolved target %q: %q",
					name, publicTable, target, got)
			}
			if got2 := strings.Count(rest, publicTable); got2 != 1 {
				t.Fatalf("%s: mutationTableSQL produced %d occurrences of the public table name %q after the outer target, want exactly 1", name, got2, publicTable)
			}

			// The ON CLUSTER clause must land on the OUTER statement, before
			// the inner subquery's FROM — never inside the subquery, which
			// would be invalid SQL (ON CLUSTER is DDL/mutation syntax, not
			// valid on a plain SELECT).
			if idx := strings.Index(got, onCluster); idx == -1 || idx > strings.Index(got, "SELECT") {
				t.Fatalf("%s: ON CLUSTER clause did not land before the inner SELECT: %q", name, got)
			}
		})
	}
}

// resolveMutationTarget's caller-visible behavior when NOT sharded (empty
// onCluster, target == publicTable) must leave the DELETE unchanged in
// shape — mutationTableSQL with an empty onCluster simply erases the
// placeholder rather than leaving a stray space or empty ON CLUSTER token
// behind, and both placeholder occurrences resolve to the SAME table name.
func TestMutationTableSQLEmptyOnClusterLeavesNoStrayToken(t *testing.T) {
	t.Parallel()

	got := mutationTableSQL(deleteStaleMetricsGaugeSQLTemplate, "otel_metrics_gauge", "otel_metrics_gauge", "")
	if strings.Contains(got, "ON CLUSTER") {
		t.Fatalf("empty onCluster left an ON CLUSTER token behind: %q", got)
	}
	if !strings.Contains(got, "ALTER TABLE otel_metrics_gauge DELETE") {
		t.Fatalf("expected the unsharded ALTER TABLE shape to be preserved verbatim, got: %q", got)
	}
	if got2 := strings.Count(got, "otel_metrics_gauge"); got2 != 2 {
		t.Fatalf("expected exactly 2 occurrences of otel_metrics_gauge (outer + inner, both unredirected), got %d: %q", got2, got)
	}
}
