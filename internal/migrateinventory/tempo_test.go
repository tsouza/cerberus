package migrateinventory

import (
	"strings"
	"testing"
)

// TestTempoInventory_OutOfScopeReasonIsSpecific pins that the recorded reason
// names the actual mechanism (block-based spans, no head-block/ranked-stats
// API) rather than a generic "not supported" placeholder — a specific reason
// is the honesty contract this section exists to satisfy.
func TestTempoInventory_OutOfScopeReasonIsSpecific(t *testing.T) {
	inv := NewTempoInventory("http://tempo.example:3200")

	if inv.Source != "http://tempo.example:3200" {
		t.Errorf("source = %q, want the supplied base URL", inv.Source)
	}
	if inv.OutOfScope != TempoInventoryOutOfScopeReason {
		t.Errorf("out-of-scope reason = %q, want the package constant", inv.OutOfScope)
	}
	for _, mustContain := range []string{"block-based spans", "TSDB-head-block", "cardinality-stats"} {
		if !strings.Contains(inv.OutOfScope, mustContain) {
			t.Errorf("out-of-scope reason should name %q, got: %q", mustContain, inv.OutOfScope)
		}
	}
	if inv.OutOfScope == "not supported" || inv.OutOfScope == "" {
		t.Errorf("out-of-scope reason must not be a generic placeholder, got: %q", inv.OutOfScope)
	}
}
