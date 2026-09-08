package migrateverify

import (
	"strings"
	"testing"
)

// TestEmptyMatch_SeparatesAgreementFromEvidence pins the distinction the
// verify report exists to make: a match over two EMPTY results compared
// nothing, so it is agreement without evidence.
//
// ComparedUnits is a sum, so one query that diffed three series satisfied
// every aggregate and per-family evidence rule for its whole family. A run
// of thirty replayed queries where twenty-nine returned empty on both
// backends therefore printed "VERIFICATION PASSED — all 30 queries matched"
// and passed the cutover gate on the strength of one. That is the realistic
// shape of a stale dashboard corpus, a wrong tenant header, or a replay
// window that starts before ingest did.
func TestEmptyMatch_SeparatesAgreementFromEvidence(t *testing.T) {
	t.Parallel()

	const (
		total          = 30
		withEvidence   = 1
		comparedSeries = 3
	)

	rr := &reportRun{byHead: map[string]*headAccum{}}
	rr.record(QueryResult{
		Head: HeadProm, Kind: "matrix", Verdict: VerdictMatch,
		ComparedUnits: comparedSeries, ComparedSeries: comparedSeries,
	})
	for range total - withEvidence {
		rr.record(QueryResult{
			Head: HeadProm, Kind: "matrix", Verdict: VerdictMatch,
			ComparedUnits: 0, ComparedSeries: 0,
		})
	}

	s := rr.rep.Summary

	if s.Total != total || s.Match != total {
		t.Fatalf("Total=%d Match=%d, want %d/%d", s.Total, s.Match, total, total)
	}
	if s.EmptyMatch != total-withEvidence {
		t.Errorf(
			"Summary.EmptyMatch = %d, want %d — a match over two empty results compared nothing "+
				"and must be counted apart from one that did",
			s.EmptyMatch, total-withEvidence,
		)
	}

	fam := rr.family(HeadProm, "matrix")
	if fam.EmptyMatch != total-withEvidence {
		t.Errorf("FamilySummary.EmptyMatch = %d, want %d", fam.EmptyMatch, total-withEvidence)
	}
}

// TestMatchEvidencePhrase_SaysWhatTheAgreementRestsOn pins the banner text
// an operator reads before a cutover. "all N queries matched" is true and
// misleading when most of those queries compared nothing.
func TestMatchEvidencePhrase_SaysWhatTheAgreementRestsOn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		summary Summary
		want    string
	}{
		{
			name:    "every match compared something",
			summary: Summary{Total: 30, Match: 30, EmptyMatch: 0},
			want:    "all over compared results",
		},
		{
			name:    "one of thirty compared something",
			summary: Summary{Total: 30, Match: 30, EmptyMatch: 29},
			want:    "1 over compared results, 29 over empty ones",
		},
		{
			name:    "nothing compared anything",
			summary: Summary{Total: 30, Match: 30, EmptyMatch: 30},
			want:    "NONE over compared results — every match was empty on both backends",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := matchEvidencePhrase(tc.summary); got != tc.want {
				t.Errorf("matchEvidencePhrase() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerificationBanner_NamesTheEvidenceSplit is the end-to-end half: the
// rendered report, not just the helper, must carry the split.
func TestVerificationBanner_NamesTheEvidenceSplit(t *testing.T) {
	t.Parallel()

	rep := Report{Summary: Summary{Total: 30, Match: 30, EmptyMatch: 29}}

	var sb strings.Builder
	if err := rep.WriteText(&sb); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := sb.String()
	if !strings.Contains(out, "29 over empty ones") {
		t.Errorf(
			"the banner does not say how much of the agreement rests on evidence:\n%s",
			firstLines(out, 3),
		)
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
