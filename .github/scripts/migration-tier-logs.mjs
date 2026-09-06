// migration-tier-logs.mjs — dump one migration tier's compose stack state and
// per-service log tail, parameterized by tier + service list so
// `migration-tier1-logs` and `migration-tier2-logs` (just/migration.just)
// share ONE implementation instead of the identical loop shape duplicated
// per tier (CLAUDE.md invariant 15, issue #3099, epic #3091).
//
// The CI job runs this on failure BEFORE teardown, so a red run carries the
// evidence instead of a bare assertion message; teardown then deletes the
// containers this reads from, which is why that ordering matters (see
// just/migration.just's own comment on migration-tier1-logs /
// migration-tier2-logs). Every docker command here is run the same way the
// replaced `|| true`-guarded bash was: this script NEVER fails on its own,
// nor lets a container that already exited mask the real failure.
//
// Usage:
//   node .github/scripts/migration-tier-logs.mjs
//
// Env:
//   MIGRATION_LOG_TIER      required; used only to LABEL the narration lines
//                            ("tier1" -> "tier-1"), matching the replaced
//                            recipes' own wording exactly.
//   MIGRATION_COMPOSE_FILES required; whitespace-separated compose file
//                            path(s) — one for Tier-1, Tier-1's plus the
//                            ruler leg's for Tier-2 — passed to `docker
//                            compose` as repeated `-f` flags, in order.
//   MIGRATION_LOG_SERVICES  required; whitespace-separated service names to
//                            dump, in order (just/migration.just's
//                            MIGRATION_TIER1_SERVICES / MIGRATION_TIER2_SERVICES).
//   MIGRATION_LOG_TAIL      required; trailing line count passed to `docker
//                            compose logs --tail`.
//
// Exit: always 0 — a dump must never turn a lane red on its own.

import { spawnSync } from 'node:child_process';
import process from 'node:process';

import { error, log } from './lib/gh.mjs';

// tierLabel — "tier1" -> "tier-1", matching every existing `echo "==> migration
// tier-N ..."` line's hyphenated spelling. Identical mapping to
// migration-tier-seed.mjs's own — kept as a single-line duplicate rather than
// a shared import, since sharing a one-line regex across two small,
// independently-testable scripts would trade a trivial duplication for a
// cross-script coupling neither script otherwise needs.
export function tierLabel(tier) {
  return tier.replace(/^tier(\d+)$/, 'tier-$1');
}

// parseList — a whitespace-separated env value to a non-empty-token array.
export function parseList(value) {
  return (value || '').split(/\s+/).filter((s) => s !== '');
}

// composeFileArgs — one repeated `-f <file>` pair per compose file, in order.
export function composeFileArgs(files) {
  return files.flatMap((f) => ['-f', f]);
}

// psArgs — the `docker` argv that prints one tier's compose stack state.
export function psArgs(files) {
  return ['compose', ...composeFileArgs(files), 'ps'];
}

// logsArgsFor — the `docker` argv that dumps one service's trailing log lines.
export function logsArgsFor(files, service, tail) {
  return ['compose', ...composeFileArgs(files), 'logs', `--tail=${tail}`, service];
}

function isMain() {
  const invoked = process.argv[1] || '';
  return invoked.endsWith('migration-tier-logs.mjs');
}

if (isMain()) {
  const tier = process.env.MIGRATION_LOG_TIER || '';
  const files = parseList(process.env.MIGRATION_COMPOSE_FILES);
  const services = parseList(process.env.MIGRATION_LOG_SERVICES);
  const tail = process.env.MIGRATION_LOG_TAIL || '';

  if (!tier || files.length === 0 || services.length === 0 || !tail) {
    error(
      'migration-tier-logs: MIGRATION_LOG_TIER, MIGRATION_COMPOSE_FILES, MIGRATION_LOG_SERVICES and ' +
        'MIGRATION_LOG_TAIL must all be set (see just/migration.just).',
    );
    process.exit(1);
  }

  const label = tierLabel(tier);
  log(`==> migration ${label} compose state`);
  spawnSync('docker', psArgs(files), { stdio: 'inherit' });

  for (const service of services) {
    log(`==> migration ${label} logs: ${service}`);
    spawnSync('docker', logsArgsFor(files, service, tail), { stdio: 'inherit' });
  }

  // Never fails on its own — matches the replaced recipe's `|| true` guard
  // on every command.
  process.exit(0);
}
