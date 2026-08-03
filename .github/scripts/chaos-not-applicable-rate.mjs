// chaos-not-applicable-rate.mjs — drought detector for the chaos lane's
// silent not-applicable outcomes.
//
// chaos-run.mjs's ch-network-partition and route-memo-activation scenarios
// each have a legitimate not-applicable exit: their precondition (kube-router
// actually enforcing NetworkPolicy in this k3d image; the pinned 24h/15s
// query actually crossing CERBERUS_CH_QUERY_MAX_MEMORY this run) is
// environment- or data-dependent, so one not-applicable run is not itself a
// bug — see each scenario's own header comment in chaos-run.mjs. The risk is
// DRIFT: if the precondition permanently stops materialising (kube-router
// silently drops enforcement; the pinned query permanently stops crossing the
// cap), the lane reports success FOREVER while exercising neither contract,
// and a green track record looks identical to a working one.
//
// This script mines the last N runs of the `chaos` job (e2e.yml) via the
// GitHub API — the lane runs push-to-main + nightly + manual only, so run
// history IS the persisted record; no baseline file to commit back. For each
// scenario chaos-run.mjs can record not-applicable for (discovered by
// scanning that file's source for notApplicableMarker() calls — one list,
// not two), it counts how many of the sampled runs the scenario actually
// EXECUTED in, and how many of those were not-applicable. A scenario that
// ran in at least MIN_RUNS sampled runs and was not-applicable in ALL of them
// is a drought: nothing has exercised its contract in the whole window.
//
// Deliberately NOT a PR check — same reasoning as release-gate-drift.yml:
// drift here comes from real-world conditions (a cluster image, live data
// volume) accumulating over many runs, not from any single PR's diff, so
// gating PRs on it would red them for something they did not do.
//
// Env contract:
//   GITHUB_TOKEN        auth for the Actions API (default GITHUB_TOKEN
//                        suffices — actions:read, not the admin rights
//                        release-gate-drift.mjs needs for branch protection).
//   GITHUB_REPOSITORY   owner/repo (runner-provided).
//   GITHUB_API_URL      API base (default https://api.github.com).
//   CHAOS_RATE_WORKFLOW workflow file basename (default e2e.yml).
//   CHAOS_RATE_JOB      job name to sample        (default chaos).
//   CHAOS_RATE_WINDOW   how many recent completed runs of that job to
//                       sample (default 12 — roughly 2 weeks of nightlies).
//   CHAOS_RATE_MIN_RUNS minimum EXECUTED runs a scenario needs before the
//                       gate is willing to call a 100% rate a drought,
//                       rather than too little history to judge (default 5).
//   argv `--self-test` runs the pure-logic assertions and exits.
//
// Exit: 0 = no scenario is in drought (or too few runs sampled to judge
// any of them), 1 = at least one scenario was not-applicable in every
// sampled run it executed in.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import process from 'node:process';

import { error, notice, log, appendStepSummary } from './lib/gh.mjs';

const defaultWorkflow = 'e2e.yml';
const defaultJob = 'chaos';
const defaultWindow = 12;
const defaultMinRuns = 5;
const maxPerPage = 100;

const CHAOS_RUN_MJS = new URL('./chaos-run.mjs', import.meta.url);

// notApplicableMarkerRegex matches the stable prefix chaos-run.mjs's
// notApplicableMarker() emits: "chaos-not-applicable: <scenario> reason=<r>".
// Kept as a regex literal here (not imported) so this script can still parse
// logs from before it existed — see chaos-run.mjs's own comment on why the
// two files don't share the const.
const notApplicableMarkerRegex = /chaos-not-applicable: (\S+) reason=(\S+)/g;

