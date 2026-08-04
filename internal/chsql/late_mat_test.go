package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestIsLateMatCandidateConditions walks each of the four guard
// conditions in turn, asserting the gate rejects on a miss and accepts
// on a full match. The conditions (in isLateMatCandidate's order):
//
//  1. Top node is *chplan.Project with at least one Projection.
//  2. Project.Input is *chplan.Limit (Count > 0) wrapping either a
//     Filter+Scan or a bare Scan.
//  3. Scan.Table is registered in the late-mat shape registry with
//     non-empty WideColumns + RowKey.
//  4. At least one projection column-ref points at a wide column.
//
// The test asserts gate behaviour — full emission shape is covered by
// the TXTAR fixtures under test/spec/codegen/late_mat/.
func TestIsLateMatCandidateConditions(t *testing.T) {
	t.Parallel()

	wideCol := func(name string) chplan.Projection {
		return chplan.Projection{Expr: &chplan.ColumnRef{Name: name}}
	}
	thinCol := func(name string) chplan.Projection {
		return chplan.Projection{Expr: &chplan.ColumnRef{Name: name}}
	}

	tests := []struct {
		name   string
		plan   chplan.Node
		want   bool
		reason string
	}{
		{
			name: "full match logs Project(Limit(Filter(Scan)))",
			plan: &chplan.Project{
				Projections: []chplan.Projection{
					wideCol("Body"),
					thinCol("Timestamp"),
				},
				Input: &chplan.Limit{
					Count: 100,
					Input: &chplan.Filter{
						Predicate: &chplan.Binary{
							Op:    chplan.OpGe,
							Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
							Right: &chplan.LitInt{V: 9},
						},
						Input: &chplan.Scan{Table: "otel_logs"},
					},
				},
			},
			want:   true,
			reason: "wide column Body + Limit + registered table → match",
		},
		{
			name: "match without Filter (Project(Limit(Scan)))",
			plan: &chplan.Project{
				Projections: []chplan.Projection{wideCol("Body")},
				Input: &chplan.Limit{
					Count: 50,
					Input: &chplan.Scan{Table: "otel_logs"},
				},
			},
			want:   true,
			reason: "Project(Limit(Scan)) is a valid trigger shape",
		},
		{
			name: "condition 1 miss: no projections",
			plan: &chplan.Project{
				Projections: nil,
				Input: &chplan.Limit{
					Count: 100,
					Input: &chplan.Scan{Table: "otel_logs"},
				},
			},
			want:   false,
			reason: "empty projection list",
		},
		{
			name: "condition 1 miss: top is not Project",
			plan: &chplan.Limit{
				Count: 100,
				Input: &chplan.Scan{Table: "otel_logs"},
			},
			want:   false,
			reason: "Limit directly at the top",
		},
		{
			name: "condition 2 miss: no Limit downstream",
			plan: &chplan.Project{
				Projections: []chplan.Projection{wideCol("Body")},
				Input: &chplan.Filter{
					Predicate: &chplan.Binary{
						Op:    chplan.OpEq,
						Left:  &chplan.ColumnRef{Name: "ServiceName"},
						Right: &chplan.LitString{V: "api"},
					},
					Input: &chplan.Scan{Table: "otel_logs"},
				},
			},
			want:   false,
			reason: "Filter without Limit → would materialise twice → skip",
		},
		{
			name: "condition 2 miss: Limit Count <= 0",
			plan: &chplan.Project{
				Projections: []chplan.Projection{wideCol("Body")},
				Input: &chplan.Limit{
					Count: 0,
					Input: &chplan.Scan{Table: "otel_logs"},
				},
			},
			want:   false,
			reason: "Limit Count is zero → treated as no-limit",
		},
		{
			name: "condition 2 miss: Limit wraps non-Scan/Filter",
			plan: &chplan.Project{
				Projections: []chplan.Projection{wideCol("Body")},
				Input: &chplan.Limit{
					Count: 100,
					Input: &chplan.Project{
						Projections: []chplan.Projection{wideCol("Body")},
						Input:       &chplan.Scan{Table: "otel_logs"},
					},
				},
			},
			want:   false,
			reason: "Limit wraps a Project, not Filter/Scan",
		},
		{
			name: "condition 3 miss: unregistered table",
			plan: &chplan.Project{
				Projections: []chplan.Projection{wideCol("Body")},
				Input: &chplan.Limit{
					Count: 100,
					Input: &chplan.Scan{Table: "otel_metrics_gauge"},
				},
			},
			want:   false,
			reason: "metrics_gauge has no wide-column registration",
		},
		{
			name: "condition 4 miss: no wide columns in projection",
			plan: &chplan.Project{
				Projections: []chplan.Projection{
					thinCol("Timestamp"),
					thinCol("SeverityNumber"),
				},
				Input: &chplan.Limit{
					Count: 100,
					Input: &chplan.Scan{Table: "otel_logs"},
				},
			},
			want:   false,
			reason: "projection is all thin columns",
		},
		{
			name: "condition 4 miss: wide column nested in func call",
			plan: &chplan.Project{
				Projections: []chplan.Projection{
					{
						Expr: &chplan.FuncCall{
							Name: "length",
							Args: []chplan.Expr{&chplan.ColumnRef{Name: "Body"}},
						},
					},
				},
				Input: &chplan.Limit{
					Count: 100,
					Input: &chplan.Scan{Table: "otel_logs"},
				},
			},
			want:   false,
			reason: "length(Body) doesn't expose Body itself — gate conservatively rejects",
		},
		{
			name: "full match traces Project(Limit(Filter(Scan))) with SpanAttributes",
			plan: &chplan.Project{
				Projections: []chplan.Projection{
					wideCol("SpanAttributes"),
					thinCol("TraceId"),
					thinCol("SpanId"),
				},
				Input: &chplan.Limit{
					Count: 25,
					Input: &chplan.Filter{
						Predicate: &chplan.Binary{
							Op:    chplan.OpEq,
							Left:  &chplan.ColumnRef{Name: "ServiceName"},
							Right: &chplan.LitString{V: "api"},
						},
						Input: &chplan.Scan{Table: "otel_traces"},
					},
				},
			},
			want:   true,
			reason: "wide column SpanAttributes + Limit + registered traces table",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, ok := (&emitter{}).isLateMatCandidate(tt.plan)
			if ok != tt.want {
				t.Fatalf("isLateMatCandidate: got %t, want %t (%s)", ok, tt.want, tt.reason)
			}
			if ok && m == nil {
				t.Fatal("isLateMatCandidate returned (nil, true) — invariant violation")
			}
			if !ok && m != nil {
				t.Fatal("isLateMatCandidate returned (non-nil, false) — invariant violation")
			}
		})
	}
}

