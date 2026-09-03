package chplan

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// synthetic fixtures. Every value here is invented for the test — no
// production table/column names.

func joinScanFixture() Node {
	return &Scan{Table: "fixture_metrics"}
}

// joinCarrierCases returns one minimal fixture per join-bearing Node kind
// HasJoin claims to find — see HasJoin's own doc for the chsql evidence
// (which QueryBuilder.Join call each one traces back to) behind every row.
func joinCarrierCases() []struct {
	name string
	node Node
} {
	return []struct {
		name string
		node Node
	}{
		{"VectorJoin (PromQL vector matching)", &VectorJoin{}},
		{"HistogramVectorJoin (group_left/group_right)", &HistogramVectorJoin{}},
		{"HistogramFloatVectorJoin", &HistogramFloatVectorJoin{}},
		{"MixedVectorJoin", &MixedVectorJoin{}},
		{"InfoJoin (info())", &InfoJoin{}},
		{"StructuralJoin (TraceQL)", &StructuralJoin{}},
		{"CrossJoin", &CrossJoin{}},
		{"NestedSetAnnotate (TraceQL structure-tab numbering)", &NestedSetAnnotate{}},
		{
			"MetricsCompare with RootLookup (TraceQL compare() root-span lookup)",
			&MetricsCompare{Inner: joinScanFixture(), RootLookup: joinScanFixture()},
		},
		{
			"RangeWindow with DeltaPrefixAggregateInput (delta-prefix LEFT JOIN)",
			&RangeWindow{
				Input:                     joinScanFixture(),
				DeltaPrefixAggregateInput: &Scan{Table: "fixture_metrics_delta_prefix"},
			},
		},
		{
			// cerberus issue #3014: instant rate()/increase() over a
			// temporality-projected counter, with NO DeltaPrefixAggregateInput
			// at all — the default, no-backfill shape. Reaches
			// instantDeltaPrefixSource's unconditional LEFT/CROSS JOIN
			// (internal/chsql/range_window.go's emitWindowedArrayExtrapolated),
			// which the row above's condition alone never sees.
			"RangeWindow instant rate() over temporality-projected counter (instant delta-prefix JOIN, #3014)",
			&RangeWindow{
				Input:             joinScanFixture(),
				Func:              "rate",
				OuterRange:        0,
				TemporalityColumn: "AggregationTemporality",
			},
		},
	}
}

// wantJoinCarrierCount pins the row count of joinCarrierCases so a table row
// silently dropped (as opposed to a switch arm dropped, which
// TestHasJoin_CoversEveryJoinEmittingNode catches independently by deriving
// straight from join.go's source) still fails loudly rather than the table
// quietly shrinking to whatever HasJoin currently does.
const wantJoinCarrierCount = 11

// TestHasJoin_DetectsEveryJoinCarrier covers every join-bearing chplan node
// HasJoin claims to find: each one alone is enough to trip the detector.
func TestHasJoin_DetectsEveryJoinCarrier(t *testing.T) {
	t.Parallel()
	cases := joinCarrierCases()
	if len(cases) != wantJoinCarrierCount {
		t.Fatalf("joinCarrierCases has %d rows, want %d — update wantJoinCarrierCount alongside "+
			"any deliberate change to the carrier set", len(cases), wantJoinCarrierCount)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !HasJoin(tc.node) {
				t.Errorf("HasJoin(%s) = false; want true", tc.name)
			}
		})
	}
}

// TestHasJoin_NonJoinPlansUnaffected pins the negative side: a bare scan, an
// aggregation, a RangeWindow with no delta-prefix side feed, and a
// MetricsCompare with no RootLookup all report no join.
func TestHasJoin_NonJoinPlansUnaffected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		node Node
	}{
		{"bare Scan", &Scan{Table: "fixture_metrics"}},
		{"Aggregate over Scan", &Aggregate{Input: joinScanFixture()}},
		{"RangeWindow with no delta-prefix side feed", &RangeWindow{Input: joinScanFixture()}},
		{"MetricsCompare with no RootLookup", &MetricsCompare{Inner: joinScanFixture()}},
		{"VectorSetOp (semi/anti-join lowers to WHERE IN, never a SQL JOIN)", &VectorSetOp{Left: joinScanFixture(), Right: joinScanFixture()}},
		{
			// delta() is a gauge delta, not a counter — extrapolationKind's
			// isCounter() (and IsCounterRangeWindowFunc) is false for it, so
			// needsDeltaFirstLevel never goes true and instantDeltaPrefixSource
			// never runs, regardless of TemporalityColumn.
			"RangeWindow instant delta() over temporality-projected column (not a counter func)",
			&RangeWindow{
				Input:             joinScanFixture(),
				Func:              "delta",
				OuterRange:        0,
				TemporalityColumn: "AggregationTemporality",
			},
		},
		{
			// No TemporalityColumn: windowTemporalityProjected(r) is false, so
			// needsDeltaFirstLevel is false and instantDeltaPrefixSource never
			// runs even for a counter Func.
			"RangeWindow instant rate() with no TemporalityColumn",
			&RangeWindow{Input: joinScanFixture(), Func: "rate", OuterRange: 0},
		},
		{
			// Matrix shape (OuterRange > 0) with no DeltaPrefixAggregateInput:
			// the matrix emitter's default fallback is deltaMatrixLevelSource, a
			// window function over the fanned-out rows — genuinely join-free,
			// unlike the instant shape's default fallback.
			"RangeWindow matrix rate() over temporality-projected counter, no DeltaPrefixAggregateInput",
			&RangeWindow{
				Input:             joinScanFixture(),
				Func:              "rate",
				OuterRange:        10 * time.Minute,
				Step:              time.Minute,
				TemporalityColumn: "AggregationTemporality",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if HasJoin(tc.node) {
				t.Errorf("HasJoin(%s) = true; want false", tc.name)
			}
		})
	}
}