// scenariosWithNotApplicableBranch discovers every scenario name
// chaos-run.mjs can record not-applicable for, by scanning its source for
// notApplicableMarker('<scenario>', ...) call sites. Derived from source
// (not a hand-maintained list) so a new not-applicable branch is picked up
// automatically — the same "one registry, not two" discipline as
// forbid-skip.mjs's CHECKS registry.
export function scenariosWithNotApplicableBranch(chaosRunSrc) {
  const names = new Set();
  const re = /notApplicableMarker\('([a-z0-9-]+)'/g;
  let m;
  while ((m = re.exec(chaosRunSrc)) !== null) names.add(m[1]);
  return [...names].sort();
}

// scenarioOutcomeInLog classifies whether `scenario` executed in this run's
// job log, and if so whether it was not-applicable. Returns null when the
// scenario has no trace in the log at all (CHAOS_SCENARIOS / CHAOS_PHASE
// selected it out for that run) — such a run must NOT count in that
// scenario's denominator, or a lane that simply never selects the scenario
// would masquerade as a 100% drought.
export function scenarioOutcomeInLog(logText, scenario) {
  const ranMarkers = [
    `scenario ${scenario}: all resilience contracts held`, // notice() on pass
    `[${scenario}]`, // error(`[${s.name}] ${f}`) on a real assertion failure
  ];
  let notApplicable = false;
  let ran = ranMarkers.some((marker) => logText.includes(marker));

  notApplicableMarkerRegex.lastIndex = 0;
  let m;
  while ((m = notApplicableMarkerRegex.exec(logText)) !== null) {
    if (m[1] === scenario) {
      notApplicable = true;
      ran = true;
    }
  }
  if (!ran) return null;
  return notApplicable ? 'not-applicable' : 'exercised';
}

// droughtReport tallies, per scenario, how many sampled runs it executed in
// and how many of those were not-applicable, then flags any scenario that
// crossed minRuns executed runs with a 100% not-applicable rate.
export function droughtReport({ scenarios, logs, minRuns }) {
  const tally = new Map(scenarios.map((s) => [s, { executed: 0, notApplicable: 0 }]));
  for (const logText of logs) {
    for (const scenario of scenarios) {
      const outcome = scenarioOutcomeInLog(logText, scenario);
      if (outcome === null) continue;
      const t = tally.get(scenario);
      t.executed += 1;
      if (outcome === 'not-applicable') t.notApplicable += 1;
    }
  }
  const drought = [];
  for (const [scenario, t] of tally) {
    if (t.executed >= minRuns && t.notApplicable === t.executed) {
      drought.push({ scenario, executed: t.executed, notApplicable: t.notApplicable });
    }
  }
  return { tally, drought };
}

async function apiJson(url, headers, what) {
  const res = await fetch(url, { headers });
  if (!res.ok) {
    throw new Error(`${what}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  return res.json();
}

async function apiText(url, headers, what) {
  const res = await fetch(url, { headers });
  if (!res.ok) {
    throw new Error(`${what}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  return res.text();
}

async function main() {
  const workflow = process.env.CHAOS_RATE_WORKFLOW || defaultWorkflow;
  const jobName = process.env.CHAOS_RATE_JOB || defaultJob;
  const window = Number(process.env.CHAOS_RATE_WINDOW || defaultWindow);
  const minRuns = Number(process.env.CHAOS_RATE_MIN_RUNS || defaultMinRuns);
  const repo = process.env.GITHUB_REPOSITORY;
  const token = process.env.GITHUB_TOKEN;
  const apiBase = process.env.GITHUB_API_URL || 'https://api.github.com';

  if (!repo) throw new Error('GITHUB_REPOSITORY is unset');
  if (!token) throw new Error('GITHUB_TOKEN is unset');

  const headers = {
    Accept: 'application/vnd.github+json',
    Authorization: `Bearer ${token}`,
    'X-GitHub-Api-Version': '2022-11-28',
  };

  const scenarios = scenariosWithNotApplicableBranch(readFileSync(CHAOS_RUN_MJS, 'utf8'));
  if (scenarios.length === 0) {
    throw new Error(
      'discovered ZERO scenarios with a not-applicable branch in chaos-run.mjs — either every ' +
        'not-applicable exit was removed, or notApplicableMarker() calls no longer match the ' +
        "scanner's regex; either way this gate has nothing to check, which is itself a failure",
    );
  }

  const runsResp = await apiJson(
    `${apiBase}/repos/${repo}/actions/workflows/${encodeURIComponent(workflow)}/runs` +
      `?status=completed&per_page=${Math.min(window, maxPerPage)}`,
    headers,
    `list ${workflow} runs`,
  );
  const runs = runsResp.workflow_runs ?? [];
  if (runs.length === 0) {
    throw new Error(`${workflow} reports zero completed runs — either it never ran or the token cannot see it`);
  }

  const logs = [];
  for (const run of runs) {
    const jobsResp = await apiJson(
      `${apiBase}/repos/${repo}/actions/runs/${run.id}/jobs?per_page=${maxPerPage}`,
      headers,
      `list jobs for run ${run.id}`,
    );
    const job = (jobsResp.jobs ?? []).find((j) => j.name === jobName);
    if (!job || job.status !== 'completed') continue;
    const logText = await apiText(
      `${apiBase}/repos/${repo}/actions/jobs/${job.id}/logs`,
      headers,
      `read log for job ${job.id}`,
    );
    logs.push(logText);
  }
  if (logs.length === 0) {
    throw new Error(
      `sampled ${runs.length} ${workflow} run(s) but none had a completed "${jobName}" job — ` +
        'the job name may have changed, or the lane never actually ran',
    );
  }

  const { tally, drought } = droughtReport({ scenarios, logs, minRuns });

  const summaryRows = scenarios.map((s) => {
    const t = tally.get(s);
    const rate = t.executed === 0 ? 'n/a' : `${Math.round((100 * t.notApplicable) / t.executed)}%`;
    return `| ${s} | ${t.executed} | ${t.notApplicable} | ${rate} |`;
  });
  appendStepSummary(
    [
      '## chaos not-applicable drought check',
      '',
      `- job logs sampled: **${logs.length}** (of ${runs.length} recent \`${workflow}\` runs)`,
      `- drought threshold: not-applicable in every one of >= ${minRuns} executed runs`,
      '',
      '| scenario | executed | not-applicable | rate |',
      '|---|---|---|---|',
      ...summaryRows,
      '',
      drought.length === 0
        ? 'No scenario is in drought.'
        : drought
            .map(
              (d) =>
                `- ❌ **${d.scenario}**: not-applicable in all ${d.executed} sampled runs it executed in — ` +
                'its contract has not been exercised in the whole window.',
            )
            .join('\n'),
    ].join('\n'),
  );

  if (drought.length > 0) {
    for (const d of drought) {
      error(
        `${d.scenario}: not-applicable in all ${d.executed} sampled runs (>= ${minRuns} required) — ` +
          'its precondition has not materialised in the whole sampled window; either the environment ' +
          'regressed (e.g. kube-router silently stopped enforcing NetworkPolicy) or the precondition ' +
          'itself needs revisiting',
        { title: 'chaos-not-applicable-rate' },
      );
    }
    process.exit(1);
  }

  notice(
    `chaos not-applicable drought check: no scenario in drought across ${logs.length} sampled run(s) ` +
      `(${scenarios.join(', ')})`,
    { title: 'chaos-not-applicable-rate' },
  );
}

function selfTest() {
  // scenariosWithNotApplicableBranch discovers names from source, ignoring
  // unrelated identifiers.
  const fakeSrc = [
    "notApplicableMarker('scenario-a', 'reason-x');",
    "someOtherCall('scenario-a');",
    "notApplicableMarker('scenario-b', 'reason-y');",
    "notApplicableMarker('scenario-a', 'reason-z');", // duplicate: dedupes
  ].join('\n');
  assert.deepEqual(scenariosWithNotApplicableBranch(fakeSrc), ['scenario-a', 'scenario-b']);
  assert.deepEqual(scenariosWithNotApplicableBranch('no matches here'), []);

  // The REAL chaos-run.mjs must currently expose exactly these two — pins
  // the discovery regex against silent breakage (e.g. a rename of
  // notApplicableMarker that stops matching).
  const realSrc = readFileSync(CHAOS_RUN_MJS, 'utf8');
  assert.deepEqual(scenariosWithNotApplicableBranch(realSrc), ['ch-network-partition', 'route-memo-activation']);

  // scenarioOutcomeInLog: no trace at all -> null (excluded from denominator).
  assert.equal(scenarioOutcomeInLog('nothing relevant here', 'ch-network-partition'), null);

  // Ran and passed.
  assert.equal(
    scenarioOutcomeInLog('scenario ch-network-partition: all resilience contracts held', 'ch-network-partition'),
    'exercised',
  );

  // Ran and failed (a real assertion failure) still counts as EXECUTED, not
  // not-applicable — a scenario that fires and fails is proof the
  // precondition materialised.
  assert.equal(
    scenarioOutcomeInLog('[ch-network-partition] breaker did not trip within budget', 'ch-network-partition'),
    'exercised',
  );

  // Ran and hit the not-applicable branch.
  assert.equal(
    scenarioOutcomeInLog(
      'chaos-not-applicable: ch-network-partition reason=netpol-not-enforced — kube-router is NOT enforcing',
      'ch-network-partition',
    ),
    'not-applicable',
  );

  // A DIFFERENT scenario's marker in the same log must not cross-contaminate.
  assert.equal(
    scenarioOutcomeInLog(
      'chaos-not-applicable: route-memo-activation reason=memory-cap-not-crossed — never crossed the cap',
      'ch-network-partition',
    ),
    null,
  );

  // droughtReport: a scenario not-applicable in every one of >= minRuns
  // executed runs is flagged; one that executed in fewer than minRuns runs is
  // NOT flagged even at 100%, and one with any exercised run is never
  // flagged regardless of rate.
  const naLog = (s) => `chaos-not-applicable: ${s} reason=x — na`;
  const okLog = (s) => `scenario ${s}: all resilience contracts held`;

  const droughtCase = droughtReport({
    scenarios: ['scenario-a', 'scenario-b', 'scenario-c'],
    logs: [
      `${naLog('scenario-a')}\n${naLog('scenario-b')}`,
      `${naLog('scenario-a')}\n${okLog('scenario-b')}`,
      `${naLog('scenario-a')}\n${naLog('scenario-b')}`,
      `${naLog('scenario-a')}\n${naLog('scenario-b')}`,
      `${naLog('scenario-a')}\n${naLog('scenario-b')}`,
      // scenario-c never appears in any log — always excluded, never flagged.
    ],
    minRuns: 5,
  });
  assert.deepEqual(
    droughtCase.drought.map((d) => d.scenario),
    ['scenario-a'],
  );
  assert.equal(droughtCase.tally.get('scenario-c').executed, 0);

  const insufficientHistoryCase = droughtReport({
    scenarios: ['scenario-a'],
    logs: [naLog('scenario-a'), naLog('scenario-a')],
    minRuns: 5,
  });
  assert.deepEqual(insufficientHistoryCase.drought, []);

  log('chaos-not-applicable-rate self-test: OK');
}

if (process.argv.includes('--self-test')) {
  selfTest();
} else {
  main().catch((e) => {
    error(String(e?.message ?? e), { title: 'chaos-not-applicable-rate' });
    process.exit(1);
  });
}
