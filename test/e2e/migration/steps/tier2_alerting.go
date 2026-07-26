package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// This file holds MIG-18: what the single-ruler Tier-2 substrate can honestly
// prove about the shadow ruler's alert lifecycle, plus the comparator the
// eventual incumbent-versus-shadow diff will use.
//
// What the live scenario asserts (steps at the bottom of this file), all of it
// against tiers/tier2-ruler's real Grafana, real cerberus and real dead-end
// receiver:
//
//   - the shadow ruler puts the probe alert into PENDING and keeps it there
//     for its provisioned hold-down before it reports it firing — a real
//     state-machine observation, not a timestamp guess;
//   - the firing notification actually reaches the dead-end receiver, and no
//     other receiver exists for it to reach;
//   - that notification carries the rule's provisioned labels verbatim, and
//     an annotation RENDERED from the alert's own label set rather than
//     shipped as an unexpanded template;
//   - clearing the condition produces a matching RESOLVE edge for the same
//     alert identity, ordered after the firing edge.
//
// What it does NOT assert, and why: MIG-18's story ultimately wants alert
// firing PARITY — the eval-interval-quantized diff of an INCUMBENT ruler's
// notification stream against the shadow's, and an MWMBR SLO burn-rate delta
// held at zero across a full bake window. Both need a second, genuinely
// independent ruler with its own dead-end Alertmanager (the story's own
// fixture line in docs/migration-testing.md section 6: "Tier-2 dual rulers +
// dead-end Alertmanagers"), and an MWMBR rule fixture provisioned on both
// sides. This substrate stands up ONE ruler and ONE receiver, so there is no
// second stream to diff — one ruler diffed against itself is definitionally
// clean and would prove nothing.
//
// The comparator for that future diff is nonetheless real, pure and
// unit-tested here and now — QuantizeToEvalInterval / DiffAlertStreams /
// BakeWindowHoldsZero, pinned by tier2_alerting_test.go — so landing the
// incumbent leg is a substrate change, not an algorithm change. Section 6's
// MIG-18 row records exactly that split.

// AlertEvent is one fire or resolve edge from a ruler's notification stream.
type AlertEvent struct {
	RuleName    string
	Labels      map[string]string
	Annotations map[string]string
	Status      string // "firing" or "resolved"
	At          time.Time
}

// QuantizeToEvalInterval floors t to the evaluation-interval boundary at or
// before it. This is the docs' explicit non-epsilon predicate for
// alert-firing parity: two independent rulers on independent evaluation
// schedules only agree on "fired in the same evaluation", never to
// millisecond precision, so comparing raw timestamps (with any float
// epsilon, however generous) is the wrong shape of check entirely.
func QuantizeToEvalInterval(t time.Time, evalInterval time.Duration) time.Time {
	if evalInterval <= 0 {
		return t
	}
	return t.Truncate(evalInterval)
}

// AlertStreamDiff is DiffAlertStreams's verdict: false-positive (shadow
// fired, incumbent did not), false-negative (incumbent fired, shadow did
// not) and timing-skew (both fired the same rule+labels+status, but not in
// the same quantized evaluation) counts, plus how many events matched
// cleanly.
type AlertStreamDiff struct {
	FalsePositive int
	FalseNegative int
	TimingSkew    int
	Matched       int
}

// Clean reports whether the diff carries zero divergence of any kind — the
// PASS shape MIG-18 asserts.
func (d AlertStreamDiff) Clean() bool {
	return d.FalsePositive == 0 && d.FalseNegative == 0 && d.TimingSkew == 0
}

// alertEventKey identifies "the same alert" across two independently
// evaluated streams: rule name, status and the full label set (sorted, so
// two label maps with the same pairs in different iteration order compare
// equal).
func alertEventKey(e AlertEvent) string {
	keys := make([]string, 0, len(e.Labels))
	for k := range e.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(e.RuleName)
	b.WriteByte('|')
	b.WriteString(e.Status)
	for _, k := range keys {
		b.WriteByte('|')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(e.Labels[k])
	}
	return b.String()
}

