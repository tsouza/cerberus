//go:build chdb

package spec

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

const parityReuseQuery = `SELECT '' AS MetricName, CAST(map(), 'Map(String, String)') AS Attributes, toDateTime64('2026-01-01 00:00:01', 9) AS TimeUnix, toFloat64(pi()) AS Value FROM (SELECT 1)`

func TestRunParityReusesRoundTripWork(t *testing.T) {
	eval := ParityEval{Start: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)}

	t.Run("seed is not applied twice", func(t *testing.T) {
		// ApplySeed deliberately does not rewrite CREATE VIEW. A second seed
		// pass would therefore fail with VIEW_ALREADY_EXISTS, making this a
		// behavioural pin that parity consumes the database RunRoundTripSQL
		// already populated instead of relying on seed idempotency.
		c := loadParityReuseFixture(
			t,
			"CREATE VIEW parity_seed_once AS SELECT 1;",
			parityReuseQuery,
		)
		result := RunRoundTripSQL(t, c, parityReuseQuery, nil)
		RunParity(t, c, eval, result)
	})

	t.Run("projection columns come from the completed round trip", func(t *testing.T) {
		// The fixture's pinned SQL is intentionally executable only as an
		// alarm: the optimized caller-supplied query below produces the same
		// pinned values and column labels. Parity must use the driver columns
		// carried by its RoundTripResult; re-executing this section merely to
		// call rows.Columns would trip throwIf.
		pinnedAlarm := `SELECT throwIf(1, 'projection SQL was re-executed') AS MetricName, CAST(map(), 'Map(String, String)') AS Attributes, toDateTime64('2026-01-01 00:00:01', 9) AS TimeUnix, toFloat64(pi()) AS Value FROM (SELECT 1)`
		c := loadParityReuseFixture(t, "", pinnedAlarm)
		result := RunRoundTripSQL(t, c, parityReuseQuery, nil)
		wantColumns := []string{"MetricName", "Attributes", "TimeUnix", "Value"}
		if !slices.Equal(result.projectionColumns, wantColumns) {
			t.Fatalf("RunRoundTripSQL columns = %v, want %v", result.projectionColumns, wantColumns)
		}
		RunParity(t, c, eval, result)
	})
}

func loadParityReuseFixture(t *testing.T, extraSeed, pinnedSQL string) *Case {
	t.Helper()
	body := `-- query.promql --
pi()
-- parity --
oracle: prometheus
endpoint: /api/v1/query
scope: full
-- seed --
CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
` + extraSeed + `
-- expected_rows --
[["", {}, "2026-01-01T00:00:01Z", 3.141592653589793]]
-- sql --
` + pinnedSQL + "\n"
	path := filepath.Join(t.TempDir(), "parity_reuse.txtar")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}
