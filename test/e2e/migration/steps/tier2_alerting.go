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

	"github.com/tsouza/cerberus/internal/migrateverify"
)

// This file holds MIG-18's comparator: alert-firing parity between an
// incumbent ruler and cerberus's shadow ruler (Grafana-managed alerting),
// per docs/migration-testing.md section 5's explicit non-epsilon predicate —
// "comparator quantizes fire/resolve timestamps to the shared evaluation
// interval; that quantization is the correct definition of 'fired in the
// same evaluation', not an allow-list" — plus the multi-window
// multi-burn-rate (MWMBR) SLO delta-holds-zero-across-the-bake-window check
// the same section requires.
//
// The comparator (AlertEvent / QuantizeToEvalInterval / DiffAlertStreams /
// BakeWindowHoldsZero) is real, pure, unit-tested logic — see
// tier2_alerting_test.go — independent of any live stack. The step bindings
// below wire it into a live scenario for the SHADOW leg only: this Tier-2
// substrate (tiers/tier2-ruler) stands up Grafana-managed alerting plus one
// dead-end receiver, but no genuinely independent INCUMBENT ruler +
// Alertmanager leg exists yet — MIG-18's own fixture requirement is "Tier-2
// dual rulers + dead-end Alertmanagers" (docs/migration-testing.md's TEST
// table), which this substrate does not provide. The incumbent-side step
// below fails with that gap named explicitly rather than fabricating a
// result; see this branch's task report for the full accounting.

// AlertEvent is one fire or resolve edge from a ruler's notification stream.
type AlertEvent struct {
	RuleName string
	Labels   map[string]string
	Status   string // "firing" or "resolved"
	At       time.Time
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
	var payload grafanaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("parse grafana webhook payload: %w", err)
	}
	out := make([]AlertEvent, 0, len(payload.Alerts))
	for _, a := range payload.Alerts {
		ev := AlertEvent{RuleName: a.Labels["alertname"], Labels: a.Labels, Status: a.Status}
		switch a.Status {
		case "firing":
			ev.At = a.StartsAt
		case "resolved":
			ev.At = a.EndsAt
		default:
			return nil, fmt.Errorf("alert %q carries unrecognised status %q", ev.RuleName, a.Status)
		}
		out = append(out, ev)
	}
	return out, nil
}

// tier2AlertingState is MIG-18's accumulated state: the incumbent and
// shadow rulers' notification streams, plus any MWMBR bake-window samples.
type tier2AlertingState struct {
	incumbent []AlertEvent
	shadow    []AlertEvent
	diff      AlertStreamDiff
	diffSet   bool
	mwmbr     []MWMBRSample
}

// errTier2NoIncumbentRuler is the honest, specific gap this substrate has
// today: MIG-18 needs a genuinely independent SECOND ruler + Alertmanager
// leg to diff against (docs/migration-testing.md's own TEST-table fixture
// requirement: "Tier-2 dual rulers + dead-end Alertmanagers"), and
// tiers/tier2-ruler stands up only the shadow leg (Grafana-managed
// alerting). Landing a real incumbent Prometheus ruler + Alertmanager +
// second dead-end receiver is out of this change's scope — see the task
// report for the full accounting of why.
var errTier2NoIncumbentRuler = errors.New(
	"MIG-18 needs an independent incumbent ruler + Alertmanager leg to diff the shadow ruler against; " +
		"this Tier-2 substrate (tiers/tier2-ruler) stands up only the shadow leg (Grafana-managed alerting) " +
		"plus its dead-end receiver — no incumbent Prometheus ruler / Alertmanager exists in this compose stack yet",
)

// errTier2NoMWMBRRule is the same class of gap for the MWMBR half: the rule
// fixture this substrate provisions (grafana/alerting/rules.yaml) is the
// kube-prometheus-stack recording rule + NodeCPUSaturation threshold alert
// only — no multi-window multi-burn-rate SLO rule is provisioned on either
// side to sample a bake-window delta from.
var errTier2NoMWMBRRule = errors.New(
	"MIG-18's MWMBR check needs a multi-window multi-burn-rate SLO rule provisioned on both the incumbent and " +
		"shadow rulers; this Tier-2 substrate's rule fixture (grafana/alerting/rules.yaml) provisions only the " +
		"kube-prometheus-stack recording rule + NodeCPUSaturation threshold alert — no MWMBR rule exists yet",
)

