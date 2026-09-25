package chclient

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// TestQueryLogActualsSQL_DifferOnlyInTable pins the two record-selection
// statements to one another: they must read different tables and be
// byte-identical everywhere else, so a selection fix can never land on one
// source and miss the other.
func TestQueryLogActualsSQL_DifferOnlyInTable(t *testing.T) {
	const localFrom, unionFrom = "FROM system.query_log\n", "FROM system.all_query_log\n"
	if !strings.Contains(queryLogActualsLocalSQL, localFrom) {
		t.Fatalf("local statement does not read system.query_log:\n%s", queryLogActualsLocalSQL)
	}
	if !strings.Contains(queryLogActualsUnionSQL, unionFrom) {
		t.Fatalf("union statement does not read system.all_query_log:\n%s", queryLogActualsUnionSQL)
	}
	if got := strings.Replace(queryLogActualsUnionSQL, unionFrom, localFrom, 1); got != queryLogActualsLocalSQL {
		t.Errorf("the statements differ beyond the table:\nlocal:\n%s\nunion:\n%s", queryLogActualsLocalSQL, queryLogActualsUnionSQL)
	}
}

// TestQueryLogActualsSQL_SelectsInitiatorFinishRows pins the record-selection
// predicates the accounting depends on: finished queries only, initiators
// only (a remote child's row is a fragment of the initiator's totals), never
// the HTTP transport's connection hello, one row per (hostname, query_id), a
// strict cursor over the total order and the settle horizon.
func TestQueryLogActualsSQL_SelectsInitiatorFinishRows(t *testing.T) {
	for _, want := range []string{
		"type = 'QueryFinish'",
		"is_initial_query = 1",
		"query != ?",
		"(toUnixTimestamp64Micro(event_time_microseconds), hostname, query_id) > (?, ?, ?)",
		"event_time_microseconds <= now64(6) - toIntervalMillisecond(?)",
		"ORDER BY event_time_microseconds, hostname, query_id",
		"LIMIT 1 BY hostname, query_id",
	} {
		if !strings.Contains(queryLogActualsLocalSQL, want) {
			t.Errorf("record selection lacks %q:\n%s", want, queryLogActualsLocalSQL)
		}
	}
}

// TestIsQueryLogUnionRefusal pins which failures move the reconciler onto the
// local log. Only a server answer that the union is not provisioned for this
// reader does; a transport failure, or a server error about one read (a
// timeout, a memory limit), must leave the cursor for a retry — falling back
// would move it past rows only the union holds.
func TestIsQueryLogUnionRefusal(t *testing.T) {
	const (
		timeoutExceeded     = 159
		memoryLimitExceeded = 241
	)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"unknown table", &clickhouse.Exception{Code: chCodeUnknownTable}, true},
		{"access denied", &clickhouse.Exception{Code: chCodeAccessDenied}, true},
		{"unknown identifier", &clickhouse.Exception{Code: chCodeUnknownIdentifier}, true},
		{"no such column", &clickhouse.Exception{Code: chCodeNoSuchColumnInTable}, true},
		{"wrapped unknown table", fmt.Errorf("read: %w", &clickhouse.Exception{Code: chCodeUnknownTable}), true},
		{"timeout exceeded", &clickhouse.Exception{Code: timeoutExceeded}, false},
		{"memory limit exceeded", &clickhouse.Exception{Code: memoryLimitExceeded}, false},
		{"dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isQueryLogUnionRefusal(tc.err); got != tc.want {
				t.Fatalf("isQueryLogUnionRefusal(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestQueryLogActualsArgs_BindEveryPlaceholder pins that each placeholder the
// record selection filters on receives the value it is written for — above
// all that the HTTP connection hello is what `query != ?` excludes, and the
// shape prefix what log_comment must start with — and that the statement has
// exactly as many placeholders as bound values.
func TestQueryLogActualsArgs_BindEveryPlaceholder(t *testing.T) {
	req := QueryLogActualsRequest{
		After:         QueryLogCursor{EventTime: time.UnixMicro(1_790_000_000_123_456), Hostname: "ch-0", QueryID: "q"},
		SettleDelay:   15 * time.Second,
		ShapeIDPrefix: "cerb:",
		Limit:         1000,
	}
	args := queryLogActualsArgs(req)
	for _, sql := range []string{queryLogActualsLocalSQL, queryLogActualsUnionSQL} {
		if got := strings.Count(sql, "?"); got != len(args) {
			t.Fatalf("statement has %d placeholders, %d values are bound:\n%s", got, len(args), sql)
		}
		for _, tc := range []struct {
			clause string
			want   any
		}{
			{"startsWith(log_comment, ?)", "cerb:"},
			{"query != ?", HTTPConnectionHelloQuery},
			{"toIntervalMillisecond(?)", int64(15_000)},
			{"LIMIT ?", 1000},
		} {
			at := strings.Index(sql, tc.clause)
			if at < 0 {
				t.Fatalf("statement lacks %q:\n%s", tc.clause, sql)
			}
			position := strings.Count(sql[:at], "?")
			if got := args[position]; got != tc.want {
				t.Errorf("%q binds %#v, want %#v", tc.clause, got, tc.want)
			}
		}
	}
}
