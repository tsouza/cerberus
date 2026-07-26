//go:build migration_tier2

// Package migration also holds the substrate self-check for Tier-2 (Layer
// 14): the ruler tier that extends the Tier-1 dual-backend stack with a real
// query-only external ruler (Grafana-managed alerting) pointed at cerberus,
// plus a dead-end webhook receiver its one contact point routes to.
//
// This file proves the mechanism only — Grafana can evaluate a real rule
// against cerberus, and a fired notification actually reaches the dead-end
// receiver — before any MIG-09/13/18/19/24 scenario is written against this
// substrate. Same prove-the-mechanism-first sequencing tier1_stack_test.go
// used for Tier-1.
//
// Gated by the `migration_tier2` build tag so the required `check` lane's
// `go test ./...` never reaches for Docker — the same posture the
// `migration_tier1` tag takes for Tier-1. Deliberately never built together
// with `migration_tier1` (see the Justfile's two separate `go vet` lines):
// the two tiers run against different compose stacks, never the same
// process, so this file's helpers are locally named rather than shared with
// tier1_stack_test.go's identically-shaped ones.
//
// Run via: just migration-tier2-up && just migration-tier2-run
package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Endpoint defaults mirror the published port map in
// tiers/tier2-ruler/docker-compose.ruler.yml (cerberus's own port is
// tier1-dual's, reused unchanged since it's the same instance). Every one is
// env-overridable so the same assertions can run against a stack published
// on other ports.
const (
	tier2DefaultCerberusURL = "http://127.0.0.1:27080"
	tier2DefaultGrafanaURL  = "http://127.0.0.1:27400"
	tier2DefaultDeadEndURL  = "http://127.0.0.1:27450"
	tier2CerberusURLEnvKey  = "TIER2_CERBERUS_URL"
	tier2GrafanaURLEnvKey   = "TIER2_GRAFANA_URL"
	tier2DeadEndURLEnvKey   = "TIER2_DEADEND_URL"
)

// Budgets. Each is a bounded deadline on a poll loop — never a sleep, never a
// retry-then-continue: every one of them fails the test when it expires.
const (
	// tier2CerberusReadyWait mirrors tier1's cerberusReadyWait: the collector
	// must have provisioned every table cerberus's read side expects.
	tier2CerberusReadyWait = 2 * time.Minute
	// tier2GrafanaReadyWait covers Grafana's cold boot plus provisioning the
	// datasource and the four alerting-as-code files.
	tier2GrafanaReadyWait = 90 * time.Second
	// tier2DeadEndReadyWait covers the receiver's own cold start.
	tier2DeadEndReadyWait = 30 * time.Second
	// tier2RuleEvalWait bounds how long the poll waits for the NodeCPUSaturation
	// rule group to report a live evaluation against cerberus. The group's
	// `interval: 15s` (grafana/alerting/rules.yaml) sets the expected tick
	// cadence; this budget covers a couple of missed ticks besides the first.
	tier2RuleEvalWait = 60 * time.Second
	// tier2RuleEvalPoll is the gap between rule-health polls.
	tier2RuleEvalPoll = 2 * time.Second
	// tier2NotificationWait bounds how long the poll waits for the dead-end
	// receiver to observe the test notification Grafana was asked to send.
	tier2NotificationWait = 30 * time.Second
	// tier2NotificationPoll is the gap between notification-count polls.
	tier2NotificationPoll = time.Second
	// tier2HTTPTimeout bounds one request in this file's poll loops.
	tier2HTTPTimeout = 15 * time.Second
	// tier2ErrBodyLimit caps how much of a failing response body is quoted
	// back into a test failure message.
	tier2ErrBodyLimit = 4096
)

// The rule fixture. Mirrored, not invented: these names must stay in lock-step
// with grafana/alerting/{rules,contact-points}.yaml, whose own comments name
// this file back — a rename on either side breaks the other loudly.
const (
	tier2RecordingGroupName = "node.rules"
	tier2AlertGroupName     = "node.alerts"
	tier2RecordingRuleName  = "node:node_cpu_utilisation:avg1m"
	tier2AlertRuleName      = "NodeCPUSaturation"
	tier2ContactPointName   = "dead-end"
	tier2ContactPointUID    = "dead-end-webhook"
	// tier2ContactPointURL is the address Grafana itself dials — inside the
	// compose network, not this test's own host-published one.
	tier2ContactPointURL = "http://dead-end-receiver:8090/webhook"
)

