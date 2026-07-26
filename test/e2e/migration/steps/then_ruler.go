package steps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// The rule fixture MIG-09 drives. Mirrored from
// tiers/tier2-ruler/tier2_ruler_test.go's substrate self-check, not
// reinvented: both must stay in lock-step with
// grafana/alerting/{rules,contact-points}.yaml, whose own comments name
// these constants' source files back.
const (
	rulerRecordingGroup = "node.rules"
	rulerRecordingRule  = "node:node_cpu_utilisation:avg1m"
	rulerAlertGroup     = "node.alerts"
	rulerAlertRule      = "NodeCPUSaturation"
	rulerContactPoint   = "dead-end"
	rulerContactUID     = "dead-end-webhook"
	// rulerContactURL is the address Grafana itself dials — inside the
	// compose network, not this test's own host-published one.
	rulerContactURL = "http://dead-end-receiver:8090/webhook"
)

// Budgets. Every one is a bounded deadline on a poll loop — never a sleep,
// never a retry-then-continue: each fails the step when it expires.
const (
	// rulerEvalWait bounds how long the poll waits for both rule groups to
	// report a live evaluation against cerberus. The groups' `interval: 10s`
	// (grafana/alerting/rules.yaml) sets the expected tick cadence; this
	// budget covers a couple of missed ticks besides the first.
	rulerEvalWait = 90 * time.Second
	// rulerLandingWait bounds how long the poll waits for the recording
	// rule's output to round-trip through write-relay's federate scrape and
	// land in ClickHouse. The federate job's `scrape_interval: 5s`
	// (otel-collector-config.yaml) sets the expected cadence; this budget
	// covers cold-start (a fresh recording-rule tick, a fresh federate tick,
	// and the exporter's own flush) besides steady-state polling.
	rulerLandingWait = 2 * time.Minute
	// rulerNotificationWait bounds how long the poll waits for the dead-end
	// receiver to observe the test notification Grafana was asked to send.
	rulerNotificationWait = 30 * time.Second
	// rulerPoll is the gap between every poll loop's attempts.
	rulerPoll = 2 * time.Second
	// rulerHTTPTimeout bounds one request inside any poll loop in this file.
	rulerHTTPTimeout = 15 * time.Second
	// rulerErrBodyLimit caps how much of a failing response body a step
	// quotes back into its failure message.
	rulerErrBodyLimit = 4096
)

// The ClickHouse connection MIG-09's seeding step dials. Tier-2 reuses
// Tier-1's own ClickHouse instance (same compose project — see
// tiers/tier2-ruler/docker-compose.ruler.yml's header comment), so these
// mirror lib/live.go's TIER1_CH_* contract on the Tier-1 Gherkin mechanism
// rather than inventing a second one.
const (
	rulerCHAddrEnvKey      = "TIER1_CH_ADDR"
	rulerCHDatabaseEnvKey  = "TIER1_CH_DATABASE"
	rulerCHUsernameEnvKey  = "TIER1_CH_USERNAME"
	rulerCHPasswordEnvKey  = "TIER1_CH_PASSWORD" //nolint:gosec // env var NAME, not a credential value
	defaultRulerCHAddr     = "127.0.0.1:27000"
	defaultRulerCHDatabase = "otel"
	defaultRulerCHUsername = "cerberus"
	defaultRulerCHPassword = "cerberus" //nolint:gosec // default fixture credential, not a real one
)

// The recording rule fixture (grafana/alerting/rules.yaml) is reused
// verbatim from the kube-prometheus-stack archetype's own rule files, and
// its expression depends on `node_cpu_seconds_total` — a metric the
// three-signal fixture `just migration-tier2-seed` writes never declares
// (three-signal seeds its own synthetic `cerberus_migration_*` names). Without
// it the rule's query legitimately answers an empty vector — Grafana reports
// health=ok (an empty result is still a successful evaluation) and there is
// nothing for the write-back leg to write, which reads identically to a
// broken write path from the outside. This constant set seeds exactly the
// one series the fixture's `expr` needs, directly into ClickHouse — the same
// leg InsertFixture uses for the three-signal fixture, just for a series no
// declaration carries.
const (
	rulerCPUMetric      = "node_cpu_seconds_total"
	rulerCPUServiceName = "node-exporter"
	rulerCPUCoreCount   = 2
	// rulerCPUSeedWindow must exceed the recording rule's rate() window (5m,
	// grafana/alerting/rules.yaml) so cerberus never extrapolates past data
	// this step actually wrote.
	rulerCPUSeedWindow = 6 * time.Minute
	rulerCPUSeedStep   = 15 * time.Second
	// rulerCPUIdleRatePerSecond is how many idle-seconds accrue per real
	// second — close to 1 (mostly idle), so the derived utilisation
	// (1 - idle rate) stays well under NodeCPUSaturation's 0.9 threshold and
	// the alert this scenario deliberately keeps inactive.
	rulerCPUIdleRatePerSecond = 0.95
)

