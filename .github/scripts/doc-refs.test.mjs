// doc-refs.test.mjs — node:test guard for the doc-to-code reference gate.
//
// Runs on the CHEAP lint/check lane (`node --test .github/scripts/*.test.mjs`)
// — no setup-node, no deps — so a stale doc citation fails on a much cheaper
// required check than any heavy job, and on every PR (including docs-only
// PRs). Mirrors compose-smoke-matrix.test.mjs / dashboard-matrix.test.mjs.
//
// Guards three layers:
//   1. the live docs tree is clean (the real invariant: every cited path
//      exists, every pin is in range);
//   2. the extraction regex captures the shapes it must (nested module
//      paths whole, line ranges, dir refs) and rejects non-anchored paths;
//   3. the verdict + candidate-resolution logic actually fires on a dead
//      file, an out-of-range pin, and a missing dir — so the detectors can't
//      rot into a silent no-op.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  scan,
  extractRefs,
  checkFileRef,
  checkDirRef,
  candidatePaths,
  isUpstream,
} from './doc-refs.mjs';

test('live docs tree: every cited code path exists and pins are in range', () => {
  const { violations } = scan();
  assert.deepEqual(violations, [], `unexpected dead references:\n${violations.join('\n')}`);
});

test('a nested-module path is captured WHOLE, not truncated to its cmd/ tail', () => {
  const { files } = extractRefs('see `compatibility/prometheus/cmd/seed/prom_remote.go` here');
  assert.equal(files.length, 1);
  assert.equal(files[0].path, 'compatibility/prometheus/cmd/seed/prom_remote.go');
});

test('a :start-end line range parses with the HIGH bound as the pin', () => {
  const { files } = extractRefs('`internal/promql/lower.go:1924-1926`');
  assert.equal(files.length, 1);
  assert.equal(files[0].path, 'internal/promql/lower.go');
  assert.equal(files[0].line, 1926);
});

test('a single :line pin parses', () => {
  const { files } = extractRefs('`internal/config/config.go:425`');
  assert.equal(files[0].line, 425);
});

test('a trailing-slash token is classified as a DIR ref, not a file ref', () => {
  const { files, dirs } = extractRefs('staged under `internal/foo/bar/`');
  assert.equal(files.length, 0);
  assert.deepEqual(dirs, ['internal/foo/bar/']);
});

test('a non-anchored path (docs/*, pkg/*) is NOT treated as a code reference', () => {
  const { files, dirs } = extractRefs('`docs/clickhouse-contrib/x.md` and `pkg/util/x.go`');
  assert.equal(files.length, 0);
  assert.equal(dirs.length, 0);
});

test('candidatePaths offers both repo-root and doc-relative interpretations for ../', () => {
  const cands = candidatePaths('../test/surface-parity/', 'docs/coverage.md');
  assert.ok(cands.includes('test/surface-parity/'), `got: ${cands.join(', ')}`);
});

test('candidatePaths strips a leading ./ to the repo-root interpretation', () => {
  const cands = candidatePaths('./test/rejection-parity/', 'docs/compatibility.md');
  assert.ok(cands.includes('test/rejection-parity/'), `got: ${cands.join(', ')}`);
});

test('checkFileRef HARD-FAILS on a missing file (the motivating dead-ref case)', () => {
  const v = checkFileRef({ path: 'cmd/seed/prom_remote.go', line: null }, {
    exists: () => false,
    count: () => 0,
  });
  assert.ok(v && v.includes('missing file'), `expected a missing-file verdict; got: ${v}`);
});

test('checkFileRef tolerates an in-range pin but fails an out-of-range pin (bounds only)', () => {
  const probes = { exists: () => true, count: () => 500 };
  assert.equal(checkFileRef({ path: 'internal/x.go', line: 425 }, probes), null);
  const v = checkFileRef({ path: 'internal/x.go', line: 9999 }, probes);
  assert.ok(v && v.includes('out of range'), `expected an out-of-range verdict; got: ${v}`);
});

test('a vendored upstream snapshot is never a target', () => {
  assert.ok(isUpstream('compatibility/prometheus/upstream/foo.go'));
  const v = checkFileRef({ path: 'compatibility/prometheus/upstream/foo.go', line: null }, {
    exists: () => false,
    count: () => 0,
  });
  assert.equal(v, null);
});

test('checkDirRef fails a missing directory', () => {
  const v = checkDirRef('internal/gone/', { dirExists: () => false });
  assert.ok(v && v.includes('missing directory'), `expected a missing-dir verdict; got: ${v}`);
});
