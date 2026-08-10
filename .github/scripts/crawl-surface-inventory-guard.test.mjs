// crawl-surface-inventory-guard.test.mjs — pins for the crawl surface
// inventory ratchet.
//
// A ratchet is only worth its runtime if it goes RED on the corruption it
// claims to catch. This gate's whole reason to exist is #1568's finding that
// GitHub's server-side merge ignores `.gitattributes`' `-merge` driver, which
// means the only thing standing between a blended
// grafana-surface-inventory.<stack>.json and `main` is the gate itself — and a
// gate that always says "clean" is indistinguishable from a repository with no
// corruption. So every check it advertises gets a negative control here.
//
// The fixtures are hand-written canonical bytes rather than output of the
// script's own canonicalize(): generating the expected form from the code
// under test would make the positive control tautological — it would pass for
// any canonicalize() whatsoever, including a broken one. Stating the bytes
// independently is what makes "this file is canonical" a claim the test can
// falsify.
//
// The real script is driven as a subprocess over a temp CRAWL_DIR, so what is
// pinned is the gate as CI invokes it (discovery, exit code, message), not a
// hand-picked internal function.

import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const SCRIPT = fileURLToPath(new URL('./crawl-surface-inventory-guard.mjs', import.meta.url));
const REPO_ROOT = fileURLToPath(new URL('../..', import.meta.url));

const FIXTURE_STACK = 'fixture';
const FIXTURE_NAME = `grafana-surface-inventory.${FIXTURE_STACK}.json`;

// The canonical bytes, written out by hand: keys {doc, stack, surfaces}, rows
// {url, lean}, rows in byCodepoint order on url, two-space indent, trailing
// newline. This is marshalInventory's contract stated independently of the
// code that checks it.
const CANONICAL = `{
  "doc": "fixture inventory",
  "stack": "${FIXTURE_STACK}",
  "surfaces": [
    {
      "url": "/",
      "lean": true
    },
    {
      "url": "/a/app/explore",
      "lean": true
    },
    {
      "url": "/d/uid",
      "lean": false
    }
  ]
}
`;

