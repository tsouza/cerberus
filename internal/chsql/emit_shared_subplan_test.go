package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// sharedSubplanScan returns a Scan whose own statement renders to exactly
// bodyBytes bytes, padded through the length of one projected column name.
func sharedSubplanScan(t *testing.T, bodyBytes int) (*chplan.Scan, string) {
	t.Helper()
	scan := func(pad string) *chplan.Scan {
		return &chplan.Scan{
			Table:   "otel_traces",
			Columns: []string{"TraceId", "SpanId", pad},
			Roles: []chplan.Column{
				{Name: "TraceId", Role: chplan.RoleTraceID},
				{Name: "SpanId", Role: chplan.RoleSpanID},
			},
		}
	}
	render := func(s *chplan.Scan) string {
		sql, _, _, err := newEmitter(context.Background()).renderInline(s)
		if err != nil {
			t.Fatalf("render scan: %v", err)
		}
		return sql
	}
	const minPad = "p"
	pad := strings.Repeat(minPad, len(minPad)+bodyBytes-len(render(scan(minPad))))
	s := scan(pad)
	body := render(s)
	if len(body) != bodyBytes {
		t.Fatalf("padded scan renders %d bytes; want %d", len(body), bodyBytes)
	}
	return s, body
}

// TestRenderShared_HoistsARepeatFromTheFloorUp pins the hoisting threshold
// on a node two parents share: a body one byte short of
// minSharedSubplanHoistBytes is inlined at both occurrences, and a body of
// exactly that size is declared once as a CTE that both occurrences
// reference.
func TestRenderShared_HoistsARepeatFromTheFloorUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		bodyBytes int
		hoisted   bool
	}{
		{"one byte below the floor", minSharedSubplanHoistBytes - 1, false},
		{"at the floor", minSharedSubplanHoistBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			scan, body := sharedSubplanScan(t, tc.bodyBytes)
			sql, _, err := Emit(context.Background(), &chplan.SetOperation{
				Left: scan, Right: scan, Op: chplan.SetUnion,
				TraceIDColumn: "TraceId", SpanIDColumn: "SpanId",
			})
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			copies, refs := strings.Count(sql, body), strings.Count(sql, "SELECT * FROM "+sharedSubplanCTEPrefix)
			wantCopies, wantRefs := 2, 0
			if tc.hoisted {
				wantCopies, wantRefs = 1, 2
			}
			if copies != wantCopies || refs != wantRefs {
				t.Errorf("shared %d-byte body: %d copies and %d CTE references; want %d and %d in\n%s",
					tc.bodyBytes, copies, refs, wantCopies, wantRefs, sql)
			}
		})
	}
}