// TestEmitLateMatShape smoke-tests the rendered SQL shape on the
// canonical OTel logs Body+Timestamp+LIMIT 100 case. The TXTAR
// fixtures under test/spec/codegen/late_mat/ are the byte-for-byte
// TestIsLateMatCandidate_CtxOverriddenTableName is the #1703 regression:
// a deployment that overrides its logs table name (CERBERUS_SCHEMA_LOGS_TABLE
// / the schema.logs.table config key) ends up with Scan.Table set to
// something other than "otel_logs" — a name the package-level, default-OTel-
// name-keyed lateMatShapes registry has never heard of. Before this fix,
// isLateMatCandidate silently fell through to "no match" for every such
// deployment: no error, no metric, just a quiet loss of the late-mat
// optimisation on exactly the LIMIT+WHERE queries it exists to help.
func TestIsLateMatCandidate_CtxOverriddenTableName(t *testing.T) {
	t.Parallel()

	const customTable = "custom_logs"
	plan := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
		Input: &chplan.Limit{
			Count: 100,
			Input: &chplan.Scan{Table: customTable},
		},
	}
	wide := []string{"Body", "ResourceAttributes", "LogAttributes"}
	rowKey := []string{"Timestamp", "TraceId", "SpanId"}

	t.Run("without threaded shape, an unregistered custom table never matches", func(t *testing.T) {
		t.Parallel()
		if _, ok := (&emitter{}).isLateMatCandidate(plan); ok {
			t.Fatal("expected no match for a table absent from the static registry and with no ctx-threaded shape")
		}
	})

	t.Run("ctx-threaded shape for the matching table makes it match", func(t *testing.T) {
		t.Parallel()
		ctx := WithLateMatShape(context.Background(), customTable, wide, rowKey)
		table, shape, ok := lateMatShapeFromCtx(ctx)
		if !ok || table != customTable {
			t.Fatalf("lateMatShapeFromCtx: got (%q, %t), want (%q, true)", table, ok, customTable)
		}
		e := &emitter{ctxLateMatTable: table, ctxLateMatShape: shape}
		m, ok := e.isLateMatCandidate(plan)
		if !ok || m == nil {
			t.Fatalf("isLateMatCandidate: got (%v, %t), want a match once the ctx-threaded shape resolves %q", m, ok, customTable)
		}
	})

	t.Run("ctx-threaded shape for a DIFFERENT table falls back to the static registry (still no match)", func(t *testing.T) {
		t.Parallel()
		ctx := WithLateMatShape(context.Background(), "some_other_table", wide, rowKey)
		table, shape, _ := lateMatShapeFromCtx(ctx)
		e := &emitter{ctxLateMatTable: table, ctxLateMatShape: shape}
		if _, ok := e.isLateMatCandidate(plan); ok {
			t.Fatal("expected no match: the ctx-threaded shape is for a different table than the Scan's, so it must not apply, and the static registry has no entry for customTable either")
		}
	})

	t.Run("existing default-table callers are unaffected: otel_logs still matches via the static registry alone", func(t *testing.T) {
		t.Parallel()
		defaultPlan := &chplan.Project{
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "Body"}},
				{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
			},
			Input: &chplan.Limit{
				Count: 100,
				Input: &chplan.Scan{Table: "otel_logs"},
			},
		}
		if _, ok := (&emitter{}).isLateMatCandidate(defaultPlan); !ok {
			t.Fatal("expected the pre-#1703 default-table path (no ctx-threaded shape) to keep matching unchanged")
		}
	})
}

