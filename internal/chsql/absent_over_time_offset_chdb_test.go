//go:build chdb

// chDB-backed proof that range-mode absent_over_time reports its per-anchor
// output timestamp on the UNSHIFTED request grid, independent of the PromQL
// `offset` modifier. `offset` shifts only WHICH samples the lookback window
// reads (the membership base), never the eval timestamp the result is stamped
// at — same invariant range_window's gridAnchorFrag fix enforces and the
// PromQL oracle implements.
//
// The metric queried is DELIBERATELY absent at every anchor (the seed holds a
// different series), so every grid anchor survives the NOT-IN anti-filter and
// the emitted TimeUnix set is exactly the eval grid [Start, End]. The offset
// and no-offset arms must therefore emit the IDENTICAL anchor-timestamp set. A
// buggy emitter that stamps the offset-shifted base emits t-offset instead.
package chsql_test

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const absentOffsetSeed = `
CREATE OR REPLACE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String) DEFAULT map(),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree ORDER BY (MetricName, Attributes, TimeUnix);
INSERT INTO otel_metrics_gauge (MetricName, Attributes, TimeUnix, Value) VALUES
    ('present', map('job', 'api'), toDateTime64('2026-01-01 00:03:00', 9), 1.0);
`

func TestAbsentOverTime_OffsetOutputGrid(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	for _, stmt := range splitSeedStatements(absentOffsetSeed) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n--- stmt ---\n%s", err, stmt)
		}
	}

	noOffset := runAbsentEmit(t, db, `absent_over_time(missing[5m])`)
	withOffset := runAbsentEmit(t, db, `absent_over_time(missing[5m] offset 5m)`)

	if len(noOffset) == 0 {
		t.Fatalf("no-offset arm produced zero anchors — fixture must yield a fully-absent grid")
	}
	if len(withOffset) != len(noOffset) {
		t.Fatalf("anchor-count divergence: no-offset=%d with-offset=%d\nno-offset=%v\nwith-offset=%v",
			len(noOffset), len(withOffset), noOffset, withOffset)
	}
	for i := range noOffset {
		if !withOffset[i].Equal(noOffset[i]) {
			t.Errorf("anchor[%d]: offset arm reported %s but the UNSHIFTED grid anchor is %s — "+
				"offset must not move the output timestamp",
				i, withOffset[i].Format(time.RFC3339), noOffset[i].Format(time.RFC3339))
		}
	}
	t.Logf("absent_over_time offset grid: %d anchors, offset and no-offset arms report identical timestamps",
		len(noOffset))
}

func runAbsentEmit(t *testing.T, db *sql.DB, query string) []time.Time {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, time.Minute)
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit %q: %v", query, err)
	}
	wrapped := "SELECT `TimeUnix` FROM (" + sqlStr + ") ORDER BY `TimeUnix`"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query %q: %v\nSQL: %s", query, err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	var out []time.Time
	for rows.Next() {
		var ts time.Time
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, ts.UTC())
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err %q: %v", query, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}