// DiffAlertStreams matches an incumbent and a shadow ruler's notification
// streams by alert identity (rule + status + labels) and, within a match,
// by the eval-interval-quantized instant each edge landed at.
func DiffAlertStreams(incumbent, shadow []AlertEvent, evalInterval time.Duration) AlertStreamDiff {
	incBy := quantizedByKey(incumbent, evalInterval)
	shadowBy := quantizedByKey(shadow, evalInterval)

	var diff AlertStreamDiff
	seen := make(map[string]bool, len(incBy))
	for key, incTimes := range incBy {
		seen[key] = true
		shTimes := shadowBy[key]
		for t := range incTimes {
			switch {
			case shTimes[t]:
				diff.Matched++
			case len(shTimes) > 0:
				// The same alert fired on both sides, just not in the same
				// quantized evaluation — a timing skew, not a miss.
				diff.TimingSkew++
			default:
				diff.FalseNegative++
			}
		}
	}
	for key, shTimes := range shadowBy {
		if seen[key] {
			continue
		}
		diff.FalsePositive += len(shTimes)
	}
	return diff
}

// quantizedByKey buckets events by alertEventKey, each bucket holding the
// set of eval-interval-quantized instants that key fired at.
func quantizedByKey(events []AlertEvent, evalInterval time.Duration) map[string]map[time.Time]bool {
	out := make(map[string]map[time.Time]bool, len(events))
	for _, e := range events {
		key := alertEventKey(e)
		if out[key] == nil {
			out[key] = map[time.Time]bool{}
		}
		out[key][QuantizeToEvalInterval(e.At, evalInterval)] = true
	}
	return out
}

// MWMBRSample is one burn-rate delta observation at an instant within the
// bake window: incumbent's evaluated burn rate minus the shadow's.
type MWMBRSample struct {
	At    time.Time
	Delta float64
}

// BakeWindowHoldsZero asserts docs section 5's MWMBR predicate: the delta
// must hold zero across the FULL bake window, not a spot check — a burn-rate
// rule evaluated continuously can disagree only once and still page
// incorrectly, so a single passing sample proves nothing about the rest of
// the window.
func BakeWindowHoldsZero(samples []MWMBRSample, tolerance float64) error {
	if len(samples) == 0 {
		return errors.New("mwmbr bake-window check: zero samples observed; a check over an empty window proves nothing")
	}
	for _, s := range samples {
		if math.Abs(s.Delta) > tolerance {
			return fmt.Errorf(
				"mwmbr burn-rate delta at %s = %g, want zero within %g — the bake window did not hold",
				s.At, s.Delta, tolerance,
			)
		}
	}
	return nil
}

// grafanaWebhookAlert is the subset of Grafana's (Alertmanager-compatible)
// webhook notifier payload ParseGrafanaWebhookEvents reads.
type grafanaWebhookAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt"`
}

type grafanaWebhookPayload struct {
	Alerts []grafanaWebhookAlert `json:"alerts"`
}

// ParseGrafanaWebhookEvents decodes one dead-end-receiver-captured webhook
// body into its AlertEvents: a firing alert's edge is its startsAt, a
// resolved alert's edge is its endsAt.
func ParseGrafanaWebhookEvents(body []byte) ([]AlertEvent, error) {
	return parseGrafanaWebhook(body, "")
}

// ParseGrafanaWebhookEventsNamed is ParseGrafanaWebhookEvents narrowed to one
// alert name. It exists because the ONE dead-end receiver captures every
// notification the substrate produces — MIG-09's synthetic contact-point test
// among them — while a scenario asserts about exactly one alert. Narrowing is
// scoping, not tolerance: a MALFORMED notification for the named alert still
// fails, and the narrowing is by the alert's own identity rather than by any
// property of what went wrong.
func ParseGrafanaWebhookEventsNamed(body []byte, alertName string) ([]AlertEvent, error) {
	return parseGrafanaWebhook(body, alertName)
}

