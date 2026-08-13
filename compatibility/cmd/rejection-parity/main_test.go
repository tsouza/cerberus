package main

import (
	"strings"
	"testing"
	"time"
)

// The verdict vocabulary is this tool's whole output: the scorer, the
// ratchet and the exit code all read it. A misclassified status pair is
// therefore not a cosmetic bug — it either hides a divergence that closed
// (so the catalogue rots) or fails a run for a divergence that is still
// exactly where the entry says it is.
func TestDivergenceVerdictClassifiesEachStatusPair(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		cerbStatus int
		refStatus  int
		want       string
		wantDetail bool
	}{
		{"cerberus now accepts", 200, 200, "divergence_resolved", true},
		{"still divergent", 400, 200, "divergence_confirmed", false},
		{"reference now rejects too", 400, 422, "divergence_closed", true},
		{"neither side is a 2xx or a 4xx", 500, 503, "hard_error", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, detail := divergenceVerdict(tc.cerbStatus, tc.refStatus, []byte("ref body"), []byte("cerb body"))
			if verdict != tc.want {
				t.Fatalf("verdict = %q, want %q", verdict, tc.want)
			}
			if got := detail != ""; got != tc.wantDetail {
				t.Fatalf("detail %q: has detail = %v, want %v", detail, got, tc.wantDetail)
			}
		})
	}
}

// A cerberus 2xx wins over a reference 2xx: the entry claims cerberus
// rejects, so cerberus answering is the resolution regardless of what the
// reference did. Pinned separately because the switch's arm order is the
// only thing that expresses it.
func TestDivergenceVerdictPrefersTheCerberusSideResolution(t *testing.T) {
	t.Parallel()

	if verdict, _ := divergenceVerdict(200, 400, nil, nil); verdict != "divergence_resolved" {
		t.Fatalf("verdict = %q, want divergence_resolved", verdict)
	}
}

func TestResolveEvalTimeDefaultsToNowAndRejectsNonRFC3339(t *testing.T) {
	t.Parallel()

	now, err := resolveEvalTime("  ")
	if err != nil {
		t.Fatalf("blank -eval-time: %v", err)
	}
	if now.IsZero() || now.Location() != time.UTC {
		t.Fatalf("blank -eval-time = %v (UTC=%v), want a non-zero UTC instant", now, now.Location())
	}

	fixed, err := resolveEvalTime("2026-01-02T03:04:05+01:00")
	if err != nil {
		t.Fatalf("RFC3339 -eval-time: %v", err)
	}
	if want := time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC); !fixed.Equal(want) || fixed.Location() != time.UTC {
		t.Fatalf("parsed -eval-time = %v, want %v in UTC", fixed, want)
	}

	// A malformed flag must stop the run rather than silently evaluating at
	// "now" — the eval-time-sensitive guards are exactly the ones a wrong
	// window makes structurally unreachable.
	if _, err := resolveEvalTime("yesterday"); err == nil {
		t.Fatal("resolveEvalTime(\"yesterday\") = nil error, want a parse failure")
	}
}

func TestSnippetTrimsAndBoundsTheBodyItQuotes(t *testing.T) {
	t.Parallel()

	if got := snippet([]byte("  boom\n")); got != "boom" {
		t.Fatalf("snippet = %q, want %q", got, "boom")
	}
	long := snippet([]byte(strings.Repeat("a", 512)))
	if !strings.HasSuffix(long, "…") {
		t.Fatalf("a long body was not marked as truncated: %q", long)
	}
	if got := len([]rune(long)); got != 301 {
		t.Fatalf("truncated snippet = %d runes, want 301 (300 + the ellipsis)", got)
	}
}

// Fatal decides the exit code, so the three fatal verdicts and the
// non-fatal ones are pinned in both directions: a hard error is a run the
// harness could not score, not a claim this package made and got wrong.
func TestReportFatalOnlyForFalsifiedClaims(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		rep  Report
		want bool
	}{
		{"clean", Report{Total: 10, Parity: 10}, false},
		{"wrong rejection", Report{WrongRejection: 1}, true},
		{"divergence resolved", Report{DivergenceResolved: 1}, true},
		{"divergence closed", Report{DivergenceClosed: 1}, true},
		{"divergence confirmed", Report{DivergenceConfirmed: 7}, false},
		{"stale catalogue", Report{StaleCatalogue: 3}, false},
		{"hard errors", Report{HardErrors: 4}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.rep.Fatal(); got != tc.want {
				t.Fatalf("Fatal() = %v, want %v", got, tc.want)
			}
		})
	}
}
