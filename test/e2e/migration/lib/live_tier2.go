package lib

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Tier-2 live-stack env var names. Mirrors the Tier-1 contract
// (test/e2e/migration/lib/live.go on the migration-tier1-gherkin-mechanism
// branch): a Tier-2 scenario reads these rather than hardcoding a literal, so
// the same scenarios run unchanged against a stack published on different
// ports. Every one falls back to the exact port map
// tiers/tier2-ruler/docker-compose.ruler.yml publishes (mirrored in
// tier2_ruler_test.go's own defaultTier2* constants, which the compose
// stack's substrate self-check pins against), so the common case —
// `just migration-tier2-up` then driving this suite — needs no export at all.
const (
	EnvTier2CerberusURL = "TIER2_CERBERUS_URL"
	EnvTier2GrafanaURL  = "TIER2_GRAFANA_URL"
	EnvTier2DeadEndURL  = "TIER2_DEADEND_URL"
)

// defaultTier2* mirror the published port map in
// tiers/tier2-ruler/docker-compose.ruler.yml, byte-for-byte the same
// defaults tier2_ruler_test.go's substrate self-check already pins against.
const (
	defaultTier2CerberusURL = "http://127.0.0.1:27080"
	defaultTier2GrafanaURL  = "http://127.0.0.1:27400"
	defaultTier2DeadEndURL  = "http://127.0.0.1:27450"
)

// Tier2LiveEndpoints is the live Tier-2 stack a scenario drives: the ruler
// (Grafana), the cerberus under test it evaluates rules against, and the
// dead-end receiver its one contact point routes to.
type Tier2LiveEndpoints struct {
	CerberusURL string
	GrafanaURL  string
	DeadEndURL  string
}

// tier2EnvOr returns the named env var, or fallback when it is unset or empty.
func tier2EnvOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// LoadTier2LiveEndpoints reads the Tier-2 env vars the harness needs, falling
// back to the docker-compose.ruler.yml port map for anything unset.
func LoadTier2LiveEndpoints() Tier2LiveEndpoints {
	return Tier2LiveEndpoints{
		CerberusURL: tier2EnvOr(EnvTier2CerberusURL, defaultTier2CerberusURL),
		GrafanaURL:  tier2EnvOr(EnvTier2GrafanaURL, defaultTier2GrafanaURL),
		DeadEndURL:  tier2EnvOr(EnvTier2DeadEndURL, defaultTier2DeadEndURL),
	}
}

// tier2StackReadyTimeout bounds how long RequireLive waits for one endpoint.
// A stack `just migration-tier2-up` already brought up and waited `--wait`
// on should answer immediately; this only guards against a stale env var
// pointing at a container that was since torn down.
const tier2StackReadyTimeout = 90 * time.Second

// RequireLive probes every Tier-2 endpoint's readiness path, failing with the
// FIRST unreachable one named. Without this, a scenario against a
// half-torn-down stack fails deep inside a live HTTP call with a bare
// connection-refused, which does not say which of three endpoints is the
// problem.
func (le Tier2LiveEndpoints) RequireLive(ctx context.Context) error {
	checks := []struct{ name, url string }{
		{"cerberus", le.CerberusURL + "/readyz"},
		{"grafana", le.GrafanaURL + "/api/health"},
		{"dead-end receiver", le.DeadEndURL + "/healthz"},
	}
	for _, c := range checks {
		if err := seed.WaitHTTPOK(ctx, c.url, tier2StackReadyTimeout); err != nil {
			return fmt.Errorf("migration harness: the tier-2 stack is not live: %s: %w", c.name, err)
		}
	}
	return nil
}
