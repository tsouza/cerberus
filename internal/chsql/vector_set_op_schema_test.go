package chsql_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

func setOpSchemaProject(columns []chplan.Column) chplan.Node {
	projections := make([]chplan.Projection, len(columns))
	for i, column := range columns {
		projections[i] = chplan.Projection{
			Expr:  &chplan.ColumnRef{Name: column.Name},
			Alias: column.Name,
		}
	}
	return &chplan.Project{Input: &chplan.OneRow{}, Projections: projections, Roles: columns}
}

func schemaRoutedSetOp(left, right chplan.Node) *chplan.VectorSetOp {
	return &chplan.VectorSetOp{
		Left: left, Right: right, Op: chplan.VectorSetOr,
		MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}
}

func TestEmitVectorSetOpCanonicalizesCustomRoleNames(t *testing.T) {
	t.Parallel()
	custom := []chplan.Column{
		{Name: "source_name", Role: chplan.RoleMetricName},
		{Name: "source_labels", Role: chplan.RoleAttributes},
		{Name: "source_time", Role: chplan.RoleTimestamp},
		{Name: "source_value", Role: chplan.RoleValue},
	}

	sql, _, err := chsql.Emit(context.Background(), schemaRoutedSetOp(
		setOpSchemaProject(custom),
		setOpSchemaProject(custom),
	))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range []string{
		"`source_name` AS `MetricName`",
		"`source_labels` AS `Attributes`",
		"`source_time` AS `TimeUnix`",
		"`source_value` AS `Value`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("SQL missing role alias %q: %s", want, sql)
		}
	}
}

func TestEmitVectorSetOpCanonicalizesCustomAnchorRole(t *testing.T) {
	t.Parallel()
	custom := []chplan.Column{
		{Name: "source_labels", Role: chplan.RoleAttributes},
		{Name: "source_anchor", Role: chplan.RoleAnchor},
		{Name: "source_value", Role: chplan.RoleValue},
	}

	sql, _, err := chsql.Emit(context.Background(), schemaRoutedSetOp(
		setOpSchemaProject(custom),
		setOpSchemaProject(custom),
	))
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if want := "`source_anchor` AS `TimeUnix`"; !strings.Contains(sql, want) {
		t.Fatalf("SQL missing anchor role alias %q: %s", want, sql)
	}
}

func TestEmitVectorSetOpRejectsOpaqueAndInvalidArms(t *testing.T) {
	t.Parallel()
	valid := setOpSchemaProject(setOpTestRoles())
	invalid := append(setOpTestRoles(), chplan.Column{Name: "other_value", Role: chplan.RoleValue})

	for _, tc := range []struct {
		name string
		arm  chplan.Node
	}{
		{name: "opaque", arm: setOpSchemaProject([]chplan.Column{{Name: "private"}})},
		{name: "open", arm: &chplan.Scan{Table: "samples", Roles: setOpTestRoles()}},
		{name: "invalid", arm: setOpSchemaProject(invalid)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := chsql.Emit(context.Background(), schemaRoutedSetOp(tc.arm, valid))
			if !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestEmitVectorSetOpRejectsOutputKindFlagMismatch(t *testing.T) {
	t.Parallel()
	floatArm := setOpSchemaProject(setOpTestRoles())
	histogramColumns := append(setOpTestRoles(), chplan.HistogramPayloadColumns()...)
	histogramArm := setOpSchemaProject(histogramColumns)
	mixedColumns := append(slices.Clone(histogramColumns), chplan.Column{
		Name: chplan.MixedDiscriminatorColumn,
		Role: chplan.RoleDiscriminator,
	})
	mixedArm := setOpSchemaProject(mixedColumns)

	for _, tc := range []struct {
		name string
		plan *chplan.VectorSetOp
	}{
		{name: "plain_or_histograms", plan: schemaRoutedSetOp(histogramArm, histogramArm)},
		{name: "histogram_or_floats", plan: func() *chplan.VectorSetOp {
			plan := schemaRoutedSetOp(floatArm, floatArm)
			plan.Histogram = true
			return plan
		}()},
		{name: "plain_and_forwarded_mixed", plan: func() *chplan.VectorSetOp {
			plan := schemaRoutedSetOp(mixedArm, floatArm)
			plan.Op = chplan.VectorSetAnd
			return plan
		}()},
		{name: "mixed_or_floats", plan: func() *chplan.VectorSetOp {
			plan := schemaRoutedSetOp(floatArm, floatArm)
			plan.Mixed = true
			return plan
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := chsql.Emit(context.Background(), tc.plan); !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit = %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestEmitNaryVectorSetOpPropagatesUntrustedArm(t *testing.T) {
	t.Parallel()
	n := naryOp(chplan.VectorSetOr, "a", "b")
	n.Arms[1] = setOpSchemaProject([]chplan.Column{{Name: "private"}})
	if _, _, err := chsql.Emit(context.Background(), n); !errors.Is(err, chsql.ErrUnsupported) {
		t.Fatalf("Emit = %v, want ErrUnsupported", err)
	}
}

func TestEmitNaryVectorSetOpRejectsArmKindFlagMismatch(t *testing.T) {
	t.Parallel()
	histogramColumns := append(setOpTestRoles(), chplan.HistogramPayloadColumns()...)
	mixedColumns := append(slices.Clone(histogramColumns), chplan.Column{
		Name: chplan.MixedDiscriminatorColumn,
		Role: chplan.RoleDiscriminator,
	})

	for _, tc := range []struct {
		name      string
		arm       chplan.Node
		histogram bool
	}{
		{name: "plain_with_histogram", arm: setOpSchemaProject(histogramColumns)},
		{name: "plain_with_mixed", arm: setOpSchemaProject(mixedColumns)},
		{name: "histogram_with_float", arm: setOpSchemaProject(setOpTestRoles()), histogram: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := naryOp(chplan.VectorSetOr, "a", "b")
			n.Histogram = tc.histogram
			n.Arms[1] = tc.arm
			if _, _, err := chsql.Emit(context.Background(), n); !errors.Is(err, chsql.ErrUnsupported) {
				t.Fatalf("Emit = %v, want ErrUnsupported", err)
			}
		})
	}
}
