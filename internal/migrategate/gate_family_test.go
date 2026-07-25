package migrategate_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/migrategate"
	"github.com/tsouza/cerberus/internal/migrateverify"
)

// TestEvalVerify_DeadSiblingFamilyBlocksOnAHealthyHead pins the masking rule one
// level below the per-lane one. A single loki lane compared 12 log entries and
// zero series: the head's own evidence counter is non-zero, so both the aggregate
// guard and any per-HEAD guard clear it, and only the per-family rule sees that
// the operator's metric panels were never proved.
func TestEvalVerify_DeadSiblingFamilyBlocksOnAHealthyHead(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 2, Match: 2, ComparedUnits: 12},
		Heads: []migrateverify.HeadSummary{{
			Head: migrateverify.HeadLoki, Configured: true,
			Summary: migrateverify.Summary{Total: 2, Match: 2, ComparedUnits: 12},
			Families: []migrateverify.FamilySummary{
				{Kind: migrateverify.KindLogStream, Unit: migrateverify.UnitLogEntries, Total: 1, Match: 1, Compared: 12},
				{Kind: migrateverify.KindMetricMatrix, Unit: migrateverify.UnitSeries, Total: 1, Match: 1},
			},
		}},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("a family that compared nothing must block, however healthy its sibling: %+v", dec)
	}
	st := verifyStage(t, dec)
	if !st.Blocking {
		t.Errorf("verify stage must block, got %+v", st)
	}
	if !containsSubstr(st.Reasons, "head loki family metric-matrix compared nothing: 0 series were diffed") {
		t.Errorf("the reason must name the head AND the shape, in that shape's unit, got %+v", st.Reasons)
	}
	if containsSubstr(st.Reasons, "family log-stream compared nothing") {
		t.Errorf("the healthy log-stream family must not be accused, got %+v", st.Reasons)
	}
}

// TestEvalVerify_HeadWithNoFamilyPartitionBlocks pins that a report claiming
// replays without saying what shape they were is not evidence. Nothing attributes
// its compared count to a comparator, so the per-family rule has nothing to check,
// and the run must not pass on the strength of an unattributed number.
func TestEvalVerify_HeadWithNoFamilyPartitionBlocks(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 3, Match: 3, ComparedSeries: 7, ComparedUnits: 7},
		Heads: []migrateverify.HeadSummary{{
			Head: migrateverify.HeadProm, Configured: true,
			Summary: migrateverify.Summary{Total: 3, Match: 3, ComparedSeries: 7, ComparedUnits: 7},
		}},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("a head that replayed queries without reporting any family must block: %+v", dec)
	}
	if !containsSubstr(verifyStage(t, dec).Reasons, "head prom family (unattributed) compared nothing") {
		t.Errorf("the reason must say the evidence could not be attributed to any shape, got %+v", verifyStage(t, dec).Reasons)
	}
}

// TestEvalVerify_UndecidableWarnsAndDoesNotBlock pins the undecidable verdict's
// two halves: it is honest ("I could not judge this in full"), so it does not
// block on its own — and it is never silent, so the decision names it alongside
// every limitation that produced it.
func TestEvalVerify_UndecidableWarnsAndDoesNotBlock(t *testing.T) {
	limits := []migrateverify.Limitation{{
		Code: migrateverify.LimitLogEntryOrder, Count: 12,
		Detail: "entry order within a stream was not compared",
	}}
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 2, Match: 1, Undecidable: 1, ComparedUnits: 12, Limitations: limits},
		Heads: []migrateverify.HeadSummary{{
			Head: migrateverify.HeadLoki, Configured: true,
			Summary: migrateverify.Summary{Total: 2, Match: 1, Undecidable: 1, ComparedUnits: 12, Limitations: limits},
			Families: []migrateverify.FamilySummary{{
				Kind: migrateverify.KindLogStream, Unit: migrateverify.UnitLogEntries,
				Total: 2, Match: 1, Undecidable: 1, Compared: 12, Limitations: limits,
			}},
		}},
	}
	dec := evalWithVerify(t, rep)
	st := verifyStage(t, dec)
	if st.Verdict != migrategate.VerdictWarn || st.Blocking {
		t.Errorf("an undecidable comparison that still compared 12 units WARNs, got %q blocking=%v reasons=%+v",
			st.Verdict, st.Blocking, st.Reasons)
	}
	if !containsSubstr(st.Reasons, "reached no parity claim") {
		t.Errorf("the caveat must say the run reached no parity claim on those queries, got %+v", st.Reasons)
	}
	if !containsSubstr(st.Reasons, "["+migrateverify.LimitLogEntryOrder+"] covering 12 compared units") {
		t.Errorf("every limitation must be named with the count it covers, got %+v", st.Reasons)
	}
}

// TestEvalVerify_UndecidableWithNoEvidenceStillBlocks is the other half: an
// undecidable verdict must never green-light a family that compared nothing. The
// two rules are independent on purpose — the verdict says what was concluded, the
// evidence counter says whether anything was examined.
func TestEvalVerify_UndecidableWithNoEvidenceStillBlocks(t *testing.T) {
	rep := migrateverify.Report{
		Summary: migrateverify.Summary{Total: 2, Undecidable: 2},
		Heads: []migrateverify.HeadSummary{{
			Head: migrateverify.HeadLoki, Configured: true,
			Summary: migrateverify.Summary{Total: 2, Undecidable: 2},
			Families: []migrateverify.FamilySummary{{
				Kind: migrateverify.KindLogStream, Unit: migrateverify.UnitLogEntries, Total: 2, Undecidable: 2,
			}},
		}},
	}
	dec := evalWithVerify(t, rep)
	if dec.Pass {
		t.Fatalf("an undecidable-only run that compared nothing must block: %+v", dec)
	}
	if !verifyStage(t, dec).Blocking {
		t.Errorf("verify stage must block, got %+v", verifyStage(t, dec))
	}
}