// TestHasJoin_ReachesJoinNestedInScalarSubquery is the WalkDeep-matters
// proof: a join buried inside a Filter predicate's ScalarSubquery is
// invisible to Walk (Children()-only) and must still be found by HasJoin.
// This is the exact shape a shallow-Walk join detector misses — see
// routememo/key.go's own history (cerberus issue #3008 added case arms to a
// Walk-based switch that still could not see a join here).
func TestHasJoin_ReachesJoinNestedInScalarSubquery(t *testing.T) {
	t.Parallel()

	buried := &VectorJoin{}
	plan := &Filter{
		Input:     joinScanFixture(),
		Predicate: &ScalarSubquery{Input: buried},
	}

	// Prove the shallow traversal really cannot see it — otherwise this
	// test would not distinguish WalkDeep from Walk at all.
	shallowFound := false
	Walk(plan, func(n Node) bool {
		if _, ok := n.(*VectorJoin); ok {
			shallowFound = true
		}
		return true
	})
	if shallowFound {
		t.Fatal("Walk reached the VectorJoin inside the ScalarSubquery; this fixture no longer " +
			"distinguishes Walk from WalkDeep")
	}

	if !HasJoin(plan) {
		t.Error("HasJoin: join nested inside a ScalarSubquery predicate not found; want true")
	}
}

// joinHandledTypes returns the Node type names HasJoin's switch (join.go)
// dispatches on, read out of the source by parsing it — never by re-typing
// the case list, which would just be a second hand-maintained copy of the
// first.
func joinHandledTypes(t *testing.T) map[string]bool {
	t.Helper()

	const scannedFile = "join.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, scannedFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", scannedFile, err)
	}

	handled := map[string]bool{}
	var arms int
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "HasJoin" {
			return true
		}
		ast.Inspect(fn, func(inner ast.Node) bool {
			cc, ok := inner.(*ast.CaseClause)
			if !ok {
				return true
			}
			arms++
			for _, expr := range cc.List {
				if name := joinCaseTypeName(expr); name != "" {
					handled[name] = true
				}
			}
			return true
		})
		return false
	})
	if arms == 0 {
		t.Fatalf("scanned %s and found no case arms in HasJoin — the scan lost its grip on the "+
			"source shape, so this ratchet is vacuous", scannedFile)
	}
	return handled
}

