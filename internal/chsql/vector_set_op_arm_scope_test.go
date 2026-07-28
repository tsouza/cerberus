package chsql_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// logsTimestampColumn is the OTel-CH logs table's sample timestamp
// column. It differs from the canonical Sample slot name (TimeUnix)
// the metrics schema uses, which is what makes the LogQL head the one
// that exposes a set-op arm reading the wrong scope.
const logsTimestampColumn = "Timestamp"

// canonicalTimestampColumn is the Sample-tuple timestamp slot every
// set-op arm projection must expose, whatever the arm's own schema
// calls the column underneath.
const canonicalTimestampColumn = "TimeUnix"

// setOpArmMatrixRangeWindow builds the LogQL range-mode arm shape: a
// matrix RangeWindow over the logs table (whose own timestamp column is
// `Timestamp`) wrapped in the canonical-Sample Project that the LogQL
// lowering stacks on top of every range aggregation. That Project is
// the arm's outermost SELECT, and it re-aliases the grid anchor to
// `TimeUnix` — so the arm's scope exposes `TimeUnix` and nothing named
// `Timestamp`.
func setOpArmMatrixRangeWindow(table string) chplan.Node {
	rw := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: table},
		Func:            "rate",
		Range:           5 * time.Minute,
		Step:            time.Minute,
		OuterRange:      3 * time.Minute,
		TimestampColumn: logsTimestampColumn,
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "ResourceAttributes"}},
	}
	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: &chplan.ColumnRef{Name: "ResourceAttributes"}, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: canonicalTimestampColumn},
			{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
		},
	}
}

// armScopeSetOp wires two such arms into a step-aligned VectorSetOp,
// the shape a LogQL range query over `rate({…}[5m]) <op> rate({…}[5m])`
// lowers to.
func armScopeSetOp(op chplan.VectorSetOpKind) *chplan.VectorSetOp {
	return &chplan.VectorSetOp{
		Left:             setOpArmMatrixRangeWindow("otel_logs"),
		Right:            setOpArmMatrixRangeWindow("otel_logs"),
		Op:               op,
		StepAligned:      true,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  canonicalTimestampColumn,
		ValueColumn:      "Value",
	}
}

// timestampAliasRE matches every `<ident> AS <canonical timestamp>`
// projection in the emitted SQL and captures the source identifier.
var timestampAliasRE = regexp.MustCompile("`([A-Za-z_][A-Za-z0-9_]*)` AS `" + canonicalTimestampColumn + "`")

// TestEmitVectorSetOp_ArmTimestampBindsToArmScope pins the identifier a
// set-op arm's canonical projection reads its timestamp from.
//
// The arm's outermost node is a Project that already exposes the
// canonical Sample tuple, so its SELECT surfaces the timestamp under
// the set op's TimestampColumn. Classifying the arm by the RangeWindow
// buried underneath instead makes the projection read that node's own
// column name — `Timestamp` for the logs schema — which the arm's scope
// never exposes, and ClickHouse rejects the statement with code 47
// "Unknown expression identifier `Timestamp`". chDB resolves the same
// reference leniently, so only a strict executor or this assertion
// catches it.
func TestEmitVectorSetOp_ArmTimestampBindsToArmScope(t *testing.T) {
	t.Parallel()

	for _, op := range []chplan.VectorSetOpKind{
		chplan.VectorSetAnd,
		chplan.VectorSetOr,
		chplan.VectorSetUnless,
	} {
		t.Run(string(op), func(t *testing.T) {
			t.Parallel()
			assertTimestampAliasesBindToArmScope(t, emitSetOp(t, armScopeSetOp(op)), 2)
		})
	}
}

// TestEmitNaryVectorSetOp_ArmTimestampBindsToArmScope pins the same
// contract on the flattened N-ary chain, which reuses the binary
// emitter's arm canonicalisation.
func TestEmitNaryVectorSetOp_ArmTimestampBindsToArmScope(t *testing.T) {
	t.Parallel()

	n := &chplan.NaryVectorSetOp{
		Arms: []chplan.Node{
			setOpArmMatrixRangeWindow("otel_logs"),
			setOpArmMatrixRangeWindow("otel_logs"),
			setOpArmMatrixRangeWindow("otel_logs"),
		},
		Op:               chplan.VectorSetOr,
		StepAligned:      true,
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  canonicalTimestampColumn,
		ValueColumn:      "Value",
	}
	sql, _, err := chsql.Emit(context.Background(), n)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	assertTimestampAliasesBindToArmScope(t, sql, 3)
}

// canonicalArmProjection is the arm-level SELECT the emitter must
// produce: the four canonical slots read straight through, stacked
// directly on the arm's own canonical Project (which is what surfaces
// `anchor_ts AS TimeUnix`). On the broken emitter the third slot reads
// `Timestamp AS TimeUnix` instead.
const canonicalArmProjection = "SELECT `MetricName`, `Attributes`, `" + canonicalTimestampColumn + "`, `Value` " +
	"FROM (SELECT ? AS `MetricName`, `ResourceAttributes` AS `Attributes`, `anchor_ts` AS `" + canonicalTimestampColumn + "`"

// assertTimestampAliasesBindToArmScope checks that no projection aliases
// the arm's underlying schema timestamp column into the canonical slot,
// and that every arm's canonical projection reads the column the arm
// actually exposes.
func assertTimestampAliasesBindToArmScope(t *testing.T, sql string, arms int) {
	t.Helper()

	for _, m := range timestampAliasRE.FindAllStringSubmatch(sql, -1) {
		if m[1] == logsTimestampColumn {
			t.Errorf("arm projection reads %q, an identifier the arm's own SELECT does not "+
				"expose (it surfaces the anchor as %q); ClickHouse rejects this with code 47. sql=%s",
				logsTimestampColumn, canonicalTimestampColumn, sql)
		}
	}

	if got := strings.Count(sql, canonicalArmProjection); got != arms {
		t.Errorf("canonical arm projection count = %d, want %d (%q); sql=%s",
			got, arms, canonicalArmProjection, sql)
	}
}