// parseGrafanaWebhook decodes a captured webhook body, keeping every alert
// when alertName is empty and only the named one otherwise.
func parseGrafanaWebhook(body []byte, alertName string) ([]AlertEvent, error) {
	var payload grafanaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse grafana webhook payload: %w", err)
	}
	out := make([]AlertEvent, 0, len(payload.Alerts))
	for _, a := range payload.Alerts {
		name := a.Labels["alertname"]
		if alertName != "" && name != alertName {
			continue
		}
		ev := AlertEvent{RuleName: name, Labels: a.Labels, Annotations: a.Annotations, Status: a.Status}
		switch a.Status {
		case alertStatusFiring:
			ev.At = a.StartsAt
		case alertStatusResolved:
			ev.At = a.EndsAt
		default:
			return nil, fmt.Errorf("alert %q carries unrecognised status %q", ev.RuleName, a.Status)
		}
		out = append(out, ev)
	}
	return out, nil
}

// The two notification statuses Grafana's webhook notifier emits.
const (
	alertStatusFiring   = "firing"
	alertStatusResolved = "resolved"
)

// === MIG-18's live fire/resolve probe ===
//
// Every constant below mirrors a field of the `migration.probe` rule group in
// tiers/tier2-ruler/grafana/alerting/rules.yaml, whose own comment names this
// file back. A drift on either side turns a real assertion into a vacuous one,
// so they are spelled out here rather than read out of the ruler at runtime:
// reading them back from the thing under test would make every assertion
// self-fulfilling.
const (
	mig18ProbeMetric     = "cerberus_migration_alert_probe"
	mig18ProbeAlertName  = "MigrationShadowRulerProbe"
	mig18ProbeService    = "migration-lane"
	mig18SeverityKey     = "severity"
	mig18SeverityValue   = "warning"
	mig18ProbeLabelKey   = "probe"
	mig18ProbeLabelValue = "fire-resolve"
	mig18SummaryKey      = "summary"
	// mig18TemplateOpen is Grafana's annotation-template opening delimiter. An
	// annotation that still carries it was shipped verbatim rather than
	// rendered, which is the failure the rendered-annotation assertion exists
	// to catch.
	mig18TemplateOpen = "{{"
	// mig18ProbeHoldDown must equal the probe rule's `for:`. It is the pending
	// window MIG-18 asserts the shadow ruler actually honored before firing.
	mig18ProbeHoldDown = 30 * time.Second
	// mig18FiringValue and mig18ClearedValue straddle the probe rule's
	// threshold (`gt 0.5`), so arming and clearing are unambiguous rather than
	// borderline.
	mig18FiringValue  = 1.0
	mig18ClearedValue = 0.0
)

// Seed geometry for the probe series. The arming window only has to outlast
// the rule's own lookback resolution — the rule reads an INSTANT vector, so a
// couple of minutes of samples ending at "now" is ample, and a shorter window
// than the recording rule's keeps the seed cheap.
const (
	mig18SeedWindow = 2 * time.Minute
	mig18SeedStep   = 15 * time.Second
)

// Budgets. Each is a bounded deadline on a poll loop — never a sleep, never a
// retry-then-continue: each one fails its step when it expires.
const (
	// mig18FiringWait covers the rule group's next tick, the full hold-down,
	// and a couple of missed ticks besides.
	mig18FiringWait = 3 * time.Minute
	// mig18NotifyWait covers the notification policy's group_wait plus
	// delivery to the dead-end receiver.
	mig18NotifyWait = 2 * time.Minute
	// mig18ResolveWait covers the evaluation ticks it takes the cleared series
	// to drive the alert back to normal, plus the policy's group_interval
	// flush of the resolved notification.
	mig18ResolveWait = 3 * time.Minute
	mig18Poll        = 2 * time.Second
)

// Grafana reports an alert instance's state as "Pending"/"Alerting" while the
// Prometheus-compatible shape the same endpoint serves uses
// "pending"/"firing". Both spellings are accepted, compared lower-cased —
// this is protocol tolerance about how one state is NAMED, not tolerance about
// whether the state was reached.
var (
	mig18PendingStates = map[string]bool{"pending": true}
	mig18FiringStates  = map[string]bool{"alerting": true, "firing": true}
)

