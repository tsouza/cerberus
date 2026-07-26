package steps

import (
	"testing"
	"time"
)

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestQuantizeToEvalInterval(t *testing.T) {
	interval := 10 * time.Second
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already on boundary", "2026-01-01T00:00:10Z", "2026-01-01T00:00:10Z"},
		{"mid-interval floors down", "2026-01-01T00:00:17Z", "2026-01-01T00:00:10Z"},
		{"just before next boundary", "2026-01-01T00:00:19Z", "2026-01-01T00:00:10Z"},
		{"zero seconds", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := QuantizeToEvalInterval(mustParseTime(t, tc.in), interval)
			want := mustParseTime(t, tc.want)
			if !got.Equal(want) {
				t.Fatalf("QuantizeToEvalInterval(%s, %s) = %s, want %s", tc.in, interval, got, want)
			}
		})
	}
}

func TestQuantizeToEvalInterval_ZeroIntervalIsIdentity(t *testing.T) {
	in := mustParseTime(t, "2026-01-01T00:00:17Z")
	got := QuantizeToEvalInterval(in, 0)
	if !got.Equal(in) {
		t.Fatalf("QuantizeToEvalInterval with zero interval = %s, want identity %s", got, in)
	}
}

func TestDiffAlertStreams_SubIntervalSkewIsNotADivergence(t *testing.T) {
	// The doc's whole point: two independent rulers on independent
	// schedules produce sub-interval fire/resolve skew that is not a
	// cerberus artifact. Two events 3s apart, both inside the same 10s
	// evaluation bucket, must match cleanly — a raw-timestamp float
	// comparison would instead call this a divergence.
	interval := 10 * time.Second
	labels := map[string]string{"severity": "warning"}
	incumbent := []AlertEvent{{RuleName: "NodeCPUSaturation", Labels: labels, Status: "firing", At: mustParseTime(t, "2026-01-01T00:00:11Z")}}
	shadow := []AlertEvent{{RuleName: "NodeCPUSaturation", Labels: labels, Status: "firing", At: mustParseTime(t, "2026-01-01T00:00:14Z")}}

	diff := DiffAlertStreams(incumbent, shadow, interval)
	if !diff.Clean() {
		t.Fatalf("expected a clean diff for sub-interval skew, got %+v", diff)
	}
	if diff.Matched != 1 {
		t.Fatalf("Matched = %d, want 1", diff.Matched)
	}
}

func TestDiffAlertStreams_CrossBoundaryIsTimingSkew(t *testing.T) {
	// Same alert, same rough time, but landing in two DIFFERENT 10s
	// evaluation buckets: a real timing-skew divergence, not noise.
	interval := 10 * time.Second
	labels := map[string]string{"severity": "warning"}
	incumbent := []AlertEvent{{RuleName: "NodeCPUSaturation", Labels: labels, Status: "firing", At: mustParseTime(t, "2026-01-01T00:00:09Z")}}
	shadow := []AlertEvent{{RuleName: "NodeCPUSaturation", Labels: labels, Status: "firing", At: mustParseTime(t, "2026-01-01T00:00:11Z")}}

	diff := DiffAlertStreams(incumbent, shadow, interval)
	if diff.Clean() {
		t.Fatalf("expected a cross-boundary timing skew, got a clean diff")
	}
	if diff.TimingSkew != 1 {
		t.Fatalf("TimingSkew = %d, want 1 (got %+v)", diff.TimingSkew, diff)
	}
}

func TestDiffAlertStreams_FalsePositiveAndFalseNegative(t *testing.T) {
	interval := 10 * time.Second
	onlyIncumbent := AlertEvent{RuleName: "OnlyIncumbent", Labels: map[string]string{}, Status: "firing", At: mustParseTime(t, "2026-01-01T00:00:00Z")}
	onlyShadow := AlertEvent{RuleName: "OnlyShadow", Labels: map[string]string{}, Status: "firing", At: mustParseTime(t, "2026-01-01T00:00:00Z")}

	diff := DiffAlertStreams([]AlertEvent{onlyIncumbent}, []AlertEvent{onlyShadow}, interval)
	if diff.FalseNegative != 1 {
		t.Fatalf("FalseNegative = %d, want 1 (incumbent fired, shadow silent)", diff.FalseNegative)
	}
	if diff.FalsePositive != 1 {
		t.Fatalf("FalsePositive = %d, want 1 (shadow fired, incumbent silent)", diff.FalsePositive)
	}
	if diff.Matched != 0 {
		t.Fatalf("Matched = %d, want 0", diff.Matched)
	}
}

