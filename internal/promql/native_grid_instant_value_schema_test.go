package promql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestNativeTSGridInstantNodeCarriesClosedValueRole(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	roles := metricRoles(s)
	names := make([]string, len(roles))
	for i, role := range roles {
		names[i] = role.Name
	}
	rw := &chplan.RangeWindow{
		Input: &chplan.Scan{
			Table:   s.GaugeTable,
			Columns: names,
			Roles:   roles,
		},
		Func:            "rate",
		Range:           5 * time.Minute,
		End:             time.Unix(1_700_000_000, 0).UTC(),
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}

	native := nativeTSGridInstantNode(rw, "rate", s)
	if native == nil {
		t.Fatal("eligible instant grid did not lower natively")
	}
	value, ok := native.InputValueColumn()
	if !ok || value != s.ValueColumn {
		t.Fatalf("InputValueColumn() = (%q, %v), want (%q, true)", value, ok, s.ValueColumn)
	}
}
