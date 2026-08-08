package testsql

import (
	"strings"
	"testing"
)

// fanOutSQL is the shape internal/promql's chdb fixtures emit: a
// `SELECT *` off a two-arm metrics fan-out, wrapped in the projection
// that reads ServiceName and ResourceAttributes off the merged relation.
// Trimmed to the identifiers that matter; the real statement is the same
// shape with more arithmetic.
const fanOutSQL = "SELECT `MetricName` AS `MetricName`, " +
	"mapUpdate(mapFilter((k, v) -> (k NOT IN (?, ?)), `ResourceAttributes`), map(?, toString(`ServiceName`))) AS `Attributes`, " +
	"`TimeUnix` AS `TimeUnix`, `Value` AS `Value` " +
	"FROM (SELECT * FROM merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$') WHERE (`MetricName` IN (?)))"

// gaugeOnlySeed is the pre-fix internal/promql/sort_by_label_chdb_test.go
// seed: it creates only one of the two arms the fan-out scans, and that
// arm carries no ServiceName.
const gaugeOnlySeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('http_requests', map('handler', 'b'), toDateTime64('2026-01-01 00:00:00', 9), 30.0);`

// bothArmsSeed materialises both arms with the full projected column set.
const bothArmsSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    ` + "`MetricName`" + ` String,
    ` + "`Attributes`" + ` Map(String, String),
    ` + "`ResourceAttributes`" + ` Map(String, String) DEFAULT map(),
    ` + "`ServiceName`" + ` LowCardinality(String) DEFAULT '',
    ` + "`TimeUnix`" + ` DateTime64(9),
    ` + "`Value`" + ` Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
CREATE OR REPLACE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    ServiceName LowCardinality(String) DEFAULT '',
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);`

// TestCheckSeedCoversFanOut_MissingArm pins the exact coupling issue
// #1910 records: the seed creates one arm of the fan-out and relies on a
// sibling fixture in the shared chDB session for the other.
func TestCheckSeedCoversFanOut_MissingArm(t *testing.T) {
	err := CheckSeedCoversFanOut(gaugeOnlySeed, fanOutSQL)
	if err == nil {
		t.Fatal("a seed that creates only otel_metrics_gauge must not pass a fan-out over gauge AND sum")
	}
	if !strings.Contains(err.Error(), "otel_metrics_sum") {
		t.Errorf("error must name the uncreated arm; got: %v", err)
	}
	if !strings.Contains(err.Error(), "ServiceName") {
		t.Errorf("error must name the column no created arm declares; got: %v", err)
	}
}

// TestCheckSeedCoversFanOut_MissingColumn pins the second half: both arms
// exist, but neither declares a column the emitted SQL projects, so the
// query would resolve it only off some other table a sibling created.
func TestCheckSeedCoversFanOut_MissingColumn(t *testing.T) {
	seed := strings.ReplaceAll(bothArmsSeed, "ServiceName", "SomeOtherColumn")
	err := CheckSeedCoversFanOut(seed, fanOutSQL)
	if err == nil {
		t.Fatal("a seed whose arms omit a projected column must not pass")
	}
	if !strings.Contains(err.Error(), `column "ServiceName"`) {
		t.Errorf("error must name the missing column; got: %v", err)
	}
	if strings.Contains(err.Error(), "never created by this seed") {
		t.Errorf("both arms exist, so no arm should be reported missing; got: %v", err)
	}
}

// TestCheckSeedCoversFanOut_SelfSufficient is the positive direction: a
// seed that materialises every arm with every projected column passes.
func TestCheckSeedCoversFanOut_SelfSufficient(t *testing.T) {
	if err := CheckSeedCoversFanOut(bothArmsSeed, fanOutSQL); err != nil {
		t.Fatalf("a self-sufficient seed must pass: %v", err)
	}
}

// TestCheckSeedCoversFanOut_ColumnUnionAcrossArms pins that the column
// half honours merge()'s UNION semantics: a column carried by one arm and
// not the other is satisfied, because that is how production's metric
// tables genuinely differ (only the histogram tables carry Count / Sum).
func TestCheckSeedCoversFanOut_ColumnUnionAcrossArms(t *testing.T) {
	seed := strings.Replace(bothArmsSeed, "`ServiceName` LowCardinality(String) DEFAULT '',", "", 1)
	if err := CheckSeedCoversFanOut(seed, fanOutSQL); err != nil {
		t.Fatalf("ServiceName on the sum arm alone must satisfy a merge() fan-out: %v", err)
	}
}

