package promql

import (
	"errors"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestScalarOperandPolicyAndIdentity(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	mixedInput := func() *chplan.VectorSetOp {
		return &chplan.VectorSetOp{
			Mixed:            true,
			MetricNameColumn: s.MetricNameColumn,
			AttributesColumn: s.AttributesColumn,
			TimestampColumn:  s.TimestampColumn,
			ValueColumn:      s.ValueColumn,
		}
	}
	for _, site := range []mixedAdmissionSite{mixedOperandAdmission, mixedPlanAdmission} {
		key := mixedWrapperKey{family: mixedScalarFamily, site: site}
		t.Run(string(site), func(t *testing.T) {
			saved := mixedOperandPolicies[key]
			t.Cleanup(func() { mixedOperandPolicies[key] = saved })
			for _, mode := range []mixedOperandPolicy{mixedReject, mixedBespoke, mixedPreserve} {
				mixedOperandPolicies[key] = mode
				calls := 0
				var node chplan.Node
				var err error
				if site == mixedOperandAdmission {
					node, err = lowerScalarMixedOperand(func() (chplan.Node, error) {
						calls++
						return &chplan.OneRow{}, nil
					})
				} else {
					node, err = scalarFloatRows(mixedInput())
				}
				if err == nil || node != nil || calls != 0 {
					t.Errorf("site=%s mode=%v node=%T err=%v calls=%d", site, mode, node, err, calls)
				}
				for _, ordinary := range []chplan.Node{&chplan.OneRow{}, &chplan.VectorSetOp{
					Histogram:        true,
					MetricNameColumn: s.MetricNameColumn,
					AttributesColumn: s.AttributesColumn,
					TimestampColumn:  s.TimestampColumn,
					ValueColumn:      s.ValueColumn,
				}} {
					if got, ordinaryErr := scalarFloatRows(ordinary); ordinaryErr != nil || got != ordinary {
						t.Errorf("nonmixed operand changed: %T %v", got, ordinaryErr)
					}
				}
			}
		})
	}
	want := &chplan.OneRow{}
	wantErr := errors.New("original loader error")
	calls := 0
	got, err := lowerScalarMixedOperand(func() (chplan.Node, error) {
		calls++
		return want, wantErr
	})
	if got != want || err != wantErr || calls != 1 {
		t.Fatalf("direct identity: node=%T err=%v calls=%d", got, err, calls)
	}
	mixed := mixedInput()
	narrowed, err := scalarFloatRows(mixed)
	if err != nil {
		t.Fatal(err)
	}
	filter, ok := narrowed.(*chplan.Filter)
	if !ok || filter.Input != mixed || !chplan.IsMixedFloatNarrowing(filter) {
		t.Fatalf("generic operand must narrow completed union, got %#v", narrowed)
	}
}
