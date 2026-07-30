// Unit tests for compose-smoke-scope.mjs — run by `node --test` in ci.yml's
// `forbid-skip` job.
//
// The decisions pinned here are the ones whose failure mode is silence. A gate
// that wrongly says "in scope" costs 25 minutes of runner time and is obvious.
// One that wrongly says "out of scope" reports a clean green short-circuit and
// lets a plan/live-server defect through to the release lane — indistinguishable,
// from the outside, from a PR that genuinely had nothing to check.

import assert from 'node:assert/strict';
import test from 'node:test';

import { HARNESS_PATHS, NON_BOOTING_EVENTS, SCOPE_PATHS, decide, stalePaths } from './compose-smoke-scope.mjs';

const pr = { eventName: 'pull_request', headRef: 'fix/whatever' };

test('every declared scope path exists in the tree', () => {
  // The lists are plain strings: a rename turns an entry into one that matches
  // no diff, and the lane silently stops gating on the surface that entry was
  // added to guard. The driver checks this too — pinned here so it fails in the
  // fast `forbid-skip` job rather than only when e2e.yml next runs.
  assert.deepEqual(stalePaths([...HARNESS_PATHS, ...SCOPE_PATHS]), []);
});

test('a stack-visible change boots the stack', () => {
  for (const path of [
    'internal/chsql/emit_node.go',
    'internal/api/tempo/search.go',
    'internal/chclient/client.go',
    'cmd/cerberus/main.go',
  ]) {
    const { inScope } = decide({ ...pr, changed: [path] });
    assert.equal(inScope, true, `${path} should boot the compose stack`);
  }
});

test('a change to the lane harness boots the stack', () => {
  const { inScope, reason } = decide({ ...pr, changed: ['docker-compose.yml'] });
  assert.equal(inScope, true);
  assert.match(reason, /harness/);
});

test('a change outside the scope short-circuits', () => {
  for (const path of [
    'docs/engine.md',
    'README.md',
    'internal/promql/lower.go',
    'internal/optimizer/prewhere.go',
    'test/spec/promql/rate.txtar',
  ]) {
    const { inScope } = decide({ ...pr, changed: [path] });
    assert.equal(inScope, false, `${path} should not boot the compose stack`);
  }
});

test('one in-scope path among many out-of-scope ones still boots the stack', () => {
  const { inScope } = decide({
    ...pr,
    changed: ['docs/engine.md', 'README.md', 'internal/chsql/emit.go', 'CHANGELOG.md'],
  });
  assert.equal(inScope, true);
});

test('a prefix that only shares a path SEGMENT does not count as in scope', () => {
  // `internal/apitest/…` is not under `internal/api`. Substring matching would
  // claim it, which is the harmless direction — but the same bug in reverse
  // (`internal/chsqlx` shadowing `internal/chsql`) is not, so segment-wise
  // matching is pinned in both lists.
  const { inScope } = decide({ ...pr, changed: ['internal/apitest/helper.go'] });
  assert.equal(inScope, false);
});

test('an uncomputable diff boots the stack rather than skipping it', () => {
  const { inScope, reason } = decide({ ...pr, changed: null });
  assert.equal(inScope, true);
  assert.match(reason, /could not be computed/);
});

test('an empty diff short-circuits', () => {
  const { inScope } = decide({ ...pr, changed: [] });
  assert.equal(inScope, false);
});

test('push, schedule and release PRs always run the full lane', () => {
  const events = [
    { eventName: 'push', headRef: '' },
    { eventName: 'schedule', headRef: '' },
    { eventName: 'pull_request', headRef: 'release/1.13.x' },
  ];
  for (const ev of events) {
    // Given a diff that is unambiguously out of scope: these events must ignore
    // it entirely. The lane's whole value on the publish path is that it runs
    // regardless of what the commit happened to touch.
    const { inScope } = decide({ ...ev, changed: ['docs/engine.md'] });
    assert.equal(inScope, true, `${ev.eventName} ${ev.headRef} should run the full lane`);
  }
});

test('workflow_dispatch does not boot the stack, whatever the diff says', () => {
  // The guard this module replaced listed push / schedule / release-PR and left
  // workflow_dispatch out, so dispatching e2e.yml never booted compose. That
  // exclusion is deliberate — the workflow's only dispatch input regenerates the
  // k3d crawl inventory — and it is easy to lose by accident, because
  // runsFullLane() answers `true` for every non-PR event. Pinned in both
  // directions: even a diff that WOULD put a PR in scope must not boot here.
  for (const changed of [['docs/engine.md'], ['internal/chsql/emit_node.go'], null]) {
    const { inScope } = decide({ eventName: 'workflow_dispatch', headRef: '', changed });
    assert.equal(inScope, false, `workflow_dispatch with changed=${JSON.stringify(changed)} must short-circuit`);
  }
});

test('the non-booting event list is explicit, never an allow-list', () => {
  // The inverse — listing the events that DO boot — would make an event nobody
  // anticipated skip the lane silently, which reports as a clean green.
  assert.deepEqual(NON_BOOTING_EVENTS, ['workflow_dispatch']);
  for (const ev of ['push', 'schedule', 'pull_request']) {
    assert.ok(!NON_BOOTING_EVENTS.includes(ev), `${ev} must still reach the scope decision`);
  }
});

test('the scope list stays narrow on purpose', () => {
  // Widening SCOPE_PATHS to `internal` would re-gate the lane on nearly every
  // PR and undo the de-gating it was traded for. If a future change genuinely
  // needs that, it should be an explicit decision, not a one-word edit that
  // nothing notices.
  assert.ok(!SCOPE_PATHS.includes('internal'), 'SCOPE_PATHS must not swallow all of internal/');
  assert.ok(!SCOPE_PATHS.includes('.'), 'SCOPE_PATHS must not swallow the repo root');
});
