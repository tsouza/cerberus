package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/test/spec"
)

// TestLowerLinkAndEvent pins the lowering of TraceQL link-traversal and
// span-event spanset filters onto chplan.NestedArrayExists. The TXTAR
// fixtures under test/spec/traceql/ (link_*.txtar, event_*.txtar) cover
// the same shapes end-to-end; this file asserts the IR + SQL directly
// so failures point at the structural mistake rather than a fixture
// diff.
func TestLowerLinkAndEvent(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name       string
		query      string
		wantCol    string
		wantKey    string
		wantOp     chplan.BinaryOp
		wantValStr string
		wantSQL    string
	}{
		{
			name:       "link_span_id",
			query:      `{ link.span_id = "abc" }`,
			wantCol:    "Links",
			wantKey:    "span_id",
			wantOp:     chplan.OpEq,
			wantValStr: "abc",
			wantSQL:    "SELECT * FROM `otel_traces` WHERE arrayExists(x -> x[?] = ?, `Links`.`Attributes`)",
		},
		{
			name:       "link_trace_id",
			query:      `{ link.trace_id = "deadbeef" }`,
			wantCol:    "Links",
			wantKey:    "trace_id",
			wantOp:     chplan.OpEq,
			wantValStr: "deadbeef",
			wantSQL:    "SELECT * FROM `otel_traces` WHERE arrayExists(x -> x[?] = ?, `Links`.`Attributes`)",
		},
		{
			name:       "link_attribute",
			query:      `{ link.environment = "prod" }`,
			wantCol:    "Links",
			wantKey:    "environment",
			wantOp:     chplan.OpEq,
			wantValStr: "prod",
			wantSQL:    "SELECT * FROM `otel_traces` WHERE arrayExists(x -> x[?] = ?, `Links`.`Attributes`)",
		},
		{
			name:       "event_name",
			query:      `{ event.name = "exception" }`,
			wantCol:    "Events",
			wantKey:    "name",
			wantOp:     chplan.OpEq,
			wantValStr: "exception",
			wantSQL:    "SELECT * FROM `otel_traces` WHERE arrayExists(x -> x[?] = ?, `Events`.`Attributes`)",
		},
		{
			name:       "event_dotted_attribute",
			query:      `{ event.exception.type = "ConnectionError" }`,
			wantCol:    "Events",
			wantKey:    "exception.type",
			wantOp:     chplan.OpEq,
			wantValStr: "ConnectionError",
			wantSQL:    "SELECT * FROM `otel_traces` WHERE arrayExists(x -> x[?] = ?, `Events`.`Attributes`)",
		},
		{
			name:       "event_attribute_inequality",
			query:      `{ event.severity != "info" }`,
			wantCol:    "Events",
			wantKey:    "severity",
			wantOp:     chplan.OpNe,
			wantValStr: "info",
			wantSQL:    "SELECT * FROM `otel_traces` WHERE arrayExists(x -> x[?] != ?, `Events`.`Attributes`)",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			pred := filterPredicate(t, plan)
			n, ok := pred.(*chplan.NestedArrayExists)
			if !ok {
				t.Fatalf("predicate type = %T, want *chplan.NestedArrayExists", pred)
			}
			if n.Column != tc.wantCol {
				t.Errorf("Column = %q, want %q", n.Column, tc.wantCol)
			}
			if n.SubField != "Attributes" {
				t.Errorf("SubField = %q, want %q", n.SubField, "Attributes")
			}
			if n.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", n.Key, tc.wantKey)
			}
			if n.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", n.Op, tc.wantOp)
			}
			gotVal, ok := n.Value.(*chplan.LitString)
			if !ok {
				t.Fatalf("Value type = %T, want *chplan.LitString", n.Value)
			}
			if gotVal.V != tc.wantValStr {
				t.Errorf("Value = %q, want %q", gotVal.V, tc.wantValStr)
			}

			sqlStr, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if sqlStr != tc.wantSQL {
				t.Errorf("sql mismatch\n got: %s\nwant: %s", sqlStr, tc.wantSQL)
			}
			if len(args) != 2 {
				t.Fatalf("args len = %d, want 2", len(args))
			}
			if got, _ := args[0].(string); got != tc.wantKey {
				t.Errorf("args[0] = %v, want %q", args[0], tc.wantKey)
			}
			if got, _ := args[1].(string); got != tc.wantValStr {
				t.Errorf("args[1] = %v, want %q", args[1], tc.wantValStr)
			}
		})
	}
}

// TestLowerLinkEventBareReferenceProbesExistence pins the lowering of a
// bare link- / event-scoped attribute used as the whole filter
// expression (`{ link.span_id }`, `{ event.name }`). Reference Tempo's
// SpansetFilter.validate accepts a bare attribute (it types to
// TypeAttribute) and matches the span when the attribute resolves to a
// non-nil value — for the per-element Nested columns that is "at least
// one element carries the key". Cerberus lowers it to the same hasKey
// NestedArrayExists probe `<attr> != nil` produces, rather than
// rejecting (the rejection-parity layer flagged the old 422 as a
// wrong_rejection).
func TestLowerLinkEventBareReferenceProbesExistence(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	// Compose the bare-reference shape directly via the AST: a
	// SpansetFilter whose expression is a bare link-scoped Attribute.
	root := &tempo.RootExpr{
		Pipeline: tempo.Pipeline{
			Elements: []tempo.PipelineElement{
				&tempo.SpansetFilter{Expression: tempo.NewScopedAttribute(tempo.AttributeScopeLink, false, "span_id")},
			},
		},
	}
	plan, err := traceql.Lower(context.Background(), root, s)
	if err != nil {
		t.Fatalf("Lower(bare link.span_id): %v — reference accepts the bare reference", err)
	}
	printed := spec.PrintChplan(plan)
	if !strings.Contains(printed, "nestedArrayExists(Links.Attributes hasKey \"span_id\")") {
		t.Errorf("Lower(bare link.span_id) plan:\n%s\nwant a hasKey existence probe on Links.Attributes", printed)
	}
}

// TestLowerNestedArrayExistsEqual exercises the Expr.Equal contract for
// the new chplan type — defensive coverage against future refactors
// that re-use Expr.Equal in optimizer rewrites.
func TestLowerNestedArrayExistsEqual(t *testing.T) {
	t.Parallel()

	a := &chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}}
	b := &chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}}
	if !a.Equal(b) {
		t.Error("Equal returned false for structurally identical NestedArrayExists")
	}

	differ := []chplan.Expr{
		&chplan.NestedArrayExists{Column: "Events", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}},
		&chplan.NestedArrayExists{Column: "Links", SubField: "Other", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}},
		&chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "other", Op: chplan.OpEq, Value: &chplan.LitString{V: "v"}},
		&chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpNe, Value: &chplan.LitString{V: "v"}},
		&chplan.NestedArrayExists{Column: "Links", SubField: "Attributes", Key: "k", Op: chplan.OpEq, Value: &chplan.LitString{V: "other"}},
		&chplan.LitString{V: "v"},
	}
	for i, d := range differ {
		if a.Equal(d) {
			t.Errorf("case %d: Equal returned true for differing expr %T", i, d)
		}
	}
}

// filterPredicate extracts the predicate of the top-level chplan.Filter
// (or t.Fatal if the plan isn't shaped as Filter(Scan)). Centralises the
// type assertions used by every test case in this file.
func filterPredicate(t *testing.T, plan chplan.Node) chplan.Expr {
	t.Helper()
	f, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("plan = %T, want *chplan.Filter", plan)
	}
	return f.Predicate
}