// rulerEnvOr returns the named env var, or fallback when it is unset or
// empty — this file's own tiny copy of the pattern every live-stack helper
// in this harness uses, rather than exporting one more symbol from lib for a
// single caller.
func rulerEnvOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// registerRulerSteps binds the MIG-09 steps.
func (w *World) registerRulerSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the shadow-ruler stack is live$`, w.givenTier2Live)
	ctx.Step(`^the recording rule's underlying series is seeded into ClickHouse$`, w.givenUnderlyingSeriesSeeded)
	ctx.Step(`^the operator waits for the rule fixture to evaluate against cerberus$`, w.whenRuleFixtureEvaluates)
	ctx.Step(`^the recording rule evaluates against cerberus without a query error$`, w.thenRecordingRuleEvaluates)
	ctx.Step(`^the alert rule evaluates against cerberus without a query error$`, w.thenAlertRuleEvaluates)
	ctx.Step(`^the operator polls until the recorded series lands in ClickHouse$`, w.whenRecordedSeriesLands)
	ctx.Step(`^the recorded series is selectable through cerberus$`, w.thenRecordedSeriesSelectable)
	ctx.Step(`^the operator records the dead-end receiver's baseline and sends a test notification through its one contact point$`,
		w.whenTestNotificationSent)
	ctx.Step(`^the dead-end receiver is the only place the notification reaches$`, w.thenNotificationReachesDeadEnd)
}

// givenTier2Live reads the live Tier-2 endpoints from the environment
// `just migration-tier2-up` publishes, and proves every one of them answers
// before any later step drives a live call against them — a scenario against
// a half-torn-down stack must fail here, naming the unreachable endpoint,
// rather than deep inside a rule-evaluation poll with a bare
// connection-refused.
func (w *World) givenTier2Live() error {
	le := lib.LoadTier2LiveEndpoints()
	ctx, cancel := context.WithTimeout(context.Background(), tier2RuleEvalWaitBudget)
	defer cancel()
	if err := le.RequireLive(ctx); err != nil {
		return err
	}
	w.tier2.endpoints, w.tier2.live = le, true
	return nil
}

// givenUnderlyingSeriesSeeded writes the recording rule's underlying
// `node_cpu_seconds_total` series directly into ClickHouse — see the
// constant block above for why the three-signal fixture never carries it.
func (w *World) givenUnderlyingSeriesSeeded() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx := context.Background()
	conn, err := seed.DialCH(ctx, seed.CHConfig{
		Addr:     rulerEnvOr(rulerCHAddrEnvKey, defaultRulerCHAddr),
		Database: rulerEnvOr(rulerCHDatabaseEnvKey, defaultRulerCHDatabase),
		Username: rulerEnvOr(rulerCHUsernameEnvKey, defaultRulerCHUsername),
		Password: rulerEnvOr(rulerCHPasswordEnvKey, defaultRulerCHPassword),
	})
	if err != nil {
		return fmt.Errorf("dial clickhouse for the recording rule's underlying series: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := seed.InsertCounterSeries(ctx, conn, rulerUnderlyingCPUSeries(time.Now())); err != nil {
		return fmt.Errorf("seed %s: %w", rulerCPUMetric, err)
	}
	return nil
}

