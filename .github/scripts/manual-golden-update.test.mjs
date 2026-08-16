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

test('plan expands all into one matrix row per concrete shard', () => {
  const plan = buildPlan('all');
  assert.equal(plan.matrix.include.length, 10);
  assert.ok(plan.matrix.include.every((row) => row.shard !== 'all'));
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
    {
      shard: 'cardinality',
      command_shards: 'solver promql cardinality',
      allowed_shards: 'solver promql cardinality',
      needs_chdb: true,
    },
  ]);
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
  assert.match(workflow, /strategy:\n\s+fail-fast: true\n\s+matrix:/);
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
      env: {
        ...process.env,
        MODE: 'apply-push',
        TARGET_ROOT: publish,
        PATCH_ROOT: patches,
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
