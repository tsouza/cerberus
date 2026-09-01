package chclient

import (
	"context"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
)

// external_trace_ids.go closes the chclient half of cerberus issue #2783: the
// native-protocol external-table push that replaces the phase-B literal
// TraceId IN-list splice (internal/api/tempo/structural_two_phase.go's
// restrictStructural) on wide closures. internal/api/tempo is the other
// half — it decides WHEN to switch (the literal splice's estimated byte cost
// against a threshold) and names the table on the emitted plan
// (chplan.StructuralJoin.TraceIDExternalTable); this file only carries the
// VALUES onto the wire.
//
// externalTraceIDColumnType is String because every on-disk TraceId column
// cerberus reads (schema.Traces.TraceIDColumn) is String — the external
// table's single column matches it exactly so the `IN (SELECT … FROM …)`
// comparison chsql.inExternalTraceIDsFrag emits needs no cast.
const externalTraceIDColumnType = "String"

// WithExternalTraceIDs returns ctx carrying a native-protocol external
// (temporary) table named table, with one column named column of type
// String, holding ids — the values a chplan.StructuralJoin.TraceIDExternalTable
// reference resolves against. Every Client method that derives its dispatch
// context through queryContext (Query, QueryStrings, and the rest) picks it
// up automatically: clickhouse.Context/queryOptions merge with whatever is
// already on ctx (clickhouse-go/v2's own contract — see context.go's
// Context()), so stacking this ahead of a Client call composes with the
// settings/query-id options queryContext adds on top, in either order.
//
// Lifetime and safety:
//
//   - Per-query, like a bound parameter. The driver serialises the table's
//     rows onto the wire as part of the ONE query dispatch that carries this
//     ctx (conn_send_query.go's sendQuery: the external tables ride the same
//     packet as the query body, before the empty end-of-data block). Building
//     the *ext.Table below never mutates ids, and encoding a table's block does
//     not drain it — clickhouse-go's own external-table example queries the
//     same *ext.Table more than once off one build — so calling this again on
//     a fresh ctx for a retry, or reusing the returned ctx across several
//     route-B shard dispatches, resends the SAME complete row set every time;
//     nothing carries over silently and nothing is short a row.
//   - Query-scoped on the SERVER side too: ClickHouse tears the backing
//     temporary storage down once the declaring query finishes, so two
//     concurrent dispatches attaching the SAME table name (this package
//     always uses one fixed name, tempo.externalTraceIDTableName) never
//     collide — there is no cross-request registry to clash in.
//   - Native-protocol only. The caller (internal/api/tempo) gates this on
//     Config.Protocol == clickhouse.Native at boot (cmd/cerberus/main.go),
//     mirroring the issue's own scope: HTTP-protocol external data support is
//     out of scope for #2783 and untested here.
//
// ids are sent as-is with no further validation — the caller is responsible
// for handing the exact on-disk TraceId values a literal splice would
// otherwise carry (internal/api/tempo.padTraceIDs is cerberus's canonical
// normalizer for that).
func WithExternalTraceIDs(ctx context.Context, table, column string, ids []string) (context.Context, error) {
	t, err := ext.NewTable(table, ext.Column(column, externalTraceIDColumnType))
	if err != nil {
		return ctx, fmt.Errorf("chclient: external trace-id table %q: %w", table, err)
	}
	for _, id := range ids {
		if err := t.Append(id); err != nil {
			return ctx, fmt.Errorf("chclient: external trace-id table %q: append: %w", table, err)
		}
	}
	return clickhouse.Context(ctx, clickhouse.WithExternalTable(t)), nil
}
