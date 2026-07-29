//go:build chdb

package optcorpus

import (
	"database/sql"
	"strings"
	"testing"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chsql"
)

// This file pins the ClickHouse SEMANTICS queryLogTypeMember exists to exploit,
// against a real ClickHouse engine, in the required `probe` lane.
//
// The shape assertions in chsource_test.go prove cerberus EMITS
// CAST('<name>', toTypeName(type)) rather than a bare literal. They cannot prove
// the emitted form is the one that matters, because that claim is about how
// ClickHouse resolves a String against an Enum8 — a property of the server, not
// of the SQL text. Without this file, a future simplification back to a bare
// literal would keep the corpus silently blind to exception exits and the only
// test that noticed would be a string comparison an author could equally well
// "fix" by updating the expected SQL. chDB is the real engine, so it answers the
// question the shape test cannot ask.
//
// query_log itself is out of reach here (chDB has no system.query_log), which is
// exactly why the probe below re-declares the column type verbatim: the property
// under test is the Enum8 resolution rule, and it is identical on any column
// carrying that type.

// queryLogTypeMembers is ClickHouse's declaration of system.query_log.type:
// Enum8('QueryStart' = 1, 'QueryFinish' = 2, 'ExceptionBeforeStart' = 3,
// 'ExceptionWhileProcessing' = 4). Values start at 1, not 0.
var queryLogTypeMembers = []chsql.EnumPair{
	{Name: "QueryStart", Value: 1},
	{Name: queryLogFinishMember, Value: 2},
	{Name: "ExceptionBeforeStart", Value: 3},
	{Name: queryLogExceptionMember, Value: 4},
}

// queryLogTypeNonMember is a plausible MISSPELLING of an existing member — the
// mistake this whole mechanism defends against. It is not a member of the enum
// above, and it must never be treated as one.
const queryLogTypeNonMember = "ExceptionWhileProcessed"

// unknownEnumElementError is the fragment ClickHouse error 691
// (UNKNOWN_ELEMENT_OF_ENUM) puts in its message.
const unknownEnumElementError = "UNKNOWN_ELEMENT_OF_ENUM"

// TestQueryLogTypeMemberResolvesAgainstTheColumnType is the engine-level proof
// of the reconciler's read predicate, in three parts:
//
//	bare non-member    — matches zero rows and reports NO error. This is the
//	                     failure mode: a corpus that silently records nothing.
//	CAST non-member    — ClickHouse error 691, naming the members it did find.
//	                     The typo becomes a startup-visible failure.
//	CAST real members  — resolve, and select exactly their own rows.
//
// The middle case is what makes the first survivable: with the CAST form a
// misspelled member cannot be mistaken for an empty result.
func TestQueryLogTypeMemberResolvesAgainstTheColumnType(t *testing.T) {
	db := openQueryLogProbeDB(t)

	t.Run("bare non-member matches nothing and raises nothing", func(t *testing.T) {
		got := countWhere(t, db, chsql.Eq(
			chsql.BareIdent(queryLogTypeColumn),
			chsql.InlineLit(queryLogTypeNonMember),
		))
		if got != 0 {
			t.Fatalf("bare non-member matched %d rows; want 0", got)
		}
	})

	t.Run("CAST non-member is a server error", func(t *testing.T) {
		sqlText, args := probeCountQuery(chsql.Eq(
			chsql.BareIdent(queryLogTypeColumn),
			queryLogTypeMember(queryLogTypeNonMember),
		))
		var n uint64
		err := db.QueryRow(sqlText, args...).Scan(&n)
		if err == nil {
			t.Fatalf("CAST of non-member %q must fail, but matched %d rows", queryLogTypeNonMember, n)
		}
		if !strings.Contains(err.Error(), unknownEnumElementError) {
			t.Fatalf("error must be %s, got: %v", unknownEnumElementError, err)
		}
	})

	for _, member := range []string{queryLogFinishMember, queryLogExceptionMember} {
		t.Run("CAST resolves "+member, func(t *testing.T) {
			got := countWhere(t, db, chsql.Eq(
				chsql.BareIdent(queryLogTypeColumn),
				queryLogTypeMember(member),
			))
			if got != 1 {
				t.Fatalf("CAST of member %q matched %d rows; want 1", member, got)
			}
		})
	}

	t.Run("the reconciler's own IN-list selects both terminal classes", func(t *testing.T) {
		got := countWhere(t, db, chsql.In(
			chsql.BareIdent(queryLogTypeColumn),
			queryLogTypeMember(queryLogFinishMember),
			queryLogTypeMember(queryLogExceptionMember),
		))
		if got != 2 {
			t.Fatalf("terminal IN-list matched %d rows; want 2 (one finish, one exception)", got)
		}
	})
}

// queryLogTypeProbe is the local stand-in for system.query_log: one row per
// member, in a single column named `type` carrying ClickHouse's own
// query_log.type declaration, so queryLogTypeMember's toTypeName(type) resolves
// against exactly the type it resolves against in production. The population is
// a projection rather than a table because the property under test is the type,
// and CAST to the Enum8 gives the column that type without any DDL or storage.
func queryLogTypeProbe() *chsql.QueryBuilder {
	names := make([]chsql.Frag, 0, len(queryLogTypeMembers))
	for _, m := range queryLogTypeMembers {
		names = append(names, chsql.InlineLit(m.Name))
	}
	return chsql.NewQuery().SelectAs(
		chsql.Cast(
			chsql.Call("arrayJoin", chsql.Array(names...)),
			chsql.RenderDDL(chsql.TypeEnum8(queryLogTypeMembers...)),
		),
		queryLogTypeColumn,
	)
}

// probeCountQuery builds SELECT count() FROM <probe> WHERE <pred> through the
// typed builder.
func probeCountQuery(pred chsql.Frag) (string, []any) {
	return chsql.NewQuery().
		From(chsql.Subquery(queryLogTypeProbe())).
		Select(chsql.Call("count")).
		Where(pred).
		Build()
}

// countWhere runs probeCountQuery and fails the test on any error, so the
// callers that expect success stay free of error plumbing.
func countWhere(t *testing.T, db *sql.DB, pred chsql.Frag) uint64 {
	t.Helper()
	sqlText, args := probeCountQuery(pred)
	var n uint64
	if err := db.QueryRow(sqlText, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", sqlText, err)
	}
	return n
}

// openQueryLogProbeDB opens chDB. Every query carries its own population (see
// queryLogTypeProbe), so a predicate's match count is the number of members it
// resolved.
func openQueryLogProbeDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("ping chdb: %v", err)
	}
	return db
}
