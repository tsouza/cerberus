// coverage-verdict.test.mjs — `node --test .github/scripts/coverage-verdict.test.mjs`
//
// Wired into coverage.yml's `coverage-plan` job, which runs BEFORE any lane
// spends a runner measuring anything, so the verdict this gate will publish is
// proven able to go red before it is ever asked to go green.
//
// Every branch of classifyMeasurement() is driven here, in both directions:
// the two that pass (measured-and-clear, honestly-not-measured) and the four
// that fail (a heavy run with nothing to show for it, a missed floor, a
// non-heavy run holding a profile, a gate that ran when it should not have).
// A verdict script whose only test was "the happy path returns MEASURED" would
// be the same vacuous green it exists to remove.

import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import test from 'node:test';
import { laneRecordPath, resolveUpdateLanes } from './coverage-summary.mjs';
import {
  MAX_TRUNK_CANDIDATE_PROBES,
  MAX_UNMEASURED_TRUNK_COMMITS,
  TRUNK_RUN_SCAN_PAGE_SIZE,
  VERDICT_BROKEN,
  VERDICT_MEASURED,
  VERDICT_NOT_MEASURED,
  classifyMeasurement,
  describeTrunk,
  fetchTrunkMeasurement,
  inspectEvidence,
  renderBanner,
  selectMeasuredRunCandidates,
} from './coverage-verdict.mjs';

const PROFILE = 'cover-merged.out';
const RECORD = `${PROFILE}.lanes.json`;
const PROFILE_TEXT = 'mode: set\ngithub.com/tsouza/cerberus/internal/x/x.go:1.1,2.2 1 1\n';
const DIGEST = createHash('sha256').update(PROFILE_TEXT).digest('hex');

// A readFile stub over an in-memory { path: contents } map. A path that is not
// in the map throws the ENOENT the real readFileSync would.
function files(map) {
  return (path) => {
    if (!(path in map)) {
      const e = new Error(`ENOENT: no such file or directory, open '${path}'`);
      e.code = 'ENOENT';
      throw e;
    }
    return map[path];
  };
}

const fullLaneFiles = files({
  [PROFILE]: PROFILE_TEXT,
  [RECORD]: JSON.stringify({ lanes: 'default+chdb', profileSha256: DIGEST }),
});

const provable = inspectEvidence(PROFILE, fullLaneFiles);
const absent = inspectEvidence(PROFILE, files({}));

test('inspectEvidence proves a full-lane profile bound to its own bytes', () => {
  assert.equal(provable.present, true);
  assert.equal(provable.provable, true);
  assert.equal(provable.lanes, 'default+chdb');
  assert.equal(provable.digest, DIGEST);
});

test('inspectEvidence refuses a profile whose record describes other bytes', () => {
  const e = inspectEvidence(
    PROFILE,
    files({ [PROFILE]: PROFILE_TEXT, [RECORD]: JSON.stringify({ lanes: 'default+chdb', profileSha256: 'f'.repeat(64) }) }),
  );
  assert.equal(e.present, true);
  assert.equal(e.provable, false);
  assert.match(e.reason, /describes a different profile/);
});

test('inspectEvidence refuses a default-only profile', () => {
  const text = PROFILE_TEXT;
  const e = inspectEvidence(
    PROFILE,
    files({ [PROFILE]: text, [RECORD]: JSON.stringify({ lanes: 'default', profileSha256: DIGEST }) }),
  );
  assert.equal(e.provable, false);
  assert.match(e.reason, /'default' lane set, not 'default\+chdb'/);
});

test('inspectEvidence refuses a profile with no lane record at all', () => {
  const e = inspectEvidence(PROFILE, files({ [PROFILE]: PROFILE_TEXT }));
  assert.equal(e.present, true);
  assert.equal(e.provable, false);
  assert.match(e.reason, /does not exist, so the lane set/);
});

// The validation is coverage-summary.mjs's, not a second copy of it: the same
// function that decides whether a profile may become floors decides whether it
// counts as a measurement. If these two ever disagreed about which bytes are
// proven, the digest binding would have stopped meaning anything.
test('inspectEvidence delegates to the floor-update path validator', () => {
  const recordPath = laneRecordPath(PROFILE);
  assert.equal(recordPath, RECORD);
  const bad = resolveUpdateLanes('{oops', PROFILE_TEXT, RECORD);
  assert.equal(inspectEvidence(PROFILE, files({ [PROFILE]: PROFILE_TEXT, [RECORD]: '{oops' })).reason, bad.err);
});

