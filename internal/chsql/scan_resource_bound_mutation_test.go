package chsql

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file kills the LIVED gremlins mutants in the scan_resource_bound
// cluster (phase-2 mutation lane, ./internal/chsql, 95% efficacy floor). The
// three emit-time fail-closed guards are pure functions with no golden/fixture
// coverage that isolates every branch, so each disjunct and boundary is pinned
// directly. Tests are white-box (package chsql) to reach the unexported guards.
//
// Mutant inventory (file:line:col → what the flip does):
//   - scan_resource_bound.go:97:23/29/38 — the requireScanResourceBound guard
//     `emitterSpansTable == "" || table != emitterSpansTable`. Negating either
//     conjunct, or turning `||` into `&&`, would either reject a scoped-out
//     table or wave through an unbounded scan over the enforced table.
//   - scan_resource_bound.go:129:22 — the requireInnerSpansScanBound guard
//     `spansTable == "" || findScanTable(inner) != spansTable`. `||`→`&&` would
//     reject a non-spans inner (or wave through the windowless spans inner).
//   - scan_resource_bound.go:211:19/25/34/60 — the requireSpansScanWindow guard
//     `ctxSpansTable == "" || table != ctxSpansTable || tsCol == ""`.
//   - scan_resource_bound.go:214:15/20/31 — the windowless-scan check
//     `startNano == 0 && endNano == 0`. `&&`→`||` would reject a one-sided
//     (still-pruning) window; negating either half would miss the both-zero
//     genuinely-windowless case.

const (
	scanBoundEnforcedTable = "otel_traces"
	scanBoundOtherTable    = "otel_metrics_sum"
	scanBoundTSColumn      = "Timestamp"
)

// TestRequireScanResourceBound_GuardBranches pins scan_resource_bound.go:97.
func TestRequireScanResourceBound_GuardBranches(t *testing.T) {
	t.Parallel()
	none := scanResourceBound{}  // zero value → kind spansBoundNone
	bounded := traceIDSetBound() // kind spansBoundTraceIDSet → a real bound

	// A none witness over a DIFFERENT table is scoped out → nil. `||`→`&&`
	// (@97:29) or negating `table != emitterSpansTable` (@97:38) would proceed
	// to the bound check and reject.
	if err := requireScanResourceBound(scanBoundEnforcedTable, scanBoundOtherTable, none); err != nil {
		t.Fatalf("none witness over a scoped-out table must return nil, got %v", err)
	}
	// A none witness over the ENFORCED table must be rejected. Negating
	// `emitterSpansTable == ""` (@97:23) or `table != emitterSpansTable`
	// (@97:38) would flip the guard true and wave it through as nil.
	if err := requireScanResourceBound(scanBoundEnforcedTable, scanBoundEnforcedTable, none); !errors.Is(err, ErrUnboundedSpansScan) {
		t.Fatalf("none witness over the enforced table must be rejected, got %v", err)
	}
	// Enforcement disabled (emitterSpansTable == "") with a none witness over an
	// empty table → nil. Negating `emitterSpansTable == ""` (@97:23) would send
	// this to the bound check and reject.
	if err := requireScanResourceBound("", "", none); err != nil {
		t.Fatalf("disabled enforcement must return nil, got %v", err)
	}
	// A real (trace-id set) bound over the enforced table passes (positive
	// control for the bound-check arm at line 100).
	if err := requireScanResourceBound(scanBoundEnforcedTable, scanBoundEnforcedTable, bounded); err != nil {
		t.Fatalf("bounded witness over the enforced table must pass, got %v", err)
	}
}

// TestRequireInnerSpansScanBound_GuardBranch pins scan_resource_bound.go:129.
func TestRequireInnerSpansScanBound_GuardBranch(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	zeroWin := &chplan.RangeWindow{} // Start/End zero
	fullWin := &chplan.RangeWindow{Start: start, End: start.Add(time.Hour)}
	spansInner := &chplan.Scan{Table: scanBoundEnforcedTable}
	otherInner := &chplan.Scan{Table: scanBoundOtherTable}

	// Inner scans a DIFFERENT table → scoped out → nil even with a zero window.
	// `||`→`&&` (@129:22) would proceed to the window check and reject.
	if err := requireInnerSpansScanBound(zeroWin, otherInner, scanBoundEnforcedTable); err != nil {
		t.Fatalf("non-spans inner must return nil, got %v", err)
	}
	// Inner scans the spans table with a zero window → rejected.
	if err := requireInnerSpansScanBound(zeroWin, spansInner, scanBoundEnforcedTable); !errors.Is(err, ErrUnboundedSpansScan) {
		t.Fatalf("windowless spans inner must be rejected, got %v", err)
	}
	// Inner scans the spans table with a full window → nil (positive control).
	if err := requireInnerSpansScanBound(fullWin, spansInner, scanBoundEnforcedTable); err != nil {
		t.Fatalf("windowed spans inner must pass, got %v", err)
	}
	// Enforcement disabled (spansTable == "") → nil.
	if err := requireInnerSpansScanBound(zeroWin, spansInner, ""); err != nil {
		t.Fatalf("disabled enforcement must return nil, got %v", err)
	}
}

// TestRequireSpansScanWindow_GuardBranches pins scan_resource_bound.go:211+214.
func TestRequireSpansScanWindow_GuardBranches(t *testing.T) {
	t.Parallel()

	// Disabled enforcement via the FIRST disjunct (ctxSpansTable == "", table
	// also "" so `table != ctxSpansTable` is false) → nil. `||`→`&&` (@211:25)
	// or negating `ctxSpansTable == ""` (@211:19) would fall to the window check
	// and reject the both-zero window.
	if err := requireSpansScanWindow("", "", scanBoundTSColumn, 0, 0); err != nil {
		t.Fatalf("disabled enforcement must return nil, got %v", err)
	}
	// Enforced table + opted-in tsCol + both bounds zero → windowless scan
	// rejected. This is the only input reaching the window check with all guard
	// disjuncts false, so it pins the table/tsCol negations (@211:34/60 — each
	// would flip the guard true and return nil) and the both-zero negations
	// (@214:15/31 — each would drop the both-zero case out of the error arm).
	if err := requireSpansScanWindow(scanBoundEnforcedTable, scanBoundEnforcedTable, scanBoundTSColumn, 0, 0); !errors.Is(err, ErrUnboundedSpansScan) {
		t.Fatalf("windowless recursive spans scan must be rejected, got %v", err)
	}
	// One-sided windows still prune partitions → nil. `&&`→`||` (@214:20) would
	// reject either one-sided window.
	if err := requireSpansScanWindow(scanBoundEnforcedTable, scanBoundEnforcedTable, scanBoundTSColumn, 1, 0); err != nil {
		t.Fatalf("start-only window must pass, got %v", err)
	}
	if err := requireSpansScanWindow(scanBoundEnforcedTable, scanBoundEnforcedTable, scanBoundTSColumn, 0, 1); err != nil {
		t.Fatalf("end-only window must pass, got %v", err)
	}
	// Full window → nil (positive control).
	if err := requireSpansScanWindow(scanBoundEnforcedTable, scanBoundEnforcedTable, scanBoundTSColumn, 1, 1); err != nil {
		t.Fatalf("full window must pass, got %v", err)
	}
}
