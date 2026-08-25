import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

import { buildPlan, main, pathOwnedBy, validateBranch } from './manual-golden-update.mjs';

const SCRIPT = fileURLToPath(new URL('./manual-golden-update.mjs', import.meta.url));
const REPO_ROOT = fileURLToPath(new URL('../../', import.meta.url));

function command(name, args, options = {}) {
  const result = spawnSync(name, args, { encoding: 'utf8', ...options });
  assert.equal(result.status, 0, `${name} ${args.join(' ')} failed: ${result.stderr}`);
  return result.stdout.trim();
}

function git(root, args) {
  return command('git', ['-C', root, ...args], {
    env: {
      ...process.env,
      GIT_AUTHOR_NAME: 'test',
      GIT_AUTHOR_EMAIL: 'test@example.invalid',
      GIT_COMMITTER_NAME: 'test',
      GIT_COMMITTER_EMAIL: 'test@example.invalid',
    },
  });
}

function write(root, relative, body) {
  const target = path.join(root, relative);
  mkdirSync(path.dirname(target), { recursive: true });
  writeFileSync(target, body);
}

test('plan expands all into one matrix row per concrete shard except cardinality', () => {
  const plan = buildPlan('all');
  // 10 concrete shards total (SHARD_NAMES); cardinality is excluded from the
  // ordinary per-shard matrix because its own regeneration is CI-matrix-sharded
  // separately (update-golden.yml's cardinality-leg/cardinality-seal jobs, #2341).
  assert.equal(plan.matrix.include.length, 9);
  assert.ok(plan.matrix.include.every((row) => row.shard !== 'all'));
  assert.ok(plan.matrix.include.every((row) => row.shard !== 'cardinality'));
  assert.ok(plan.cardinality !== null);
});

test('later-stage rows regenerate selected predecessors as context', () => {
  const plan = buildPlan('solver,promql cardinality');
  assert.deepEqual(plan.selected, ['solver', 'promql', 'cardinality']);
  assert.deepEqual(plan.matrix.include, [
    {
      shard: 'solver',
      command_shards: 'solver',
      allowed_shards: 'solver',
      needs_chdb: false,
    },
    {
      shard: 'promql',
      command_shards: 'solver promql',
      allowed_shards: 'solver promql',
      needs_chdb: true,
    },
  ]);
});

test('cardinality is reported separately from the matrix, with its selected predecessors', () => {
  assert.equal(buildPlan('promql').cardinality, null);
  assert.deepEqual(buildPlan('solver,promql cardinality').cardinality, {
    predecessors: ['solver', 'promql'],
  });
  // cardinality alone: no predecessor shard was also selected, so its legs
  // regenerate against the corpus as committed rather than re-deriving it.
  assert.deepEqual(buildPlan('cardinality').cardinality, { predecessors: [] });
});

test('buildPlan merges the diff-implied shards into the requested set', () => {
  // Mirrors test/regression/golden_shard_coverage_test.go's own "fires on an
  // under-covering set" fixture: a PromQL fixture change feeds the solver
  // decision baseline and the cardinality baseline, and its own fixture goes
  // through the parity ledgers — so `promql` alone under-covers it. Here that
  // must MERGE those shards in rather than fail.
  const plan = buildPlan('promql', {
    repoRoot: REPO_ROOT,
    changedFiles: ['test/spec/promql/fixture_under_test.txtar'],
  });
  assert.deepEqual(plan.requested, ['promql']);
  assert.deepEqual(plan.added, ['parity', 'solver', 'cardinality']);
  assert.deepEqual(plan.selected, ['parity', 'solver', 'promql', 'cardinality']);
});

test('buildPlan adds nothing once the requested set already covers the diff', () => {
  const plan = buildPlan('parity solver promql cardinality', {
    repoRoot: REPO_ROOT,
    changedFiles: ['test/spec/promql/fixture_under_test.txtar'],
  });
  assert.deepEqual(plan.added, []);
  assert.deepEqual(plan.selected, ['parity', 'solver', 'promql', 'cardinality']);
});

test('buildPlan implies nothing without a repoRoot, matching the static dump catalogue', () => {
  const plan = buildPlan('promql', { changedFiles: ['test/spec/promql/fixture_under_test.txtar'] });
  assert.deepEqual(plan.added, []);
  assert.deepEqual(plan.selected, ['promql']);
});