// goldens; this test just asserts the high-signal markers (two
// SELECTs, INNER JOIN, scan/w aliases) so a regression that drops to
// the single-SELECT path fails loudly here even when the txtar
// fixtures are unchanged.
func TestEmitLateMatShape(t *testing.T) {
	t.Parallel()

	plan := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
			{Expr: &chplan.ColumnRef{Name: "SeverityNumber"}},
		},
		Input: &chplan.Limit{
			Count: 100,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{
					Op:    chplan.OpGe,
					Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
					Right: &chplan.LitInt{V: 9},
				},
				Input: &chplan.Scan{Table: "otel_logs"},
			},
		},
	}

	sql, args, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(args) != 1 {
		t.Fatalf("args: got %d, want 1 (the predicate literal)", len(args))
	}
	if got, want := args[0].(int64), int64(9); got != want {
		t.Fatalf("args[0]: got %d, want %d", got, want)
	}

	wantMarkers := []string{
		"INNER JOIN `otel_logs` AS w",
		"LIMIT 100) AS scan", // inner SELECT closes with LIMIT then aliased scan
		"`scan`.`Timestamp` = `w`.`Timestamp`",
		"`scan`.`TraceId` = `w`.`TraceId`",
		"`scan`.`SpanId` = `w`.`SpanId`",
		"`w`.`Body`", // wide col routed to JOIN side
		"WHERE",      // inner WHERE present
		"LIMIT 100",  // inner LIMIT present
	}
	for _, marker := range wantMarkers {
		if !strings.Contains(sql, marker) {
			t.Errorf("rendered SQL missing marker %q\nSQL: %s", marker, sql)
		}
	}

	// Confirm SELECT appears exactly twice (outer + inner).
	if got := strings.Count(sql, "SELECT "); got != 2 {
		t.Errorf("expected exactly 2 SELECT keywords in late-mat output, got %d\nSQL: %s", got, sql)
	}
}

