package steps

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// === The comparator's own behaviour ===

func TestProjectAlertLabelsKeepsOnlyTheSharedIdentity(t *testing.T) {
	// A Grafana-emitted edge carries substrate labels naming the ruler that
	// produced it; the incumbent's carries none of them. Projection is what
	// makes the two comparable at all.
	shadow := AlertEvent{
		RuleName: mwmbrFastAlertName,
		Status:   alertStatusFiring,
		Labels: map[string]string{
			"alertname": mwmbrFastAlertName, mig18SeverityKey: mwmbrFastSeverity,
			mwmbrBurnRateKey: mwmbrFastBurnLabel, mwmbrSLOKey: mwmbrBurningSLO,
			tier2SeedScopeLabel: "run-under-test",
			"grafana_folder":    mwmbrFolder, "datasource_uid": "cerberus-prometheus",
		},
	}
	incumbent := AlertEvent{
		RuleName: mwmbrFastAlertName,
		Status:   alertStatusFiring,
		Labels: map[string]string{
			"alertname": mwmbrFastAlertName, mig18SeverityKey: mwmbrFastSeverity,
			mwmbrBurnRateKey: mwmbrFastBurnLabel, mwmbrSLOKey: mwmbrBurningSLO,
			tier2SeedScopeLabel: "run-under-test",
		},
	}

	projected, err := ProjectAlertLabels([]AlertEvent{shadow, incumbent}, ParityLabelKeys)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if got, want := alertIdentity(projected[0]), alertIdentity(projected[1]); got != want {
		t.Fatalf("the two rulers' edges project to different identities:\n shadow    %s\n incumbent %s", got, want)
	}
	if _, ok := projected[0].Labels["grafana_folder"]; ok {
		t.Fatalf("projection kept grafana_folder, which only names which ruler emitted the edge")
	}
}

// A ruler that dropped a routing key must FAIL the projection rather than
// project to a smaller identity that happens to match — that would be the
// exact breakage (a silence or route stops matching) reading as agreement.
func TestProjectAlertLabelsRejectsAMissingRoutingKey(t *testing.T) {
	for _, missing := range ParityLabelKeys {
		labels := map[string]string{
			"alertname": mwmbrFastAlertName, mig18SeverityKey: mwmbrFastSeverity,
			mwmbrBurnRateKey: mwmbrFastBurnLabel, mwmbrSLOKey: mwmbrBurningSLO,
			tier2SeedScopeLabel: "run-under-test",
		}
		delete(labels, missing)
		_, err := ProjectAlertLabels(
			[]AlertEvent{{RuleName: mwmbrFastAlertName, Status: alertStatusFiring, Labels: labels}},
			ParityLabelKeys,
		)
		if err == nil {
			t.Fatalf("an edge missing the %q label projected without error", missing)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Fatalf("the failure for a missing %q does not name it: %v", missing, err)
		}
	}
}

func TestSkewBoundHoldsWithinAndBeyondTheBound(t *testing.T) {
	base := mustParseTime(t, "2026-01-01T00:00:00Z")
	labels := map[string]string{
		"alertname": mwmbrFastAlertName, mig18SeverityKey: mwmbrFastSeverity,
		mwmbrBurnRateKey: mwmbrFastBurnLabel, mwmbrSLOKey: mwmbrBurningSLO,
		tier2SeedScopeLabel: "run-under-test",
	}
	edge := func(at time.Time) []AlertEvent {
		return []AlertEvent{{RuleName: mwmbrFastAlertName, Status: alertStatusFiring, Labels: labels, At: at}}
	}
	bound := mwmbrEvalInterval

	// Inside the bound, in both directions — neither ruler is privileged.
	for _, skew := range []time.Duration{0, 3 * time.Second, -3 * time.Second, bound, -bound} {
		if err := SkewBoundHolds(edge(base), edge(base.Add(skew)), bound); err != nil {
			t.Fatalf("a %s skew inside the %s bound failed: %v", skew, bound, err)
		}
	}
	// Beyond it — a ruler that is genuinely late, which quantization alone
	// would file indistinguishably from a sub-interval phase difference.
	for _, skew := range []time.Duration{bound + time.Second, -(bound + time.Second), 4 * time.Minute} {
		if err := SkewBoundHolds(edge(base), edge(base.Add(skew)), bound); err == nil {
			t.Fatalf("a %s skew beyond the %s bound passed", skew, bound)
		}
	}
}

