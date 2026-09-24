package chclient

import (
	"strings"
	"testing"
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
// only (a remote child's row is a fragment of the initiator's totals), one
// row per (hostname, query_id), a strict cursor over the total order, the
// settle horizon and the start-time window.
func TestQueryLogActualsSQL_SelectsInitiatorFinishRows(t *testing.T) {
	for _, want := range []string{
		"type = 'QueryFinish'",
		"is_initial_query = 1",
		"(toUnixTimestamp64Micro(event_time_microseconds), hostname, query_id) > (?, ?, ?)",
		"event_time_microseconds <= now64(6) - toIntervalMillisecond(?)",
		"query_start_time_microseconds >= now64(6) - toIntervalMillisecond(?)",
		"ORDER BY event_time_microseconds, hostname, query_id",
		"LIMIT 1 BY hostname, query_id",
	} {
		if !strings.Contains(queryLogActualsLocalSQL, want) {
			t.Errorf("record selection lacks %q:\n%s", want, queryLogActualsLocalSQL)
		}
	}
}