// TestCheckSeedCoversFanOut_NoFanOut keeps a single-table scan out of
// scope: an unseeded plain table fails with UNKNOWN_TABLE, which is not
// the confusable failure this check exists for.
func TestCheckSeedCoversFanOut_NoFanOut(t *testing.T) {
	sqlText := "SELECT `Value` FROM `otel_metrics_gauge` WHERE (`ServiceName` = ?)"
	if err := CheckSeedCoversFanOut("", sqlText); err != nil {
		t.Fatalf("a statement with no merge() fan-out must pass: %v", err)
	}
}

// TestCheckSeedCoversFanOut_UndecodablePattern pins that an unfamiliar
// fan-out regex errors rather than silently passing — a check that skips
// what it cannot parse is a gap wearing a green tick.
func TestCheckSeedCoversFanOut_UndecodablePattern(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"unanchored", "SELECT * FROM merge(currentDatabase(), 'otel_metrics_.*')"},
		{"wildcard_arm", "SELECT * FROM merge(currentDatabase(), '^(otel_metrics_.*)$')"},
		{"one_argument", "SELECT * FROM merge('^(otel_metrics_gauge)$')"},
		{"unquoted_pattern", "SELECT * FROM merge(currentDatabase(), pattern)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckSeedCoversFanOut(bothArmsSeed, tc.sql); err == nil {
				t.Fatalf("pattern this check cannot decompose must error, not pass: %s", tc.sql)
			}
		})
	}
}

// TestCheckSeedCoversFanOut_SingleArmAndQuotedDB covers the two shapes
// chsql's mergeTableFrag emits besides the common one: a one-member
// fan-out (still parenthesised) and an explicitly named database.
func TestCheckSeedCoversFanOut_SingleArmAndQuotedDB(t *testing.T) {
	sqlText := "SELECT `ServiceName` AS `s` FROM merge('otel', '^(otel_metrics_sum)$')"
	if err := CheckSeedCoversFanOut(bothArmsSeed, sqlText); err != nil {
		t.Fatalf("single-arm fan-out against a named database must pass: %v", err)
	}
}

// TestCheckSeedCoversFanOut_IgnoresNonColumnIdentifiers pins the
// identifier classifier: CTE heads, relation operands and AS targets are
// names, not base columns, and must not be demanded of the arms.
func TestCheckSeedCoversFanOut_IgnoresNonColumnIdentifiers(t *testing.T) {
	sqlText := "WITH `roots` AS (SELECT * FROM merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')) " +
		"SELECT `Value` AS `depth` FROM `roots` JOIN `otel_metrics_sum` ON ?"
	if err := CheckSeedCoversFanOut(bothArmsSeed, sqlText); err != nil {
		t.Fatalf("CTE / relation / alias names must not be treated as base columns: %v", err)
	}
}

// TestCheckSeedCoversFanOut_IgnoresStringLiterals pins that a `merge(`
// or a backtick inside a SQL string literal is inert.
func TestCheckSeedCoversFanOut_IgnoresStringLiterals(t *testing.T) {
	sqlText := "SELECT `Value` AS `Value`, 'merge(currentDatabase(), ''^(nope)$'')' AS `lit` " +
		"FROM merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')"
	if err := CheckSeedCoversFanOut(bothArmsSeed, sqlText); err != nil {
		t.Fatalf("string-literal content must not be parsed as SQL: %v", err)
	}
}

// TestCheckSeedCoversFanOut_MergeSuffixIsNotACall pins that an identifier
// merely ending in `merge` — CH has plenty — is not read as the merge()
// table function.
func TestCheckSeedCoversFanOut_MergeSuffixIsNotACall(t *testing.T) {
	sqlText := "SELECT arrayMerge(`Value`) FROM `otel_metrics_gauge`"
	if err := CheckSeedCoversFanOut("", sqlText); err != nil {
		t.Fatalf("arrayMerge() must not be read as merge(): %v", err)
	}
}
