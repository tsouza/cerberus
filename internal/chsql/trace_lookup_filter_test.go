package chsql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func spansWithTraceRole() chplan.Node {
	return &chplan.Scan{Table: "otel_traces", Roles: []chplan.Column{{Name: "TraceId", Role: chplan.RoleTraceID}}}
}

func lookupRelation(valueAlias string) chplan.Node {
	return &chplan.Aggregate{
		Input:          &chplan.Scan{Table: "otel_traces"},
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "TraceId"}},
		GroupByAliases: []string{chplan.TraceLookupKeyColumn},
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnArgMin,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: "SpanName"}, &chplan.ColumnRef{Name: "Timestamp"}},
			Alias: valueAlias,
		}},
	}
}

// TestEmitTraceLookupFilter_JoinsEveryLookupOnTheInputTraceID pins the shape:
// the input rows only (m.*), one LEFT JOIN per lookup keyed from the input's
// trace-id ROLE column onto TraceLookupKeyColumn, and the predicate in the
// WHERE above the joins.
func TestEmitTraceLookupFilter_JoinsEveryLookupOnTheInputTraceID(t *testing.T) {
	t.Parallel()
	n := &chplan.TraceLookupFilter{
		Input:   spansWithTraceRole(),
		Lookups: []chplan.Node{lookupRelation("a"), lookupRelation("b")},
		Predicate: &chplan.Binary{
			Op:    chplan.OpOr,
			Left:  &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "a"}, Right: &chplan.ColumnRef{Name: "SpanName"}},
			Right: &chplan.Binary{Op: chplan.OpEq, Left: &chplan.ColumnRef{Name: "b"}, Right: &chplan.ColumnRef{Name: "SpanName"}},
		},
	}
	sql, _, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"SELECT `m`.* FROM (SELECT * FROM `otel_traces`) AS m LEFT JOIN (",
		") AS l0 ON m.`TraceId` = l0.`" + chplan.TraceLookupKeyColumn + "` LEFT JOIN (",
		") AS l1 ON m.`TraceId` = l1.`" + chplan.TraceLookupKeyColumn + "` WHERE ((`a` = `SpanName`) OR (`b` = `SpanName`))",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL lacks %q:\n%s", want, sql)
		}
	}
}

func TestEmitTraceLookupFilter_RejectsIncompleteNode(t *testing.T) {
	t.Parallel()
	pred := &chplan.LitBool{V: true}
	for name, n := range map[string]*chplan.TraceLookupFilter{
		"no lookups":       {Input: spansWithTraceRole(), Predicate: pred},
		"no predicate":     {Input: spansWithTraceRole(), Lookups: []chplan.Node{lookupRelation("a")}},
		"nil lookup":       {Input: spansWithTraceRole(), Lookups: []chplan.Node{nil}, Predicate: pred},
		"no trace-id role": {Input: &chplan.Scan{Table: "otel_traces"}, Lookups: []chplan.Node{lookupRelation("a")}, Predicate: pred},
	} {
		if _, _, err := chsql.Emit(context.Background(), n); !errors.Is(err, chsql.ErrUnsupported) {
			t.Errorf("%s: Emit error = %v, want ErrUnsupported", name, err)
		}
	}
}

// TestEmitTraceLookupFilter_ExposeLookupsSelectsValueColumns pins the
// annotating shape: with ExposeLookups and no Predicate the node selects the
// input rows plus every lookup's value column under its bare name, never the
// lookup key, and carries no WHERE.
func TestEmitTraceLookupFilter_ExposeLookupsSelectsValueColumns(t *testing.T) {
	t.Parallel()
	n := &chplan.TraceLookupFilter{
		Input:         spansWithTraceRole(),
		Lookups:       []chplan.Node{lookupRelation("a"), lookupRelation("b")},
		ExposeLookups: true,
	}
	sql, _, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	want := "SELECT `m`.*, `l0`.`a` AS `a`, `l1`.`b` AS `b` FROM (SELECT * FROM `otel_traces`) AS m LEFT JOIN ("
	if !strings.HasPrefix(sql, want) {
		t.Errorf("emitted SQL does not start with %q:\n%s", want, sql)
	}
	if strings.Contains(sql, " WHERE ") || strings.Contains(sql, "`l0`.`"+chplan.TraceLookupKeyColumn+"` AS") {
		t.Errorf("annotating node emitted a WHERE or selected a lookup key:\n%s", sql)
	}
}

// TestEmitTraceLookupFilter_ExposeLookupsRejectsOpenLookup pins that an
// exposing node whose lookup cannot name its columns fails at emit rather
// than selecting a guessed column.
func TestEmitTraceLookupFilter_ExposeLookupsRejectsOpenLookup(t *testing.T) {
	t.Parallel()
	n := &chplan.TraceLookupFilter{
		Input:         spansWithTraceRole(),
		Lookups:       []chplan.Node{&chplan.Scan{Table: "otel_traces"}},
		ExposeLookups: true,
	}
	if _, _, err := chsql.Emit(context.Background(), n); !errors.Is(err, chsql.ErrUnsupported) {
		t.Fatalf("Emit error = %v, want ErrUnsupported", err)
	}
}