func tier2EnvOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestTier2Substrate asserts the Tier-2 ruler substrate satisfies the
// contract MIG-09/13/18/19/24 will rest on: Grafana loads the rule fixture
// without a provisioning error, its alert rule evaluates against cerberus for
// real (a live query round-trip, not a mock), and a fired notification
// actually reaches the dead-end receiver rather than vanishing silently.
// Sub-tests share the stack and run in order.
func TestTier2Substrate(t *testing.T) {
	ctx := context.Background()

	t.Run("cerberus_ready", func(t *testing.T) {
		base := tier2EnvOr(tier2CerberusURLEnvKey, tier2DefaultCerberusURL)
		if err := seed.WaitHTTPOK(ctx, base+"/readyz", tier2CerberusReadyWait); err != nil {
			t.Fatalf("cerberus never became ready: %v", err)
		}
	})

	t.Run("grafana_provisioned_the_rule_fixture", func(t *testing.T) {
		base := tier2EnvOr(tier2GrafanaURLEnvKey, tier2DefaultGrafanaURL)
		if err := seed.WaitHTTPOK(ctx, base+"/api/health", tier2GrafanaReadyWait); err != nil {
			t.Fatalf("grafana never became ready: %v", err)
		}

		rules, err := tier2FetchRules(ctx, base)
		if err != nil {
			t.Fatalf("fetch rule groups: %v", err)
		}
		recording, ok := tier2FindRule(rules, tier2RecordingGroupName, tier2RecordingRuleName)
		if !ok {
			t.Fatalf("recording rule %s/%s not provisioned; groups: %+v", tier2RecordingGroupName, tier2RecordingRuleName, rules)
		}
		if recording.Type != "recording" {
			t.Fatalf("rule %s type = %q, want %q", tier2RecordingRuleName, recording.Type, "recording")
		}
		alert, ok := tier2FindRule(rules, tier2AlertGroupName, tier2AlertRuleName)
		if !ok {
			t.Fatalf("alert rule %s/%s not provisioned; groups: %+v", tier2AlertGroupName, tier2AlertRuleName, rules)
		}
		if alert.Type != "alerting" {
			t.Fatalf("rule %s type = %q, want %q", tier2AlertRuleName, alert.Type, "alerting")
		}
	})

	t.Run("grafana_evaluates_the_alert_against_cerberus", func(t *testing.T) {
		base := tier2EnvOr(tier2GrafanaURLEnvKey, tier2DefaultGrafanaURL)

		var last error
		deadline := time.Now().Add(tier2RuleEvalWait)
		for {
			rules, err := tier2FetchRules(ctx, base)
			if err == nil {
				if alert, ok := tier2FindRule(rules, tier2AlertGroupName, tier2AlertRuleName); ok {
					if alert.LastEvaluation != "" {
						// "ok" (condition resolved) and "nodata" (the query
						// round-tripped to cerberus and came back empty,
						// because this substrate self-check seeds no
						// node_cpu_seconds_total input — MIG-13/MIG-19 do
						// that, over the write-back bridge described in
						// docker-compose.ruler.yml's header comment) both mean
						// cerberus ANSWERED the query. Only "error" means the
						// query itself failed.
						if alert.Health == "ok" || alert.Health == "nodata" {
							return
						}
						last = fmt.Errorf("rule health = %q, lastError = %q", alert.Health, alert.LastError)
					} else {
						last = fmt.Errorf("rule has not evaluated yet")
					}
				} else {
					last = fmt.Errorf("alert rule %s/%s not found", tier2AlertGroupName, tier2AlertRuleName)
				}
			} else {
				last = err
			}
			if time.Now().After(deadline) {
				t.Fatalf("NodeCPUSaturation never evaluated cleanly against cerberus within %s: %v", tier2RuleEvalWait, last)
			}
			time.Sleep(tier2RuleEvalPoll)
		}
	})

	t.Run("dead_end_receiver_captures_a_notification", func(t *testing.T) {
		grafanaBase := tier2EnvOr(tier2GrafanaURLEnvKey, tier2DefaultGrafanaURL)
		deadEndBase := tier2EnvOr(tier2DeadEndURLEnvKey, tier2DefaultDeadEndURL)

		if err := seed.WaitHTTPOK(ctx, deadEndBase+"/healthz", tier2DeadEndReadyWait); err != nil {
			t.Fatalf("dead-end receiver never became ready: %v", err)
		}

		baseline, err := tier2NotificationCount(ctx, deadEndBase)
		if err != nil {
			t.Fatalf("read baseline notification count: %v", err)
		}

		// Exercises the real, provisioned contact point end to end: Grafana's
		// own "test contact point" API sends an actual HTTP POST through the
		// SAME receiver config grafana/alerting/contact-points.yaml declares,
		// over the SAME network path a real fired alert would use — the
		// notification-policy routing is the only leg this bypasses, and
		// notification-policies.yaml carries no second branch for it to pick
		// wrong.
		if err := tier2SendTestNotification(ctx, grafanaBase); err != nil {
			t.Fatalf("send test notification: %v", err)
		}

		var last error
		deadline := time.Now().Add(tier2NotificationWait)
		for {
			count, err := tier2NotificationCount(ctx, deadEndBase)
			if err == nil && count > baseline {
				return
			}
			if err != nil {
				last = err
			} else {
				last = fmt.Errorf("notification count = %d, want > %d", count, baseline)
			}
			if time.Now().After(deadline) {
				t.Fatalf("dead-end receiver never captured the test notification within %s: %v", tier2NotificationWait, last)
			}
			time.Sleep(tier2NotificationPoll)
		}
	})
}

