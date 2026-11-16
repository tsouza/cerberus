package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmitSearchTraceLimit_RequiresBothInputRoles pins that both named child-schema roles are required.
func TestEmitSearchTraceLimit_RequiresBothInputRoles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		traceID   string
		timestamp string
		wantErr   bool
	}{
		{name: "both present", traceID: "TraceId", timestamp: "Timestamp", wantErr: false},
		{name: "timestamp unset", traceID: "TraceId", timestamp: "", wantErr: true},
		{name: "trace id unset", traceID: "", timestamp: "Timestamp", wantErr: true},
		{name: "both unset", traceID: "", timestamp: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := &chplan.SearchTraceLimit{
				Input:      &chplan.Scan{Table: "otel_traces", Columns: []string{tc.traceID, tc.timestamp}, Roles: []chplan.Column{{Name: tc.traceID, Role: chplan.RoleTraceID}, {Name: tc.timestamp, Role: chplan.RoleTimestamp}}},
				TraceLimit: 7,
			}
			sql, _, err := Emit(context.Background(), n)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("Emit err = %v, want ErrUnsupported (SQL: %s)", err, sql)
				}
				return
			}
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
		})
	}
}

// searchTraceLimitRowSource is a closed spans row source whose trace-id and
// timestamp drivers are named traceID and timestamp, filtered to SpanName =
// name so two sources render apart.
func searchTraceLimitRowSource(traceID, timestamp, name string) chplan.Node {
	return &chplan.Filter{
		Input: &chplan.Scan{
			Table:   "otel_traces",
			Columns: []string{traceID, timestamp, "SpanName"},
			Roles:   []chplan.Column{{Name: traceID, Role: chplan.RoleTraceID}, {Name: timestamp, Role: chplan.RoleTimestamp}},
		},
		Predicate: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "SpanName"}, Right: &chplan.LitString{V: name}},
	}
}

// TestEmitSearchTraceLimit_RankRendersTheRanking pins that a SearchTraceLimit
// with a Rank ranks its traces over Rank and drains them from Input: the outer
// SELECT reads Input, the top-N subquery reads Rank.
func TestEmitSearchTraceLimit_RankRendersTheRanking(t *testing.T) {
	t.Parallel()
	n := &chplan.SearchTraceLimit{
		Input:      searchTraceLimitRowSource("TraceId", "Timestamp", "drain"),
		Rank:       searchTraceLimitRowSource("TraceId", "Timestamp", "rank"),
		TraceLimit: 7,
	}
	sql, args, err := Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	ranking := strings.Index(sql, "GLOBAL IN (")
	if ranking < 0 {
		t.Fatalf("no top-N subquery in %s", sql)
	}
	if len(args) != 2 || args[0] != "drain" || args[1] != "rank" {
		t.Fatalf("args = %v, want [drain rank]: the outer drain reads Input, the ranking reads Rank (SQL: %s)", args, sql)
	}
	if strings.Count(sql[:ranking], "?") != 1 || strings.Count(sql[ranking:], "?") != 1 {
		t.Fatalf("drain / rank predicates not split around the top-N subquery: %s", sql)
	}
}

// TestEmitSearchTraceLimit_RankDriverMismatch pins that a Rank whose trace-id
// or timestamp driver is named differently from Input's is rejected: the
// ranking subquery selects and orders by Input's driver names.
func TestEmitSearchTraceLimit_RankDriverMismatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		traceID, timestamp string
		wantErr            bool
	}{
		{name: "same drivers", traceID: "TraceId", timestamp: "Timestamp"},
		{name: "trace id differs", traceID: "Tid", timestamp: "Timestamp", wantErr: true},
		{name: "timestamp differs", traceID: "TraceId", timestamp: "Ts", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := &chplan.SearchTraceLimit{
				Input:      searchTraceLimitRowSource("TraceId", "Timestamp", "drain"),
				Rank:       searchTraceLimitRowSource(tc.traceID, tc.timestamp, "rank"),
				TraceLimit: 7,
			}
			sql, _, err := Emit(context.Background(), n)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Emit: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("Emit err = %v, want ErrUnsupported (SQL: %s)", err, sql)
			}
		})
	}
}
