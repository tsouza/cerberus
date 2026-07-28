package prom

import (
	"testing"
	"time"
)

// A windowless metadata request (both bounds zero) is the Grafana
// variable-refresh shape: the handler has to invent a lower bound, and how
// far back it reaches decides whether discovery can silently omit metrics
// that reference Prometheus would have returned. The bound is configurable
// precisely because cerberus provisions no TTL by default, so the fallback
// is a scan bound rather than a retention-derived guarantee.
func TestBoundMetadataWindow_WindowlessUsesConfiguredLookback(t *testing.T) {
	t.Parallel()

	const configured = 90 * 24 * time.Hour
	h := &Handler{MetadataLookback: configured}

	before := time.Now().UTC()
	start, end := h.boundMetadataWindow(time.Time{}, time.Time{})
	after := time.Now().UTC()

	if end.Before(before) || end.After(after) {
		t.Fatalf("windowless end should be anchored at now, got %s (call spanned %s..%s)", end, before, after)
	}
	if got := end.Sub(start); got != configured {
		t.Fatalf("windowless lookback = %s, want the configured %s", got, configured)
	}
}

func TestBoundMetadataWindow_WindowlessFallsBackWhenUnset(t *testing.T) {
	t.Parallel()

	h := &Handler{}

	start, end := h.boundMetadataWindow(time.Time{}, time.Time{})

	if got := end.Sub(start); got != defaultMetadataLookback {
		t.Fatalf("unconfigured lookback = %s, want the %s fallback", got, defaultMetadataLookback)
	}
}

// A non-positive configured value is not a request for a zero-width window
// — it is the "unset" encoding cmd/cerberus produces when no TTL is
// provisioned either, so it must land on the fallback rather than collapse
// discovery to an empty window.
func TestBoundMetadataWindow_NonPositiveLookbackFallsBack(t *testing.T) {
	t.Parallel()

	for _, lb := range []time.Duration{0, -time.Hour} {
		h := &Handler{MetadataLookback: lb}
		start, end := h.boundMetadataWindow(time.Time{}, time.Time{})
		if got := end.Sub(start); got != defaultMetadataLookback {
			t.Fatalf("lookback %s: window = %s, want the %s fallback", lb, got, defaultMetadataLookback)
		}
	}
}

// A request that supplies either bound is honored verbatim — the one-sided
// window stays open-ended, matching reference Prometheus's MinTime/MaxTime
// default. The lookback must never narrow an operator-supplied window.
func TestBoundMetadataWindow_SuppliedBoundsHonoredVerbatim(t *testing.T) {
	t.Parallel()

	h := &Handler{MetadataLookback: time.Hour}
	supplied := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name             string
		inStart, inEnd   time.Time
		wantStart, wantE time.Time
	}{
		{"start only", supplied, time.Time{}, supplied, time.Time{}},
		{"end only", time.Time{}, supplied, time.Time{}, supplied},
		{"both", supplied, supplied.Add(time.Hour), supplied, supplied.Add(time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStart, gotEnd := h.boundMetadataWindow(tc.inStart, tc.inEnd)
			if !gotStart.Equal(tc.wantStart) || !gotEnd.Equal(tc.wantE) {
				t.Fatalf("got [%s, %s], want [%s, %s]", gotStart, gotEnd, tc.wantStart, tc.wantE)
			}
		})
	}
}
