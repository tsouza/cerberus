package chclient

import (
	"errors"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// chCodeUnknownTable is ClickHouse's UNKNOWN_TABLE server error code
// (ErrorCodes.cpp: 60), raised as "Table <db>.<table> doesn't exist." A
// query that names a table this deployment never provisioned (most often
// because it doesn't ingest that signal — see the Prom metadata surface's
// per-metric-type table union, #1949) or a table dropped externally after
// boot both surface here.
const chCodeUnknownTable = 60

// IsUnknownTable reports whether err is ClickHouse's UNKNOWN_TABLE
// rejection (code 60) anywhere in its chain. Unlike a memory-limit or
// timeout rejection this is not a resource-exhaustion signal — the table
// named in the query simply does not exist — so a caller that unions
// across a configured-but-optional set of tables (a deployment that does
// not ingest every telemetry signal) can catch it and degrade to "no rows
// from that table" instead of failing the whole request with a generic
// 502.
//
// Detection is typed first — errors.As against *clickhouse.Exception,
// mirroring isDatabaseAbsent / isMemoryLimitExceeded / IsQueryTimeout — with
// a narrow string-matching fallback for the case where the driver wraps the
// exception opaquely enough to defeat errors.As. The phrases are
// deliberately narrow to the UNKNOWN_TABLE vocabulary so a successful query
// whose result data merely mentions a table name is never misclassified.
func IsUnknownTable(err error) bool {
	if err == nil {
		return false
	}
	var ex *clickhouse.Exception
	if errors.As(err, &ex) && ex.Code == chCodeUnknownTable {
		return true
	}
	return matchesUnknownTablePhrase(err)
}

// matchesUnknownTablePhrase is the string-matching fallback for
// IsUnknownTable. "unknown_table" is the distinctive code name the server
// appends; the "table … doesn't exist" pair covers a wrapper that drops the
// code name but keeps the message. ClickHouse phrases this rejection with
// the contraction "doesn't exist" (as opposed to UNKNOWN_DATABASE's "does
// not exist"), so the two never collide.
func matchesUnknownTablePhrase(err error) bool {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "unknown_table") {
		return true
	}
	return strings.Contains(msg, "table") && strings.Contains(msg, "doesn't exist")
}
