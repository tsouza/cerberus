package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The canonical grid every carrier fixture below is built on. The values are
// irrelevant to a shape id — it is literal-free by construction — but a carrier
// still needs a well-formed grid to be the node it claims to be.
var (
	shapeGridStart = time.Unix(1_700_000_000, 0).UTC()
	shapeGridEnd   = shapeGridStart.Add(time.Hour)
)

const (
	shapeGridStep      = 15 * time.Second
	shapeCarrierWindow = 5 * time.Minute
)

// carrierTokenCase pairs a [chplan.GridCarrier] kind with the modifier token
// planShapeID must emit for it.
type carrierTokenCase struct {
	// kind is the chplan struct name, checked against the package source by
	// TestShapeModifiers_CoverEveryGridCarrier.
	kind  string
	node  func(input chplan.Node) chplan.Node
	token string
}

func carrierTokenCases() []carrierTokenCase {
	return []carrierTokenCase{
		{
			kind:  "RangeWindow",
			token: "rw",
			node: func(in chplan.Node) chplan.Node {
				return &chplan.RangeWindow{
					Input: in, Func: "rate", Range: shapeCarrierWindow, Step: shapeGridStep,
					OuterRange: shapeGridEnd.Sub(shapeGridStart), Start: shapeGridStart, End: shapeGridEnd,
					TimestampColumn: "TimeUnix", ValueColumn: "Value",
				}
			},
		},
		{
			kind:  "RangeWindowNative",
			token: "rwn",
			node: func(in chplan.Node) chplan.Node {
				return &chplan.RangeWindowNative{
					Input: in, Func: "rate", Range: shapeCarrierWindow, Step: shapeGridStep,
					Start: shapeGridStart, End: shapeGridEnd,
					TimestampColumn: "TimeUnix", ValueColumn: "Value",
				}
			},
		},
		{
			kind:  "RangeWindowResample",
			token: "rwr",
			node: func(in chplan.Node) chplan.Node {
				return &chplan.RangeWindowResample{
					Input: in, Start: shapeGridStart, End: shapeGridEnd, Step: shapeGridStep,
					Lookback: shapeCarrierWindow, MetricNameCol: "MetricName", AttributesCol: "Attributes",
					TimestampCol: "TimeUnix", ValueCol: "Value",
				}
			},
		},
		{
			kind:  "RangeLWR",
			token: "lwr",
			node: func(in chplan.Node) chplan.Node {
				return &chplan.RangeLWR{
					Input: in, Start: shapeGridStart, End: shapeGridEnd, Step: shapeGridStep,
					Lookback: shapeCarrierWindow, MetricNameCol: "MetricName", AttributesCol: "Attributes",
					TimestampCol: "TimeUnix", ValueCol: "Value",
				}
			},
		},
		{
			kind:  "RangeBucketFanout",
			token: "rbf",
			node: func(in chplan.Node) chplan.Node {
				return &chplan.RangeBucketFanout{
					Input: in, Start: shapeGridStart, End: shapeGridEnd, Step: shapeGridStep,
					Lookback:     shapeCarrierWindow,
					AnchorAlias:  "anchor_ts",
					TimestampCol: "TimeUnix",
				}
			},
		},
		{
			kind:  "AbsentOverTime",
			token: "aot",
			node: func(in chplan.Node) chplan.Node {
				return &chplan.AbsentOverTime{
					Input: in, Range: shapeCarrierWindow, Start: shapeGridStart, End: shapeGridEnd,
					Step: shapeGridStep, TimestampColumn: "TimeUnix", ValueColumn: "Value",
					MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
				}
			},
		},
		{
			kind:  "StepGrid",
			token: "sg",
			node: func(chplan.Node) chplan.Node {
				return &chplan.StepGrid{Start: shapeGridStart, End: shapeGridEnd, Step: shapeGridStep}
			},
		},
	}
}

// TestShapeModifiers_EveryGridCarrierGetsAToken asserts each
// [chplan.GridCarrier] kind contributes its modifier token to the shape id.
//
// Reverting the vocabulary widening drops the lwr / rbf / aot / sg rows: a
// histogram fan-out over a union collapses to the SAME id as a bare projection,
// so an operator clustering system.query_log by log_comment finds the expensive
// shapes hidden inside the cheap bucket and cannot tell the two apart at all.
//
// The tokens are per-kind rather than per-family because the per-anchor work
// differs by kind — a fan-out over histogram buckets and a last-value lookback
// cost nothing alike — and a shared token would merge exactly the populations
// an operator is trying to separate.
// hasModifierToken reports whether id carries token as a WHOLE modifier.
//
// A substring test does not discriminate: "rw" is a prefix of both "rwn" and
// "rwr", so a shape id that stamped a plain RangeWindow token for a native or
// resampled carrier would satisfy `strings.Contains(id, ";rw")` and the row
// would pass while the id it asserts on is wrong. Splitting on the modifier
// separator and comparing whole tokens is the only form that fails there.
func hasModifierToken(id, token string) bool {
	parts := strings.Split(id, ";")
	// parts[0] is the "cerb:<root>" prefix, never a modifier.
	for _, mod := range parts[1:] {
		if mod == token {
			return true
		}
	}
	return false
}

