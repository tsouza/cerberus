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
// Gherkin harness: a Tier-1 scenario reads them rather than hardcoding a
// literal, so the same scenarios run unchanged against a stack published on
// different ports. Every one of them falls back to the exact port map
// `docker-compose.dual.yml` publishes (mirrored in tier1_stack_test.go's own
// `default*` constants, which the compose stack's substrate self-checks pin
// against), so the common case — `just migration-tier1-up` then
// `just migration-tier1-seed` then driving this suite — needs no export at
// all; only a stack published on non-default ports needs one.
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

// defaultTier1* mirror the published port map in
// tiers/tier1-dual/docker-compose.dual.yml, byte-for-byte the same defaults
// tier1_stack_test.go's substrate self-checks already pin against — so a
// stack `just migration-tier1-up` just brought up is reachable without
// exporting anything.
const (
	defaultTier1CHAddr      = "127.0.0.1:27000"
	defaultTier1CHDatabase  = "otel"
	defaultTier1CHUsername  = "cerberus"
	defaultTier1CHPassword  = "cerberus" //nolint:gosec // default fixture credential, not a real one
	defaultTier1PromURL     = "http://127.0.0.1:27090"
	defaultTier1LokiURL     = "http://127.0.0.1:27100"
	defaultTier1TempoURL    = "http://127.0.0.1:27200"
	defaultTier1TempoOTLP   = "127.0.0.1:27201"
	defaultTier1CerberusURL = "http://127.0.0.1:27080"
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

// tier1Vars pairs every env var LoadLiveEndpoints reads with the struct field
// it fills in and the default that matches docker-compose.dual.yml's
// published port map, so an operator overriding one endpoint (a stack
// published on non-default ports) does not have to export all ten.
var tier1Vars = []struct {
	name string
	dflt string
	dst  func(*LiveEndpoints) *string
}{
	{EnvTier1CHAddr, defaultTier1CHAddr, func(e *LiveEndpoints) *string { return &e.CHAddr }},
	{EnvTier1CHDatabase, defaultTier1CHDatabase, func(e *LiveEndpoints) *string { return &e.CHDatabase }},
	{EnvTier1CHUsername, defaultTier1CHUsername, func(e *LiveEndpoints) *string { return &e.CHUsername }},
	{EnvTier1CHPassword, defaultTier1CHPassword, func(e *LiveEndpoints) *string { return &e.CHPassword }},
	{EnvTier1PromURL, defaultTier1PromURL, func(e *LiveEndpoints) *string { return &e.PromURL }},
	{EnvTier1LokiURL, defaultTier1LokiURL, func(e *LiveEndpoints) *string { return &e.LokiURL }},
	{EnvTier1TempoURL, defaultTier1TempoURL, func(e *LiveEndpoints) *string { return &e.TempoURL }},
	{EnvTier1TempoOTLPAddr, defaultTier1TempoOTLP, func(e *LiveEndpoints) *string { return &e.TempoOTLPAddr }},
	{EnvTier1CerberusURL, defaultTier1CerberusURL, func(e *LiveEndpoints) *string { return &e.CerberusURL }},
}

// envOr returns the named env var, or fallback when it is unset or empty.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// defaultTier1ManifestPath is TIER1_MANIFEST's fallback: the path
// `just migration-tier1-seed` writes by default, resolved against the
// repository root rather than hardcoded relative to the working directory —
// a Tier-1 test package's working directory is its own package directory
// (e.g. tiers/tier1-dual/), not the harness root tier1_parity_test.go's own
// relative default assumes.
func defaultTier1ManifestPath() (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	return HarnessPath(root, ".out", "manifest.json"), nil
}

// LoadLiveEndpoints reads the Tier-1 env vars the harness needs, falling back
// to the docker-compose.dual.yml port map for anything unset. Unlike Tier-0's
// OfflineEnv (which SETS a fixed environment), a Tier-1 scenario runs against
// a stack the operator already brought up out-of-band
// (`just migration-tier1-up` / `just migration-tier1-seed`); reading rather
// than assuming lets the same scenarios run unchanged against a stack
// published on different ports, while the default keeps the common case —
// the stack `just migration-tier1-up` just brought up — reachable without
// exporting anything.
func LoadLiveEndpoints() (LiveEndpoints, error) {
	var le LiveEndpoints
	for _, v := range tier1Vars {
		*v.dst(&le) = envOr(v.name, v.dflt)
	}
	manifestDefault, err := defaultTier1ManifestPath()
	if err != nil {
		return LiveEndpoints{}, fmt.Errorf("migration harness: resolve the default manifest path: %w", err)
	}
	le.ManifestPath = envOr(EnvTier1Manifest, manifestDefault)
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