test('inspectEvidence refuses an unparseable and a misshapen lane record', () => {
  const bad = inspectEvidence(PROFILE, files({ [PROFILE]: PROFILE_TEXT, [RECORD]: '{oops' }));
  assert.equal(bad.provable, false);
  assert.match(bad.reason, /not readable as JSON/);

  const shapeless = inspectEvidence(PROFILE, files({ [PROFILE]: PROFILE_TEXT, [RECORD]: '{"lanes":42}' }));
  assert.equal(shapeless.provable, false);
  assert.match(shapeless.reason, /not a lane record/);
});

test('inspectEvidence reports an empty profile as present but unprovable', () => {
  const e = inspectEvidence(PROFILE, files({ [PROFILE]: '   \n' }));
  assert.equal(e.present, true);
  assert.equal(e.provable, false);
  assert.match(e.reason, /is empty/);
});

test('a heavy run with a clear floor gate reports MEASURED and passes', () => {
  const v = classifyMeasurement({ runHeavy: 'true', gateOutcome: 'success', evidence: provable, eventName: 'push' });
  assert.equal(v.verdict, VERDICT_MEASURED);
  assert.equal(v.ok, true);
  assert.match(v.detail, new RegExp(DIGEST));
});

test('a heavy run whose floor gate failed reports MEASURED and FAILS', () => {
  const v = classifyMeasurement({ runHeavy: 'true', gateOutcome: 'failure', evidence: provable, eventName: 'push' });
  assert.equal(v.verdict, VERDICT_MEASURED);
  assert.equal(v.ok, false, 'a missed floor must not exit 0');
  assert.match(v.headline, /reported "failure"/);
});

test('a heavy run with no profile at all FAILS rather than reporting a measurement', () => {
  const v = classifyMeasurement({ runHeavy: 'true', gateOutcome: 'skipped', evidence: absent, eventName: 'push' });
  assert.equal(v.verdict, VERDICT_BROKEN);
  assert.equal(v.ok, false);
  assert.match(v.detail, /is not present/);
});

test('a heavy run holding only a default-lane profile FAILS', () => {
  const narrow = inspectEvidence(
    PROFILE,
    files({ [PROFILE]: PROFILE_TEXT, [RECORD]: JSON.stringify({ lanes: 'default', profileSha256: DIGEST }) }),
  );
  const v = classifyMeasurement({ runHeavy: 'true', gateOutcome: 'success', evidence: narrow, eventName: 'push' });
  assert.equal(v.verdict, VERDICT_BROKEN);
  assert.equal(v.ok, false, 'a narrow profile must never be reported as a measurement against full-lane floors');
});

test('an ordinary pull request reports NOT MEASURED, and says so in the detail', () => {
  const v = classifyMeasurement({
    runHeavy: 'false',
    gateOutcome: 'skipped',
    evidence: absent,
    eventName: 'pull_request',
  });
  assert.equal(v.verdict, VERDICT_NOT_MEASURED);
  assert.equal(v.ok, true, 'skipping the lanes on a PR is a latency choice, not a failure');
  assert.match(v.detail, /no coverage percentage whatsoever/);
  assert.match(v.headline, /no coverage was measured on this commit/);
});

test('a non-heavy run that somehow holds a profile FAILS: claim and evidence disagree', () => {
  const v = classifyMeasurement({
    runHeavy: 'false',
    gateOutcome: 'skipped',
    evidence: provable,
    eventName: 'pull_request',
  });
  assert.equal(v.verdict, VERDICT_BROKEN);
  assert.equal(v.ok, false);
});

// An empty GATE_OUTCOME must reach the same place as `skipped`. `main()`
// normalises it, and this pins the reason: if a runner ever handed the step an
// empty `outcome` for a skipped step, the un-normalised value would fall into
// the BROKEN branch below and turn the required `coverage` context red on every
// pull request in the repository — the exact inverse of this script's purpose.
test('an empty floor-gate outcome is read as skipped, not as a broken run', () => {
  const v = classifyMeasurement({ runHeavy: 'false', gateOutcome: '', evidence: absent, eventName: 'pull_request' });
  assert.equal(v.verdict, VERDICT_BROKEN, 'un-normalised, an empty outcome IS broken — main() is what normalises it');
  const normalised = classifyMeasurement({
    runHeavy: 'false',
    gateOutcome: '' || 'skipped',
    evidence: absent,
    eventName: 'pull_request',
  });
  assert.equal(normalised.verdict, VERDICT_NOT_MEASURED);
  assert.equal(normalised.ok, true);
});