// registerTier2AlertingSteps binds MIG-18. The shadow-ruler leg is real and
// live; the incumbent-ruler and MWMBR-rule legs fail with the specific gap
// named above rather than fabricating a result — see this file's header.
func (w *World) registerTier2AlertingSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the incumbent ruler evaluates the same rule set over the same overlap$`, w.givenTier2IncumbentRulerEvaluated)
	ctx.Step(`^the shadow ruler evaluates the same rule set over the same overlap$`, w.whenTier2ShadowRulerEvaluated)
	ctx.Step(`^fire and resolve timestamps are diffed, eval-interval-quantized$`, w.whenTier2FireResolveDiffed)
	ctx.Step(`^false-positive, false-negative and timing-skew counts are all zero$`, w.thenTier2AlertStreamsClean)
	ctx.Step(`^a multi-window multi-burn-rate SLO rule is armed near threshold on both rulers$`, w.givenTier2MWMBRRuleArmed)
	ctx.Step(
		`^the multi-window multi-burn-rate SLO delta holds zero across the full bake window$`,
		w.thenTier2MWMBRBakeWindowHoldsZero,
	)
}

// givenTier2IncumbentRulerEvaluated is the honest blocker: see
// errTier2NoIncumbentRuler.
func (w *World) givenTier2IncumbentRulerEvaluated() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	return errTier2NoIncumbentRuler
}

// whenTier2ShadowRulerEvaluated fetches the shadow ruler's (Grafana's)
// captured notification stream from the dead-end receiver — the one leg of
// MIG-18 this substrate can genuinely exercise today.
func (w *World) whenTier2ShadowRulerEvaluated() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), tier2QueryTimeout)
	defer cancel()
	events, err := fetchDeadEndAlertEvents(ctx, w.tier2.DeadEndURL)
	if err != nil {
		return fmt.Errorf("fetch the shadow ruler's captured notifications: %w", err)
	}
	w.tier2Alerting.shadow = events
	return nil
}

// whenTier2FireResolveDiffed computes the eval-interval-quantized diff. It
// is unreachable in this substrate today because
// givenTier2IncumbentRulerEvaluated always fails first — it is implemented
// in full so the comparator wiring is ready the moment an incumbent leg
// lands.
func (w *World) whenTier2FireResolveDiffed() error {
	w.tier2Alerting.diff = DiffAlertStreams(w.tier2Alerting.incumbent, w.tier2Alerting.shadow, tier2RecordingRuleInterval)
	w.tier2Alerting.diffSet = true
	return nil
}

// thenTier2AlertStreamsClean asserts MIG-18's PASS shape: zero divergence of
// every kind the diff tracks.
func (w *World) thenTier2AlertStreamsClean() error {
	if !w.tier2Alerting.diffSet {
		return fmt.Errorf("the fire/resolve streams were never diffed; the scenario must diff them first")
	}
	d := w.tier2Alerting.diff
	if !d.Clean() {
		return fmt.Errorf(
			"alert-firing parity diverged: false_positive=%d false_negative=%d timing_skew=%d (matched=%d)",
			d.FalsePositive, d.FalseNegative, d.TimingSkew, d.Matched,
		)
	}
	return nil
}

// givenTier2MWMBRRuleArmed is the honest blocker for the MWMBR half: see
// errTier2NoMWMBRRule.
func (w *World) givenTier2MWMBRRuleArmed() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	return errTier2NoMWMBRRule
}

// thenTier2MWMBRBakeWindowHoldsZero asserts the bake-window predicate over
// whatever samples were collected, reusing migrateverify.DefaultTolerance —
// the same exact-parity epsilon MIG-13/MIG-19 reuse — rather than inventing
// a second, unexplained float tolerance for what is still "the same value,
// computed twice" at bottom. Like whenTier2FireResolveDiffed, it is
// unreachable today (givenTier2MWMBRRuleArmed always fails first) but
// implemented in full.
func (w *World) thenTier2MWMBRBakeWindowHoldsZero() error {
	return BakeWindowHoldsZero(w.tier2Alerting.mwmbr, migrateverify.DefaultTolerance)
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

// fetchDeadEndAlertEvents fetches every notification the dead-end receiver
// has captured and parses each one's body into its AlertEvents.
func fetchDeadEndAlertEvents(ctx context.Context, deadEndURL string) ([]AlertEvent, error) {
	target := strings.TrimRight(deadEndURL, "/") + "/notifications/list"
	var resp deadEndListResponse
	if err := fetchJSONGet(ctx, target, &resp); err != nil {
		return nil, err
	}
	out := make([]AlertEvent, 0, len(resp.Notifications))
	for _, n := range resp.Notifications {
		events, err := ParseGrafanaWebhookEvents(n.Body)
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
