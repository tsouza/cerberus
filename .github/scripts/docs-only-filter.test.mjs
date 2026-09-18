// docs-only-filter.test.mjs — node:test guard for the docs-only composite's
// two halves. The filter is rendered from the registry's non-impact globs
// (one source of truth for six workflows and the quickstart canary), and the
// verdict is `true` on exactly one shape: a pull request whose `code` filter
// positively came back `false`.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { loadRegistry } from './ci-lane-contract.mjs';
import { FILTER_KEY, docsOnly, filtersYAML } from './docs-only-filter.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(HERE, '..', '..');
const CLI = join(HERE, 'docs-only-filter.mjs');

test('filtersYAML renders the `**` baseline minus every non-impact glob, in registry order', () => {
  assert.equal(filtersYAML(['**/*.md', 'docs/**']), "code:\n  - '**'\n  - '!**/*.md'\n  - '!docs/**'\n");
  assert.equal(FILTER_KEY, 'code');
});

test('filtersYAML refuses an empty glob list and an unrenderable glob', () => {
  assert.throws(() => filtersYAML([]), /declares no known_nonimpact_globs/);
  assert.throws(() => filtersYAML(["it's"]), /unrenderable/);
});

test('docsOnly is true on a pull request whose code filter said false, and nowhere else', () => {
  assert.equal(docsOnly({ eventName: 'pull_request', codeChanged: 'false' }), true);
  assert.equal(docsOnly({ eventName: 'pull_request', codeChanged: 'true' }), false);
  assert.equal(docsOnly({ eventName: 'pull_request', codeChanged: '' }), false, 'no verdict is not docs-only');
  for (const eventName of ['push', 'schedule', 'workflow_dispatch', 'merge_group', '']) {
    assert.equal(docsOnly({ eventName, codeChanged: 'false' }), false, eventName);
  }
});

test('MODE=filters renders exactly the live registry globs into $GITHUB_OUTPUT', () => {
  const dir = mkdtempSync(join(tmpdir(), 'docs-only-'));
  try {
    const output = join(dir, 'out');
    const res = spawnSync(process.execPath, [CLI], {
      cwd: REPO_ROOT,
      encoding: 'utf8',
      env: { ...process.env, MODE: 'filters', GITHUB_OUTPUT: output },
    });
    assert.equal(res.status, 0, res.stderr);
    const written = readFileSync(output, 'utf8');
    const registry = loadRegistry('.github/ci-lanes.json', { root: REPO_ROOT });
    const expected = filtersYAML(registry.impact_selection.known_nonimpact_globs);
    assert.equal(written, `filters<<DOCS_ONLY_FILTERS_EOF\n${expected}DOCS_ONLY_FILTERS_EOF\n`);
    assert.ok(registry.impact_selection.known_nonimpact_globs.includes('README*'), 'README* is a non-impact path');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('MODE=compute writes docs_only and an unknown MODE fails', () => {
  const dir = mkdtempSync(join(tmpdir(), 'docs-only-'));
  try {
    const output = join(dir, 'out');
    const run = (env) =>
      spawnSync(process.execPath, [CLI], { cwd: REPO_ROOT, encoding: 'utf8', env: { ...process.env, GITHUB_OUTPUT: output, ...env } });
    assert.equal(run({ MODE: 'compute', EVENT_NAME: 'pull_request', CODE_CHANGED: 'false' }).status, 0);
    assert.match(readFileSync(output, 'utf8'), /^docs_only=true$/m);
    assert.equal(run({ MODE: 'compute', EVENT_NAME: 'push', CODE_CHANGED: 'false' }).status, 0);
    assert.match(readFileSync(output, 'utf8'), /docs_only=false$/m);
    const bad = run({ MODE: 'nope' });
    assert.equal(bad.status, 1);
    assert.match(bad.stdout + bad.stderr, /::error::.*MODE must be filters or compute/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