func TestShapeModifiers_EveryGridCarrierGetsAToken(t *testing.T) {
	t.Parallel()

	for _, tc := range carrierTokenCases() {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			// Wrapped in a Project so the carrier is a modifier, not the root
			// kind — the root token would otherwise mask a missing modifier.
			plan := &chplan.Project{Input: tc.node(&chplan.Scan{Table: "otel_metrics_sum"})}

			id := planShapeID(plan)
			if !hasModifierToken(id, tc.token) {
				t.Errorf("shape id %q for %s carries no %q modifier; the carrier is invisible to "+
					"log_comment clustering", id, tc.kind, tc.token)
			}
		})
	}
}

// TestShapeModifiers_DistinguishesFanoutFromBareProject is the concrete
// collision the missing tokens caused: a plan that demonstrably contains a
// range fan-out, a union and a join must not hash to the id of a plain
// Project over an Aggregate.
func TestShapeModifiers_DistinguishesFanoutFromBareProject(t *testing.T) {
	t.Parallel()

	groupBy := []chplan.Expr{
		&chplan.ColumnRef{Name: "MetricName"},
		&chplan.ColumnRef{Name: "Attributes"},
		&chplan.ColumnRef{Name: "ServiceName"},
	}
	unionScan := &chplan.Scan{
		Table:       "otel_metrics_exponential_histogram",
		UnionTables: []string{"otel_metrics_histogram"},
	}
	fanout := &chplan.RangeBucketFanout{
		Input: unionScan, Start: shapeGridStart, End: shapeGridEnd, Step: shapeGridStep,
		Lookback: shapeCarrierWindow, AnchorAlias: "anchor_ts", TimestampCol: "TimeUnix",
	}
	rich := &chplan.Project{
		Input: &chplan.Aggregate{
			GroupBy: groupBy,
			Input: &chplan.VectorJoin{
				Left:  fanout,
				Right: &chplan.StepGrid{Start: shapeGridStart, End: shapeGridEnd, Step: shapeGridStep},
			},
		},
	}
	bare := &chplan.Project{
		Input: &chplan.Aggregate{GroupBy: groupBy, Input: &chplan.Scan{Table: "otel_metrics_sum"}},
	}

	richID, bareID := planShapeID(rich), planShapeID(bare)
	if richID == bareID {
		t.Fatalf("fan-out + union + join and a bare aggregate share the shape id %q", richID)
	}
	const wantRich = "cerb:project;agg=3;rbf;sg;join;union"
	if richID != wantRich {
		t.Errorf("shape id = %q; want %q", richID, wantRich)
	}
}

// TestShapeModifiers_CoverEveryGridCarrier keeps the table above in lockstep
// with chplan: a carrier kind added there without a token here fails HERE,
// rather than silently shipping a plan shape that log_comment cannot see.
//
// It identifies a carrier the same way chplan's own completeness ratchet does —
// by the (Start, End, Step) grid field signature — because the registry that
// ratchet checks is unexported.
func TestShapeModifiers_CoverEveryGridCarrier(t *testing.T) {
	t.Parallel()

	want := gridDeclaringChplanStructs(t)
	got := make([]string, 0, len(carrierTokenCases()))
	for _, tc := range carrierTokenCases() {
		got = append(got, tc.kind)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("shape-id carrier table covers [%s]; chplan declares [%s]. Every GridCarrier kind "+
			"needs a modifier token in shapeModifiers.", strings.Join(got, ","), strings.Join(want, ","))
	}
}

// gridDeclaringChplanStructs parses internal/chplan and returns the sorted names
// of every struct declaring the (Start time.Time, End time.Time, Step
// time.Duration) grid triple — the field signature chplan's own GridCarrier
// completeness ratchet keys on.
func gridDeclaringChplanStructs(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join("..", "chplan")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			var hasStart, hasEnd, hasStep bool
			for _, fld := range st.Fields.List {
				typeName := fieldTypeName(fld.Type)
				for _, id := range fld.Names {
					switch {
					case id.Name == "Start" && typeName == "time.Time":
						hasStart = true
					case id.Name == "End" && typeName == "time.Time":
						hasEnd = true
					case id.Name == "Step" && typeName == "time.Duration":
						hasStep = true
					}
				}
			}
			if hasStart && hasEnd && hasStep {
				names = append(names, ts.Name.Name)
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatal("found no grid-declaring structs in internal/chplan; the scan is broken, not the vocabulary")
	}
	sort.Strings(names)
	return names
}

// fieldTypeName renders a qualified type expression as "pkg.Name", or "" for a
// shape this scan does not care about.
func fieldTypeName(e ast.Expr) string {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkg.Name + "." + sel.Sel.Name
}
