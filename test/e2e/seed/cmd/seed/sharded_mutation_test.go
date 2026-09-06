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
		name   string
		engine string
		want   string
	}{
		{"Distributed wrapper redirects to the local table", "Distributed", table + ddl.DataShardLocalSuffix},
		{"MergeTree is already the storage table", "MergeTree", table},
		{"ReplicatedMergeTree is already the storage table", "ReplicatedMergeTree", table},
		{"ReplacingMergeTree is already the storage table", "ReplacingMergeTree", table},
		{"ReplicatedReplacingMergeTree is already the storage table", "ReplicatedReplacingMergeTree", table},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mutationTargetForEngine(table, tc.engine); got != tc.want {
				t.Fatalf("mutationTargetForEngine(%q, %q) = %q, want %q", table, tc.engine, got, tc.want)
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
		if got := mutationTargetForEngine(table, engine); got != table {
			t.Fatalf("mutationTargetForEngine(%q, %q) = %q, want the unredirected table name %q", table, engine, got, table)
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

	const target = "otel_metrics_gauge_local"

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

			const wantOccurrences = 2
			if got := strings.Count(tpl, mutationTablePlaceholder); got != wantOccurrences {
				t.Fatalf("%s contains %d occurrences of the placeholder, want %d (one for the outer statement, one for the inner max(...) subquery's FROM)",
					name, got, wantOccurrences)
			}

			got := mutationTableSQL(tpl, target)
			if strings.Contains(got, mutationTablePlaceholder) {
				t.Fatalf("%s: mutationTableSQL left an unsubstituted placeholder behind: %q", name, got)
			}
			if got2 := strings.Count(got, target); got2 != wantOccurrences {
				t.Fatalf("%s: mutationTableSQL produced %d occurrences of %q, want %d", name, got2, target, wantOccurrences)
			}
		})
	}
}