// tier2RuleGroup and tier2Rule decode the subset of Grafana's Prometheus-
// compatible ruler API (GET /api/prometheus/grafana/api/v1/rules) this file
// asserts on.
type tier2RulesResponse struct {
	Data struct {
		Groups []tier2RuleGroup `json:"groups"`
	} `json:"data"`
}

type tier2RuleGroup struct {
	Name  string      `json:"name"`
	Rules []tier2Rule `json:"rules"`
}

type tier2Rule struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Health         string `json:"health"`
	LastError      string `json:"lastError"`
	LastEvaluation string `json:"lastEvaluation"`
}

// tier2FetchRules fetches Grafana's live rule-group state.
func tier2FetchRules(ctx context.Context, grafanaBase string) ([]tier2RuleGroup, error) {
	var out tier2RulesResponse
	if err := tier2FetchJSON(ctx, grafanaBase+"/api/prometheus/grafana/api/v1/rules", &out); err != nil {
		return nil, err
	}
	return out.Data.Groups, nil
}

// tier2FindRule looks up one rule by its provisioned group and rule name.
func tier2FindRule(groups []tier2RuleGroup, group, name string) (tier2Rule, bool) {
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
	return tier2Rule{}, false
}

// tier2NotificationsResponse decodes the dead-end receiver's /notifications
// endpoint (test/e2e/migration/tiers/tier2-ruler/receiver/main.go).
type tier2NotificationsResponse struct {
	Count int `json:"count"`
}

// tier2NotificationCount reads the dead-end receiver's current capture count.
func tier2NotificationCount(ctx context.Context, deadEndBase string) (int, error) {
	var out tier2NotificationsResponse
	if err := tier2FetchJSON(ctx, deadEndBase+"/notifications", &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// tier2SendTestNotification asks Grafana to fire one synthetic notification
// through the substrate's real, provisioned "dead-end" contact point.
func tier2SendTestNotification(ctx context.Context, grafanaBase string) error {
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
			Name: tier2ContactPointName,
			Configs: []receiverConfig{{
				UID:  tier2ContactPointUID,
				Name: tier2ContactPointUID,
				Type: "webhook",
				Settings: map[string]string{
					"url":        tier2ContactPointURL,
					"httpMethod": http.MethodPost,
				},
			}},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode test-notification payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, tier2HTTPTimeout)
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
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, tier2ErrBodyLimit))
	if resp.StatusCode != http.StatusOK {
		if readErr != nil {
			return fmt.Errorf("POST %s returned %d (body unreadable: %v)", url, resp.StatusCode, readErr)
		}
		return fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, string(respBody))
	}
	return nil
}

// tier2FetchJSON GETs url, requiring a 200 with a JSON body, and decodes it
// into out.
func tier2FetchJSON(ctx context.Context, url string, out any) error {
	reqCtx, cancel := context.WithTimeout(ctx, tier2HTTPTimeout)
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, tier2ErrBodyLimit))
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