test('a non-heavy run whose floor gate nevertheless ran FAILS', () => {
  for (const gateOutcome of ['success', 'failure']) {
    const v = classifyMeasurement({ runHeavy: 'false', gateOutcome, evidence: absent, eventName: 'pull_request' });
    assert.equal(v.verdict, VERDICT_BROKEN, gateOutcome);
    assert.equal(v.ok, false, gateOutcome);
  }
});

test('an unset RUN_HEAVY is treated as not-heavy, never as a measurement', () => {
  const v = classifyMeasurement({ runHeavy: '', gateOutcome: 'skipped', evidence: absent, eventName: 'push' });
  assert.equal(v.verdict, VERDICT_NOT_MEASURED);
  assert.equal(v.ok, true);
});

test('selectMeasuredRunCandidates keeps only successful runs of measuring events on the trunk', () => {
  const runs = [
    { id: 1, conclusion: 'success', event: 'pull_request', head_branch: 'main', head_sha: 'pr', html_url: 'u1' },
    { id: 2, conclusion: 'cancelled', event: 'push', head_branch: 'main', head_sha: 'killed', html_url: 'u2' },
    { id: 3, conclusion: 'failure', event: 'push', head_branch: 'main', head_sha: 'red', html_url: 'u3' },
    { id: 4, conclusion: 'success', event: 'push', head_branch: 'release/1.2.x', head_sha: 'other', html_url: 'u4' },
    { id: 5, conclusion: 'success', event: 'push', head_branch: 'main', head_sha: 'good', updated_at: 'T', html_url: 'u5' },
    { id: 6, conclusion: 'success', event: 'schedule', head_branch: 'main', head_sha: 'older', html_url: 'u6' },
  ];
  assert.deepEqual(
    selectMeasuredRunCandidates(runs, { currentSha: 'now', trunkBranch: 'main' }).map((c) => c.sha),
    ['good', 'older'],
  );
});

test('selectMeasuredRunCandidates never credits the run for the commit under judgement', () => {
  const runs = [{ id: 1, conclusion: 'success', event: 'push', head_branch: 'main', head_sha: 'self', html_url: 'u' }];
  assert.deepEqual(selectMeasuredRunCandidates(runs, { currentSha: 'self', trunkBranch: 'main' }), []);
});

test('selectMeasuredRunCandidates returns nothing when no run in the window could have measured', () => {
  const runs = [{ id: 1, conclusion: 'cancelled', event: 'push', head_branch: 'main', head_sha: 'a' }];
  assert.deepEqual(selectMeasuredRunCandidates(runs, { currentSha: 'b', trunkBranch: 'main' }), []);
});

// The load-bearing one: a `release/*`-headed PR's merge produces a SUCCESSFUL
// push run on main that measured nothing (RUN_HEAVY=false). Crediting it would
// reset the staleness counter on a non-measurement — the same substitution of
// a claim for evidence this whole script exists to remove.
test('a successful trunk run that uploaded no profile is not credited as a measurement', async () => {
  const calls = [];
  const out = await fetchTrunkMeasurement({
    repo: 'tsouza/cerberus',
    token: 't',
    currentSha: 'now',
    fetchImpl: async (url) => {
      calls.push(url);
      if (url.includes('/workflows/')) {
        return {
          ok: true,
          json: async () => ({
            workflow_runs: [
              { id: 90, conclusion: 'success', event: 'push', head_branch: 'main', head_sha: 'redundant', updated_at: 'T2', html_url: 'u90' },
              { id: 80, conclusion: 'success', event: 'push', head_branch: 'main', head_sha: 'measured', updated_at: 'T1', html_url: 'u80' },
            ],
          }),
        };
      }
      if (url.includes('/runs/90/artifacts')) return { ok: true, json: async () => ({ artifacts: [] }) };
      if (url.includes('/runs/80/artifacts')) {
        return { ok: true, json: async () => ({ artifacts: [{ name: 'coverage-profile' }] }) };
      }
      return { ok: true, json: async () => ({ ahead_by: 3 }) };
    },
  });
  assert.equal(out.run.sha, 'measured', 'the newest SUCCESS is not automatically the newest MEASUREMENT');
  assert.equal(out.aheadBy, 3);
  assert.equal(out.scanned, 2);
  assert.ok(calls.some((u) => u.includes('/runs/90/artifacts')), 'the redundant run must actually be probed');
});

