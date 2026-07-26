package steps

import (
	"strings"
	"testing"
	"time"
)

// The Tier-2 write-back seed and its two anti-degeneracy oracles are pure
// functions, so the properties MIG-19 leans on are pinned here rather than
// discovered as a Tier-2 flake two hours into a CI job.

// TestTier2SourceSamplesAreAMonotonicRamp pins the two properties MIG-19's
// verdict depends on: the counter never goes backwards (a decrease is a
// counter reset, which is exactly the artifact colliding seed identities used
// to produce), and its slope strictly increases (a constant slope makes
// `1 - rate(...)` flat, and a flat comparison proves nothing).
func TestTier2SourceSamplesAreAMonotonicRamp(t *testing.T) {
	start := mustParseTime(t, "2026-07-26T12:00:00Z")
	samples := tier2SourceSamples(start)

	wantLen := int(tier2SeedWindow/tier2SeedStep) + 1
	if len(samples) != wantLen {
		t.Fatalf("tier2SourceSamples returned %d samples, want %d", len(samples), wantLen)
	}
	if !samples[0].Time.Equal(start) {
		t.Fatalf("first sample is at %s, want %s", samples[0].Time, start)
	}
	wantEnd := start.Add(tier2SeedWindow)
	if last := samples[len(samples)-1].Time; !last.Equal(wantEnd) {
		t.Fatalf("last sample is at %s, want %s", last, wantEnd)
	}

	var prevSlope float64
	for i := 1; i < len(samples); i++ {
		if !samples[i].Time.After(samples[i-1].Time) {
			t.Fatalf("sample %d is at %s, not after %s", i, samples[i].Time, samples[i-1].Time)
		}
		if samples[i].Value <= samples[i-1].Value {
			t.Fatalf("sample %d value %g does not exceed %g — a counter that goes flat or backwards reads as a reset",
				i, samples[i].Value, samples[i-1].Value)
		}
		slope := (samples[i].Value - samples[i-1].Value) / samples[i].Time.Sub(samples[i-1].Time).Seconds()
		if i > 1 && slope <= prevSlope {
			t.Fatalf("slope over step %d is %g, which does not exceed the previous %g — a constant slope makes the "+
				"recorded value flat and the sample-by-sample comparison degenerate", i, slope, prevSlope)
		}
		prevSlope = slope
	}
}

// TestTier2SeedIdleRateStaysInsideTheAlertBound pins the seed geometry against
// the two bounds that keep the Tier-2 suite honest: a derived utilisation of
// zero re-introduces the degenerate all-zero comparison, and one at or above
// NodeCPUSaturation's threshold starts dropping notifications into the same
// dead-end receiver MIG-09 and MIG-18 count against.
func TestTier2SeedIdleRateStaysInsideTheAlertBound(t *testing.T) {
	steps := int(tier2SeedWindow / tier2SeedStep)
	for i := range steps + 1 {
		rate := tier2SeedIdleRateAt(i, steps)
		utilisation := 1 - rate
		if utilisation <= 0 {
			t.Fatalf("step %d derives utilisation %g, which is not above zero", i, utilisation)
		}
		if utilisation >= nodeCPUSaturationThreshold {
			t.Fatalf("step %d derives utilisation %g, which reaches NodeCPUSaturation's threshold %g",
				i, utilisation, nodeCPUSaturationThreshold)
		}
	}
}

// TestTier2SeedIdleRateAtDegenerateRampIsTheStartRate covers the guard that
// keeps a zero-length ramp from dividing by zero.
func TestTier2SeedIdleRateAtDegenerateRampIsTheStartRate(t *testing.T) {
	if got := tier2SeedIdleRateAt(0, 0); got != tier2SeedIdleRateStart {
		t.Fatalf("tier2SeedIdleRateAt(0, 0) = %g, want %g", got, tier2SeedIdleRateStart)
	}
}

// TestTier2ScopedSourceExprNarrowsTheRuleExpression asserts the narrowing is
// derived from the rule's own expression — it must add the scope matcher and
// change nothing else, so the two can never drift apart.
func TestTier2ScopedSourceExprNarrowsTheRuleExpression(t *testing.T) {
	const scope = "landed-samples-match-abc123"
	got, err := tier2ScopedSourceExpr(scope)
	if err != nil {
		t.Fatalf("tier2ScopedSourceExpr: %v", err)
	}
	if !strings.Contains(got, tier2ScopeMatcher(scope)) {
		t.Fatalf("scoped expression %q does not carry the matcher %q", got, tier2ScopeMatcher(scope))
	}
	// Removing exactly the injected matcher must reproduce the rule's own
	// expression byte-for-byte; anything else means the narrowing rewrote
	// something it had no business rewriting.
	back := strings.Replace(got, ","+tier2ScopeMatcher(scope), "", 1)
	if back != nodeCPUSourceExpr {
		t.Fatalf("stripping the injected matcher yields %q, want the rule expression %q", back, nodeCPUSourceExpr)
	}
}

