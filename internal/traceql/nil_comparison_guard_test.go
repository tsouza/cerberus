package traceql

import (
	"strings"
	"testing"

	traceql "github.com/tsouza/cerberus/internal/traceql/ast"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerNilComparisonGuardsRejectOpNotExists pins the lowering's own
// copy of the reference's `<x> = nil` rule.
//
// Since #3260 the rule's primary home is the parse-stage validator
// (internal/traceql/ast.validateNilComparison), which is the stage the
// reference applies it in and the one whose errors the Tempo head
// answers 400. No query reaching Lower can therefore carry an
// OpNotExists over an intrinsic or over resource.service.name, and
// these two guards are unreachable from the wire — exactly as the
// reference's own second copy in tempodb/encoding/vparquet4's
// `checkConditions` is unreachable behind pkg/traceql/ast_validate.go.
//
// They are kept rather than deleted because falling through them is
// silently WRONG rather than merely unhandled: everything below the
// intrinsic guard computes an OpExists answer, so an OpNotExists that
// slipped past would lower to `true` — a filter matching every span —
// and the resource.service.name guard would emit a
// `NOT mapContains(ResourceAttributes, 'service.name')` that is false
// on every conforming span. This test is what keeps them honest: it
// calls them directly, past Parse, so "unreachable" never decays into
// "untested" (a guard no test can fail is a gap, not a defence).
func TestLowerNilComparisonGuardsRejectOpNotExists(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	t.Run("intrinsic", func(t *testing.T) {
		t.Parallel()
		// Every intrinsic, so the guard cannot be narrowed to the
		// handful the switch beneath it names.
		for _, in := range []traceql.Intrinsic{
			traceql.IntrinsicKind,
			traceql.IntrinsicName,
			traceql.IntrinsicStatus,
			traceql.IntrinsicDuration,
			traceql.IntrinsicChildCount,
			traceql.IntrinsicEventName,
			traceql.IntrinsicLinkTraceID,
			traceql.IntrinsicNestedSetLeft,
		} {
			attr := traceql.NewIntrinsic(in)
			_, err := lowerNilComparison(traceql.OpNotExists, attr, s)
			if err == nil {
				t.Errorf("lowerNilComparison(OpNotExists, %s): want a rejection, got none", in)
				continue
			}
			if !strings.Contains(err.Error(), "intrinsics cannot be nil") {
				t.Errorf("lowerNilComparison(OpNotExists, %s) error = %q, want it to name the intrinsic rule", in, err)
			}
		}
	})

	t.Run("resource_service_name", func(t *testing.T) {
		t.Parallel()
		attr := traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, "service.name")
		_, err := lowerNilComparison(traceql.OpNotExists, attr, s)
		if err == nil {
			t.Fatal("lowerNilComparison(OpNotExists, resource.service.name): want a rejection, got none")
		}
		if !strings.Contains(err.Error(), "resource.service.name cannot be nil") {
			t.Errorf("lowerNilComparison error = %q, want it to name the resource.service.name rule", err)
		}
	})

	// The other half: OpExists over the same operands must still lower,
	// so the guards cannot be widened into a blanket nil-comparison
	// rejection that would take `{ kind != nil }` off every Traces
	// Drilldown breakdown panel.
	t.Run("op_exists_still_lowers", func(t *testing.T) {
		t.Parallel()
		for _, attr := range []traceql.Attribute{
			traceql.NewIntrinsic(traceql.IntrinsicKind),
			traceql.NewIntrinsic(traceql.IntrinsicChildCount),
			traceql.NewScopedAttribute(traceql.AttributeScopeResource, false, "service.name"),
			traceql.NewScopedAttribute(traceql.AttributeScopeSpan, false, "foo"),
		} {
			if _, err := lowerNilComparison(traceql.OpExists, attr, s); err != nil {
				t.Errorf("lowerNilComparison(OpExists, %s) = %v, want it to lower", attr, err)
			}
		}
	})
}
