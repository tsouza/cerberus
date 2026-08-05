package promql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"
)

// TestWireArms_ClassifiesEachDomain pins the four-way split: a pinned
// `__name__=` matcher classifies WireArmWireNamePinned, an unpinned
// `__name__` matcher classifies WireArmWireNameUnpinned, an `le` matcher
// classifies WireArmWireBound, and everything else classifies
// WireArmStorage — in original order. This is the direct fix for the
// #1756 hard constraint: the pre-migration skeleton keyed its switch on
// m.Name alone and could not distinguish a pinned matcher from an
// unpinned one, the entire distinction the wire-name family turns on.
func TestWireArms_ClassifiesEachDomain(t *testing.T) {
	t.Parallel()

	nameEq := mustMatcher(t, labels.MatchEqual, "__name__", "demo_api_request_duration_seconds_bucket")
	nameRe := mustMatcher(t, labels.MatchRegexp, "__name__", "demo_api.*")
	nameNeq := mustMatcher(t, labels.MatchNotEqual, "__name__", "demo_api_request_duration_seconds_bucket")
	nameNre := mustMatcher(t, labels.MatchNotRegexp, "__name__", "demo_api.*")
	le := mustMatcher(t, labels.MatchEqual, "le", "0.5")
	job := mustMatcher(t, labels.MatchEqual, "job", "api")

	got := wireArms([]*labels.Matcher{nameEq, le, job, nameRe, nameNeq, nameNre})

	want := []WireArm{
		WireArmWireNamePinned,
		WireArmWireBound,
		WireArmStorage,
		WireArmWireNameUnpinned,
		WireArmWireNameUnpinned,
		WireArmWireNameUnpinned,
	}
	if len(got.Arms) != len(want) {
		t.Fatalf("Arms len = %d, want %d", len(got.Arms), len(want))
	}
	for i, w := range want {
		if got.Arms[i] != w {
			t.Errorf("Arms[%d] (matcher %s) = %s, want %s", i, got.Matchers[i], got.Arms[i], w)
		}
	}

	if diff := len(got.WireName()); diff != 4 {
		t.Errorf("WireName() len = %d, want 4", diff)
	}
	if diff := len(got.WireNamePinned()); diff != 1 {
		t.Errorf("WireNamePinned() len = %d, want 1", diff)
	}
	if diff := len(got.WireNameUnpinned()); diff != 3 {
		t.Errorf("WireNameUnpinned() len = %d, want 3", diff)
	}
	if diff := len(got.WireBound()); diff != 1 {
		t.Errorf("WireBound() len = %d, want 1", diff)
	}
	if diff := len(got.Storage()); diff != 1 {
		t.Errorf("Storage() len = %d, want 1", diff)
	}
	if got.Storage()[0] != job {
		t.Errorf("Storage()[0] = %v, want the job matcher", got.Storage()[0])
	}
	if got.WireNamePinned()[0] != nameEq {
		t.Errorf("WireNamePinned()[0] = %v, want the pinned name matcher", got.WireNamePinned()[0])
	}
}

// TestWireArms_EmptyInput is the degenerate no-matchers case: wireArms must
// not panic and must return an empty-but-valid WireArms.
func TestWireArms_EmptyInput(t *testing.T) {
	t.Parallel()

	got := wireArms(nil)
	if len(got.Matchers) != 0 || len(got.Arms) != 0 {
		t.Fatalf("wireArms(nil) = %+v, want empty", got)
	}
	if len(got.Storage()) != 0 || len(got.WireName()) != 0 || len(got.WireBound()) != 0 {
		t.Fatalf("filtered slices on empty WireArms must all be empty")
	}
}

// TestWireArm_String pins the diagnostic String() rendering used in test
// failure messages above, so a future arm addition can't silently regress
// to "unknown" without a test noticing.
func TestWireArm_String(t *testing.T) {
	t.Parallel()
	cases := map[WireArm]string{
		WireArmStorage:          "storage",
		WireArmWireNamePinned:   "wire-name-pinned",
		WireArmWireNameUnpinned: "wire-name-unpinned",
		WireArmWireBound:        "wire-bound",
		WireArm(99):             "unknown",
	}
	for arm, want := range cases {
		if got := arm.String(); got != want {
			t.Errorf("WireArm(%d).String() = %q, want %q", arm, got, want)
		}
	}
}

// TestWireArms_ResolveName pins the decision surface #1756 requires:
// given a wire suffix, a pinned matcher resolves to either
// DecisionStorageBare (with the stripped bare name) or
// DecisionUnsatisfiable, never DecisionWireSynthetic — and an unpinned
// matcher always resolves to DecisionWireSynthetic. This is the exact
// distinction the PREWHERE performance invariant depends on: only
// DecisionStorageBare keeps the predicate a bare-column comparison
// internal/chsql/prewhere.go can promote; see
// TestHistogramQuantileMatcherPredicate_PinnedNameStaysBareColumn for the
// plan-shape assertion built on top of this.
func TestWireArms_ResolveName(t *testing.T) {
	t.Parallel()

	const suffix = "_bucket"

	cases := []struct {
		name       string
		matcher    *labels.Matcher
		wantDecide WireNameDecision
		wantBare   string
	}{
		{
			name:       "pinned_with_suffix_resolves_bare",
			matcher:    mustMatcher(t, labels.MatchEqual, "__name__", "demo_bucket"),
			wantDecide: DecisionStorageBare,
			wantBare:   "demo",
		},
		{
			name:       "pinned_without_suffix_unsatisfiable",
			matcher:    mustMatcher(t, labels.MatchEqual, "__name__", "demo"),
			wantDecide: DecisionUnsatisfiable,
			wantBare:   "",
		},
		{
			name:       "unpinned_regex_always_synthetic",
			matcher:    mustMatcher(t, labels.MatchRegexp, "__name__", "demo.*"),
			wantDecide: DecisionWireSynthetic,
			wantBare:   "",
		},
		{
			name:       "unpinned_not_equal_always_synthetic",
			matcher:    mustMatcher(t, labels.MatchNotEqual, "__name__", "demo_bucket"),
			wantDecide: DecisionWireSynthetic,
			wantBare:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := wireArms([]*labels.Matcher{tc.matcher})
			decision, bare := w.ResolveName(0, suffix)
			if decision != tc.wantDecide {
				t.Errorf("ResolveName decision = %v, want %v", decision, tc.wantDecide)
			}
			if bare != tc.wantBare {
				t.Errorf("ResolveName bare = %q, want %q", bare, tc.wantBare)
			}
		})
	}
}
