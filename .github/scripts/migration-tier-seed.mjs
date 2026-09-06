// migration-tier-seed.mjs — seed one migration tier's archetype fixtures into
// its running stack, parameterized by tier so `migration-tier1-seed` and
// `migration-tier2-seed` (just/migration.just) share ONE implementation
// instead of a near-duplicate recipe/script per tier (CLAUDE.md invariant 15,
// issue #3099, epic #3091).
//
// Folds `migration-tier1-seed`'s own default-archetype-list handling: an
// explicit archetype argument seeds ONLY that one (for iterating on a single
// story locally once the stack is already up); the empty default seeds every
// archetype the tier declares, into the ONE compose-stack lifecycle the
// canonical `just migration-tier1` / `migration-tier2` composite (and the CI
// migration-e2e.yml jobs) drive.
//
// Tier-1 seeds two archetypes by default (MIG-02/06/07/08's
// kube-prometheus-stack plus every other @tier1 scenario's three-signal — see
// just/migration.just's own comment on MIGRATION_TIER1_ARCHETYPES). Tier-2
// runs against the same seeded window Tier-1 does — the ruler tier adds
// Grafana-managed alerting on top of the SAME cerberus/ClickHouse pair, not a
// second corpus — so its default list is three-signal alone; this was
// previously `migration-tier2-seed`'s hardcoded, parameter-less command,
// which DEFAULT_ARCHETYPES.tier2 now reproduces exactly rather than special-cases.
//
// Manifest path convention (test/e2e/migration/lib/live.go's LoadManifest,
// and tier1_parity_test.go's plain-Go substrate self-check, both hardcode this):
// three-signal writes the historical unsuffixed manifest.json; every other
// archetype writes manifest-<archetype>.json.
//
// Usage:
//   node .github/scripts/migration-tier-seed.mjs [archetype]
//
// Env:
//   MIGRATION_SEED_TIER  required; "tier1" or "tier2".
//
// Exit: the first failing `go run` seeder's own exit status; 0 once every
// archetype seeds cleanly; 1 for an unrecognised tier.

import { spawnSync } from 'node:child_process';
import process from 'node:process';

import { error, log } from './lib/gh.mjs';

// DEFAULT_ARCHETYPES — the archetype set each tier seeds when no explicit
// archetype argument is given. Tier-1 mirrors just/migration.just's own
// MIGRATION_TIER1_ARCHETYPES; Tier-2 mirrors the fixture
// `migration-tier2-seed` always seeded before this extraction.
export const DEFAULT_ARCHETYPES = {
  tier1: ['three-signal', 'kube-prometheus-stack'],
  tier2: ['three-signal'],
};

// tierLabel — "tier1" -> "tier-1", matching every existing `echo "==> migration
// tier-N ..."` line's hyphenated spelling.
export function tierLabel(tier) {
  return tier.replace(/^tier(\d+)$/, 'tier-$1');
}

// resolveArchetypes — the archetype list to seed for `tier`, given the
// (possibly empty) requested archetype argument. Returns null for an
// unrecognised tier so the caller can fail loudly rather than silently
// seeding nothing.
export function resolveArchetypes(tier, requested, defaults = DEFAULT_ARCHETYPES) {
  if (!Object.prototype.hasOwnProperty.call(defaults, tier)) return null;
  if (requested && requested.trim() !== '') return [requested.trim()];
  return [...defaults[tier]];
}

// manifestPathFor — the manifest path one archetype's seed run publishes to.
// three-signal keeps the historical unsuffixed path; every other archetype
// gets its own `manifest-<archetype>.json`.
export function manifestPathFor(archetype) {
  return archetype === 'three-signal'
    ? 'test/e2e/migration/.out/manifest.json'
    : `test/e2e/migration/.out/manifest-${archetype}.json`;
}

// seedArgsFor — the full `go run` argv that seeds one archetype's fixture.
export function seedArgsFor(archetype, manifestPath) {
  return [
    'run',
    './test/e2e/migration/cmd/seed',
    '--fixture',
    `test/e2e/migration/archetypes/${archetype}/seed/fixture.json`,
    '--manifest',
    manifestPath,
  ];
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('migration-tier-seed.mjs');
}

if (isMain()) {
  const tier = process.env.MIGRATION_SEED_TIER || '';
  const requested = process.argv[2] || '';

  const archetypes = resolveArchetypes(tier, requested);
  if (archetypes === null) {
    error(
      `migration-tier-seed: unrecognised MIGRATION_SEED_TIER "${tier}" — expected one of: ${Object.keys(DEFAULT_ARCHETYPES).join(', ')}.`,
    );
    process.exit(1);
  }

  const label = tierLabel(tier);
  for (const archetype of archetypes) {
    const manifest = manifestPathFor(archetype);
    log(`==> migration ${label} seed (${archetype}, manifest ${manifest})`);
    const res = spawnSync('go', seedArgsFor(archetype, manifest), { stdio: 'inherit' });
    if (res.error) {
      error(`migration-tier-seed: failed to run the Go seeder for ${archetype}: ${res.error.message}`);
      process.exit(1);
    }
    if (res.status !== 0) process.exit(res.status ?? 1);
  }
  process.exit(0);
}