// joinCaseTypeName returns the bare type name of one `case *T:` entry, or ""
// for a shape the scan does not recognise.
func joinCaseTypeName(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if ident, ok := e.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// nonJoinKinds is the explicit, hand-maintained roster of every OTHER
// chplan.Node concrete type — one that HasJoin's switch does NOT treat as
// join-bearing — paired with the reason its chsql emitter never renders a
// SQL JOIN. It is never trusted on its own:
// TestHasJoin_CoversEveryJoinEmittingNode asserts its keys, UNIONED with
// joinHandledTypes' derived set, are exactly the sealed chplan.Node set, so
// a new Node type added to the package without a decision in ONE of the two
// lists fails here by name — the same derive-and-diff shape every other
// exhaustiveness ratchet in this package uses (see sealed_kinds_test.go).
//
// The classification is BY EMISSION, verified against internal/chsql at
// authoring time (every QueryBuilder.Join call site in the package was
// swept and matched back to its Node — see join.go's own doc), not by
// whether the type's name or doc mentions "join":
//   - VectorSetOp's own doc calls `and`/`unless` a "semi-join"/"anti-join",
//     but internal/chsql/vector_set_op.go lowers both to a
//     `WHERE ... IN (subquery)` / `NOT IN (subquery)` predicate, never a
//     JOIN clause.
//   - SetOperation, UnionAll and NaryVectorSetOp all combine two relations,
//     but via SQL UNION [ALL], not JOIN.
var nonJoinKinds = map[string]string{
	// --- leaf nodes: no second relation to combine at all ---
	"Scan":     "a single table scan",
	"OneRow":   "a synthetic single-row source",
	"StepGrid": "a synthetic anchor-timestamp series",

	// --- single-input reshaping nodes: read one relation, never combine two ---
	"Filter":                         "row-level predicate over its one Input",
	"Project":                        "column reshaping over its one Input",
	"Aggregate":                      "GROUP BY over its one Input",
	"Limit":                          "row-count cap over its one Input",
	"OrderBy":                        "sort over its one Input",
	"TopK":                           "sort + cap over its one Input",
	"SearchTraceLimit":               "trace-count cap over its one Input",
	"AbsentOverTime":                 "presence check over its one Input",
	"HistogramQuantile":              "quantile arithmetic over its one Input",
	"HistogramQuantileNative":        "quantile arithmetic over its one Input",
	"HistogramProjection":            "column reshaping over its one Input",
	"MetricsAggregate":               "GROUP BY over its one Inner",
	"MetricsHistogramOverTime":       "histogram fold over its one Inner",
	"MetricsSecondStage":             "post-aggregation reshaping over its one Input",
	"RangeWindowGridNative":          "windowed-array fold over its one Input",
	"RangeBucketFanout":              "per-bucket fan-out over its one Input, via array functions",
	"RangeBucketGridNative":          "windowed-array fold over its one Input, ARRAY JOIN only",
	"RangeLWR":                       "last-value-lookback fold over its one Input",
	"RangeWindowGridNativeInstant":   "windowed-array fold over its one Input",
	"RangeWindowGridNativeVectorAgg": "windowed-array fold over its one Input, ARRAY JOIN only",
	"RangeWindowStaleResample":       "resample fold over its one Input, ARRAY JOIN only",

	// --- multi-arm combinators that emit SQL UNION, never JOIN ---
	"UnionAll":        "combines its Inputs via UNION ALL",
	"SetOperation":    "combines Left/Right via UNION/INTERSECT/EXCEPT DISTINCT",
	"NaryVectorSetOp": "combines its Arms via UNION ALL + dedup, not a JOIN",
	"VectorSetOp":     "PromQL and/unless: semi/anti-join by NAME, WHERE IN/NOT IN subquery by emission",
}

// TestHasJoin_CoversEveryJoinEmittingNode is the completeness ratchet the M2
// milestone's join-carrier-registry criterion asks for: it keys on EMISSION
// BEHAVIOUR, not on a type name ending in "Join" (that heuristic both
// misses RangeWindow's delta-prefix carrier and false-positives on
// chplan.LabelJoin, an Expr that can never reach a Node switch at all — see
// TestHasJoin_NonJoinPlansUnaffected's VectorSetOp row for the mirror-image
// trap: a NAME that says "join" but an emission that never renders one).
//
// It partitions every concrete chplan.Node type — derived from the
// planNode() marker declarations in this package's source, via the same
// sealedscan machinery every other exhaustiveness ratchet here uses, never
// hand-counted — into joinHandledTypes (derived from HasJoin's own switch
// arms in join.go) and nonJoinKinds (hand-classified, with a reason per
// entry). A Node type landing in NEITHER when a new one is added fails
// here by name: the author must add it to join.go's switch (if its chsql
// emitter renders a JOIN) or to nonJoinKinds (with a reason, if it does
// not) — there is no default that lets it pass silently either way.
func TestHasJoin_CoversEveryJoinEmittingNode(t *testing.T) {
	t.Parallel()

	joinKinds := joinHandledTypes(t)
	for name := range joinKinds {
		if _, dup := nonJoinKinds[name]; dup {
			t.Errorf("%s is in BOTH HasJoin's switch and nonJoinKinds — pick one", name)
		}
	}

	covered := make(map[string]bool, len(joinKinds)+len(nonJoinKinds))
	for name := range joinKinds {
		covered[name] = true
	}
	for name := range nonJoinKinds {
		covered[name] = true
	}
	assertCoversEverySealedKind(t, nodeMarkerMethod, covered, "join.go's HasJoin switch + join_test.go's nonJoinKinds",
		"add the new Node type to HasJoin's switch in join.go if its chsql emitter renders a SQL "+
			"JOIN, else to nonJoinKinds in join_test.go with the reason it does not")
}