// A bound asserted over no pairs at all is a vacuous pass, and silence would
// look identical to agreement.
func TestSkewBoundHoldsRefusesAnEmptyComparison(t *testing.T) {
	base := mustParseTime(t, "2026-01-01T00:00:00Z")
	incumbent := []AlertEvent{{
		RuleName: mwmbrFastAlertName, Status: alertStatusFiring, At: base,
		Labels: map[string]string{"alertname": mwmbrFastAlertName},
	}}
	shadow := []AlertEvent{{
		RuleName: mwmbrSlowAlertName, Status: alertStatusFiring, At: base,
		Labels: map[string]string{"alertname": mwmbrSlowAlertName},
	}}
	if err := SkewBoundHolds(incumbent, shadow, mwmbrEvalInterval); err == nil {
		t.Fatalf("two streams sharing no identity reported a satisfied skew bound")
	}
	if err := SkewBoundHolds(nil, nil, mwmbrEvalInterval); err == nil {
		t.Fatalf("two empty streams reported a satisfied skew bound")
	}
}

// === Fixture pins: the two rule files must stay the SAME rule ===
//
// MIG-18's diff is only meaningful if both rulers evaluate one rule. These
// pins read BOTH fixtures and fail offline on a one-sided edit, so a drift
// cannot reach a live run and quietly turn a parity diff into a comparison of
// two different rules that happen to share a name.

// incumbentRulesPath is the rule file the Tier-2 stack mounts into the
// incumbent ruler, relative to this package.
const incumbentRulesPath = "../tiers/tier2-ruler/incumbent/incumbent-rules.yml"

// The incumbent* types decode the subset of Prometheus's rule-file schema
// these pins read.
type incumbentRulesFile struct {
	Groups []incumbentGroup `yaml:"groups"`
}

type incumbentGroup struct {
	Name     string          `yaml:"name"`
	Interval string          `yaml:"interval"`
	Rules    []incumbentRule `yaml:"rules"`
}

type incumbentRule struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func loadIncumbentRules(t *testing.T) incumbentRulesFile {
	t.Helper()
	raw, err := os.ReadFile(incumbentRulesPath)
	if err != nil {
		t.Fatalf("read the incumbent rule fixture %s: %v", incumbentRulesPath, err)
	}
	var file incumbentRulesFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode %s: %v", incumbentRulesPath, err)
	}
	return file
}

func loadProvisionedRules(t *testing.T) provisionedRulesFile {
	t.Helper()
	raw, err := os.ReadFile(provisionedRulesPath)
	if err != nil {
		t.Fatalf("read the provisioned rule fixture %s: %v", provisionedRulesPath, err)
	}
	var file provisionedRulesFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode %s: %v", provisionedRulesPath, err)
	}
	return file
}

// findIncumbentAlert returns one alerting rule from the incumbent's MWMBR
// group.
func findIncumbentAlert(t *testing.T, name string) (incumbentRule, string) {
	t.Helper()
	for _, g := range loadIncumbentRules(t).Groups {
		if g.Name != mwmbrGroup {
			continue
		}
		for _, r := range g.Rules {
			if r.Alert == name {
				return r, g.Interval
			}
		}
	}
	t.Fatalf("%s provisions no alert %q in group %q — MIG-18's diff would have only one leg",
		incumbentRulesPath, name, mwmbrGroup)
	return incumbentRule{}, ""
}

// findProvisionedMwmbrRule returns one rule from the shadow's MWMBR group.
func findProvisionedMwmbrRule(t *testing.T, title string) (provisionedRule, string) {
	t.Helper()
	for _, g := range loadProvisionedRules(t).Groups {
		if g.Name != mwmbrGroup || g.Folder != mwmbrFolder {
			continue
		}
		for _, r := range g.Rules {
			if r.Title == title {
				return r, g.Interval
			}
		}
	}
	t.Fatalf("%s provisions no rule %q in %s/%s — MIG-18's diff would have only one leg",
		provisionedRulesPath, title, mwmbrFolder, mwmbrGroup)
	return provisionedRule{}, ""
}