test('branch validation rejects the protected branch and ref-shaped input', () => {
  assert.throws(() => validateBranch('main', 'main'), /protected branch/);
  assert.throws(() => validateBranch('refs/heads/topic', 'main'), /short branch name/);
  assert.throws(() => validateBranch('topic branch', 'main'), /invalid branch name/);
  assert.equal(validateBranch('agent/topic', 'main'), 'agent/topic');
});

test('generated path ownership understands recursive roots and one-segment globs', () => {
  assert.equal(pathOwnedBy('test/spec/promql/foo.txtar', ['test/spec/promql']), true);
  assert.equal(pathOwnedBy('test/spec/logql/foo.txtar', ['test/spec/promql']), false);
  const migration = ['test/e2e/migration/archetypes/*/expected'];
  assert.equal(
    pathOwnedBy('test/e2e/migration/archetypes/three-signal/expected/tier1.json', migration),
    true,
  );
  assert.equal(
    pathOwnedBy('test/e2e/migration/archetypes/three-signal/seed/fixture.json', migration),
    false,
  );
});

test('packaging rejects a generated symlink before it becomes an artifact', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'manual-golden-update-unsafe-'));
  try {
    git(root, ['init', '-q', '-b', 'agent/topic']);
    const generated = 'test/spec/promql/a.txtar';
    write(root, generated, 'old\n');
    git(root, ['add', '-A']);
    git(root, ['commit', '-qm', 'test: seed']);
    rmSync(path.join(root, generated));
    symlinkSync('../../../../README.md', path.join(root, generated));

    assert.throws(
      () =>
        main({
          MODE: 'package',
          TARGET_ROOT: root,
          SHARD: 'promql',
          ALLOWED_SHARDS: 'promql',
          OUTPUT_PATH: path.join(root, 'patches', 'promql.patch'),
        }),
      /generated changes must be regular, non-executable files/,
    );
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('workflow keeps write authority out of matrix jobs and publishes once', () => {
  const workflow = readFileSync(new URL('../workflows/update-golden.yml', import.meta.url), 'utf8');
  const regenerate = workflow.slice(
    workflow.indexOf('\n  regenerate:'),
    workflow.indexOf('\n  publish:'),
  );
  assert.match(workflow, /workflow_dispatch:/);
  assert.match(workflow, /shards:/);
  assert.match(workflow, /branch:/);
  // fail-fast is OFF (issue #2368): one shard's failure must not cancel
  // every other concurrent shard's in-progress regeneration and discard its
  // budget for nothing — each leg uploads its own patch independently, so a
  // failed leg only ever costs its own shard.
  assert.match(workflow, /strategy:\n(?:\s+#.*\n)*\s+fail-fast: false\n\s+matrix:/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /uses: actions\/upload-artifact@v7/);
  assert.match(workflow, /gh run download/);
  assert.match(workflow, /token: \$\{\{ secrets\.RELEASE_PAT \}\}/);
  assert.equal((workflow.match(/MODE: apply-push/g) ?? []).length, 1);
  assert.match(regenerate, /permissions:\n\s+contents: read/);
  assert.match(regenerate, /cache: false/);
  assert.doesNotMatch(regenerate, /\bsecrets\./);
  assert.doesNotMatch(regenerate, /uses: actions\/cache@/);
});

test('two independently generated shard patches publish as one commit', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'manual-golden-update-'));
  try {
    const origin = path.join(root, 'origin.git');
    const seed = path.join(root, 'seed');
    command('git', ['init', '--bare', '-q', origin]);
    git(root, ['init', '-q', '-b', 'agent/topic', seed]);
    write(seed, 'test/spec/promql/a.txtar', 'promql old\n');
    write(seed, 'test/spec/logql/a.txtar', 'logql old\n');
    git(seed, ['add', '-A']);
    git(seed, ['commit', '-qm', 'test: seed']);
    git(seed, ['remote', 'add', 'origin', origin]);
    git(seed, ['push', '-q', '-u', 'origin', 'agent/topic']);
    const targetSha = git(seed, ['rev-parse', 'HEAD']);

    const patches = path.join(root, 'patches');
    mkdirSync(patches);
    for (const shard of ['promql', 'logql']) {
      const checkout = path.join(root, `generate-${shard}`);
      command('git', ['clone', '-q', '--branch', 'agent/topic', origin, checkout]);
      write(checkout, `test/spec/${shard}/a.txtar`, `${shard} new\n`);
      command('node', [SCRIPT], {
        env: {
          ...process.env,
          MODE: 'package',
          TARGET_ROOT: checkout,
          SHARD: shard,
          ALLOWED_SHARDS: shard,
          OUTPUT_PATH: path.join(patches, `${shard}.patch`),
        },
      });
    }

    const publish = path.join(root, 'publish');
    command('git', ['clone', '-q', origin, publish]);
    git(publish, ['checkout', '-q', '--detach', targetSha]);
    const outputs = path.join(root, 'outputs');
    command('node', [SCRIPT], {
      cwd: root,
      env: {
        ...process.env,
        MODE: 'apply-push',
        // Match Actions: the controller, sibling target checkout, and artifact
        // directory are all workspace-relative.
        TARGET_ROOT: 'publish',
        PATCH_ROOT: 'patches',
        BRANCH: 'agent/topic',
        DEFAULT_BRANCH: 'main',
        SHARDS_INPUT: 'promql logql',
        TARGET_SHA: targetSha,
        GITHUB_OUTPUT: outputs,
      },
    });

    const newSha = git(seed, ['ls-remote', origin, 'refs/heads/agent/topic']).split(/\s+/)[0];
    assert.notEqual(newSha, targetSha);
    assert.equal(git(publish, ['show', `${newSha}:test/spec/promql/a.txtar`]), 'promql new');
    assert.equal(git(publish, ['show', `${newSha}:test/spec/logql/a.txtar`]), 'logql new');
    assert.match(readFileSync(outputs, 'utf8'), /changed=true/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

// MODE=apply-predecessors / MODE=apply-legs — cardinality-leg's own
// "Apply predecessor context" step and cardinality-seal's own "Apply every
// leg's patch" step, extracted out of update-golden.yml's inline `run: |`
// blocks (finding #10).

function initTargetRepo(root, baseline) {
  const target = path.join(root, 'target');
  git(root, ['init', '-q', '-b', 'main', target]);
  for (const [relative, body] of Object.entries(baseline)) write(target, relative, body);
  git(target, ['add', '-A']);
  git(target, ['commit', '-qm', 'test: seed']);
  return target;
}

function packageShardPatch({ root, target, shard, change, outputPath, label = shard }) {
  const checkout = path.join(root, `generate-${label}`);
  command('git', ['clone', '-q', target, checkout]);
  for (const [relative, body] of Object.entries(change)) write(checkout, relative, body);
  mkdirSync(path.dirname(outputPath), { recursive: true });
  command('node', [SCRIPT], {
    env: {
      ...process.env,
      MODE: 'package',
      TARGET_ROOT: checkout,
      SHARD: shard,
      ALLOWED_SHARDS: shard,
      OUTPUT_PATH: outputPath,
    },
  });
  return outputPath;
}

test('apply-predecessors applies and commits each selected predecessor patch that has real content', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'manual-golden-update-'));
  try {
    const target = initTargetRepo(root, { 'test/spec/promql/a.txtar': 'promql old\n' });
    const headBefore = git(target, ['rev-parse', 'HEAD']);

    const patches = path.join(root, 'patches');
    packageShardPatch({
      root,
      target,
      shard: 'promql',
      change: { 'test/spec/promql/a.txtar': 'promql new\n' },
      outputPath: path.join(patches, 'golden-patch-promql', 'promql.patch'),
    });

    command('node', [SCRIPT], {
      cwd: root,
      env: {
        ...process.env,
        MODE: 'apply-predecessors',
        TARGET_ROOT: 'target',
        PATCH_ROOT: 'patches',
        PREDECESSORS: 'promql',
      },
    });

    assert.equal(readFileSync(path.join(target, 'test/spec/promql/a.txtar'), 'utf8'), 'promql new\n');
    const headAfter = git(target, ['rev-parse', 'HEAD']);
    assert.notEqual(headAfter, headBefore, 'a predecessor commit must exist so assertOwnedChanges sees only this leg\'s own diff');
    assert.equal(git(target, ['status', '--porcelain']), '', 'the predecessor content must be COMMITTED, not left staged');
    assert.match(git(target, ['log', '-1', '--format=%s']), /predecessor context for cardinality-leg/);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('apply-predecessors skips a missing or empty predecessor patch and commits nothing at all', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'manual-golden-update-'));
  try {
    const target = initTargetRepo(root, { 'test/spec/promql/a.txtar': 'promql old\n' });
    const headBefore = git(target, ['rev-parse', 'HEAD']);
    mkdirSync(path.join(root, 'patches'), { recursive: true });

    command('node', [SCRIPT], {
      cwd: root,
      env: {
        ...process.env,
        MODE: 'apply-predecessors',
        TARGET_ROOT: 'target',
        PATCH_ROOT: 'patches',
        // Neither shard has a patch directory at all — the predecessor's own
        // regeneration produced no change, matching the original
        // `[ ! -s "$patch" ]` shell test for a missing OR empty file.
        PREDECESSORS: 'promql logql',
      },
    });

    assert.equal(git(target, ['rev-parse', 'HEAD']), headBefore, 'nothing to apply means nothing to commit');
    assert.equal(git(target, ['status', '--porcelain']), '');
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('apply-predecessors applies only the predecessors that actually have content, still as one commit', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'manual-golden-update-'));
  try {
    const target = initTargetRepo(root, {
      'test/spec/promql/a.txtar': 'promql old\n',
      'test/spec/logql/a.txtar': 'logql old\n',
    });
    const patches = path.join(root, 'patches');
    packageShardPatch({
      root,
      target,
      shard: 'promql',
      change: { 'test/spec/promql/a.txtar': 'promql new\n' },
      outputPath: path.join(patches, 'golden-patch-promql', 'promql.patch'),
    });
    // logql has no patch directory at all (its own regeneration produced no
    // change) — must not be treated as an error.

    command('node', [SCRIPT], {
      cwd: root,
      env: {
        ...process.env,
        MODE: 'apply-predecessors',
        TARGET_ROOT: 'target',
        PATCH_ROOT: 'patches',
        PREDECESSORS: 'promql logql',
      },
    });

    assert.equal(readFileSync(path.join(target, 'test/spec/promql/a.txtar'), 'utf8'), 'promql new\n');
    assert.equal(readFileSync(path.join(target, 'test/spec/logql/a.txtar'), 'utf8'), 'logql old\n');
    assert.equal(git(target, ['log', '--oneline']).split('\n').length, 2, 'exactly one predecessor-context commit on top of the seed');
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});

test('apply-legs applies every non-empty leg patch, uncommitted, ignoring empty ones', () => {
  const root = mkdtempSync(path.join(tmpdir(), 'manual-golden-update-'));
  try {
    const target = initTargetRepo(root, {
      'test/perf/cardinality-baseline/a.json': 'old-a\n',
      'test/perf/cardinality-baseline/b.json': 'old-b\n',
    });
    const patches = path.join(root, 'patches');
    packageShardPatch({
      root,
      target,
      shard: 'cardinality',
      label: 'cardinality-0',
      change: { 'test/perf/cardinality-baseline/a.json': 'new-a\n' },
      outputPath: path.join(patches, 'cardinality-leg-patch-0', 'cardinality-leg-0.patch'),
    });
    packageShardPatch({
      root,
      target,
      shard: 'cardinality',
      label: 'cardinality-1',
      change: { 'test/perf/cardinality-baseline/b.json': 'new-b\n' },
      outputPath: path.join(patches, 'cardinality-leg-patch-1', 'cardinality-leg-1.patch'),
    });
    // An empty leg (its slice of the corpus produced no change) must not
    // error `git apply` out.
    mkdirSync(path.join(patches, 'cardinality-leg-patch-2'), { recursive: true });
    writeFileSync(path.join(patches, 'cardinality-leg-patch-2', 'cardinality-leg-2.patch'), '');

    const headBefore = git(target, ['rev-parse', 'HEAD']);
    command('node', [SCRIPT], {
      cwd: root,
      env: {
        ...process.env,
        MODE: 'apply-legs',
        TARGET_ROOT: 'target',
        PATCH_ROOT: 'patches',
      },
    });

    assert.equal(readFileSync(path.join(target, 'test/perf/cardinality-baseline/a.json'), 'utf8'), 'new-a\n');
    assert.equal(readFileSync(path.join(target, 'test/perf/cardinality-baseline/b.json'), 'utf8'), 'new-b\n');
    // Both legs' content lands, but uncommitted — the seal's packaging step
    // re-stages from scratch and needs nothing pre-committed.
    assert.equal(git(target, ['rev-parse', 'HEAD']), headBefore);
    assert.notEqual(git(target, ['status', '--porcelain']), '');
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
});