test('the candidate probe is bounded so a broken trunk cannot become an unbounded API walk', async () => {
  const runs = Array.from({ length: MAX_TRUNK_CANDIDATE_PROBES + 5 }, (_, i) => ({
    id: i + 1,
    conclusion: 'success',
    event: 'push',
    head_branch: 'main',
    head_sha: `s${i}`,
    html_url: `u${i}`,
  }));
  let probes = 0;
  const out = await fetchTrunkMeasurement({
    repo: 'tsouza/cerberus',
    token: 't',
    currentSha: 'now',
    fetchImpl: async (url) => {
      if (url.includes('/workflows/')) return { ok: true, json: async () => ({ workflow_runs: runs }) };
      probes += 1;
      return { ok: true, json: async () => ({ artifacts: [] }) };
    },
  });
  assert.equal(probes, MAX_TRUNK_CANDIDATE_PROBES);
  assert.equal(out.run, null);
  assert.equal(out.scanned, runs.length);
});

test('describeTrunk warns past the unmeasured-commit threshold and not before', () => {
  const run = { sha: 'abcdef1234', at: '2026-08-30T14:42:08Z', url: 'https://example/run' };
  const fresh = describeTrunk({ run, aheadBy: MAX_UNMEASURED_TRUNK_COMMITS }, { trunkBranch: 'main' });
  assert.equal(fresh.stale, false);
  const stale = describeTrunk({ run, aheadBy: MAX_UNMEASURED_TRUNK_COMMITS + 1 }, { trunkBranch: 'main' });
  assert.equal(stale.stale, true);
  assert.match(stale.line, /abcdef123/);
});

test('describeTrunk treats a total absence of measured runs as stale', () => {
  const none = describeTrunk({ run: null, aheadBy: null }, { trunkBranch: 'main' });
  assert.equal(none.stale, true);
  assert.match(none.line, new RegExp(`last ${TRUNK_RUN_SCAN_PAGE_SIZE} `));
  // The window reported is the number of runs actually returned, not the page
  // size we asked for: on a young branch or after a rename the two differ, and
  // overstating it would make the alarm read worse than the evidence supports.
  assert.match(describeTrunk({ run: null, scanned: 7 }, { trunkBranch: 'main' }).line, /last 7 /);
});

test('describeTrunk reports an unknown distance as degraded, never as fresh', () => {
  const unknown = describeTrunk(
    { run: { sha: 'abcdef1234', at: 'T', url: 'u' }, aheadBy: null },
    { trunkBranch: 'main' },
  );
  assert.equal(unknown.degraded, true);
  assert.equal(unknown.stale, false);
  assert.match(unknown.line, /unknown number of commits behind/);
});

test('describeTrunk degrades on an API error without calling it stale', () => {
  const d = describeTrunk({ error: '403 Forbidden' }, { trunkBranch: 'main' });
  assert.equal(d.stale, false);
  assert.equal(d.degraded, true);
  assert.match(d.line, /unavailable/);
});

test('fetchTrunkMeasurement turns a transport failure into { error }, never a throw', async () => {
  const out = await fetchTrunkMeasurement({
    repo: 'tsouza/cerberus',
    token: 't',
    currentSha: 'x',
    fetchImpl: async () => {
      throw new Error('getaddrinfo ENOTFOUND');
    },
  });
  assert.match(out.error, /ENOTFOUND/);
});

test('fetchTrunkMeasurement reports the run even when the compare call fails', async () => {
  const out = await fetchTrunkMeasurement({
    repo: 'tsouza/cerberus',
    token: 't',
    currentSha: 'now',
    fetchImpl: async (url) => {
      if (url.includes('/workflows/')) {
        return {
          ok: true,
          json: async () => ({
            workflow_runs: [
              { id: 7, conclusion: 'success', event: 'push', head_branch: 'main', head_sha: 'm1', updated_at: 'T', html_url: 'u' },
            ],
          }),
        };
      }
      if (url.includes('/artifacts')) {
        return { ok: true, json: async () => ({ artifacts: [{ name: 'coverage-profile' }] }) };
      }
      return { ok: false, status: 404, statusText: 'Not Found' };
    },
  });
  assert.equal(out.run.sha, 'm1');
  assert.equal(out.aheadBy, null, 'a failed /compare must not invent a distance');
});

test('renderBanner leads with the verdict so it is the first thing a reader sees', () => {
  const md = renderBanner({
    verdict: VERDICT_NOT_MEASURED,
    headline: 'no coverage was measured on this commit',
    detail: 'because reasons',
    trunkLine: 'last measured at x',
    trunkBranch: 'main',
  });
  assert.match(md.split('\n')[0], /^## coverage measurement: NOT MEASURED$/);
  assert.match(md, /`main` coverage: last measured at x/);
});