// rulerUnderlyingCPUSeries builds the node_cpu_seconds_total{mode="idle"}
// series for rulerCPUCoreCount CPU cores, monotonically increasing over
// rulerCPUSeedWindow up to now, so the recording rule's
// `rate(...[5m])` has real, in-window samples to compute over the moment
// this step returns.
func rulerUnderlyingCPUSeries(now time.Time) []seed.MetricSeries {
	start := now.Add(-rulerCPUSeedWindow)
	out := make([]seed.MetricSeries, 0, rulerCPUCoreCount)
	for core := range rulerCPUCoreCount {
		samples := make([]seed.Sample, 0)
		value := 0.0
		for t := start; !t.After(now); t = t.Add(rulerCPUSeedStep) {
			samples = append(samples, seed.Sample{Time: t, Value: value})
			value += rulerCPUIdleRatePerSecond * rulerCPUSeedStep.Seconds()
		}
		out = append(out, seed.MetricSeries{
			MetricName:  rulerCPUMetric,
			ServiceName: rulerCPUServiceName,
			Attributes:  map[string]string{"cpu": fmt.Sprintf("%d", core), "mode": "idle"},
			Samples:     samples,
		})
	}
	return out
}

// tier2RuleEvalWaitBudget bounds the live-stack readiness probe itself,
// distinct from rulerEvalWait (which bounds waiting for a RULE to evaluate
// once the stack already answers healthchecks).
const tier2RuleEvalWaitBudget = 2 * time.Minute

// requireTier2Live fails a step that needs the live stack when the Given
// establishing it never ran.
func (w *World) requireTier2Live() error {
	if !w.tier2.live {
		return fmt.Errorf("the tier-2 ruler stack has not been established; the scenario must establish it first")
	}
	return nil
}

// rulerRuleGroup and rulerRule decode the subset of Grafana's
// Prometheus-compatible ruler API (GET /api/prometheus/grafana/api/v1/rules)
// this file asserts on.
type rulerRuleGroup struct {
	Name  string      `json:"name"`
	Rules []rulerRule `json:"rules"`
}

type rulerRule struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Health         string `json:"health"`
	LastError      string `json:"lastError"`
	LastEvaluation string `json:"lastEvaluation"`
}

type rulerRulesResponse struct {
	Data struct {
		Groups []rulerRuleGroup `json:"groups"`
	} `json:"data"`
}

// whenRuleFixtureEvaluates polls Grafana's live rule-group state until both
// the recording rule and the alert rule have evaluated CLEANLY at least
// once, storing the last observed groups for the Then steps to read.
// "Cleanly" means Health is anything but "error" — "ok" or "nodata" both
// mean the query round-tripped; only "error" means the query (or, for the
// recording rule, its write-back) itself failed.
//
// It does not settle for the FIRST evaluation regardless of health: a fresh
// stack's very first tick can race Grafana's own datasource provisioning
// (confirmed empirically — the write-back leg 404s against the recording
// rule's OWN query datasource for exactly one tick before the write-target
// datasource is registered) and self-heals on the next tick. Stopping at
// the first evaluation would report that transient race as this scenario's
// permanent failure instead of giving the substrate the few extra ticks its
// own startup needs.
func (w *World) whenRuleFixtureEvaluates() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx := context.Background()
	deadline := time.Now().Add(rulerEvalWait)
	var last error
	for {
		groups, err := fetchRulerGroups(ctx, w.tier2.endpoints.GrafanaURL)
		if err == nil {
			w.tier2.groups = groups
			recording, recOK := findRulerRule(groups, rulerRecordingGroup, rulerRecordingRule)
			alert, alertOK := findRulerRule(groups, rulerAlertGroup, rulerAlertRule)
			recClean := recOK && recording.LastEvaluation != "" && recording.Health != "error"
			alertClean := alertOK && alert.LastEvaluation != "" && alert.Health != "error"
			if recClean && alertClean {
				return nil
			}
			last = fmt.Errorf("recording rule found=%v health=%q lastError=%q; alert rule found=%v health=%q lastError=%q",
				recOK, recording.Health, recording.LastError, alertOK, alert.Health, alert.LastError)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the rule fixture never evaluated cleanly against cerberus within %s: %w", rulerEvalWait, last)
		}
		time.Sleep(rulerPoll)
	}
}

// thenRecordingRuleEvaluates asserts the recording rule's own health names a
// real query round-trip against cerberus, not a translation or provisioning
// error.
func (w *World) thenRecordingRuleEvaluates() error {
	return w.thenRuleHealthOK(rulerRecordingGroup, rulerRecordingRule, "recording")
}

// thenAlertRuleEvaluates is thenRecordingRuleEvaluates' sibling for the
// alert rule.
func (w *World) thenAlertRuleEvaluates() error {
	return w.thenRuleHealthOK(rulerAlertGroup, rulerAlertRule, "alerting")
}

