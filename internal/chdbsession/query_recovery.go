//go:build chdb

package chdbsession

import (
	"context"
	"database/sql"
	"fmt"
)

// flushStatement is the trivial statement [flushSession] issues after a
// query error. It never reaches chdb-go's Parquet decode path — Exec, not
// Query — which is what makes it safe to run unconditionally regardless of
// what the failed query touched.
const flushStatement = "SELECT 1"

// SafeQuery runs db.QueryContext(ctx, query, args...) with the recovery
// this package's own session-lifecycle ownership implies: this package
// already owns the one place that reasons about chdb-go's process-wide
// cached session ([CloseForExit]'s doc comment), and the corruption SafeQuery
// repairs is a property of that SAME shared session, not of any one
// caller's queries.
//
// cerberus issue #3761 (and its earlier instance, #1917, fixed in
// internal/chclienttest before this package existed): chdb-go v1.12.0 hands
// query results back as Parquet, and a ClickHouse exception on one query
// corrupts the Parquet page index chdb-go decodes for the very NEXT query
// that SUCCEEDS on that same session — parquet-go v0.30.1 panics inside
// NewGenericReader ("missing required field: 1:FIELD<LIST>") instead of
// returning an error. Because chdb-go caches one session per process, this
// reaches every caller of `sql.Open("chdb", "")` in a chdb-tagged binary,
// not only the query pair that first found it.
//
// Two independent layers fix it, mirroring the reasoning
// internal/chclienttest worked out first:
//
//  1. flushSession: issuing a trivial ExecContext (never through the
//     Parquet decode path) on db immediately after ANY query error is what
//     empirically prevents the corruption from reaching the next query —
//     proven by TestSafeQuerySurvivesThrowIfThenSuccess. This is the real
//     fix: it is what makes the next query come back with a genuine
//     result instead of a spurious failure, which a recover() alone cannot
//     do.
//  2. The recover in safeQueryContext converts a parquet-go decode panic
//     into a normal Go error rather than letting it unwind as an
//     unrecovered crash. This is defense-in-depth for any sequence the
//     flush does not fully cover — it guarantees a caller-visible error,
//     never a guarantee of a correct result.
//
// Both are mitigations for a third-party decoder that must not assume its
// input is well-formed; neither suppresses a genuine cerberus bug — a real
// regression in the query under test still surfaces as its own error or
// wrong-shaped response, not as this recovered panic.
//
// Every chdb-tagged caller that opens its own *sql.DB and may run a
// rejected (throwIf) query before a real one should route both through
// SafeQuery rather than calling db.QueryContext / db.Query directly.
func SafeQuery(ctx context.Context, db *sql.DB, query string, args ...any) (*sql.Rows, error) {
	rows, err := safeQueryContext(ctx, db, query, args...)
	if err != nil {
		flushSession(ctx, db)
	}
	return rows, err
}

// safeQueryContext runs db.QueryContext and converts a parquet-go decode
// panic (issue #3761 / #1917) into a plain error. The panic surfaces
// synchronously from inside QueryContext itself (chdb-go's PARQUET driver
// decodes the page index eagerly, before returning rows), so wrapping this
// one call site is sufficient — there is no rows object in flight yet when
// the panic fires.
func safeQueryContext(ctx context.Context, db *sql.DB, query string, args ...any) (rows *sql.Rows, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("chdbsession: recovered chdb/parquet-go decode panic on corrupted "+
				"Parquet page index (see #3761, #1917): %v", r)
		}
	}()
	return db.QueryContext(ctx, query, args...)
}

// flushSession issues a trivial statement on db that never touches the
// Parquet decode path (Exec, not Query). Measured empirically against
// chdb-go v1.12.0 / parquet-go v0.30.1: this is what actually prevents
// issue #3761's corruption from reaching the next query on the same
// session. Best-effort — its own error is discarded because the caller's
// real error from the query that triggered it is already the one being
// returned.
func flushSession(ctx context.Context, db *sql.DB) {
	_, _ = db.ExecContext(ctx, flushStatement)
}
