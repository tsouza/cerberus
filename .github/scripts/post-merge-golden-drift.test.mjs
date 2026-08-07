// post-merge-golden-drift.test.mjs — pins for the post-merge drift gate.
//
// This gate is the only thing that ever looks at the bytes a squash-merge
// actually produced (see the script's own header for why `strict: true` is the
// wrong way to get that). Nothing else fails when it is broken: a gate that
// silently reports "clean" is indistinguishable from a repository with no
// drift, and it would stay that way until the first corrupted baseline reached
// a release. So both directions are pinned here, over a real git repository
// with a real generator, driving the real script as a subprocess.
//
// The generator is a stub `just` because what is under test is the
// regenerate-and-DIFF relation, not any particular regenerator: the script
// invokes the shard table's real command through JUST_EXECUTABLE, and the stub
// stands in for a generator whose output does or does not match the committed
// bytes. Pointing this at a live shard would test chDB and the Go toolchain
// instead, and would take minutes to say the same thing.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const SCRIPT = fileURLToPath(new URL('./post-merge-golden-drift.mjs', import.meta.url));

// The shard the fixture drives. `migration` is recipe-backed, so the whole
// regeneration goes through JUST_EXECUTABLE in one command, and its goldens
// carry a `*` glob — the shape that made a per-shard pathspec match nothing at
// all and report a clean tree over a drifted one.
const SHARD = 'migration';
const RECIPE = 'migration-golden';

// Paths from the real shard table, so a table edit that moves the migration
// corpus fails this test rather than quietly making it assert nothing.
const CORPUS = 'test/e2e/migration/archetypes';
const INPUT = `${CORPUS}/checkout/query.promql`;
const GOLDEN = `${CORPUS}/checkout/expected/report.txt`;

const COMMITTED_GOLDEN = 'SELECT committed FROM golden\n';

function git(cwd, args) {
  const r = spawnSync('git', args, {
    cwd,
    encoding: 'utf8',
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: 'pin',
      GIT_AUTHOR_EMAIL: 'pin@example.invalid',
      GIT_COMMITTER_NAME: 'pin',
      GIT_COMMITTER_EMAIL: 'pin@example.invalid',
    },
  });
  assert.equal(r.status, 0, `git ${args.join(' ')} failed: ${r.stderr}`);
  return r.stdout.trim();
}

function write(dir, rel, body) {
  const abs = path.join(dir, rel);
  mkdirSync(path.dirname(abs), { recursive: true });
  writeFileSync(abs, body);
}

/**
 * A repository shaped like the one this gate runs on: a corpus, a committed
 * golden derived from it, and the two commits whose interleaving is the whole
 * failure mode — one that moved the corpus (`culprit`), and one that landed on
 * top of it without ever being checked against it (`landed`).
 *
 * `regenerates` is what the stub generator writes into the golden when the
 * script invokes it. Equal to COMMITTED_GOLDEN it models a merge whose
 * artefacts are correct; anything else models the drift.
 */
function fixture({ regenerates }) {
  // The stub generator lives OUTSIDE the repository. Inside it, git sees an
  // untracked executable and reports it as a drifted artefact no shard claims —
  // the gate would be right and the fixture wrong.
  const root = mkdtempSync(path.join(tmpdir(), 'post-merge-drift-'));
  const dir = path.join(root, 'repo');
  mkdirSync(dir, { recursive: true });
  git(dir, ['init', '-q', '-b', 'main', '.']);

  write(dir, INPUT, 'rate(http_requests_total[5m])\n');
  write(dir, GOLDEN, COMMITTED_GOLDEN);
  git(dir, ['add', '-A']);
  git(dir, ['commit', '-qm', 'base']);

  // The commit that moved main underneath the in-flight PR.
  write(dir, INPUT, 'sum(rate(http_requests_total[5m]))\n');
  git(dir, ['add', '-A']);
  git(dir, ['commit', '-qm', 'feat(migration): widen the checkout archetype query']);
  const before = git(dir, ['rev-parse', 'HEAD']);

  // The squash-merge that landed on top of it, computed against an older base.
  write(dir, 'docs/unrelated.md', 'unrelated\n');
  git(dir, ['add', '-A']);
  git(dir, ['commit', '-qm', 'docs: an unrelated change that merged last']);
  const after = git(dir, ['rev-parse', 'HEAD']);

  // The stub generator. It answers only to the recipe the shard table names,
  // so a table change that renames the recipe fails loudly here.
  const just = path.join(root, 'stub-just');
  writeFileSync(
    just,
    [
      '#!/bin/sh',
      `[ "$1" = "${RECIPE}" ] || { echo "stub-just: unexpected recipe $1" >&2; exit 1; }`,
      `cat > "${GOLDEN}" <<'GOLDEN_EOF'`,
      regenerates.replace(/\n$/, ''),
      'GOLDEN_EOF',
      '',
    ].join('\n'),
  );
  chmodSync(just, 0o755);

  return { root, dir, before, after, just };
}