// TestTier2ExpectedTickCountIsAFencepostCount pins the count oracle: N ticks
// at one interval span N-1 intervals, sub-interval jitter does not change the
// answer, and a whole missing interval does.
func TestTier2ExpectedTickCountIsAFencepostCount(t *testing.T) {
	const interval = 10 * time.Second
	cases := []struct {
		name string
		span time.Duration
		want int
	}{
		{"a single evaluation spans nothing", 0, 1},
		{"three evaluations span two intervals", 20 * time.Second, 3},
		{"a late tick still holds three", 20*time.Second - 250*time.Millisecond, 3},
		{"an early tick still holds three", 20*time.Second + 250*time.Millisecond, 3},
		{"a dropped tick widens the span by a whole interval", 30 * time.Second, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tier2ExpectedTickCount(c.span, interval); got != c.want {
				t.Fatalf("tier2ExpectedTickCount(%s, %s) = %d, want %d", c.span, interval, got, c.want)
			}
		})
	}
}

// TestTier2ExpectedTickCountRejectsNonsenseInputs covers the guards, so a
// zero interval or a reversed span reports "no expected ticks" instead of
// panicking inside the assertion that consumes it.
func TestTier2ExpectedTickCountRejectsNonsenseInputs(t *testing.T) {
	if got := tier2ExpectedTickCount(time.Minute, 0); got != 0 {
		t.Fatalf("tier2ExpectedTickCount with a zero interval = %d, want 0", got)
	}
	if got := tier2ExpectedTickCount(-time.Minute, time.Second); got != 0 {
		t.Fatalf("tier2ExpectedTickCount with a negative span = %d, want 0", got)
	}
}

// TestTier2ValuesVaryDetectsAFlatSeries is the anti-degeneracy oracle's own
// oracle: it must reject exactly the case the old constant-rate seed produced,
// a run of identical values, and accept a single differing one.
func TestTier2ValuesVaryDetectsAFlatSeries(t *testing.T) {
	flat := []landedSample{{value: 0}, {value: 0}, {value: 0}}
	if tier2ValuesVary(flat) {
		t.Fatalf("tier2ValuesVary reported variation across %v", flat)
	}
	oneDiffers := []landedSample{{value: 0.25}, {value: 0.25}, {value: 0.2517}}
	if !tier2ValuesVary(oneDiffers) {
		t.Fatalf("tier2ValuesVary reported no variation across %v", oneDiffers)
	}
}

// TestTier2SeedScopeIsUniquePerScenario pins the property the whole
// interleaving fix rests on: two scenarios never share one stored series
// identity, and the scope reads back as the scenario that wrote it.
func TestTier2SeedScopeIsUniquePerScenario(t *testing.T) {
	a := tier2SeedScopeFor("landed samples match a live re-evaluation of their source query")
	b := tier2SeedScopeFor("ruler evaluates the rule fixture and its recording-rule output round-trips through cerberus")
	if a == b {
		t.Fatalf("two scenarios derived the same seed scope %q", a)
	}
	if !strings.HasPrefix(a, "landed-samples-match-a-live-re-evaluation-of-their-source-query-") {
		t.Fatalf("seed scope %q does not read back as the scenario that wrote it", a)
	}
	if tier2SeedScopeFor("") == "" {
		t.Fatalf("an unnamed scenario derived an empty seed scope")
	}
}

// TestTier2SlugCollapsesPunctuation pins the slug's contract: lowercase, one
// separator per punctuation run, no leading or trailing separator — so the
// scope drops cleanly into a PromQL label matcher and a ClickHouse Map key.
func TestTier2SlugCollapsesPunctuation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Recording-rule output — sample by sample!", "recording-rule-output-sample-by-sample"},
		{"  leading and trailing  ", "leading-and-trailing"},
		{"MIG-19/tier2", "mig-19-tier2"},
		{"!!!", ""},
	}
	for _, c := range cases {
		if got := tier2Slug(c.in); got != c.want {
			t.Fatalf("tier2Slug(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestTier2ScopedRecordedSelectorNarrowsTheRecordedName asserts the recorded
// series read-back is scoped, which is what stops a leftover series from an
// earlier scenario or an earlier run satisfying a write-back assertion.
func TestTier2ScopedRecordedSelectorNarrowsTheRecordedName(t *testing.T) {
	const scope = "some-scenario-deadbeef"
	got := tier2ScopedRecordedSelector(scope)
	want := nodeCPURecordedMetricName + `{` + tier2SeedScopeLabel + `="` + scope + `"}`
	if got != want {
		t.Fatalf("tier2ScopedRecordedSelector(%q) = %q, want %q", scope, got, want)
	}
}