func TestDiffAlertStreams_DifferentLabelsAreDifferentAlerts(t *testing.T) {
	interval := 10 * time.Second
	at := mustParseTime(t, "2026-01-01T00:00:00Z")
	incumbent := []AlertEvent{{RuleName: "Rule", Labels: map[string]string{"instance": "a"}, Status: "firing", At: at}}
	shadow := []AlertEvent{{RuleName: "Rule", Labels: map[string]string{"instance": "b"}, Status: "firing", At: at}}

	diff := DiffAlertStreams(incumbent, shadow, interval)
	if diff.Matched != 0 || diff.FalseNegative != 1 || diff.FalsePositive != 1 {
		t.Fatalf("differing label sets must not match as the same alert: got %+v", diff)
	}
}

func TestBakeWindowHoldsZero_AllZero(t *testing.T) {
	samples := []MWMBRSample{
		{At: mustParseTime(t, "2026-01-01T00:00:00Z"), Delta: 0},
		{At: mustParseTime(t, "2026-01-01T00:01:00Z"), Delta: 1e-12},
		{At: mustParseTime(t, "2026-01-01T00:02:00Z"), Delta: -1e-12},
	}
	if err := BakeWindowHoldsZero(samples, 1e-9); err != nil {
		t.Fatalf("BakeWindowHoldsZero: unexpected error: %v", err)
	}
}

func TestBakeWindowHoldsZero_OneExcursionFailsTheWholeWindow(t *testing.T) {
	// The doc's point: the delta must hold zero across the FULL bake
	// window, not a spot check. One excursion out of many samples must
	// still fail — it must not average out or get outvoted.
	samples := []MWMBRSample{
		{At: mustParseTime(t, "2026-01-01T00:00:00Z"), Delta: 0},
		{At: mustParseTime(t, "2026-01-01T00:01:00Z"), Delta: 0},
		{At: mustParseTime(t, "2026-01-01T00:02:00Z"), Delta: 0.05},
		{At: mustParseTime(t, "2026-01-01T00:03:00Z"), Delta: 0},
	}
	if err := BakeWindowHoldsZero(samples, 1e-9); err == nil {
		t.Fatal("BakeWindowHoldsZero: expected an error for the single excursion, got nil")
	}
}

func TestBakeWindowHoldsZero_EmptyWindowIsAnError(t *testing.T) {
	if err := BakeWindowHoldsZero(nil, 1e-9); err == nil {
		t.Fatal("BakeWindowHoldsZero: expected an error over an empty window (proves nothing), got nil")
	}
}

func TestParseGrafanaWebhookEvents(t *testing.T) {
	body := []byte(`{
		"alerts": [
			{
				"status": "firing",
				"labels": {"alertname": "NodeCPUSaturation", "severity": "warning"},
				"annotations": {"summary": "Node CPU saturation"},
				"startsAt": "2026-01-01T00:00:10Z",
				"endsAt": "0001-01-01T00:00:00Z"
			},
			{
				"status": "resolved",
				"labels": {"alertname": "NodeCPUSaturation", "severity": "warning"},
				"annotations": {"summary": "Node CPU saturation"},
				"startsAt": "2026-01-01T00:00:10Z",
				"endsAt": "2026-01-01T00:20:10Z"
			}
		]
	}`)
	events, err := ParseGrafanaWebhookEvents(body)
	if err != nil {
		t.Fatalf("ParseGrafanaWebhookEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	if events[0].Status != "firing" || !events[0].At.Equal(mustParseTime(t, "2026-01-01T00:00:10Z")) {
		t.Fatalf("firing event = %+v, want status firing at startsAt", events[0])
	}
	if events[1].Status != "resolved" || !events[1].At.Equal(mustParseTime(t, "2026-01-01T00:20:10Z")) {
		t.Fatalf("resolved event = %+v, want status resolved at endsAt", events[1])
	}
	if events[0].RuleName != "NodeCPUSaturation" {
		t.Fatalf("RuleName = %q, want %q", events[0].RuleName, "NodeCPUSaturation")
	}
}

func TestParseGrafanaWebhookEvents_UnrecognisedStatusErrors(t *testing.T) {
	body := []byte(`{"alerts": [{"status": "pending", "labels": {"alertname": "X"}}]}`)
	if _, err := ParseGrafanaWebhookEvents(body); err == nil {
		t.Fatal("expected an error for an unrecognised alert status, got nil")
	}
}
