package tempo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// unwindowedRecursiveSpansSQL is the OOM bypass shape the partition-pruning
// guard rejects: the statement carries a request window (on the seed), so the
// matcher's window precondition is met, but the recursive `FROM otel_traces AS
// t` arm has no co-scope Timestamp predicate, so it reads full retention.
const unwindowedRecursiveSpansSQL = "WITH RECURSIVE c AS (" +
	"SELECT `TraceId`, `SpanId`, `ParentSpanId`, 0 AS _depth " +
	"FROM (SELECT * FROM `otel_traces` WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000)) AS _seed " +
	"UNION ALL " +
	"SELECT t.`TraceId`, t.`SpanId`, t.`ParentSpanId`, c._depth + 1 " +
	"FROM `otel_traces` AS t INNER JOIN c ON t.`TraceId` = c.`TraceId` AND t.`ParentSpanId` = c.`SpanId` " +
	"WHERE c._depth < 128" +
	") SELECT `TraceId`, `SpanId` FROM c WHERE _depth > 0"

// windowedSpansSQL is the negative control: a Timestamp range sitting directly
// on the otel_traces scan prunes partitions, so the guard passes it through.
const windowedSpansSQL = "SELECT DISTINCT arrayJoin(mapKeys(`SpanAttributes`)) AS `tag` " +
	"FROM `otel_traces` " +
	"WHERE `Timestamp` >= fromUnixTimestamp64Nano(1782571392000000000) AND `Timestamp` <= fromUnixTimestamp64Nano(1782573192000000000)"

// recordingQuerier records whether the inner querier was reached, so the
// bypass-audit tests can assert the guard fires BEFORE delegation.
type recordingQuerier struct{ reached bool }

func (r *recordingQuerier) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	r.reached = true
	return nil, nil
}

func (r *recordingQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	r.reached = true
	return nil, nil
}

// TestNewWrapsClientWithSpansScanGuard is the bypass-audit: tempo.New must
// install the guarded wrapper on Handler.Client so EVERY spans-SQL string the
// handlers execute directly (search/tags, search/tag/*/values, root lookup,
// exemplars) passes the partition-pruning guard by construction. Both Query and
// QueryStrings are covered, and the inner querier is never reached for an
// unwindowed recursive scan.
func TestNewWrapsClientWithSpansScanGuard(t *testing.T) {
	t.Parallel()

	t.Run("QueryStrings_rejects_unwindowed", func(t *testing.T) {
		t.Parallel()
		inner := &recordingQuerier{}
		h := tempo.New(inner, schema.DefaultOTelTraces(), "v", nil)
		_, err := h.Client.QueryStrings(context.Background(), unwindowedRecursiveSpansSQL)
		if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
			t.Fatalf("expected ErrUnboundedSpansScan, got %v", err)
		}
		if inner.reached {
			t.Fatalf("guard bypassed: inner querier was reached for an unwindowed recursive spans scan")
		}
	})

	t.Run("Query_rejects_unwindowed", func(t *testing.T) {
		t.Parallel()
		inner := &recordingQuerier{}
		h := tempo.New(inner, schema.DefaultOTelTraces(), "v", nil)
		_, err := h.Client.Query(context.Background(), unwindowedRecursiveSpansSQL)
		if !errors.Is(err, chsql.ErrUnboundedSpansScan) {
			t.Fatalf("expected ErrUnboundedSpansScan, got %v", err)
		}
		if inner.reached {
			t.Fatalf("guard bypassed: inner querier was reached for an unwindowed recursive spans scan")
		}
	})

	t.Run("windowed_passes_through", func(t *testing.T) {
		t.Parallel()
		inner := &recordingQuerier{}
		h := tempo.New(inner, schema.DefaultOTelTraces(), "v", nil)
		if _, err := h.Client.QueryStrings(context.Background(), windowedSpansSQL); err != nil {
			t.Fatalf("windowed spans SQL must pass through, got %v", err)
		}
		if !inner.reached {
			t.Fatalf("windowed spans SQL did not reach the inner querier — guard false-positive")
		}
	})
}