// TestEmitLateMatShape_CtxOverriddenTableName is the end-to-end #1703
// regression: the same Project(Limit(Filter(Scan))) shape as
// TestEmitLateMatShape, but scanning a custom table name that the static
// lateMatShapes registry has never heard of. Without chsql.WithLateMatShape
// threaded onto the context, Emit falls back to the canonical single-SELECT
// path (no JOIN). With it threaded, Emit produces the same two-stage
// JOIN rewrite as the default-table case, proving the fix reaches all the
// way through chsql.Emit's public entrypoint — not just the internal gate.
func TestEmitLateMatShape_CtxOverriddenTableName(t *testing.T) {
	t.Parallel()

	const customTable = "custom_logs"
	plan := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
			{Expr: &chplan.ColumnRef{Name: "SeverityNumber"}},
		},
		Input: &chplan.Limit{
			Count: 100,
			Input: &chplan.Filter{
				Predicate: &chplan.Binary{
					Op:    chplan.OpGe,
					Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
					Right: &chplan.LitInt{V: 9},
				},
				Input: &chplan.Scan{Table: customTable},
			},
		},
	}
	wide := []string{"Body", "ResourceAttributes", "LogAttributes"}
	rowKey := []string{"Timestamp", "TraceId", "SpanId"}

	t.Run("without WithLateMatShape, falls back to the canonical single-SELECT path", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		// The canonical (non-late-mat) path never emits the late-mat rewrite's
		// distinctive markers — the two-stage INNER JOIN back to the wide
		// table and its `w`-aliased scan side. Exact SELECT-nesting depth is
		// an emitter-internal subquery-composition detail, not part of this
		// regression's contract, so this only pins the semantic markers.
		if strings.Contains(sql, "INNER JOIN") {
			t.Errorf("did not expect a late-mat JOIN without a ctx-threaded shape\nSQL: %s", sql)
		}
		if strings.Contains(sql, "AS w") {
			t.Errorf("did not expect the late-mat wide-table JOIN alias without a ctx-threaded shape\nSQL: %s", sql)
		}
	})

	t.Run("with WithLateMatShape, emits the same two-stage JOIN rewrite as the default-table case", func(t *testing.T) {
		t.Parallel()
		ctx := WithLateMatShape(context.Background(), customTable, wide, rowKey)
		sql, args, err := Emit(ctx, plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if len(args) != 1 || args[0].(int64) != 9 {
			t.Fatalf("args: got %v, want [9]", args)
		}
		wantMarkers := []string{
			"INNER JOIN `" + customTable + "` AS w",
			"LIMIT 100) AS scan",
			"`scan`.`Timestamp` = `w`.`Timestamp`",
			"`scan`.`TraceId` = `w`.`TraceId`",
			"`scan`.`SpanId` = `w`.`SpanId`",
			"`w`.`Body`",
			"WHERE",
			"LIMIT 100",
		}
		for _, marker := range wantMarkers {
			if !strings.Contains(sql, marker) {
				t.Errorf("rendered SQL missing marker %q\nSQL: %s", marker, sql)
			}
		}
		if got := strings.Count(sql, "SELECT "); got != 2 {
			t.Errorf("expected exactly 2 SELECT keywords once the ctx-threaded shape resolves %q, got %d\nSQL: %s", customTable, got, sql)
		}
	})
}

// TestEmitLateMatFallback asserts that the canonical single-SELECT
// path is taken when the gate misses — guards against accidental
// rewrites firing on plans that don't match the trigger pattern.
func TestEmitLateMatFallback(t *testing.T) {
	t.Parallel()

	// Same projection list, but no LIMIT — gate condition 2 misses.
	plan := &chplan.Project{
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: "Body"}},
			{Expr: &chplan.ColumnRef{Name: "Timestamp"}},
		},
		Input: &chplan.Filter{
			Predicate: &chplan.Binary{
				Op:    chplan.OpGe,
				Left:  &chplan.ColumnRef{Name: "SeverityNumber"},
				Right: &chplan.LitInt{V: 9},
			},
			Input: &chplan.Scan{Table: "otel_logs"},
		},
	}

	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	if strings.Contains(sql, "INNER JOIN") {
		t.Errorf("plan without LIMIT should fall through to single SELECT, got:\n%s", sql)
	}
	if strings.Contains(sql, " AS scan") {
		t.Errorf("plan without LIMIT should not produce 'AS scan' alias, got:\n%s", sql)
	}
}