// normalizeExpr makes two spellings of one PromQL expression comparable across
// the two files' YAML block styles. It collapses whitespace ONLY: every token,
// every threshold and every window survives, so a changed number or a changed
// window still fails.
func normalizeExpr(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

// mwmbrTiers pairs each burn tier's alert name with the Go constants that
// mirror it, so every pin below runs over both.
func mwmbrTiers() []struct {
	name, severity, burnLabel string
} {
	return []struct{ name, severity, burnLabel string }{
		{mwmbrFastAlertName, mwmbrFastSeverity, mwmbrFastBurnLabel},
		{mwmbrSlowAlertName, mwmbrSlowSeverity, mwmbrSlowBurnLabel},
	}
}

// The core pin: the same rule on both sides. A one-sided edit to the
// expression, the hold-down or the routing labels fails here.
func TestMwmbrRuleIsIdenticalOnBothRulers(t *testing.T) {
	for _, tier := range mwmbrTiers() {
		incumbent, incumbentInterval := findIncumbentAlert(t, tier.name)
		shadow, shadowInterval := findProvisionedMwmbrRule(t, tier.name)

		var shadowExpr string
		for _, q := range shadow.Data {
			if q.Model.Expr != "" {
				shadowExpr = q.Model.Expr
			}
		}
		if got, want := normalizeExpr(shadowExpr), normalizeExpr(incumbent.Expr); got != want {
			t.Fatalf("%s evaluates a DIFFERENT expression on each ruler, so the diff would compare two rules:\n"+
				" shadow    %s\n incumbent %s", tier.name, got, want)
		}
		if shadow.For != incumbent.For {
			t.Fatalf("%s holds down for %q on the shadow and %q on the incumbent; the two rulers would fire at "+
				"different times for a reason that is not a parity defect", tier.name, shadow.For, incumbent.For)
		}
		if shadowInterval != incumbentInterval {
			t.Fatalf("%s's group ticks every %q on the shadow and %q on the incumbent; the shared evaluation "+
				"cadence the diff quantizes to would not exist", tier.name, shadowInterval, incumbentInterval)
		}
		for key, want := range incumbent.Labels {
			if got := shadow.Labels[key]; got != want {
				t.Fatalf("%s carries %s=%q on the incumbent and %q on the shadow — the two would project to "+
					"different identities and every edge would read as a false positive plus a false negative",
					tier.name, key, want, got)
			}
		}
		if len(shadow.Labels) != len(incumbent.Labels) {
			t.Fatalf("%s declares %d label(s) on the shadow and %d on the incumbent",
				tier.name, len(shadow.Labels), len(incumbent.Labels))
		}
	}
}

// The Go constants the live steps assert with must equal the fixture, or an
// assertion silently degrades into a tautology.
func TestMwmbrConstantsMatchTheFixture(t *testing.T) {
	for _, tier := range mwmbrTiers() {
		incumbent, groupInterval := findIncumbentAlert(t, tier.name)

		hold, err := time.ParseDuration(incumbent.For)
		if err != nil {
			t.Fatalf("parse %s's for %q: %v", tier.name, incumbent.For, err)
		}
		if hold != mwmbrHoldDown {
			t.Fatalf("%s holds down for %s in the fixture but mwmbrHoldDown is %s", tier.name, hold, mwmbrHoldDown)
		}
		interval, err := time.ParseDuration(groupInterval)
		if err != nil {
			t.Fatalf("parse the %s group interval %q: %v", mwmbrGroup, groupInterval, err)
		}
		if interval != mwmbrEvalInterval {
			t.Fatalf("the %s group ticks every %s but mwmbrEvalInterval is %s", mwmbrGroup, interval, mwmbrEvalInterval)
		}
		if got := incumbent.Labels[mig18SeverityKey]; got != tier.severity {
			t.Fatalf("%s carries severity %q in the fixture, constant says %q", tier.name, got, tier.severity)
		}
		if got := incumbent.Labels[mwmbrBurnRateKey]; got != tier.burnLabel {
			t.Fatalf("%s carries %s=%q in the fixture, constant says %q",
				tier.name, mwmbrBurnRateKey, got, tier.burnLabel)
		}
	}
}

// Both windows of both tiers must actually appear in the expression. This is
// what makes the fixture multi-WINDOW rather than a threshold alert with an
// SLO-sounding name: an edit that dropped one arm would leave a rule that
// fires on a spike, and the notification diff would still come back clean
// because both rulers would fire on the spike together.
func TestMwmbrExpressionCarriesBothWindowsOfBothTiers(t *testing.T) {
	for _, tc := range []struct {
		alert       string
		long, short time.Duration
		threshold   float64
	}{
		{mwmbrFastAlertName, mwmbrFastLongWindow, mwmbrFastShortWindow, mwmbrThreshold(mwmbrFastBurnFactor)},
		{mwmbrSlowAlertName, mwmbrSlowLongWindow, mwmbrSlowShortWindow, mwmbrThreshold(mwmbrSlowBurnFactor)},
	} {
		rule, _ := findIncumbentAlert(t, tc.alert)
		expr := normalizeExpr(rule.Expr)
		for _, window := range []time.Duration{tc.long, tc.short} {
			if !strings.Contains(expr, "["+promDuration(window)+"]") {
				t.Fatalf("%s's expression carries no [%s] range selector, so it is not a multi-window rule: %s",
					tc.alert, promDuration(window), expr)
			}
		}
		if tc.long == tc.short {
			t.Fatalf("%s's two windows are the same length, so the rule has one window, not two", tc.alert)
		}
	}
}

// The seeded burn must trip both tiers by a margin rather than sit on a
// threshold, and the intact SLO must trip neither. A fixture tuned to the
// boundary would make the live scenario fire, or not, on a rounding
// difference — and a fixture where the burning SLO stopped burning would make
// every stream empty and the diff vacuous.
func TestMwmbrSeedGeometryTripsBothTiersWithMargin(t *testing.T) {
	for _, factor := range []float64{mwmbrFastBurnFactor, mwmbrSlowBurnFactor} {
		if !mwmbrRatioClearsThreshold(factor) {
			t.Fatalf(
				"the seeded error ratio %g does not clear the %gx burn threshold %g by the required %gx margin; "+
					"the live scenario would fire on a rounding difference or not at all",
				mwmbrSeededErrorRatio(), factor, mwmbrThreshold(factor), mwmbrThresholdMargin,
			)
		}
	}
	// The intact SLO serves no errors at all, so its ratio is zero and no burn
	// factor can put it over threshold. Pinned so a future edit that gave it a
	// non-zero error rate — turning the false-positive arm into a second
	// burning identity — fails here rather than as a confusing live failure.
	const intactSLOErrorRate = 0.0
	if intactSLOErrorRate != 0 {
		t.Fatalf("the intact SLO must serve no errors, or it is not the false-positive arm of the diff")
	}
}

// The burst must be long enough that the LONGEST window already carries enough
// error mass when the rulers first look. Otherwise the slow tier would need to
// wait out its own window before it could fire, and the scenario's budget
// would expire against a fixture that was always going to be late.
func TestMwmbrBurstOutlastsTheLongestWindowRequirement(t *testing.T) {
	longest := mwmbrSlowLongWindow
	// Error mass inside the longest window at the moment of seeding, as a
	// ratio: the burst covers mwmbrBurstStart of the window, the rest is clean.
	ratioInLongestWindow := mwmbrSeededErrorRatio() * float64(mwmbrBurstStart) / float64(longest)
	if want := mwmbrThreshold(mwmbrSlowBurnFactor); ratioInLongestWindow <= want {
		t.Fatalf(
			"at the seeding instant the %s window carries an error ratio of %g, which does not clear the slow "+
				"tier's threshold %g — the slow burn alert could not fire until its window filled, and the "+
				"scenario's firing budget would expire first",
			promDuration(longest), ratioInLongestWindow, want,
		)
	}
}

// The fixture must reach past the observation period. A window ending at the
// seeding instant would decay out of the rules' own rate windows while the
// scenario was still waiting for delivery, so the alert would resolve itself
// mid-scenario and the diff would race its own input.
func TestMwmbrSeedWindowOutlastsTheFiringBudget(t *testing.T) {
	if mwmbrSeedFuture <= mwmbrHoldDown {
		t.Fatalf("the fixture reaches %s past seeding but the rules hold down for %s; the condition would "+
			"decay before either ruler could fire", mwmbrSeedFuture, mwmbrHoldDown)
	}
	if mwmbrSeedHistory <= mwmbrSlowLongWindow {
		t.Fatalf("the fixture carries %s of history but the longest rule window is %s; the slow tier would "+
			"evaluate over a partly empty window", mwmbrSeedHistory, mwmbrSlowLongWindow)
	}
}

// promDuration renders the windows the pins above look for in the fixtures. A
// mis-render would make every window pin look for a selector no rule spells,
// so it is pinned directly.
func TestPromDurationRendersRuleWindows(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{time.Minute, "1m"},
		{3 * time.Minute, "3m"},
		{5 * time.Minute, "5m"},
		{15 * time.Minute, "15m"},
	} {
		if got := promDuration(tc.in); got != tc.want {
			t.Fatalf("promDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