function run({ dir, before, after, just }, extraEnv = {}) {
  return spawnSync(process.execPath, [SCRIPT], {
    cwd: dir,
    encoding: 'utf8',
    env: {
      ...process.env,
      POST_MERGE_BEFORE: before,
      POST_MERGE_AFTER: after,
      POST_MERGE_SHARDS: SHARD,
      JUST_EXECUTABLE: just,
      ...extraEnv,
    },
  });
}

test('a merge whose artefacts regenerate to the committed bytes passes', (t) => {
  const fx = fixture({ regenerates: COMMITTED_GOLDEN });
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));

  const r = run(fx);
  assert.equal(r.status, 0, `expected a clean pass, got:\n${r.stdout}\n${r.stderr}`);
  assert.match(r.stdout, /regenerate to the committed bytes/);
  // The negative control has to prove the generator actually RAN — a stub that
  // was never invoked would also leave a clean tree, and would pass this test
  // while proving nothing at all.
  assert.match(r.stdout, new RegExp(`==> ${SHARD}:`));
});

test('a merge whose artefacts no longer regenerate fails and names the drift', (t) => {
  const fx = fixture({ regenerates: 'SELECT regenerated FROM golden\n' });
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));

  const r = run(fx);
  assert.equal(r.status, 1, `expected drift to fail, got:\n${r.stdout}\n${r.stderr}`);

  // The failure message IS the product: whoever reads it merged last and has
  // no other context. Each of these four is a separate thing they need.
  assert.match(r.stdout, new RegExp(`\`${SHARD}\` shard on \`main\` is not what its generator`));
  assert.match(r.stdout, new RegExp(GOLDEN.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  assert.match(r.stdout, new RegExp(`just update-golden ${SHARD}`));
  assert.match(r.stdout, /just landed: *\w+ docs: an unrelated change that merged last/);
  assert.match(
    r.stdout,
    /merged before it: *\w+ feat\(migration\): widen the checkout archetype query/,
  );
});

test('a drifted artefact outside every declared golden root is reported, not swallowed', (t) => {
  const fx = fixture({ regenerates: COMMITTED_GOLDEN });
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));

  // Same clean-golden stub, plus one write no shard declares. The gate must
  // still fail: an artefact nothing claims is defended by nothing.
  const stray = 'test/perf/unclaimed-artefact.json';
  writeFileSync(fx.just, `#!/bin/sh\nmkdir -p "$(dirname "${stray}")"\necho '{}' > "${stray}"\n`);
  chmodSync(fx.just, 0o755);

  const r = run(fx);
  assert.equal(r.status, 1, `expected an unclaimed write to fail, got:\n${r.stdout}\n${r.stderr}`);
  assert.match(r.stdout, /no shard declares as its own/);
  assert.match(r.stdout, new RegExp(stray.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
});

test('a failing regeneration command fails the gate rather than reporting clean', (t) => {
  const fx = fixture({ regenerates: COMMITTED_GOLDEN });
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));

  writeFileSync(fx.just, '#!/bin/sh\necho "generator exploded" >&2\nexit 3\n');
  chmodSync(fx.just, 0o755);

  const r = run(fx);
  assert.equal(r.status, 1, `expected a failed generator to fail the gate, got: ${r.status}`);
  assert.match(r.stdout, new RegExp(`the \`${SHARD}\` shard could not be regenerated`));
});

test('a push that implies no shard is a pass, not a silent skip', (t) => {
  const fx = fixture({ regenerates: COMMITTED_GOLDEN });
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));

  // No POST_MERGE_SHARDS override: the derivation runs for real over a diff
  // that touches only docs/, and must come back empty.
  const r = spawnSync(process.execPath, [SCRIPT], {
    cwd: fx.dir,
    encoding: 'utf8',
    env: {
      ...process.env,
      POST_MERGE_BEFORE: fx.before,
      POST_MERGE_AFTER: fx.after,
      POST_MERGE_SHARDS: '',
      JUST_EXECUTABLE: fx.just,
    },
  });
  assert.equal(r.status, 0, `expected an empty implication to pass, got:\n${r.stdout}\n${r.stderr}`);
  assert.match(r.stdout, /imply no generated shard/);
});

test('a missing POST_MERGE_BEFORE fails closed', (t) => {
  const fx = fixture({ regenerates: COMMITTED_GOLDEN });
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));

  const r = spawnSync(process.execPath, [SCRIPT], {
    cwd: fx.dir,
    encoding: 'utf8',
    env: { ...process.env, POST_MERGE_BEFORE: '', JUST_EXECUTABLE: fx.just },
  });
  assert.equal(r.status, 1, 'a gate with no merge to validate must not report success');
  assert.match(r.stdout, /POST_MERGE_BEFORE is unset/);
});
