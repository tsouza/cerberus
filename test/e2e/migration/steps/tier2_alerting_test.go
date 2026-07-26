package steps

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

func TestParseGrafanaWebhookEvents_CarriesTheInstanceStartInstant(t *testing.T) {
	// The resolve-closes-the-firing-instance assertion compares the two
	// notifications' startsAt, so the parser must carry it on BOTH edges —
	// including the resolved one, whose own At is its endsAt.
	body := []byte(`{
		"alerts": [
			{
				"status": "resolved",
				"labels": {"alertname": "X"},
				"startsAt": "2026-01-01T00:00:10Z",
				"endsAt": "2026-01-01T00:20:10Z"
			}
		]
	}`)
	events, err := ParseGrafanaWebhookEvents(body)
	if err != nil {
		t.Fatalf("ParseGrafanaWebhookEvents: %v", err)
	}
	if !events[0].StartsAt.Equal(mustParseTime(t, "2026-01-01T00:00:10Z")) {
		t.Fatalf("resolved event StartsAt = %s, want the instance start 2026-01-01T00:00:10Z", events[0].StartsAt)
	}
	if !events[0].At.Equal(mustParseTime(t, "2026-01-01T00:20:10Z")) {
		t.Fatalf("resolved event At = %s, want its endsAt 2026-01-01T00:20:10Z", events[0].At)
	}
}

func TestAlertIdentity_IgnoresStatusButNotLabels(t *testing.T) {
	// alertIdentity answers "which alert instance is this", so a fire and a
	// resolve of one alert must share it while two differently-labelled
	// alerts must not. The live resolve assertion reads it that way.
	fired := AlertEvent{RuleName: "X", Labels: map[string]string{"a": "1", "b": "2"}, Status: "firing"}
	resolved := AlertEvent{RuleName: "X", Labels: map[string]string{"b": "2", "a": "1"}, Status: "resolved"}
	if alertIdentity(fired) != alertIdentity(resolved) {
		t.Fatalf("one alert's fire and resolve carry different identities: %q vs %q",
			alertIdentity(fired), alertIdentity(resolved))
	}
	other := AlertEvent{RuleName: "X", Labels: map[string]string{"a": "1", "b": "3"}, Status: "resolved"}
	if alertIdentity(fired) == alertIdentity(other) {
		t.Fatal("alerts with different label sets share an identity")
	}
	if alertEventKey(fired) == alertEventKey(resolved) {
		t.Fatal("a fire and a resolve of one alert share an event key; the stream diff would match them")
	}
}

// === the fixture pin ========================================================
//
// MIG-18's mig18* constants restate fields of the `migration.probe` rule group
// in tiers/tier2-ruler/grafana/alerting/rules.yaml. Restating is what makes
// the live assertions real — a check that reads its expectation back out of
// the ruler it is testing asserts nothing — but a restatement nobody pins
// drifts silently, and every drift degrades a live assertion into a
// tautology. Raise the fixture's `for:` to a minute and the hold-down check
// becomes "the ruler held it for at least thirty seconds", which a one-minute
// hold-down passes while proving nothing about the minute.
//
// So the pin runs offline, against the FIXTURE (the specification), never
// against the running ruler (the thing under test).

// The provisioned* types decode the subset of Grafana's alerting
// file-provisioning schema the pins below read.
type provisionedRulesFile struct {
	Groups []provisionedGroup `yaml:"groups"`
}

type provisionedGroup struct {
	Name     string            `yaml:"name"`
	Folder   string            `yaml:"folder"`
	Interval string            `yaml:"interval"`
	Rules    []provisionedRule `yaml:"rules"`
}

type provisionedRule struct {
	Title       string             `yaml:"title"`
	For         string             `yaml:"for"`
	NoDataState string             `yaml:"noDataState"`
	Labels      map[string]string  `yaml:"labels"`
	Annotations map[string]string  `yaml:"annotations"`
	Data        []provisionedQuery `yaml:"data"`
}

// provisionedQuery is one leg of a rule's query pipeline: either a datasource
// query (carrying an `expr`) or a server-side expression (carrying an
// evaluator condition).
type provisionedQuery struct {
	RefID string `yaml:"refId"`
	Model struct {
		Expr       string                 `yaml:"expr"`
		Type       string                 `yaml:"type"`
		Conditions []provisionedCondition `yaml:"conditions"`
	} `yaml:"model"`
}

type provisionedCondition struct {
	Evaluator struct {
		Type   string    `yaml:"type"`
		Params []float64 `yaml:"params"`
	} `yaml:"evaluator"`
}

// provisionedRulesPath is the fixture the Tier-2 stack mounts into Grafana,
// relative to this package.
const provisionedRulesPath = "../tiers/tier2-ruler/grafana/alerting/rules.yaml"