// tier2AlertingState is MIG-18's accumulated state across its steps.
type tier2AlertingState struct {
	// armedAt is the wall-clock instant the above-threshold samples became
	// visible — read AFTER the insert returned, so it is the EARLIEST moment
	// the ruler could have started its pending clock. Measuring the hold-down
	// from here can only under-state it, never over-state it, which is the
	// direction an assertion is allowed to be wrong in.
	armedAt time.Time
	// armedLastSample is the instant the last above-threshold sample carries.
	// The clearing batch starts strictly after it so the two never collide on
	// one instant.
	armedLastSample time.Time
	// pendingSeen records that the ruler reported the probe alert PENDING at
	// least once before it reported it firing.
	pendingSeen bool
	// firingSeenAt is when this harness first observed the ruler reporting the
	// probe alert firing.
	firingSeenAt time.Time
	// firing and resolving are the edges the dead-end receiver captured for
	// the probe alert.
	firing    []AlertEvent
	resolving []AlertEvent
}

// registerTier2AlertingSteps binds MIG-18: the shadow ruler's own fire and
// resolve lifecycle, end to end against the live substrate. See this file's
// header for what this deliberately does not claim.
func (w *World) registerTier2AlertingSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the shadow ruler's fire-and-resolve probe is armed above its firing threshold$`, w.givenMig18ProbeArmed)
	ctx.Step(`^the operator waits for the shadow ruler to report the probe alert firing$`, w.whenMig18ProbeFires)
	ctx.Step(`^the shadow ruler reported the probe alert pending before it reported it firing$`, w.thenMig18ProbeWasPending)
	ctx.Step(`^the probe alert did not fire before its provisioned pending window had elapsed$`, w.thenMig18HoldDownHonored)
	ctx.Step(`^the operator waits for the firing notification to reach the dead-end receiver$`, w.whenMig18FiringNotificationArrives)
	ctx.Step(`^the dead-end receiver captured a firing edge for the probe alert$`, w.thenMig18FiringEdgeCaptured)
	ctx.Step(`^that firing edge carries the probe alert's provisioned labels$`, w.thenMig18FiringEdgeCarriesLabels)
	ctx.Step(
		`^that firing edge carries an annotation rendered from the probe alert's own label set$`,
		w.thenMig18FiringEdgeCarriesRenderedAnnotation,
	)
	ctx.Step(
		`^the operator clears the probe below its firing threshold and waits for the resolve notification$`,
		w.whenMig18ProbeCleared,
	)
	ctx.Step(`^the dead-end receiver captured a resolving edge for the probe alert$`, w.thenMig18ResolvingEdgeCaptured)
	ctx.Step(`^the resolving edge does not precede the firing edge$`, w.thenMig18ResolveFollowsFire)
}

// givenMig18ProbeArmed writes above-threshold samples for the probe series,
// ending at "now". Nothing in the Tier-2 ingest path produces this series, and
// the probe rule's own query window is wall-clock-relative, so a fixed
// historical window would age out of its lookback before the rule ever saw it.
func (w *World) givenMig18ProbeArmed() error {
	now := time.Now().UTC()
	last, err := w.seedMig18Probe(now.Add(-mig18SeedWindow), now, mig18FiringValue)
	if err != nil {
		return err
	}
	w.tier2Alerting.armedLastSample = last
	w.tier2Alerting.armedAt = time.Now().UTC()
	return nil
}

