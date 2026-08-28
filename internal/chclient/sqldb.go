package chclient

import (
	"database/sql"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// OpenSQLDB opens a `database/sql` handle against the same resolved
// clickhouse.Options New would dial with — the identical address list, auth,
// protocol, TLS and timeout mapping, via the same buildOptions.
//
// It exists for the read-only OFFLINE tools (`cerberus audit`), not for the
// serving path. Those tools want plain `*sql.Rows` scanning rather than the
// Client's cursor API, and want their own short-lived pool so a long
// cardinality probe cannot occupy connections the server is using to answer
// queries. The serving path keeps Client, which carries the circuit breaker,
// the per-head budgets and the query-settings plumbing this handle
// deliberately does NOT have — which is also why nothing here should grow a
// write path.
//
// clickhouse.OpenDB, like clickhouse.Open, is lazy: it never dials until the
// first query, matching every other CLI-driven ClickHouse connection in this
// codebase.
func OpenSQLDB(cfg Config) (*sql.DB, error) {
	db := clickhouse.OpenDB(buildOptions(cfg))
	if db == nil {
		return nil, fmt.Errorf("chclient: OpenDB returned no handle for %v", cfg.Addrs)
	}
	return db, nil
}
