//go:build chdb

package chclienttest

import (
	"context"
	"testing"
)

// sessionRecoverySeedDDL mirrors the production Map-column projection
// shape (an `Attributes` column, one of the names testsql.IsMapColumn
// recognises) so the query below rides the same toJSONString(...)
// rewrite production queries do, rather than an unwrapped Map column —
// which chdb-go's parquet driver cannot decode under any circumstance
// (a separate, pre-existing, unconditional bug) and would make this
// test conflate two different failure modes.
const sessionRecoverySeedDDL = `CREATE TABLE session_recovery_probe (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = Memory;
INSERT INTO session_recovery_probe VALUES ('probe', map('k', 'v'), now64(9), 1.0);`

const sessionRecoveryPlainQuery = `SELECT MetricName, Attributes, TimeUnix, Value FROM session_recovery_probe`

// sessionRecoveryBoomQuery raises a ClickHouse exception via throwIf —
// the same guard idiom internal/promql's duplicate-labelset and
// many-to-many guards plant in real emitted SQL (see
// internal/promql/duplicate_labelset_guard.go, internal/promql/info_fn.go).
const sessionRecoveryBoomQuery = sessionRecoveryPlainQuery + ` WHERE throwIf(1 = 1, 'boom') = 0`

// TestSessionSurvivesErrorThenSuccess is the regression test for issue
// #1917: a chDB session that raised a ClickHouse exception used to
// corrupt the Parquet page index of the NEXT query's result set,
// crashing parquet-go's NewGenericReader with a panic instead of
// returning an error. Left unrecovered, that panic propagates through
// a handler under test as a spurious 500 — indistinguishable from a
// real cerberus regression — even though the query that panicked was
// syntactically and semantically fine on its own.
//
// This runs the exact boom-then-plain sequence on ONE session (no
// second Client, no fresh session) and asserts the second query
// SUCCEEDS with its real row back — not merely "does not panic". A
// bare recover() that converted the panic into an error would satisfy
// a weaker test but not this one: it would still leave the plain query
// spuriously failing. The actual fix is queryContext's flush-after-error
// (see client.go), which keeps the session itself usable; the recover
// in safeQueryContext is the defense-in-depth layer that guarantees the
// failure mode is at worst a Go error, never an unrecovered panic,
// for any case the flush doesn't fully cover.
func TestSessionSurvivesErrorThenSuccess(t *testing.T) {
	c := NewChDB(t)
	ctx := context.Background()
	c.Seed(t, sessionRecoverySeedDDL)

	if _, err := c.Query(ctx, sessionRecoveryBoomQuery); err == nil {
		t.Fatalf("boom query: expected a ClickHouse exception, got nil error")
	}

	// The corruption in #1917 hits the very next query on the SAME
	// session. This must come back as a genuine success, not a panic
	// and not a spurious error — that's the whole point of the fix.
	rows, err := c.Query(ctx, sessionRecoveryPlainQuery)
	if err != nil {
		t.Fatalf("plain query after a prior exception on the same session: got error %v, want success", err)
	}
	if len(rows) != 1 {
		t.Fatalf("plain query after a prior exception: got %d rows, want 1", len(rows))
	}
	if got := rows[0].MetricName; got != "probe" {
		t.Errorf("plain query row: MetricName = %q, want %q", got, "probe")
	}
	if got := rows[0].Labels["k"]; got != "v" {
		t.Errorf("plain query row: Attributes[%q] = %q, want %q", "k", got, "v")
	}

	// A third query on the same session, past the immediately-next one,
	// must also stay healthy — the fix must not merely paper over the
	// FIRST post-exception query.
	rows2, err := c.Query(ctx, sessionRecoveryPlainQuery)
	if err != nil {
		t.Fatalf("second plain query after a prior exception: got error %v, want success", err)
	}
	if len(rows2) != 1 {
		t.Fatalf("second plain query after a prior exception: got %d rows, want 1", len(rows2))
	}
}

// TestSessionSurvivesRepeatedErrorThenSuccess pins the same behaviour
// across repeated error/success alternation on one session, since a
// fix that only accounts for a single error would still leave later
// alternations vulnerable.
func TestSessionSurvivesRepeatedErrorThenSuccess(t *testing.T) {
	c := NewChDB(t)
	ctx := context.Background()
	c.Seed(t, sessionRecoverySeedDDL)

	for i := 0; i < 3; i++ {
		if _, err := c.Query(ctx, sessionRecoveryBoomQuery); err == nil {
			t.Fatalf("round %d: boom query: expected a ClickHouse exception, got nil error", i)
		}
		rows, err := c.Query(ctx, sessionRecoveryPlainQuery)
		if err != nil {
			t.Fatalf("round %d: plain query after a prior exception: got error %v, want success", i, err)
		}
		if len(rows) != 1 {
			t.Fatalf("round %d: plain query after a prior exception: got %d rows, want 1", i, len(rows))
		}
	}
}