// thenRuleHealthOK asserts one rule's last-polled state names the given
// Grafana rule type and a health that means "cerberus answered the query"
// rather than "the query itself failed".
func (w *World) thenRuleHealthOK(group, name, wantType string) error {
	rule, ok := findRulerRule(w.tier2.groups, group, name)
	if !ok {
		return fmt.Errorf("rule %s/%s was never observed; the scenario must wait for evaluation first", group, name)
	}
	if rule.Type != wantType {
		return fmt.Errorf("rule %s/%s has type %q, want %q", group, name, rule.Type, wantType)
	}
	if rule.Health == "error" {
		return fmt.Errorf("rule %s/%s reports health=error, lastError=%q — the query against cerberus failed",
			group, name, rule.LastError)
	}
	if rule.LastEvaluation == "" {
		return fmt.Errorf("rule %s/%s has never evaluated", group, name)
	}
	return nil
}

// findRulerRule looks up one rule by its provisioned group and rule name.
func findRulerRule(groups []rulerRuleGroup, group, name string) (rulerRule, bool) {
	for _, g := range groups {
		if g.Name != group {
			continue
		}
		for _, r := range g.Rules {
			if r.Name == name {
				return r, true
			}
		}
	}
	return rulerRule{}, false
}

// fetchRulerGroups fetches Grafana's live rule-group state.
func fetchRulerGroups(ctx context.Context, grafanaBase string) ([]rulerRuleGroup, error) {
	var out rulerRulesResponse
	if err := fetchJSONInto(ctx, grafanaBase+"/api/prometheus/grafana/api/v1/rules", &out); err != nil {
		return nil, err
	}
	return out.Data.Groups, nil
}

// cerberusInstantQueryResponse decodes the subset of the Prometheus instant
// query API (`/api/v1/query`) this file reads.
type cerberusInstantQueryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// recordedSeriesResultCount is stored on the World after
// whenRecordedSeriesLands so the Then step can report a real number, not
// just pass/fail.
type recordedSeriesPoll struct {
	seriesCount int
	lastRaw     string
}

// whenRecordedSeriesLands polls cerberus's OWN Prometheus query API for the
// recording rule's metric name until it returns at least one series, or the
// budget expires. This is the "lands recording-rule output back into CH;
// those recorded series become selectable through cerberus" half of MIG-09's
// PASS assertion: it proves the write leg by reading the result back through
// cerberus's ordinary read path, not by inspecting ClickHouse directly.
func (w *World) whenRecordedSeriesLands() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx := context.Background()
	deadline := time.Now().Add(rulerLandingWait)
	var poll recordedSeriesPoll
	var last error
	for {
		count, raw, err := queryCerberusInstant(ctx, w.tier2.endpoints.CerberusURL, rulerRecordingRule)
		if err == nil {
			poll = recordedSeriesPoll{seriesCount: count, lastRaw: raw}
			if count > 0 {
				w.tier2.recordedSeries = poll
				return nil
			}
			last = fmt.Errorf("query returned zero series (raw: %s)", raw)
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			w.tier2.recordedSeries = poll
			return fmt.Errorf("%q never landed in ClickHouse and became selectable through cerberus within %s: %w",
				rulerRecordingRule, rulerLandingWait, last)
		}
		time.Sleep(rulerPoll)
	}
}

// thenRecordedSeriesSelectable asserts the poll in whenRecordedSeriesLands
// actually observed at least one series, quoting the raw response so a
// failure names what cerberus really answered.
func (w *World) thenRecordedSeriesSelectable() error {
	if w.tier2.recordedSeries.seriesCount == 0 {
		return fmt.Errorf("%q is not selectable through cerberus (last response: %s)",
			rulerRecordingRule, w.tier2.recordedSeries.lastRaw)
	}
	return nil
}

// queryCerberusInstant issues one instant PromQL query against cerberus and
// returns the number of series it answered with, plus the raw body for a
// failure message.
func queryCerberusInstant(ctx context.Context, cerberusBase, query string) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, rulerHTTPTimeout)
	defer cancel()
	target := cerberusBase + "/api/v1/query?query=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, rulerErrBodyLimit))
	if err != nil {
		return 0, "", fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, string(body), fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	var out cerberusInstantQueryResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, string(body), fmt.Errorf("decode %s body %q: %w", target, string(body), err)
	}
	if out.Status != "success" {
		return 0, string(body), fmt.Errorf("%s returned status %q, want %q", target, out.Status, "success")
	}
	return len(out.Data.Result), string(body), nil
}