// findProvisionedProbeRule returns the probe rule and the interval of the
// group carrying it, failing the test by name when the fixture provisions
// neither.
func findProvisionedProbeRule(t *testing.T) (provisionedRule, string) {
	t.Helper()
	raw, err := os.ReadFile(provisionedRulesPath)
	if err != nil {
		t.Fatalf("read the provisioned rule fixture %s: %v", provisionedRulesPath, err)
	}
	var file provisionedRulesFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode %s: %v", provisionedRulesPath, err)
	}
	for _, g := range file.Groups {
		if g.Name != mig18ProbeGroup || g.Folder != mig18ProbeFolder {
			continue
		}
		for _, r := range g.Rules {
			if r.Title == mig18ProbeAlertName {
				return r, g.Interval
			}
		}
	}
	t.Fatalf("%s provisions no rule %q in %s/%s — the live MIG-18 scenario would poll a rule that cannot exist",
		provisionedRulesPath, mig18ProbeAlertName, mig18ProbeFolder, mig18ProbeGroup)
	return provisionedRule{}, ""
}

func TestMig18ScheduleConstantsMatchProvisionedProbeRule(t *testing.T) {
	probe, groupInterval := findProvisionedProbeRule(t)

	interval, err := time.ParseDuration(groupInterval)
	if err != nil {
		t.Fatalf("parse the probe group's interval %q: %v", groupInterval, err)
	}
	if interval != mig18ProbeEvalInterval {
		t.Fatalf("the probe group evaluates every %s but mig18ProbeEvalInterval is %s — the resolve-attribution "+
			"check would compare against the wrong scheduling quantum", interval, mig18ProbeEvalInterval)
	}

	holdDown, err := time.ParseDuration(probe.For)
	if err != nil {
		t.Fatalf("parse the probe rule's for: %q: %v", probe.For, err)
	}
	if holdDown != mig18ProbeHoldDown {
		t.Fatalf("the probe rule holds down for %s but mig18ProbeHoldDown is %s — the live hold-down assertion "+
			"would accept a window shorter than the one provisioned, which is a tautology, not a check",
			holdDown, mig18ProbeHoldDown)
	}

	if probe.NoDataState != mig18ProbeNoDataState {
		t.Fatalf("the probe rule's noDataState is %q, want %q — any other value lets the rule notify on absent "+
			"data, into the same dead-end receiver MIG-09 counts its own delta against",
			probe.NoDataState, mig18ProbeNoDataState)
	}
}

func TestMig18LabelConstantsMatchProvisionedProbeRule(t *testing.T) {
	probe, _ := findProvisionedProbeRule(t)

	want := map[string]string{
		mig18SeverityKey:   mig18SeverityValue,
		mig18ProbeLabelKey: mig18ProbeLabelValue,
	}
	for key, value := range want {
		if got := probe.Labels[key]; got != value {
			t.Fatalf("the probe rule provisions %s=%q, but the live label assertion expects %q", key, got, value)
		}
	}

	summary := probe.Annotations[mig18SummaryKey]
	if !strings.Contains(summary, mig18TemplateLabelRef+mig18ProbeLabelKey) {
		t.Fatalf("the probe rule's %q annotation %q does not read the %s label through %q — the rendered-"+
			"annotation assertion would then be satisfied by a constant string",
			mig18SummaryKey, summary, mig18ProbeLabelKey, mig18TemplateLabelRef)
	}
}

// TestMig18ProbeQueryKeepsTheAnnotatedLabel pins the one property of the probe
// rule's query the rendered-annotation assertion depends on: the aggregation
// must GROUP BY the probe label rather than collapse it away. Grafana expands
// annotations against the EVALUATED RESULT's labels — a rule's own `labels:`
// are templates being expanded in the same pass, so they are not in scope for
// their own expansion — and an unlabelled reduce therefore renders
// `{{ $labels.probe }}` as `[no value]` however the rule declares them. That
// is the defect the first live Tier-2 run reported.
func TestMig18ProbeQueryKeepsTheAnnotatedLabel(t *testing.T) {
	probe, _ := findProvisionedProbeRule(t)
	for _, leg := range probe.Data {
		if !strings.Contains(leg.Model.Expr, mig18ProbeMetric) {
			continue
		}
		if !strings.Contains(leg.Model.Expr, mig18ProbeGroupBy) {
			t.Fatalf("the probe rule's query %q does not keep the %s label (%q) through its aggregation, so "+
				"Grafana would render the summary annotation with no value for it",
				leg.Model.Expr, mig18ProbeLabelKey, mig18ProbeGroupBy)
		}
		return
	}
	t.Fatalf("the probe rule has no query leg selecting %s — the live Given would seed a series nothing reads",
		mig18ProbeMetric)
}

// TestMig18SeedValuesStraddleProvisionedThreshold pins that the seeded armed
// and cleared values sit on opposite sides of the provisioned evaluator. A
// threshold that drifted outside them would leave the probe permanently
// firing or permanently clear, and the whole lifecycle scenario would assert
// about a rule that never changes state.
func TestMig18SeedValuesStraddleProvisionedThreshold(t *testing.T) {
	probe, _ := findProvisionedProbeRule(t)
	for _, leg := range probe.Data {
		for _, condition := range leg.Model.Conditions {
			for _, threshold := range condition.Evaluator.Params {
				if threshold <= mig18ClearedValue || threshold >= mig18FiringValue {
					t.Fatalf("the probe rule's threshold %g is not straddled by the seeded cleared value %g "+
						"and armed value %g — arming or clearing the probe would not change its state",
						threshold, mig18ClearedValue, mig18FiringValue)
				}
			}
			return
		}
	}
	t.Fatal("the probe rule provisions no threshold evaluator — nothing decides when it fires")
}