/** Run the real gate over a temp directory holding the given files. */
function runGuard(files) {
  const dir = mkdtempSync(path.join(tmpdir(), 'crawl-inv-guard-'));
  try {
    for (const [name, body] of Object.entries(files)) {
      writeFileSync(path.join(dir, name), body);
    }
    const res = spawnSync(process.execPath, [SCRIPT], {
      cwd: REPO_ROOT,
      env: { ...process.env, CRAWL_DIR: dir },
      encoding: 'utf8',
    });
    return { status: res.status, out: `${res.stdout}${res.stderr}` };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

/** Assert the gate rejects `body`, and that it says why in recognisable terms. */
function assertRejected(body, expectedFragment, label) {
  const { status, out } = runGuard({ [FIXTURE_NAME]: body });
  assert.equal(status, 1, `${label}: expected a non-zero exit, got ${status}\n${out}`);
  assert.match(out, expectedFragment, `${label}: message did not name the violation\n${out}`);
}

// ---------------------------------------------------------------------------
// Positive controls
// ---------------------------------------------------------------------------

test('accepts a canonical inventory', () => {
  const { status, out } = runGuard({ [FIXTURE_NAME]: CANONICAL });
  assert.equal(status, 0, `expected a clean exit\n${out}`);
  assert.match(out, /byte-identical to their canonical form/);
});

test('the committed inventories are canonical (the gate matches reality)', () => {
  // No CRAWL_DIR: the gate reads the real test/e2e/playwright/crawl tree. If
  // this ever fails, either a committed inventory was hand-edited or the
  // gate's notion of canonical has drifted from lib.ts's marshalInventory.
  const res = spawnSync(process.execPath, [SCRIPT], { cwd: REPO_ROOT, encoding: 'utf8' });
  const out = `${res.stdout}${res.stderr}`;
  assert.equal(res.status, 0, `the committed inventories are not canonical\n${out}`);
  assert.match(out, /grafana-surface-inventory\.compose\.json/);
  assert.match(out, /grafana-surface-inventory\.k3d\.json/);
});

test('covers every discovered stack, not a hard-coded pair', () => {
  const second = 'grafana-surface-inventory.other.json';
  const { status, out } = runGuard({
    [FIXTURE_NAME]: CANONICAL,
    [second]: CANONICAL.replace(`"${FIXTURE_STACK}"`, '"other"'),
  });
  assert.equal(status, 0, `expected both files to pass\n${out}`);
  assert.match(out, /2 inventory file\(s\)/);
});

// ---------------------------------------------------------------------------
// Negative controls — the #1422 blend shapes
// ---------------------------------------------------------------------------

test('rejects rows that are out of canonical order (the blend shape)', () => {
  // Two rows transposed: still valid JSON, still the same row SET, still the
  // same length — the signature of a merge that interleaved two branches.
  const swapped = CANONICAL.replace(
    `    {
      "url": "/a/app/explore",
      "lean": true
    },
    {
      "url": "/d/uid",
      "lean": false
    }`,
    `    {
      "url": "/d/uid",
      "lean": false
    },
    {
      "url": "/a/app/explore",
      "lean": true
    }`,
  );
  assert.notEqual(swapped, CANONICAL, 'fixture edit did not apply');
  assertRejected(swapped, /out of canonical order/, 'transposed rows');
});

test('rejects a duplicate url', () => {
  const dup = CANONICAL.replace(
    `    {
      "url": "/d/uid",
      "lean": false
    }`,
    `    {
      "url": "/a/app/explore",
      "lean": false
    }`,
  );
  assertRejected(dup, /duplicate url/, 'duplicate row');
});

test('rejects a row missing the lean field', () => {
  const missing = CANONICAL.replace(
    `    {
      "url": "/d/uid",
      "lean": false
    }`,
    `    {
      "url": "/d/uid"
    }`,
  );
  assertRejected(missing, /expected exactly \["url","lean"\]/, 'missing lean');
});

test('rejects a row carrying an extra field', () => {
  const extra = CANONICAL.replace(
    `      "url": "/d/uid",
      "lean": false`,
    `      "url": "/d/uid",
      "lean": false,
      "depth": "full"`,
  );
  assertRejected(extra, /expected exactly \["url","lean"\]/, 'extra field');
});

test('rejects reordered row keys', () => {
  const reordered = CANONICAL.replace(
    `      "url": "/d/uid",
      "lean": false`,
    `      "lean": false,
      "url": "/d/uid"`,
  );
  assertRejected(reordered, /expected exactly \["url","lean"\]/, 'reordered keys');
});

test('rejects a non-boolean lean', () => {
  const stringy = CANONICAL.replace('"lean": false', '"lean": "false"');
  assertRejected(stringy, /lean must be a boolean/, 'stringified lean');
});

// ---------------------------------------------------------------------------
// Negative controls — serialization drift the row checks alone would miss
// ---------------------------------------------------------------------------

test('rejects a re-indented file', () => {
  const reindented = `${JSON.stringify(JSON.parse(CANONICAL), null, 4)}\n`;
  assertRejected(reindented, /does NOT match its canonical serialized form/, 're-indent');
});

test('rejects a missing trailing newline', () => {
  assertRejected(
    CANONICAL.trimEnd(),
    /does NOT match its canonical serialized form/,
    'no trailing newline',
  );
});

test('rejects an extra top-level field', () => {
  const extra = CANONICAL.replace('  "doc":', '  "generatedAt": "2026-08-09",\n  "doc":');
  assertRejected(extra, /does NOT match its canonical serialized form/, 'extra top-level key');
});

test('rejects a duplicated JSON key that JSON.parse silently collapses', () => {
  // A blend can produce two "doc" members. JSON.parse keeps the last and the
  // structure looks clean; only the BYTES disagree.
  const doubled = CANONICAL.replace('  "doc":', '  "doc": "stale",\n  "doc":');
  assertRejected(doubled, /does NOT match its canonical serialized form/, 'duplicate JSON key');
});

// ---------------------------------------------------------------------------
// Negative controls — file identity and the crawl's own preconditions
// ---------------------------------------------------------------------------

test('rejects a stack field that disagrees with the filename', () => {
  const crossed = CANONICAL.replace(`"stack": "${FIXTURE_STACK}"`, '"stack": "k3d"');
  assertRejected(crossed, /the file and the field disagree/, 'crossed stacks');
});

test('rejects an inventory with no root surface', () => {
  const rootless = CANONICAL.replace(
    `    {
      "url": "/",
      "lean": true
    },
`,
    '',
  );
  assertRejected(rootless, /no "\/" surface/, 'missing root');
});

test('rejects a non-lean root surface', () => {
  const notLean = CANONICAL.replace(
    `      "url": "/",
      "lean": true`,
    `      "url": "/",
      "lean": false`,
  );
  assertRejected(notLean, /is not lean/, 'non-lean root');
});

test('rejects unparseable JSON', () => {
  assertRejected(`${CANONICAL.trimEnd()},\n`, /not parseable JSON/, 'trailing comma');
});

// ---------------------------------------------------------------------------
// Negative control — vacuity
// ---------------------------------------------------------------------------

test('fails when it discovers no inventory at all', () => {
  // The failure this whole family exists to close: a gate that reads nothing
  // reports clean forever. Moving or renaming the inventories must break the
  // gate loudly rather than silently disarm it.
  const { status, out } = runGuard({ 'unrelated.json': '{}\n' });
  assert.equal(status, 1, `expected a non-zero exit over an empty discovery\n${out}`);
  assert.match(out, /read nothing, so it checked nothing/);
});

test('fails when CRAWL_DIR does not exist', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'crawl-inv-guard-gone-'));
  const missing = path.join(dir, 'nope');
  try {
    const res = spawnSync(process.execPath, [SCRIPT], {
      cwd: REPO_ROOT,
      env: { ...process.env, CRAWL_DIR: missing },
      encoding: 'utf8',
    });
    assert.equal(res.status, 1);
    assert.match(`${res.stdout}${res.stderr}`, /cannot read/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// ---------------------------------------------------------------------------
// The gate's stated blind spot, pinned so it stays honest
// ---------------------------------------------------------------------------

test('a swapped lean bit passes — the limit the gate documents', () => {
  // Two rows exchange their `lean` values. Still sorted, still unique, still
  // canonical: the bytes are exactly what the generator would emit for this
  // content, so nothing offline can tell this from a legitimate crawl result.
  // Pinned deliberately — if a future change makes this detectable, the gate's
  // "what this CANNOT see" paragraph must lose it rather than keep overclaiming
  // in the other direction.
  const swappedLean = CANONICAL.replace(
    `      "url": "/a/app/explore",
      "lean": true`,
    `      "url": "/a/app/explore",
      "lean": false`,
  ).replace(
    `      "url": "/d/uid",
      "lean": false`,
    `      "url": "/d/uid",
      "lean": true`,
  );
  assert.notEqual(swappedLean, CANONICAL, 'fixture edit did not apply');
  const { status } = runGuard({ [FIXTURE_NAME]: swappedLean });
  assert.equal(status, 0, 'the gate claims not to catch this; if it now does, update its header');
});
