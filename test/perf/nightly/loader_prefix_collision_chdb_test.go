//go:build chdb

// The PREFIX-COLLISION pin for the nightly loader's tuple-field access
// (#2875).
//
// # What broke
//
// testdata/samples/*.parquet's ResourceAttributes tuple carries BOTH
// "deployment.environment" and "deployment.environment.name" — the OTel
// semantic-convention name the captured production workload emitted and its
// successor, emitted side by side during the convention's migration window
// (see sampleResourceAttributeKeys). loader.go read every tuple field by
// backtick-quoted dot access, which is ambiguous for exactly that pair:
// reading from the file() table function, ClickHouse pushes the requested
// column paths down into the format reader, and from 26.x that pushdown
// splits the requested ResourceAttributes.deployment.environment.name at
// the longest matching column prefix, asks Parquet for
// ResourceAttributes.deployment.environment (a String), and raises
//
//	Code: 10. Not found column or subcolumn
//	ResourceAttributes.deployment.environment.name in block.
//
// perf-nightly's own harness pins ClickHouse 25.9, which resolved that dot
// access correctly — so only the ts_grid_instant memory measurement, whose
// feature floor sits above 25.9 and which therefore boots its own 26.6
// container, went red. The lane's merge_posture is "never"
// (.github/ci-lanes.json), so no PR ran it and the break landed on main.
//
// # Why this pin, on this substrate
//
// The break is a fact about how a real engine resolves the loader's real
// SELECT list against a real Parquet source — not something a string
// assertion can settle. chDB (26.5) reproduces the collapse exactly, so
// this test rebuilds the hazard from first principles on it: write a
// Parquet whose ResourceAttributes tuple carries sampleResourceAttributeKeys
// verbatim (prefix pair and all), read it back through the SELECT list
// mapArraysExpr itself renders, and require every key to come back with its
// OWN distinct value. Restore dot access in mapArraysExpr and this fails
// with the CI error above; delete either half of the prefix pair and
// TestSampleResourceAttributeKeys_KeepPrefixCollision fails instead.
//
// It lives in this package rather than test/regression because the
// behaviour under test is the unexported renderer's own output, and because
// test/perf/** is already enrolled in the chdb.perf-guards lane
// (.github/ci-lanes.json) that `just perf-chdb` drives.
package nightly

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"
)

// prefixCollisionSeedPrefix marks each seeded field value so a value read
// back under the wrong key is identifiable on sight rather than merely
// unequal. The suffix is the key itself, which makes every field's value
// unique by construction — that uniqueness is what turns "each key resolved
// to its own column" into a checkable assertion.
const prefixCollisionSeedPrefix = "seed:"

func prefixCollisionSeedValue(key string) string { return prefixCollisionSeedPrefix + key }

// TestMapArraysExpr_ResolvesPrefixCollidingParquetFields is the #2875 pin.
func TestMapArraysExpr_ResolvesPrefixCollidingParquetFields(t *testing.T) {
	keys := sampleResourceAttributeKeys

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}

	fields := make([]string, len(keys))
	values := make([]string, len(keys))
	for i, k := range keys {
		fields[i] = fmt.Sprintf("`%s` String", k)
		values[i] = fmt.Sprintf("'%s'", prefixCollisionSeedValue(k))
	}
	tupleType := "Tuple(" + strings.Join(fields, ", ") + ")"

	parquetPath := filepath.Join(t.TempDir(), "resource_attributes.parquet")
	writeSQL := fmt.Sprintf(
		"INSERT INTO FUNCTION file('%s', Parquet, 'ResourceAttributes %s') SELECT CAST((%s), '%s')",
		parquetPath, tupleType, strings.Join(values, ", "), tupleType,
	)
	if _, err := db.Exec(writeSQL); err != nil {
		t.Fatalf("write parquet fixture: %v", err)
	}

	// Read every key back through the loader's OWN map expression, one
	// element access per key, so a collapsed pair shows up as the wrong
	// value rather than as a silently missing map entry.
	mapExpr := mapArraysExpr("ResourceAttributes", keys)
	projections := make([]string, len(keys))
	for i, k := range keys {
		projections[i] = fmt.Sprintf("(%s)['%s']", mapExpr, k)
	}
	readSQL := fmt.Sprintf("SELECT %s FROM file('%s', Parquet)",
		strings.Join(projections, ", "), parquetPath)

	got := make([]string, len(keys))
	dest := make([]any, len(keys))
	for i := range got {
		dest[i] = &got[i]
	}
	if err := db.QueryRow(readSQL).Scan(dest...); err != nil {
		t.Fatalf("read parquet fixture back through mapArraysExpr: %v\nSQL: %s", err, readSQL)
	}

	for i, k := range keys {
		if want := prefixCollisionSeedValue(k); got[i] != want {
			t.Errorf("key %q resolved to %q, want %q — a tuple field read the wrong column", k, got[i], want)
		}
	}
}
