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

// mutationTableSQL is the substitution half of the fix: every step-A
// cutoff SELECT template must carry the table placeholder EXACTLY once and
// carry NO ON CLUSTER placeholder (a plain SELECT is never DDL/mutation
// syntax), and every step-B ALTER/DELETE template must carry the table
// placeholder EXACTLY once plus the ON CLUSTER placeholder EXACTLY once —
// mutationTableSQL must replace every occurrence, leaving none of the
// placeholder text behind. stale.go and showcase_traceql.go type
// "@@MUTATION_TABLE@@"/"@@MUTATION_ON_CLUSTER@@" directly into each
// template rather than referencing the placeholder consts by name (see
// those files' doc comments for why — keeping each const a single
// unbroken backtick literal for test/regression/seed_test.go's
// extractBacktickConst), so this test is what actually pins the literals
// against each other: it fails the moment either side drifts.
func TestMutationTableSQLReplacesEveryTemplatePlaceholder(t *testing.T) {
	t.Parallel()

	const (
		target      = "otel_metrics_gauge_local"
		publicTable = "otel_metrics_gauge"
		onCluster   = onClusterMutationClause
	)

	selectTemplates := map[string]string{
		"selectStaleMetricsGaugeCutoffSQLTemplate":     selectStaleMetricsGaugeCutoffSQLTemplate,
		"selectStaleMetricsSumCutoffSQLTemplate":       selectStaleMetricsSumCutoffSQLTemplate,
		"selectStaleMetricsHistogramCutoffSQLTemplate": selectStaleMetricsHistogramCutoffSQLTemplate,
		"selectStaleMetricsExpHistCutoffSQLTemplate":   selectStaleMetricsExpHistCutoffSQLTemplate,
		"selectStaleLogsCutoffSQLTemplate":             selectStaleLogsCutoffSQLTemplate,
		"selectStaleBaseTracesCutoffSQLTemplate":       selectStaleBaseTracesCutoffSQLTemplate,
		"selectStaleShowcaseTracesCutoffSQLTemplate":   selectStaleShowcaseTracesCutoffSQLTemplate,
	}
	for name, tpl := range selectTemplates {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const wantTableOccurrences = 1
			if got := strings.Count(tpl, mutationTablePlaceholder); got != wantTableOccurrences {
				t.Fatalf("%s contains %d occurrences of the table placeholder, want %d (a step-A cutoff SELECT substitutes it once, for the PUBLIC table)",
					name, got, wantTableOccurrences)
			}
			if strings.Contains(tpl, mutationOnClusterPlaceholder) {
				t.Fatalf("%s contains the ON CLUSTER placeholder, want none — a step-A cutoff SELECT is a plain per-node query, never DDL/mutation syntax", name)
			}

			got := mutationTableSQL(tpl, publicTable, "")
			if strings.Contains(got, mutationTablePlaceholder) {
				t.Fatalf("%s: mutationTableSQL left an unsubstituted table placeholder behind: %q", name, got)
			}
			if got2 := strings.Count(got, publicTable); got2 != wantTableOccurrences {
				t.Fatalf("%s: mutationTableSQL produced %d occurrences of %q, want %d", name, got2, publicTable, wantTableOccurrences)
			}
		})
	}

	deleteTemplates := map[string]string{
		"deleteStaleMetricsGaugeSQLTemplate":     deleteStaleMetricsGaugeSQLTemplate,
		"deleteStaleMetricsSumSQLTemplate":       deleteStaleMetricsSumSQLTemplate,
		"deleteStaleMetricsHistogramSQLTemplate": deleteStaleMetricsHistogramSQLTemplate,
		"deleteStaleMetricsExpHistSQLTemplate":   deleteStaleMetricsExpHistSQLTemplate,
		"deleteStaleLogsSQLTemplate":             deleteStaleLogsSQLTemplate,
		"deleteStaleBaseTracesSQLTemplate":       deleteStaleBaseTracesSQLTemplate,
		"deleteStaleShowcaseTracesSQLTemplate":   deleteStaleShowcaseTracesSQLTemplate,
	}
	for name, tpl := range deleteTemplates {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			const wantTableOccurrences = 1
			if got := strings.Count(tpl, mutationTablePlaceholder); got != wantTableOccurrences {
				t.Fatalf("%s contains %d occurrences of the table placeholder, want %d (a step-B ALTER/DELETE substitutes it once, for the resolved mutation target)",
					name, got, wantTableOccurrences)
			}

			const wantOnClusterOccurrences = 1
			if got := strings.Count(tpl, mutationOnClusterPlaceholder); got != wantOnClusterOccurrences {
				t.Fatalf("%s contains %d occurrences of the ON CLUSTER placeholder, want %d", name, got, wantOnClusterOccurrences)
			}

			const cutoffBind = "{cutoff:DateTime64(9)}"
			if !strings.Contains(tpl, cutoffBind) {
				t.Fatalf("%s: step-B template must bind the literal cutoff as %s — no subquery may survive inside the mutation (issue #3124)", name, cutoffBind)
			}
			if strings.Contains(tpl, "SELECT") {
				t.Fatalf("%s: step-B template must carry no SELECT of its own — the cutoff is resolved by a separate step-A query, never a subquery nested in the mutation (issue #3124)", name)
			}

			got := mutationTableSQL(tpl, target, onCluster)
			if strings.Contains(got, mutationTablePlaceholder) {
				t.Fatalf("%s: mutationTableSQL left an unsubstituted table placeholder behind: %q", name, got)
			}
			if strings.Contains(got, mutationOnClusterPlaceholder) {
				t.Fatalf("%s: mutationTableSQL left an unsubstituted ON CLUSTER placeholder behind: %q", name, got)
			}
			if got2 := strings.Count(got, target); got2 != wantTableOccurrences {
				t.Fatalf("%s: mutationTableSQL produced %d occurrences of %q, want %d", name, got2, target, wantTableOccurrences)
			}
			if got2 := strings.Count(got, onCluster); got2 != wantOnClusterOccurrences {
				t.Fatalf("%s: mutationTableSQL produced %d occurrences of %q, want %d", name, got2, onCluster, wantOnClusterOccurrences)
			}
		})
	}
}

// resolveMutationTarget's caller-visible behavior when NOT sharded (empty
// onCluster) must leave the DELETE unchanged in shape — mutationTableSQL
// with an empty onCluster simply erases the placeholder rather than
// leaving a stray space or empty ON CLUSTER token behind.
func TestMutationTableSQLEmptyOnClusterLeavesNoStrayToken(t *testing.T) {
	t.Parallel()

	got := mutationTableSQL(deleteStaleMetricsGaugeSQLTemplate, "otel_metrics_gauge", "")
	if strings.Contains(got, "ON CLUSTER") {
		t.Fatalf("empty onCluster left an ON CLUSTER token behind: %q", got)
	}
	if !strings.Contains(got, "ALTER TABLE otel_metrics_gauge DELETE") {
		t.Fatalf("expected the unsharded ALTER TABLE shape to be preserved verbatim, got: %q", got)
	}
	if got2 := strings.Count(got, "otel_metrics_gauge"); got2 != 1 {
		t.Fatalf("expected exactly 1 occurrence of otel_metrics_gauge (the outer target; no inner subquery survives post-#3124), got %d: %q", got2, got)
	}
	if !strings.Contains(got, "{cutoff:DateTime64(9)}") {
		t.Fatalf("expected the literal cutoff bind parameter to survive mutationTableSQL untouched: %q", got)
	}
}
