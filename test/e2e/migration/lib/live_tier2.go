package lib

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Tier-2 live-stack env var names, mirroring Tier-1's TIER1_* contract
// Tier-2's own live-stack plumbing stands alone rather than extending
// LoadLiveEndpoints, since the two tiers never run against the same compose
// project at once. Every one
// falls back to the exact port map docker-compose.ruler.yml (extending
// docker-compose.dual.yml) publishes, so the common case — `just
// migration-tier2-up` then driving this suite — needs no export at all.
const (
	EnvTier2CHAddr      = "TIER2_CH_ADDR"
	EnvTier2CHDatabase  = "TIER2_CH_DATABASE"
	EnvTier2CHUsername  = "TIER2_CH_USERNAME"
	EnvTier2CHPassword  = "TIER2_CH_PASSWORD" //nolint:gosec // env var NAME, not a credential value
	EnvTier2CerberusURL = "TIER2_CERBERUS_URL"
	EnvTier2GrafanaURL  = "TIER2_GRAFANA_URL"
	EnvTier2DeadEndURL  = "TIER2_DEADEND_URL"
)

// defaultTier2* mirror the published port map in
// tiers/tier2-ruler/docker-compose.ruler.yml (cerberus's own port is
// tier1-dual's, reused unchanged since it is the same instance) — the same
// defaults tier2_ruler_test.go's substrate self-check already pins against.
const (
	defaultTier2CHAddr      = "127.0.0.1:27000"
	defaultTier2CHDatabase  = "otel"
	defaultTier2CHUsername  = "cerberus"
	defaultTier2CHPassword  = "cerberus" //nolint:gosec // default fixture credential, not a real one
	defaultTier2CerberusURL = "http://127.0.0.1:27080"
	defaultTier2GrafanaURL  = "http://127.0.0.1:27400"
	defaultTier2DeadEndURL  = "http://127.0.0.1:27450"
)

// Tier2Endpoints is the live Tier-2 stack a scenario drives assertions
// against: cerberus (both the recording rule's query target and the
// read-back side MIG-13/MIG-19 verify against), Grafana (the shadow ruler),
// the dead-end notification receiver, and a direct ClickHouse connection —
// needed only to seed the recording rule's own input series, since nothing
// in the Tier-2 stack's ingest path does that for it (see
// tier2WritebackSteps.go).
type Tier2Endpoints struct {
	CHAddr, CHDatabase, CHUsername, CHPassword string
	CerberusURL, GrafanaURL, DeadEndURL        string
}

// tier2Vars pairs every env var LoadTier2Endpoints reads with the struct
// field it fills in and the default that matches
// docker-compose.ruler.yml's published port map.
var tier2Vars = []struct {
	name string
	dflt string
	dst  func(*Tier2Endpoints) *string
}{
	{EnvTier2CHAddr, defaultTier2CHAddr, func(e *Tier2Endpoints) *string { return &e.CHAddr }},
	{EnvTier2CHDatabase, defaultTier2CHDatabase, func(e *Tier2Endpoints) *string { return &e.CHDatabase }},
	{EnvTier2CHUsername, defaultTier2CHUsername, func(e *Tier2Endpoints) *string { return &e.CHUsername }},
	{EnvTier2CHPassword, defaultTier2CHPassword, func(e *Tier2Endpoints) *string { return &e.CHPassword }},
	{EnvTier2CerberusURL, defaultTier2CerberusURL, func(e *Tier2Endpoints) *string { return &e.CerberusURL }},
	{EnvTier2GrafanaURL, defaultTier2GrafanaURL, func(e *Tier2Endpoints) *string { return &e.GrafanaURL }},
	{EnvTier2DeadEndURL, defaultTier2DeadEndURL, func(e *Tier2Endpoints) *string { return &e.DeadEndURL }},
}

// LoadTier2Endpoints reads the Tier-2 env vars the harness needs, falling
// back to docker-compose.ruler.yml's port map for anything unset.
func LoadTier2Endpoints() Tier2Endpoints {
	var e Tier2Endpoints
	for _, v := range tier2Vars {
		*v.dst(&e) = envOr(v.name, v.dflt)
	}
	return e
}

// tier2LiveProbeTimeout bounds how long RequireLive waits for one endpoint.
// A stack `just migration-tier2-up` already brought up and waited `--wait`
// on should answer immediately; this only guards against a stale env var
// pointing at a container that was since torn down.
const tier2LiveProbeTimeout = 2 * time.Minute

// RequireLive probes cerberus, Grafana and the dead-end receiver's own
// readiness paths, failing with the FIRST unreachable one named — without
// this, a scenario against a half-torn-down stack fails deep inside a
// Grafana or cerberus HTTP call with a bare connection-refused, which does
// not say which endpoint is the problem.
func (e Tier2Endpoints) RequireLive(ctx context.Context) error {
	checks := []struct{ name, url string }{
		{"cerberus", e.CerberusURL + "/readyz"},
		{"grafana", e.GrafanaURL + "/api/health"},
		{"dead-end receiver", e.DeadEndURL + "/healthz"},
	}
	for _, c := range checks {
		if err := seed.WaitHTTPOK(ctx, c.url, tier2LiveProbeTimeout); err != nil {
			return fmt.Errorf("migration harness: the tier-2 stack is not live: %s: %w", c.name, err)
		}
	}
	return nil
}

// DialCH opens the direct ClickHouse connection the write-back scenarios use
// to seed the recording rule's input series. Nothing in the Tier-2 ingest
// path seeds it on its own — the Tier-1 seeder's fixture does not carry
// node_cpu_seconds_total, and the recording rule's own relativeTimeRange is
// wall-clock-relative, so a scenario must write fresh samples ending near
// "now" every run rather than replaying a fixed historical window.
func (e Tier2Endpoints) DialCH(ctx context.Context) (driver.Conn, error) {
	return seed.DialCH(ctx, seed.CHConfig{
		Addr:     e.CHAddr,
		Database: e.CHDatabase,
		Username: e.CHUsername,
		Password: e.CHPassword,
	})
}