// seedMig18Probe writes the probe gauge at one constant value across
// [start, end], returning the instant the last sample landed at.
func (w *World) seedMig18Probe(start, end time.Time, value float64) (time.Time, error) {
	if err := w.requireTier2Live(); err != nil {
		return time.Time{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2SeedBudget)
	defer cancel()

	conn, err := w.tier2.DialCH(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("migration harness: dial clickhouse to seed the fire/resolve probe: %w", err)
	}
	defer func() { _ = conn.Close() }()

	samples := make([]seed.Sample, 0, int(end.Sub(start)/mig18SeedStep)+1)
	last := start
	for t := start; !t.After(end); t = t.Add(mig18SeedStep) {
		samples = append(samples, seed.Sample{Time: t, Value: value})
		last = t
	}
	if len(samples) == 0 {
		return time.Time{}, fmt.Errorf(
			"migration harness: the fire/resolve probe seed window [%s, %s] holds no sample instant, so it would arm nothing",
			start, end,
		)
	}
	fixture := seed.Fixture{Gauge: []seed.MetricSeries{{
		MetricName:  mig18ProbeMetric,
		ServiceName: mig18ProbeService,
		Attributes:  map[string]string{mig18ProbeLabelKey: mig18ProbeLabelValue},
		Samples:     samples,
	}}}
	if err := seed.InsertFixture(ctx, conn, fixture); err != nil {
		return time.Time{}, fmt.Errorf("migration harness: seed %s at %g: %w", mig18ProbeMetric, value, err)
	}
	return last, nil
}

// whenMig18ProbeFires polls the ruler's own live rule state until the probe
// alert reports firing, recording on the way whether it passed through
// PENDING first. Both facts are what the two Then steps assert; neither is
// re-derived from a timestamp the ruler reported about itself.
func (w *World) whenMig18ProbeFires() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx := context.Background()
	deadline := time.Now().Add(mig18FiringWait)
	var last error
	for {
		groups, err := fetchRulerGroups(ctx, w.tier2.GrafanaURL)
		if err == nil {
			rule, instance, found := findProbeInstance(groups)
			switch {
			case !found:
				// Quote the rule's own health, not just "no instance": a rule
				// whose query against cerberus is erroring produces no instance
				// either, and the two failures need different fixes.
				last = fmt.Errorf(
					"the ruler reports no alert instance for %q yet (rule health=%q lastError=%q lastEvaluation=%q)",
					mig18ProbeAlertName, rule.Health, rule.LastError, rule.LastEvaluation,
				)
			case mig18FiringStates[strings.ToLower(instance.State)]:
				w.tier2Alerting.firingSeenAt = time.Now().UTC()
				return nil
			case mig18PendingStates[strings.ToLower(instance.State)]:
				w.tier2Alerting.pendingSeen = true
				last = fmt.Errorf("the probe alert is still pending")
			default:
				last = fmt.Errorf("the probe alert reports state %q", instance.State)
			}
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the shadow ruler never reported %q firing within %s: %w",
				mig18ProbeAlertName, mig18FiringWait, last)
		}
		time.Sleep(mig18Poll)
	}
}

// findProbeInstance locates the probe rule and its single live alert
// instance. The rule's expression collapses to one series precisely so "the
// probe alert" is one identity rather than whatever cardinality the seed
// happened to write. The rule itself is returned even when it has no instance
// yet, so a caller can report its health rather than only its absence.
func findProbeInstance(groups []rulerRuleGroup) (rulerRule, rulerAlertInstance, bool) {
	for _, g := range groups {
		for _, r := range g.Rules {
			if r.Name != mig18ProbeAlertName {
				continue
			}
			if len(r.Alerts) == 0 {
				return r, rulerAlertInstance{}, false
			}
			return r, r.Alerts[0], true
		}
	}
	return rulerRule{}, rulerAlertInstance{}, false
}

// thenMig18ProbeWasPending asserts the ruler held the alert in PENDING rather
// than jumping straight to firing — the observable half of "a hold-down
// happened at all".
func (w *World) thenMig18ProbeWasPending() error {
	if w.tier2Alerting.firingSeenAt.IsZero() {
		return fmt.Errorf("the probe alert was never observed firing; the scenario must wait for it first")
	}
	if !w.tier2Alerting.pendingSeen {
		return fmt.Errorf(
			"the shadow ruler never reported %q pending before reporting it firing — an alert that skips the "+
				"pending state has no hold-down, so a repointed pager would fire on a single stray evaluation",
			mig18ProbeAlertName,
		)
	}
	return nil
}

// thenMig18HoldDownHonored asserts the measured half: the firing report could
// not have arrived before the provisioned pending window had elapsed since the
// condition was armed. Measured against this harness's own clock, not against
// a timestamp the ruler self-reported.
func (w *World) thenMig18HoldDownHonored() error {
	armed, fired := w.tier2Alerting.armedAt, w.tier2Alerting.firingSeenAt
	if armed.IsZero() {
		return fmt.Errorf("the probe was never armed; the scenario must arm it first")
	}
	if fired.IsZero() {
		return fmt.Errorf("the probe alert was never observed firing; the scenario must wait for it first")
	}
	if held := fired.Sub(armed); held < mig18ProbeHoldDown {
		return fmt.Errorf(
			"%q was reported firing %s after its condition was armed, sooner than its provisioned pending "+
				"window of %s — the ruler did not honor the hold-down",
			mig18ProbeAlertName, held, mig18ProbeHoldDown,
		)
	}
	return nil
}

