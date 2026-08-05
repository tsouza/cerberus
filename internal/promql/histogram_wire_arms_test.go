package promql

import (
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/schema"
)

// TestWireArms_ClassifiesEachDomain pins the three-way split: a `__name__`
// matcher (pinned or unpinned) classifies WireArmWireName, an `le` matcher
// classifies WireArmWireBound, and everything else classifies
// WireArmStorage — in original order.
func TestWireArms_ClassifiesEachDomain(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	nameEq := mustMatcher(t, labels.MatchEqual, "__name__", "demo_api_request_duration_seconds_bucket")
	nameRe := mustMatcher(t, labels.MatchRegexp, "__name__", "demo_api.*")
	le := mustMatcher(t, labels.MatchEqual, "le", "0.5")
	job := mustMatcher(t, labels.MatchEqual, "job", "api")

	got := wireArms([]*labels.Matcher{nameEq, le, job, nameRe}, s)

	want := []WireArm{WireArmWireName, WireArmWireBound, WireArmStorage, WireArmWireName}
	if len(got.Arms) != len(want) {
		t.Fatalf("Arms len = %d, want %d", len(got.Arms), len(want))
	}
	for i, w := range want {
		if got.Arms[i] != w {
			t.Errorf("Arms[%d] (matcher %s) = %s, want %s", i, got.Matchers[i], got.Arms[i], w)
		}
	}

	if diff := len(got.WireName()); diff != 2 {
		t.Errorf("WireName() len = %d, want 2", diff)
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
}

// TestWireArms_NoHistogramTableConfigured is the schema-gated fallback: a
// deployment with no classic-histogram routing (s.HistogramTable == "") has
// no synthetic wire surface to route around at all, so wireArms must not
// pull `le` or `__name__` out into a wire arm — mirroring the same gate
// isClassicBucketSelector already uses.
func TestWireArms_NoHistogramTableConfigured(t *testing.T) {
	t.Parallel()
	s := schema.Metrics{} // zero value: HistogramTable == ""

	le := mustMatcher(t, labels.MatchEqual, "le", "0.5")
	name := mustMatcher(t, labels.MatchEqual, "__name__", "up")

	got := wireArms([]*labels.Matcher{le, name}, s)

	for i, arm := range got.Arms {
		if arm != WireArmStorage {
			t.Errorf("Arms[%d] = %s, want %s (HistogramTable unconfigured)", i, arm, WireArmStorage)
		}
	}
}

// TestWireArms_EmptyInput is the degenerate no-matchers case: wireArms must
// not panic and must return an empty-but-valid WireArms.
func TestWireArms_EmptyInput(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	got := wireArms(nil, s)
	if len(got.Matchers) != 0 || len(got.Arms) != 0 {
		t.Fatalf("wireArms(nil, s) = %+v, want empty", got)
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
		WireArmStorage:   "storage",
		WireArmWireName:  "wire-name",
		WireArmWireBound: "wire-bound",
		WireArm(99):      "unknown",
	}
	for arm, want := range cases {
		if got := arm.String(); got != want {
			t.Errorf("WireArm(%d).String() = %q, want %q", arm, got, want)
		}
	}
}
