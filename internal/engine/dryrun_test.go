package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
)

// TestDryRunSQL_MatchesQuery is the core contract: DryRunSQL must return the
// exact SQL that Query executes, and must NOT touch the querier. It runs the
// live path (capturing the SQL sent to the fake ClickHouse), then the dry run,
// and asserts the SQL matches and the querier saw no extra call.
func TestDryRunSQL_MatchesQuery(t *testing.T) {
	q := &fakeQuerier{}
	eng := newEngine(q)
	lang := &fakeLang{name: "promql"}
	ctx := context.Background()

	if _, err := eng.Query(ctx, lang, "up"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if q.calls != 1 {
		t.Fatalf("setup: expected exactly one querier call from Query, got %d", q.calls)
	}
	executedSQL := q.gotSQL

	dr, err := eng.DryRunSQL(ctx, lang, "up")
	if err != nil {
		t.Fatalf("DryRunSQL: %v", err)
	}
	if dr.SQL != executedSQL {
		t.Errorf("dry-run SQL differs from executed SQL:\n dry:  %s\n exec: %s", dr.SQL, executedSQL)
	}
	if len(dr.Args) != len(q.gotArgs) {
		t.Errorf("dry-run args count %d != executed args count %d", len(dr.Args), len(q.gotArgs))
	}
	if q.calls != 1 {
		t.Errorf("DryRunSQL must not execute: querier calls went from 1 to %d", q.calls)
	}
	if dr.Plan == nil {
		t.Error("dry-run should expose the optimized plan for offline inspection")
	}
}

// TestDryRunSQL_ParseError pins that a parse failure is surfaced (wrapped) and
// the querier is never touched.
func TestDryRunSQL_ParseError(t *testing.T) {
	sentinel := errors.New("boom")
	q := &fakeQuerier{}
	eng := newEngine(q)
	lang := &fakeLang{
		name: "promql",
		parseFn: func(context.Context, string) (chplan.Node, engine.Meta, error) {
			return nil, engine.Meta{}, sentinel
		},
	}

	if _, err := eng.DryRunSQL(context.Background(), lang, "!!!"); err == nil {
		t.Fatal("expected a parse error from DryRunSQL")
	}
	if q.calls != 0 {
		t.Errorf("a parse failure must not reach the querier, got %d calls", q.calls)
	}
}
