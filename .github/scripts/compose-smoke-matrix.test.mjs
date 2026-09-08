// compose-smoke-matrix.test.mjs — node:test guard for the compose-smoke
// shard partition's coverage invariant.
//
// Runs on the CHEAP lint/check lane (`node --test .github/scripts/*.test.mjs`)
// — no setup-node, no deps, no compose stack — so a dropped/double-assigned
// spec fails on a much cheaper required check than compose-smoke itself, and
// on every PR (including docs-only PRs that short-circuit compose-smoke).
//
// Guards three things:
//   1. the live tree is a clean cover (the real invariant);
//   2. the UNASSIGNED detector actually fires (so it can't silently rot into
//      a no-op);
//   3. the double-assigned detector actually fires.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  discover,
  collectViolations,
  collectProfileViolations,
  composeProfilesDefined,
  shardEntry,
  shardSweepDepth,
  shardTimeoutMinutes,
  CRAWL_FRONTIER_SHARD_COUNT,
  crawlShardEntries,
  SHARDS,
} from './compose-smoke-matrix.mjs';
import {
  CRAWL_SHARD_TIMEOUT_FULL_MIN,
  CRAWL_SHARD_TIMEOUT_LEAN_MIN,
} from './lib/crawl-budget.mjs';

test('live tree: SHARDS ∪ EXCLUDED is a total, disjoint cover (no violations)', () => {
  const violations = collectViolations(discover());
  assert.deepEqual(violations, [], `unexpected coverage violations:\n${violations.join('\n')}`);
});

test('an unlisted discovered spec is flagged UNASSIGNED (the silent-gap guard)', () => {
  const synthetic = [...discover(), 'iterate-brand-new.spec.ts'];
  const violations = collectViolations(synthetic);
  assert.ok(
    violations.some((v) => v.includes('UNASSIGNED') && v.includes('iterate-brand-new.spec.ts')),
    `expected an UNASSIGNED violation for the synthetic spec; got:\n${violations.join('\n')}`,
  );
});

test('a doubly-counted discovered spec surfaces no false UNASSIGNED but a dup-discovery flag', () => {
  // Discovery returning a path twice must be caught (defends against a glob
  // returning a path twice), without spuriously failing the cover checks.
  const dup = discover();
  const violations = collectViolations([...dup, dup[0]]);
  assert.ok(
    violations.some((v) => v.includes('duplicate paths')),
    `expected a duplicate-discovery violation; got:\n${violations.join('\n')}`,
  );
});

// The crawl shard is the one the de-gate splits out, and the one that writes
// the compose surface inventory.
const CRAWL_SHARD = 'shard-crawl';
const OTHER_SHARD = 'shard-kiosk';

test('an inventory-regen dispatch sweeps the crawl shard full AND pays for it', () => {
  // The regression this pins cost a whole regen cycle to find, and it was
  // invisible as a failure: the shard's SWEEP_DEPTH expression knew that a
  // compose-inventory dispatch must sweep full (crawl.spec.ts refuses to write
  // the inventory at lean depth), while the timeout came from `isSchedule`
  // alone and stayed lean. GitHub cancelled the job at 29m27s against the lean
  // 29-minute ceiling, before the spec's own 75-minute budget could report —
  // so the regen path could not complete, and reported only a cancellation.
  // Depth and ceiling now come from one function; these assert they agree.
  for (const update of ['compose', 'both']) {
    const opts = { isSchedule: false, regeneratesComposeInventory: true };
    assert.equal(
      shardSweepDepth(CRAWL_SHARD, opts),
      'full',
      `${update} regen must sweep the crawl shard full`,
    );
    assert.equal(
      shardTimeoutMinutes(CRAWL_SHARD, opts),
      CRAWL_SHARD_TIMEOUT_FULL_MIN,
      `${update} regen must give the crawl shard the FULL ceiling`,
    );
  }
});