// tier2NotificationsResponse decodes the dead-end receiver's /notifications
// endpoint (tiers/tier2-ruler/receiver/main.go).
type tier2NotificationsResponse struct {
	Count int `json:"count"`
}

// whenTestNotificationSent records the dead-end receiver's current
// notification count as a baseline, then asks Grafana to send a test
// notification through its one real, provisioned contact point — the same
// SendTestNotification exercise tier2_ruler_test.go's substrate self-check
// runs, here as a scenario step so MIG-09 can assert the delivery is
// isolated to the dead-end receiver.
func (w *World) whenTestNotificationSent() error {
	if err := w.requireTier2Live(); err != nil {
		return err
	}
	ctx := context.Background()
	baseline, err := fetchDeadEndCount(ctx, w.tier2.endpoints.DeadEndURL)
	if err != nil {
		return fmt.Errorf("read the dead-end receiver's baseline notification count: %w", err)
	}
	w.tier2.notifBase, w.tier2.notifSeen = baseline, true
	return sendGrafanaTestNotification(ctx, w.tier2.endpoints.GrafanaURL)
}

// thenNotificationReachesDeadEnd polls the dead-end receiver until its
// notification count rises above the baseline whenTestNotificationSent
// recorded — the only contact point this substrate provisions, so a rise
// here is the whole alerting path proven end to end: "the shadow ruler fires
// into a null receiver (computes, never pages)".
func (w *World) thenNotificationReachesDeadEnd() error {
	if !w.tier2.notifSeen {
		return fmt.Errorf("no baseline notification count was recorded; the scenario must send a test notification first")
	}
	ctx := context.Background()
	deadline := time.Now().Add(rulerNotificationWait)
	var last error
	for {
		count, err := fetchDeadEndCount(ctx, w.tier2.endpoints.DeadEndURL)
		if err == nil && count > w.tier2.notifBase {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("notification count = %d, want > %d", count, w.tier2.notifBase)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the dead-end receiver never captured the test notification within %s: %w",
				rulerNotificationWait, last)
		}
		time.Sleep(rulerPoll)
	}
}

// fetchDeadEndCount reads the dead-end receiver's current capture count.
func fetchDeadEndCount(ctx context.Context, deadEndBase string) (int, error) {
	var out tier2NotificationsResponse
	if err := fetchJSONInto(ctx, deadEndBase+"/notifications", &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// sendGrafanaTestNotification asks Grafana to fire one synthetic
// notification through the substrate's real, provisioned "dead-end" contact
// point — mirrors tier2_ruler_test.go's tier2SendTestNotification.
func sendGrafanaTestNotification(ctx context.Context, grafanaBase string) error {
	type receiverConfig struct {
		UID      string            `json:"uid"`
		Name     string            `json:"name"`
		Type     string            `json:"type"`
		Settings map[string]string `json:"settings"`
	}
	type receiver struct {
		Name    string           `json:"name"`
		Configs []receiverConfig `json:"grafana_managed_receiver_configs"`
	}
	payload := struct {
		Receivers []receiver `json:"receivers"`
	}{
		Receivers: []receiver{{
			Name: rulerContactPoint,
			Configs: []receiverConfig{{
				UID:  rulerContactUID,
				Name: rulerContactUID,
				Type: "webhook",
				Settings: map[string]string{
					"url":        rulerContactURL,
					"httpMethod": http.MethodPost,
				},
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode test-notification payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, rulerHTTPTimeout)
	defer cancel()
	url := grafanaBase + "/api/alertmanager/grafana/config/api/v1/receivers/test"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, rulerErrBodyLimit))
	if resp.StatusCode != http.StatusOK {
		if readErr != nil {
			return fmt.Errorf("POST %s returned %d (body unreadable: %w)", url, resp.StatusCode, readErr)
		}
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(respBody))
	}
	return nil
}

// fetchJSONInto GETs url, requiring a 200 with a JSON body, and decodes it
// into out.
func fetchJSONInto(ctx context.Context, url string, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, rulerHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, rulerErrBodyLimit))
	if err != nil {
		return fmt.Errorf("read %s body: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s body %q: %w", url, string(body), err)
	}
	return nil
}
