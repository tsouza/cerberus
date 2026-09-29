//go:build chdb

package chdbsession_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver" // registers "chdb" sql driver

	"github.com/tsouza/cerberus/internal/chdbsession"
)

// TestSafeQuerySurvivesThrowIfThenSuccess is the isolated regression for
// cerberus issue #3761: on a chdb-go session, a query rejected via
// `throwIf` immediately followed by a query that succeeds and returns
// Parquet-encoded results used to crash parquet-go's NewGenericReader
// while decoding the SUCCESSFUL query's page index — not the rejected
// one's. This reproduces the issue's own minimal repro (`SELECT
// throwIf(1)` then `SELECT count() FROM numbers(1)`) through
// [chdbsession.SafeQuery] and asserts the second query comes back with its
// real row, not merely "does not panic".
func TestSafeQuerySurvivesThrowIfThenSuccess(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	guardRows, err := chdbsession.SafeQuery(ctx, db, "SELECT throwIf(1)")
	if guardRows != nil {
		_ = guardRows.Close()
	}
	if err == nil {
		t.Fatal("guard query: expected a ClickHouse exception, got nil error")
	}

	// The corruption in #3761 hits the very next query on the SAME
	// session — this must come back as a genuine success with its real
	// row, not a parquet-go panic and not a spurious error.
	rows, err := chdbsession.SafeQuery(ctx, db, "SELECT count() FROM numbers(1)")
	if err != nil {
		t.Fatalf("count query after a prior throwIf rejection: got error %v, want success", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			t.Fatalf("count query: rows.Err: %v", err)
		}
		t.Fatal("count query returned no rows")
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count(): %v", err)
	}
	if n != 1 {
		t.Fatalf("count() = %d, want 1", n)
	}
}

// TestSafeQuerySurvivesRepeatedThrowIfThenSuccess pins the same behaviour
// across repeated rejection/success alternation on one session, since a
// fix that only accounts for a single rejection would still leave later
// alternations vulnerable.
func TestSafeQuerySurvivesRepeatedThrowIfThenSuccess(t *testing.T) {
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	for i := range 3 {
		guardRows, err := chdbsession.SafeQuery(ctx, db, "SELECT throwIf(1)")
		if guardRows != nil {
			_ = guardRows.Close()
		}
		if err == nil {
			t.Fatalf("round %d: guard query: expected a ClickHouse exception, got nil error", i)
		}

		rows, err := chdbsession.SafeQuery(ctx, db, "SELECT count() FROM numbers(1)")
		if err != nil {
			t.Fatalf("round %d: count query after a prior throwIf rejection: got error %v, want success", i, err)
		}
		if !rows.Next() {
			t.Fatalf("round %d: count query returned no rows", i)
		}
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("round %d: scan count(): %v", i, err)
		}
		_ = rows.Close()
		if n != 1 {
			t.Fatalf("round %d: count() = %d, want 1", i, n)
		}
	}
}