test('a regen dispatch does not make the required shards pay a full sweep', () => {
  const opts = { isSchedule: false, regeneratesComposeInventory: true };
  assert.equal(shardSweepDepth(OTHER_SHARD, opts), 'lean');
});

test('ordinary PR/push and nightly depths are unchanged', () => {
  const pr = { isSchedule: false, regeneratesComposeInventory: false };
  assert.equal(shardSweepDepth(CRAWL_SHARD, pr), 'lean');
  assert.equal(shardTimeoutMinutes(CRAWL_SHARD, pr), CRAWL_SHARD_TIMEOUT_LEAN_MIN);
  assert.equal(shardSweepDepth(OTHER_SHARD, pr), 'lean');

  const nightly = { isSchedule: true, regeneratesComposeInventory: false };
  assert.equal(shardSweepDepth(CRAWL_SHARD, nightly), 'full');
  assert.equal(shardTimeoutMinutes(CRAWL_SHARD, nightly), CRAWL_SHARD_TIMEOUT_FULL_MIN);
  assert.equal(shardSweepDepth(OTHER_SHARD, nightly), 'full');
});

test('every shard ceiling outlives the spec budget at its own depth', () => {
  // The invariant behind the bug: the SPEC must time out first, so a crawl
  // failure reports a verdict instead of a cancellation. Asserting it across
  // both depths is what makes a future depth/ceiling split fail here rather
  // than 29 minutes into a regen.
  for (const opts of [
    { isSchedule: false, regeneratesComposeInventory: false },
    { isSchedule: false, regeneratesComposeInventory: true },
    { isSchedule: true, regeneratesComposeInventory: false },
  ]) {
    const depth = shardSweepDepth(CRAWL_SHARD, opts);
    const ceiling = shardTimeoutMinutes(CRAWL_SHARD, opts);
    const expected =
      depth === 'full' ? CRAWL_SHARD_TIMEOUT_FULL_MIN : CRAWL_SHARD_TIMEOUT_LEAN_MIN;
    assert.equal(
      ceiling,
      expected,
      `crawl ceiling must follow its own sweep depth (${depth})`,
    );
  }
});

test('the crawl frontier count is a positive matrix fan-out', () => {
  assert.ok(Number.isInteger(CRAWL_FRONTIER_SHARD_COUNT));
  assert.ok(CRAWL_FRONTIER_SHARD_COUNT > 1);
});

test('inventory regeneration emits one unsharded compose crawl writer', () => {
  const crawl = SHARDS.find((shard) => shard.name === CRAWL_SHARD);
  assert.ok(crawl, 'the crawl shard must exist');
  assert.deepEqual(
    crawlShardEntries(crawl, { isSchedule: false, regeneratesComposeInventory: true }).map(
      (entry) => [entry.crawlShardIndex, entry.crawlShardCount],
    ),
    [[0, 1]],
  );
});

// ---------------------------------------------------------------------------
// Compose-profile cover (#3181).
//
// The spec partition already fails on a spec no shard runs. These pin the same
// rule for the STACK: a docker-compose profile no shard boots is a silent
// coverage gap, and that gap is exactly how the Tempo structural two-phase A/B
// came to run in no lane at all while its `twophase` services sat defined and
// dead in docker-compose.yml.
// ---------------------------------------------------------------------------

const composeFixture = (yaml) => {
  const dir = mkdtempSync(join(tmpdir(), 'compose-profiles-'));
  const file = join(dir, 'docker-compose.yml');
  writeFileSync(file, yaml);
  return file;
};

test('live tree: every compose profile is booted by a shard, and vice versa', () => {
  const violations = collectProfileViolations(composeProfilesDefined());
  assert.deepEqual(violations, [], `unexpected profile violations:\n${violations.join('\n')}`);
});

