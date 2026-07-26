package lib

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Tier-1 live-stack env var names. These are the operator-facing contract
// between `just migration-tier1-up` / `just migration-tier1-seed` and the
// Gherkin harness: the compose stack and the seeder publish endpoints and a
// manifest, and a Tier-1 scenario reads them rather than assuming the
// published port map in docker-compose.dual.yml, so the same scenarios run
// unchanged against a stack published on different ports.
const (
	EnvTier1CHAddr        = "TIER1_CH_ADDR"
	EnvTier1CHDatabase    = "TIER1_CH_DATABASE"
	EnvTier1CHUsername    = "TIER1_CH_USERNAME"
	EnvTier1CHPassword    = "TIER1_CH_PASSWORD" //nolint:gosec // env var NAME, not a credential value
	EnvTier1PromURL       = "TIER1_PROM_URL"
	EnvTier1LokiURL       = "TIER1_LOKI_URL"
	EnvTier1TempoURL      = "TIER1_TEMPO_URL"
	EnvTier1TempoOTLPAddr = "TIER1_TEMPO_OTLP_ADDR"
	EnvTier1CerberusURL   = "TIER1_CERBERUS_URL"
	EnvTier1Manifest      = "TIER1_MANIFEST"
)

// LiveEndpoints is the live Tier-1 stack a scenario drives `cerberus migrate`
// against: the reference backends an operator is migrating off, the cerberus
// under test, and the path to the manifest the seeder published (which
// carries the exact [verify_start, verify_end] window the fixture covers —
// see seed.Manifest).
type LiveEndpoints struct {
	CHAddr, CHDatabase, CHUsername, CHPassword string
	PromURL, LokiURL, TempoURL, TempoOTLPAddr  string
	CerberusURL                                string
	ManifestPath                               string
}

// tier1RequiredVars pairs every env var LoadLiveEndpoints reads with the
// struct field it fills in, so a missing var is reported by the exact name an
// operator would export, and the "fails clearly if unset" contract lives in
// one table rather than one hand-written check per field.
var tier1RequiredVars = []struct {
	name string
	dst  func(*LiveEndpoints) *string
}{
	{EnvTier1CHAddr, func(e *LiveEndpoints) *string { return &e.CHAddr }},
	{EnvTier1CHDatabase, func(e *LiveEndpoints) *string { return &e.CHDatabase }},
	{EnvTier1CHUsername, func(e *LiveEndpoints) *string { return &e.CHUsername }},
	{EnvTier1CHPassword, func(e *LiveEndpoints) *string { return &e.CHPassword }},
	{EnvTier1PromURL, func(e *LiveEndpoints) *string { return &e.PromURL }},
	{EnvTier1LokiURL, func(e *LiveEndpoints) *string { return &e.LokiURL }},
	{EnvTier1TempoURL, func(e *LiveEndpoints) *string { return &e.TempoURL }},
	{EnvTier1TempoOTLPAddr, func(e *LiveEndpoints) *string { return &e.TempoOTLPAddr }},
	{EnvTier1CerberusURL, func(e *LiveEndpoints) *string { return &e.CerberusURL }},
	{EnvTier1Manifest, func(e *LiveEndpoints) *string { return &e.ManifestPath }},
}

// LoadLiveEndpoints reads every Tier-1 env var the harness needs. Unlike
// Tier-0's OfflineEnv (which SETS a fixed environment), a Tier-1 scenario
// runs against a stack the operator already brought up out-of-band
// (`just migration-tier1-up` / `just migration-tier1-seed`), so this READS
// what that step published rather than assuming its default port map. A
// missing var is a hard, named error — a scenario that fell back to a
// hardcoded default could pass against a stray container left over from a
// different lane rather than against the stack this run actually seeded.
func LoadLiveEndpoints() (LiveEndpoints, error) {
	var le LiveEndpoints
	var missing []string
	for _, v := range tier1RequiredVars {
		val := os.Getenv(v.name)
		if val == "" {
			missing = append(missing, v.name)
			continue
		}
		*v.dst(&le) = val
	}
	if len(missing) > 0 {
		return LiveEndpoints{}, fmt.Errorf(
			"migration harness: the tier-1 stack is not live: %v unset — run `just migration-tier1-up` and "+
				"`just migration-tier1-seed` first, and export their endpoints before driving this suite", missing,
		)
	}
	return le, nil
}

// stackReadyTimeout bounds how long RequireLive waits for one endpoint. A
// stack `just migration-tier1-up` already brought up and waited `--wait` on
// should answer immediately; this only guards against a stale env var
// pointing at a container that was since torn down.
const stackReadyTimeout = 30 * time.Second

// RequireLive probes every reference backend's and cerberus's own readiness
// path, failing with the FIRST unreachable one named. Without this, a
// scenario against a half-torn-down stack fails deep inside `cerberus migrate
// verify` with a bare connection-refused, which does not say which of five
// endpoints is the problem.
func (le LiveEndpoints) RequireLive(ctx context.Context) error {
	checks := []struct{ name, url string }{
		{"cerberus", le.CerberusURL + "/readyz"},
		{"reference prometheus", le.PromURL + "/-/ready"},
		{"reference loki", le.LokiURL + "/ready"},
		{"reference tempo", le.TempoURL + "/ready"},
	}
	for _, c := range checks {
		if err := seed.WaitHTTPOK(ctx, c.url, stackReadyTimeout); err != nil {
			return fmt.Errorf("migration harness: the tier-1 stack is not live: %s: %w", c.name, err)
		}
	}
	return nil
}

// LoadManifest reads the seeder's manifest: the fixture's [VerifyStart,
// VerifyEnd] window, its step, and the metric handles it seeded. A Tier-1
// scenario reads its query window from here rather than from a live
// `[-1h, now]`, because the seeder's data stopped moving the moment
// `migration-tier1-seed` returned and a window that keeps sliding cannot be
// compared.
func (le LiveEndpoints) LoadManifest() (seed.Manifest, error) {
	if le.ManifestPath == "" {
		return seed.Manifest{}, fmt.Errorf(
			"migration harness: no manifest path bound; the tier-1 stack must be established first",
		)
	}
	m, err := seed.ReadManifest(le.ManifestPath)
	if err != nil {
		return seed.Manifest{}, fmt.Errorf("migration harness: read the seeder's manifest: %w", err)
	}
	return m, nil
}

// LiveEnv builds the environment for a Tier-1 scenario command. Unlike
// OfflineEnv, it must NOT blackhole the network: a Tier-1 command's entire
// job is to reach real HTTP endpoints (the live cerberus, the live reference
// backends). It carries only the parent's PATH — so a child resolves the
// toolchain — plus whatever the caller supplies; it inherits no ambient
// CERBERUS_* or proxy variable, so a scenario's result cannot depend on
// something the developer happens to have exported. extra is appended last,
// so a case's own setting wins.
func LiveEnv(extra ...string) []string {
	env := []string{
		pathEnv + "=" + os.Getenv(pathEnv),
	}
	return append(env, extra...)
}
