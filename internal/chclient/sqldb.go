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
	opts := buildOptions(cfg)
	// buildOptions never populates Settings — the serving path applies its caps
	// per query in Client.querySettings, which this handle does not go through.
	// Left as-is an offline tool would run UNCAPPED against the deployment it is
	// inspecting: `cerberus audit`'s probes are uniqExact and arrayJoin over a
	// whole retention window, exactly the shape that costs a production server
	// real memory. A read-only tool has no business being the query that
	// destabilises what it came to measure, so the same two caps the server
	// enforces are stamped on the connection here.
	settings := clickhouse.Settings{}
	if cfg.MaxQueryMemoryBytes > 0 {
		settings["max_memory_usage"] = cfg.MaxQueryMemoryBytes
	}
	if cfg.QueryTimeout > 0 {
		settings[settingMaxExecutionTime] = cfg.QueryTimeout.Seconds()
		settings[settingTimeoutOverflowMode] = timeoutOverflowModeThrow
	}
	if len(settings) > 0 {
		opts.Settings = settings
	}

	db := clickhouse.OpenDB(opts)
	if db == nil {
		return nil, fmt.Errorf("chclient: OpenDB returned no handle for %v", cfg.Addrs)
	}
	return db, nil
}