// whenMig18FiringNotificationArrives polls the dead-end receiver until it has
// captured a firing edge for the probe alert.
func (w *World) whenMig18FiringNotificationArrives() error {
	events, err := w.pollProbeEdges(alertStatusFiring, mig18NotifyWait)
	if err != nil {
		return err
	}
	w.tier2Alerting.firing = events
	return nil
}

// whenMig18ProbeCleared drives the probe series back below its threshold and
// then polls for the resolve edge. The cleared samples start strictly AFTER
// the last armed sample, so the two batches never collide on one instant and
// the instant vector the rule evaluates unambiguously reads the cleared value.
func (w *World) whenMig18ProbeCleared() error {
	if w.tier2Alerting.armedLastSample.IsZero() {
		return fmt.Errorf("the probe was never armed; the scenario must arm it first")
	}
	start := w.tier2Alerting.armedLastSample.Add(mig18SeedStep)
	if _, err := w.seedMig18Probe(start, time.Now().UTC(), mig18ClearedValue); err != nil {
		return err
	}
	events, err := w.pollProbeEdges(alertStatusResolved, mig18ResolveWait)
	if err != nil {
		return err
	}
	w.tier2Alerting.resolving = events
	return nil
}

// pollProbeEdges polls the dead-end receiver until it has captured at least
// one edge of the given status for the probe alert, returning every such edge.
func (w *World) pollProbeEdges(status string, budget time.Duration) ([]AlertEvent, error) {
	if err := w.requireTier2Live(); err != nil {
		return nil, err
	}
	ctx := context.Background()
	deadline := time.Now().Add(budget)
	var last error
	for {
		events, err := fetchProbeAlertEvents(ctx, w.tier2.DeadEndURL)
		if err == nil {
			matching := filterByStatus(events, status)
			if len(matching) > 0 {
				return matching, nil
			}
			last = fmt.Errorf("the receiver holds %d edge(s) for %q, none of them %s",
				len(events), mig18ProbeAlertName, status)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the dead-end receiver never captured a %s edge for %q within %s: %w",
				status, mig18ProbeAlertName, budget, last)
		}
		time.Sleep(mig18Poll)
	}
}

// filterByStatus keeps the edges of one status.
func filterByStatus(events []AlertEvent, status string) []AlertEvent {
	out := make([]AlertEvent, 0, len(events))
	for _, e := range events {
		if e.Status == status {
			out = append(out, e)
		}
	}
	return out
}

// thenMig18FiringEdgeCaptured asserts the poll actually observed a firing
// edge, and reports how many.
func (w *World) thenMig18FiringEdgeCaptured() error {
	if len(w.tier2Alerting.firing) == 0 {
		return fmt.Errorf(
			"the dead-end receiver captured no firing edge for %q; the shadow ruler evaluated but nothing "+
				"reached the one contact point this substrate provisions", mig18ProbeAlertName,
		)
	}
	return nil
}

// thenMig18FiringEdgeCarriesLabels asserts the captured notification carries
// the rule's provisioned label set verbatim — the routing keys an operator's
// existing Alertmanager silences and routes are written against.
func (w *World) thenMig18FiringEdgeCarriesLabels() error {
	if err := w.thenMig18FiringEdgeCaptured(); err != nil {
		return err
	}
	edge := w.tier2Alerting.firing[0]
	want := map[string]string{
		"alertname":        mig18ProbeAlertName,
		mig18SeverityKey:   mig18SeverityValue,
		mig18ProbeLabelKey: mig18ProbeLabelValue,
	}
	for key, value := range want {
		got, ok := edge.Labels[key]
		if !ok {
			return fmt.Errorf("the firing edge for %q carries no %q label; it carries %v",
				mig18ProbeAlertName, key, edge.Labels)
		}
		if got != value {
			return fmt.Errorf("the firing edge for %q carries %s=%q, want %q",
				mig18ProbeAlertName, key, got, value)
		}
	}
	return nil
}

