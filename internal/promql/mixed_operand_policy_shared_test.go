package promql

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestMixedOperandPolicySharedBuildersRejectBeforeCallback(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	inner := &chplan.VectorSetOp{
		Mixed:            true,
		MetricNameColumn: s.MetricNameColumn,
		AttributesColumn: s.AttributesColumn,
		TimestampColumn:  s.TimestampColumn,
		ValueColumn:      s.ValueColumn,
	}
	const unknown mixedWrapperFamily = "unlisted-wrapper"
	valueCalled := false
	value, err := guardedValueProjection(inner, nil, s, lowerCtx{}, unknown, func(sampleRoleRefs) chplan.Expr {
		valueCalled = true
		return &chplan.LitFloat{V: 1}
	})
	if err == nil || value != nil || valueCalled {
		t.Fatalf("guarded value: node=%T error=%v callback=%v", value, err, valueCalled)
	}
	attributesCalled := false
	attributes, err := projectAttributesOverInner(inner, s, unknown, func(refs sampleRoleRefs) chplan.Expr {
		attributesCalled = true
		return refs.Attributes
	})
	if err == nil || attributes != nil || attributesCalled {
		t.Fatalf("attribute builder: node=%T error=%v callback=%v", attributes, err, attributesCalled)
	}
}

func TestMixedOperandPolicySharedBuildersLeaveOrdinaryFloatUnrestricted(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	inner := sampleForwardTestInput(metricRoles(s)...)
	const unknown mixedWrapperFamily = "unlisted-wrapper"
	valueCalls := 0
	value, err := guardedValueProjection(inner, nil, s, lowerCtx{}, unknown, func(refs sampleRoleRefs) chplan.Expr {
		valueCalls++
		return refs.Value
	})
	if err != nil || value == nil || valueCalls != 1 {
		t.Fatalf("ordinary value: node=%T error=%v calls=%d", value, err, valueCalls)
	}
	attributeCalls := 0
	attributes, err := projectAttributesOverInner(inner, s, unknown, func(refs sampleRoleRefs) chplan.Expr {
		attributeCalls++
		return refs.Attributes
	})
	if err != nil || attributes == nil || attributeCalls != 1 {
		t.Fatalf("ordinary attributes: node=%T error=%v calls=%d", attributes, err, attributeCalls)
	}
}