test('the two-phase A/B spec is owned by a shard that boots the twophase profile', () => {
  // The wiring the issue asked for, asserted end to end rather than inferred
  // from the profile cover alone: the shard that RUNS the A/B must be the shard
  // that STARTS the split-OFF head it compares against.
  const owner = SHARDS.find((s) => s.specs.includes('tempo_two_phase_compare.spec.ts'));
  assert.ok(owner, 'tempo_two_phase_compare.spec.ts must be assigned to a shard');
  assert.equal(owner.composeProfiles, 'twophase');
});

test('a profile no shard boots is flagged UNBOOTED (the dead-service guard)', () => {
  // Neutralize the fix: drop the profile from the owning shard. The gate must
  // reproduce the #3181 state as a violation rather than a clean cover.
  const neutralized = SHARDS.map((s) => ({ ...s, composeProfiles: undefined }));
  const violations = collectProfileViolations(composeProfilesDefined(), neutralized);
  assert.ok(
    violations.some((v) => v.includes('UNBOOTED') && v.includes('twophase')),
    `expected an UNBOOTED violation for twophase; got:\n${violations.join('\n')}`,
  );
});

test('a shard naming a profile compose does not define is flagged PHANTOM', () => {
  const violations = collectProfileViolations(new Set(['twophase']), [
    { name: 'shard-a', specs: ['x.spec.ts'], composeProfiles: 'twophase' },
    { name: 'shard-b', specs: ['y.spec.ts'], composeProfiles: 'ghost' },
  ]);
  assert.ok(
    violations.some((v) => v.includes('phantom compose profile') && v.includes('ghost')),
    `expected a phantom-profile violation; got:\n${violations.join('\n')}`,
  );
});

test('two shards booting one profile is flagged (wasted duplicate stack)', () => {
  const violations = collectProfileViolations(new Set(['twophase']), [
    { name: 'shard-a', specs: ['x.spec.ts'], composeProfiles: 'twophase' },
    { name: 'shard-b', specs: ['y.spec.ts'], composeProfiles: 'twophase' },
  ]);
  assert.ok(
    violations.some((v) => v.includes('double-booted')),
    `expected a double-booted violation; got:\n${violations.join('\n')}`,
  );
});

test('composeProfilesDefined reads both YAML sequence spellings', () => {
  const file = composeFixture(
    [
      'services:',
      '  a:',
      '    profiles: [inline-one, "inline-two"]',
      '  b:',
      '    profiles:',
      '      - block-one',
      "      - 'block-two'",
      '  c:',
      '    image: nothing',
      '',
    ].join('\n'),
  );
  assert.deepEqual(
    [...composeProfilesDefined(file)].sort(),
    ['block-one', 'block-two', 'inline-one', 'inline-two'],
  );
});

test('composeProfilesDefined throws on a profiles: value it cannot read', () => {
  // A parser that silently returned {} here would report every profile covered
  // — the same "passed having examined nothing" shape this rule exists to stop.
  for (const bad of ['    profiles: [unterminated', '    profiles: &anchor']) {
    const file = composeFixture(['services:', '  a:', bad, ''].join('\n'));
    assert.throws(() => composeProfilesDefined(file), /profiles/i, `expected a throw on: ${bad}`);
  }
});

test('a profiles: key with an empty list throws rather than reading as none', () => {
  const file = composeFixture(['services:', '  a:', '    profiles:', '  b:', '    image: x', ''].join('\n'));
  assert.throws(() => composeProfilesDefined(file), /no readable list/);
});

test('every matrix entry carries its shard\'s compose profiles', () => {
  // e2e.yml interpolates this into COMPOSE_PROFILES at job level. A shard whose
  // entry lost the field would boot the DEFAULT stack and run the A/B against a
  // head that is not up.
  const opts = { isSchedule: false, regeneratesComposeInventory: false };
  for (const shard of SHARDS) {
    assert.equal(
      shardEntry(shard, opts).composeProfiles,
      shard.composeProfiles ?? '',
      `${shard.name} matrix entry must carry its compose profiles`,
    );
  }
});