// thenMig18FiringEdgeCarriesRenderedAnnotation asserts the notification's
// annotation was RENDERED from the alert's own label set: it must carry the
// label's value and must not still carry the template delimiter. An
// unexpanded annotation is exactly the class of breakage a repointed pager
// notices only once it has already woken somebody.
func (w *World) thenMig18FiringEdgeCarriesRenderedAnnotation() error {
	if err := w.thenMig18FiringEdgeCaptured(); err != nil {
		return err
	}
	edge := w.tier2Alerting.firing[0]
	summary, ok := edge.Annotations[mig18SummaryKey]
	if !ok {
		return fmt.Errorf("the firing edge for %q carries no %q annotation; it carries %v",
			mig18ProbeAlertName, mig18SummaryKey, edge.Annotations)
	}
	if strings.Contains(summary, mig18TemplateOpen) {
		return fmt.Errorf("the %q annotation arrived unrendered, still carrying %q: %q",
			mig18SummaryKey, mig18TemplateOpen, summary)
	}
	if !strings.Contains(summary, mig18ProbeLabelValue) {
		return fmt.Errorf(
			"the %q annotation %q does not carry the alert's own %s label value %q, so it was not rendered "+
				"from the alert's label set",
			mig18SummaryKey, summary, mig18ProbeLabelKey, mig18ProbeLabelValue,
		)
	}
	return nil
}

// thenMig18ResolvingEdgeCaptured asserts the resolve half of the lifecycle
// reached the receiver too — a ruler that fires but never resolves leaves an
// operator's incident open forever.
func (w *World) thenMig18ResolvingEdgeCaptured() error {
	if len(w.tier2Alerting.resolving) == 0 {
		return fmt.Errorf(
			"the dead-end receiver captured no resolving edge for %q after its condition cleared",
			mig18ProbeAlertName,
		)
	}
	return nil
}

// thenMig18ResolveFollowsFire asserts the two edges are ordered as a lifecycle
// rather than as two unrelated notifications that happened to share a name.
func (w *World) thenMig18ResolveFollowsFire() error {
	if err := w.thenMig18FiringEdgeCaptured(); err != nil {
		return err
	}
	if err := w.thenMig18ResolvingEdgeCaptured(); err != nil {
		return err
	}
	firedAt := w.tier2Alerting.firing[0].At
	resolvedAt := w.tier2Alerting.resolving[0].At
	if resolvedAt.Before(firedAt) {
		return fmt.Errorf(
			"%q resolved at %s, before it fired at %s — the captured edges are not one alert's lifecycle",
			mig18ProbeAlertName, resolvedAt, firedAt,
		)
	}
	return nil
}

// deadEndNotification mirrors the dead-end receiver's /notifications/list
// response shape (tiers/tier2-ruler/receiver/main.go's capturedNotification).
type deadEndNotification struct {
	ReceivedAt time.Time       `json:"received_at"`
	Body       json.RawMessage `json:"body"`
}

type deadEndListResponse struct {
	Notifications []deadEndNotification `json:"notifications"`
}

// fetchProbeAlertEvents fetches every notification the dead-end receiver has
// captured and returns the edges belonging to MIG-18's probe alert. The
// receiver is shared with MIG-09's contact-point test, so the narrowing is by
// alert identity — see ParseGrafanaWebhookEventsNamed.
func fetchProbeAlertEvents(ctx context.Context, deadEndURL string) ([]AlertEvent, error) {
	target := strings.TrimRight(deadEndURL, "/") + "/notifications/list"
	var resp deadEndListResponse
	if err := fetchJSONGet(ctx, target, &resp); err != nil {
		return nil, err
	}
	out := make([]AlertEvent, 0, len(resp.Notifications))
	for _, n := range resp.Notifications {
		events, err := ParseGrafanaWebhookEventsNamed(n.Body, mig18ProbeAlertName)
		if err != nil {
			return nil, fmt.Errorf("notification received at %s: %w", n.ReceivedAt, err)
		}
		out = append(out, events...)
	}
	return out, nil
}

// fetchJSONGet GETs target, requiring a 200 with a JSON body, and decodes it
// into out.
func fetchJSONGet(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s body: %w", target, err)
	}
	return nil
}
