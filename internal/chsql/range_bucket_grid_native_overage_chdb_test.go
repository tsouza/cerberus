//go:build chdb

// Proves the density guard's rejection carries the numbers it used to decide
// (issue #2678), against a REAL engine rather than a rendered-SQL golden.
//
// A golden would pin the SQL text and say nothing about the thing this feature
// actually depends on: that ClickHouse accepts a COMPUTED throwIf message
// whose operands are scalar subqueries. That is not obvious — function
// arguments are normally eager, so a message rebuilding the guard's own probes
// could plausibly have doubled the probe cost or been rejected outright.
// Executing it settles both, and pins the behaviour against a future engine
// version changing its mind.
package chsql

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chsqltest"
)

func TestDensityGuardRejection_CarriesItsOwnNumbers(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	ctx := context.Background()

	mustExec(t, db, `
CREATE OR REPLACE TABLE hq_overage (
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (Attributes, TimeUnix)`)
	// 4 distinct attribute sets, 40 rows, every row carrying 6 bucket bounds.
	mustExec(t, db, `
INSERT INTO hq_overage
SELECT map('h', toString(number % 4)),
       toDateTime64('2026-01-01 00:00:00', 9) + number,
       arrayMap(x -> toFloat64(x), range(6))
FROM numbers(40)`)

	probe := func(expr Frag, alias string) *QueryBuilder {
		q := NewQuery().From(verbatim("hq_overage"))
		q.Select(As(expr, alias))
		return q
	}
	// A bound of one unit is crossed by any non-empty scan, so the guard fires
	// deterministically and the MESSAGE is what is under test.
	const bound, anchors = 1, 7
	guard := bucketGridDensityGuardFrag(
		probe(Call("uniqExact", Col("Attributes")), "g"),
		probe(Call("count"), "c"),
		probe(Call("max", Call("length", Col("ExplicitBounds"))), "w"),
		anchors, bound, RangeBucketGridNativeDensityBudgetMessage,
	)

	q := NewQuery().From(verbatim("hq_overage"))
	q.Select(Col("Attributes"))
	q.Where(guard)
	b := NewBuilder()
	q.Frag()(b)
	stmt, args, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	if _, qerr := db.QueryContext(ctx, stmt, args...); qerr == nil {
		t.Fatal("the guard did not fire; a one-unit bound must be crossed by any non-empty scan")
	} else {
		msg := qerr.Error()
		// The constant sentinel must survive verbatim: internal/engine's
		// route-outcome classifier matches guard messages by CONTAINMENT, and a
		// rejection it cannot recognise stops driving the failure-driven memo
		// — which would silently strand exactly the queries sharding answers.
		if !strings.Contains(msg, RangeBucketGridNativeDensityBudgetMessage) {
			t.Errorf("message lost its constant sentinel, so the engine can no longer classify\n"+
				"this rejection as a time-sliceable resource bound:\n%s", msg)
		}
		// The decided numbers must be present, which is the point of #2678.
		// width 6 and anchors 7 are the fixture's own facts, so a message that
		// merely echoed the template would fail here.
		for _, want := range []string{"units, bound 1", "7 anchors x bucket width 6"} {
			if !strings.Contains(msg, want) {
				t.Errorf("rejection does not carry %q — an operator still has to re-derive the\n"+
					"cost model by hand against the live table:\n%s", want, msg)
			}
		}
		t.Logf("rejection: %s", msg)
	}
}

func mustExec(t *testing.T, db *sql.DB, stmt string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), stmt); err != nil {
		t.Fatalf("exec %.60s...: %v", stmt, err)
	}
}
