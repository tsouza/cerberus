// Tests for the image-acquisition authentication gate.
//
// Two things are at stake and they pull in opposite directions.
//
// The first is that the gate CAN FAIL. A detector that resolves a job wrongly
// reports the same green as one that resolved it right, so "the tree is clean"
// is not evidence of anything on its own — it is equally consistent with a
// resolver that stopped reading at the first `just`. Every rule below is
// therefore proved against the REAL workflow text with a login step removed
// from it, not against a hand-written fixture that only resembles one.
//
// The second is that it fails for the RIGHT reason. The whole value of this gate
// over a grep is that the one shape which looks like a violation and is not —
// `node --test .github/scripts/compose-pull-images.test.mjs`, a unit test that
// mentions an image-pulling module and pulls nothing — is excluded by a
// structural fact rather than by its name. If that discrimination ever collapses
// into a name list, the gate has become the thing it replaced, so it is pinned
// here directly.

import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import {
  callsSharedPolicy,
  expandVars,
  isDockerHubRef,
  looksLikeImageRef,
  loginRegistry,
  parseWorkflow,
  scan,
  spawnedCommands,
  splitCommands,
  tokenise,
  violationsFor,
} from './assert-image-jobs-authenticate.mjs';

const repoRoot = process.cwd();
const workflows = join(repoRoot, '.github/workflows');

// The job used as the live specimen: it acquires through the shared policy AND
// names a Docker Hub ref two hops away (`just _pull-retry "$CH_IMAGE"` →
// `pull-images.mjs {{IMAGES}}`), so it exercises every rule at once.
const specimenJob = 'e2e.yml:startup-bench';

function realScan() {
  return scan({ root: repoRoot, workflowDir: workflows });
}

function findingsFor(id, results) {
  const hit = results.find((r) => r.id === id);
  assert.ok(hit, `${id} was not found among the scanned jobs`);
  return hit.findings;
}

// scanWith — the real workflow directory with ONE file replaced by a doctored
// copy. The Justfile and the scripts still resolve against the real tree, so the
// resolution under test is the real resolution.
function scanWith(file, edit) {
  const dir = mkdtempSync(join(tmpdir(), 'image-auth-gate-'));
  const original = readFileSync(join(workflows, file), 'utf8');
  const doctored = edit(original);
  assert.notEqual(doctored, original, 'the negative control did not change the workflow text');
  writeFileSync(join(dir, file), doctored);
  return scan({ root: repoRoot, workflowDir: dir });
}

// Removes EVERY `- name: <title>` step, each up to the next step at its own
// indentation. Deleting the login is the regression this gate exists to catch,
// so it is also the way to prove the gate catches it.
//
// Every occurrence, not the first: `Log in to Docker Hub` is the title of a step
// in six jobs of e2e.yml, so removing one match doctors whichever job happens to
// come first in the file — which is not the job under test, and leaves the
// control asserting that an untouched job is still clean.
function withoutStep(title) {
  return (text) => {
    const lines = text.split('\n');
    const out = [];
    let i = 0;
    let removed = 0;
    while (i < lines.length) {
      if (lines[i].trim() !== `- name: ${title}`) {
        out.push(lines[i]);
        i++;
        continue;
      }
      const indent = lines[i].length - lines[i].trimStart().length;
      removed++;
      i++;
      while (i < lines.length) {
        const l = lines[i];
        if (l.trim().startsWith('- ') && l.length - l.trimStart().length === indent) break;
        i++;
      }
    }
    assert.notEqual(removed, 0, `no step titled "${title}" to remove`);
    return out.join('\n');
  };
}

// ---------------------------------------------------------------------------
// The tree, as it stands.
// ---------------------------------------------------------------------------

test('every image-acquiring job in the tree authenticates', () => {
  const results = realScan();
  const violations = results.flatMap((r) => violationsFor(r.id, r.findings));
  assert.deepEqual(violations, []);
  const unresolved = results.flatMap((r) => r.findings.unresolved.map((u) => `${r.id}: ${u}`));
  assert.deepEqual(unresolved, [], 'a reference the resolver cannot follow is not a clean job');
});

test('the scan finds acquiring jobs at all', () => {
  // Without this the suite above is satisfied by a resolver that reads nothing:
  // zero jobs produce zero violations.
  const acquiring = realScan().filter((r) => r.findings.acquisitions.length > 0);
  assert.ok(acquiring.length >= 10, `only ${acquiring.length} acquiring jobs found — the resolver is not reading the tree`);
});

// ---------------------------------------------------------------------------
// The discrimination that replaces an allow-list.
// ---------------------------------------------------------------------------

test('a unit test that names an image-pulling module is not an image acquisition', () => {
  // ci.yml's discipline lane runs `node --test compose-pull-images.test.mjs`.
  // A text search for the module name flags it; reading the command does not,
  // because `--test` hands the file to the node test runner, which imports the
  // module rather than running its CLI. That is the fact doing the work — no
  // job name appears anywhere in this gate.
  const results = realScan();
  const disciplineJobs = results.filter((r) => r.id.startsWith('ci.yml:'));
  assert.ok(disciplineJobs.length > 0, 'ci.yml produced no jobs');
  for (const job of disciplineJobs) {
    assert.deepEqual(
      job.findings.acquisitions,
      [],
      `${job.id} was read as acquiring an image: ${job.findings.acquisitions.join(', ')}`,
    );
  }
});

test('the shared policy is detected by a CALL, not by a mention of it', () => {
  // lib/registry.mjs prints the primitive's signature in its header comment and
  // declares it. Reading either as a call site made chart-ci.yml's
  // `chart-validate` — which imports only a rate-limit classifier out of that
  // module and never pulls — look like an anonymous puller.
  assert.equal(callsSharedPolicy('// pullImageWithRetry(image, options) acquires one image'), false);
  assert.equal(callsSharedPolicy('export function pullImageWithRetry(image, options = {}) {'), false);
  assert.equal(callsSharedPolicy('if (!pullImageWithRetry(ref, { backoffStepSeconds })) failed++;'), true);

  const chart = realScan().find((r) => r.id === 'chart-ci.yml:chart-validate');
  assert.ok(chart, 'chart-ci.yml:chart-validate was not scanned');
  assert.deepEqual(chart.findings.acquisitions, []);
});

// ---------------------------------------------------------------------------
// Resolution — the hops between a workflow step and the registry.
// ---------------------------------------------------------------------------

test('a ref is resolved through the job env and two Justfile hops', () => {
  // `startup-bench` names the image once in the job's `env:`, spends it as
  // `just _pull-retry "$CH_IMAGE"`, and the recipe forwards it to
  // `pull-images.mjs {{IMAGES}}`. Every hop has to hold for the Docker Hub rule
  // to have a ref to judge; a break anywhere reads as "ref unknown", which is
  // silent.
  const findings = findingsFor(specimenJob, realScan());
  assert.ok(
    [...findings.refs].some((r) => r.startsWith('clickhouse/clickhouse-server:')),
    `no ClickHouse ref resolved; got ${[...findings.refs].join(', ') || '(none)'}`,
  );
  assert.equal(findings.viaSharedPolicy, true);
});

test('a variadic Justfile parameter yields every ref it holds, not none', () => {
  // `_pull-retry {{CH_TEST_IMAGE}} {{CH_TEST_IMAGE_PRIOR}}` binds both refs to a
  // single variadic parameter, so the expanded token holds two refs separated by
  // a space — a shape that matches no image ref at all. Both used to vanish.
  const findings = findingsFor('schema-integration.yml:schema-ddl', realScan());
  const ch = [...findings.refs].filter((r) => r.startsWith('clickhouse/clickhouse-server:'));
  assert.ok(ch.length >= 2, `expected both ClickHouse refs, got ${ch.join(', ') || '(none)'}`);
});

test('a composite action contributes its own steps', () => {
  // `uses: ./.github/actions/setup-buildx` acquires the BuildKit bootstrap image
  // inside the action, where no `run:` in the calling workflow mentions it.
  const findings = findingsFor('release.yml:goreleaser', realScan());
  assert.equal(findings.viaSharedPolicy, true, 'the setup-buildx bootstrap pull was not seen');
});

// ---------------------------------------------------------------------------
// Negative controls — each rule, proved against the real workflow text.
// ---------------------------------------------------------------------------

test('R1 fires when an acquiring job has no login at all', () => {
  const results = scanWith('e2e.yml', (text) =>
    withoutStep('Log in to the image mirror (GHCR)')(withoutStep('Log in to Docker Hub')(text)),
  );
  const violations = violationsFor(specimenJob, findingsFor(specimenJob, results));
  assert.equal(violations.length, 1);
  assert.match(violations[0], /no registry login step — the pull is anonymous/);
});

test('R2 fires when an acquiring job has no Docker Hub login', () => {
  const results = scanWith('e2e.yml', withoutStep('Log in to Docker Hub'));
  const violations = violationsFor(specimenJob, findingsFor(specimenJob, results));
  assert.equal(violations.length, 1);
  assert.match(violations[0], /clickhouse\/clickhouse-server:[\d.]+/);
  assert.match(violations[0], /no Docker Hub login/);
});

test('R3 fires when a job pulling through the mirror has no ghcr.io login', () => {
  const results = scanWith('e2e.yml', withoutStep('Log in to the image mirror (GHCR)'));
  const violations = violationsFor(specimenJob, findingsFor(specimenJob, results));
  assert.equal(violations.length, 1);
  assert.match(violations[0], /no ghcr\.io login/);
});

test('removing the acquisition removes the requirement, not the other way round', () => {
  // The rules key off acquiring, so a job that stops pulling stops being
  // judged — otherwise every login step in the repo would be load-bearing
  // forever and the gate would fire on cleanups.
  assert.deepEqual(violationsFor('some:job', { acquisitions: [], logins: [], refs: new Set(), viaSharedPolicy: false, unresolved: [] }), []);
});

// ---------------------------------------------------------------------------
// R4 — a login step is necessary and NOT sufficient.
//
// Run 30701755787 (job 91375112426) logged in successfully:
//
//   13:50:32.0306552Z Logging into docker.io...
//   13:50:32.3891751Z Login Succeeded!
//
// and was refused as UNAUTHENTICATED one second later:
//
//   13:50:33.1136472Z  clickhouse Pulling
//   13:50:33.3467501Z  clickhouse Error toomanyrequests: You have reached your
//                                  unauthenticated pull rate limit.
//
// `docker compose`'s own pull path does not carry the credentials the CLI wrote.
// A gate that asked only "does this job have a login step?" would have passed
// that job — a hollow green on the exact failure it is named after. So R4 asks
// whether the acquisition actually RUNS on the mirror-first path.
// ---------------------------------------------------------------------------

// Replaces the harness call with a bare `compose up`, which is the pre-#1572
// shape of the job that produced the run above: login, then straight to compose.
const withoutTheComposePrePull = (text) =>
  text.replace(
    'run: ./compatibility/prometheus/scripts/run-compatibility.sh',
    'run: docker compose -f docker-compose.yml up -d --build --wait clickhouse prometheus cerberus',
  );

test('R4 fires when a compose stack is materialised without a mirror-first pre-pull', () => {
  const results = scanWith('compatibility.yml', withoutTheComposePrePull);
  const v = findingsFor('compatibility.yml:prometheus', results);
  const violations = violationsFor('compatibility.yml:prometheus', v);
  assert.ok(
    violations.some((m) => m.includes('without pre-pulling it through the shared mirror-first')),
    `expected an R4 pre-pull violation, got ${JSON.stringify(violations)}`,
  );
  // And specifically NOT because the job is unauthenticated — it is. That is the
  // whole point: the login rules are all satisfied here.
  assert.ok(
    !violations.some((m) => m.includes('no Docker Hub login') || m.includes('no registry login')),
    'the doctored job still has both logins, so no login rule should fire',
  );
});

test('R4 fires when a job acquires images off the shared policy entirely', () => {
  const results = scanWith('compatibility.yml', withoutTheComposePrePull);
  const violations = violationsFor(
    'compatibility.yml:prometheus',
    findingsFor('compatibility.yml:prometheus', results),
  );
  assert.ok(
    violations.some((m) => m.includes('without going through the shared mirror-first policy')),
    `expected an R4 off-policy violation, got ${JSON.stringify(violations)}`,
  );
});

test('the job that writes the mirror is not required to read through it', () => {
  // Circular otherwise: `imagetools create` POPULATES the mirror, so it cannot
  // be served by it. The carve-out is the direction of the copy, not the job's
  // name — which is what keeps it from becoming an exemption.
  const f = findingsFor('mirror-images.yml:mirror', realScan());
  assert.equal(f.viaSharedPolicy, false, 'the mirroring job does not pull through the mirror');
  assert.ok(f.mirrorWrites > 0, 'its acquisitions are writes into the mirror');
  assert.deepEqual(violationsFor('mirror-images.yml:mirror', f), []);

  // But the carve-out is exactly as wide as the writes: give it one read that
  // is not a write and R4 fires, so this cannot quietly excuse a real pull.
  const withARead = { ...f, acquisitions: [...f.acquisitions, 'docker pull'] };
  assert.ok(
    violationsFor('mirror-images.yml:mirror', withARead).some((m) =>
      m.includes('without going through the shared mirror-first policy'),
    ),
  );
});

// ---------------------------------------------------------------------------
// Logging in without `docker/login-action`.
//
// "Logs in" must not be read as "uses docker/login-action". PR #1586 replaces
// both of `mirror-images.yml`'s login steps with `run: node
// .github/scripts/registry-login.mjs`, because the action has no retry input and
// that step is where the mirror workflow died on its first real run. A detector
// that recognises only the action would report the mirroring job — the one job
// in the repo whose entire purpose is reading Docker Hub — as an anonymous
// puller the moment that PR lands, and the fix for a gate that fires wrongly is
// always an exemption, which is the outcome this module exists to prevent.
//
// The two tests below pin the shape rather than the filename, on the argv the
// script actually builds. `registry-login.mjs` does not exist on this branch, so
// they are written against its source text; they hold for any module that logs
// in the same way.
// ---------------------------------------------------------------------------

// The argv-building shape from #1586's registry-login.mjs, verbatim: the host is
// appended conditionally, which is precisely why it cannot be an inline literal
// array and why reading only inline arrays would miss the login entirely.
const conditionalLoginSource = `
import { capture } from './lib/gh.mjs';
function main() {
  const registry = (process.env.REGISTRY ?? '').trim();
  const username = requiredEnv('USERNAME');
  const args = ['login', '--username', username, '--password-stdin'];
  if (registry !== '') args.push(registry);
  const res = capture('docker', args, { input: password });
}
`;

test('an argv built up in a variable is still a spawned command', () => {
  assert.deepEqual(spawnedCommands(conditionalLoginSource), [
    ['docker', 'login', '--username', '--password-stdin', '${REGISTRY}'],
  ]);
});

test('a login step names the registry it logs in to, or Docker Hub when it names none', () => {
  const [[, ...argv]] = spawnedCommands(conditionalLoginSource);
  const after = argv.slice(argv.indexOf('login') + 1);

  // With REGISTRY set, this is the mirror login and satisfies R3.
  assert.equal(loginRegistry(after, { REGISTRY: 'ghcr.io' }), 'ghcr.io');

  // Unset is not unknown: `docker login` with no host argument goes to Docker
  // Hub, which is the contract the script documents. Reading it as "some other
  // registry" would leave the Docker Hub half of R2 unsatisfied for a job that
  // is genuinely logged in to Docker Hub.
  assert.equal(loginRegistry(after, {}), '');

  // The flag arity is the substance: `--username` takes a value, so "the first
  // token that is not a flag" is the USERNAME, and a gate that read it that way
  // would record a login to a registry called `tsouza` and fail R2 on a job
  // that is correctly authenticated.
  assert.equal(loginRegistry(['--username', 'tsouza', '--password-stdin'], {}), '');
  assert.equal(loginRegistry(['-u', 'tsouza', '-p', 'secret', 'ghcr.io'], {}), 'ghcr.io');

  // A GitHub expression is genuinely unknowable, and guessing there would
  // invent a login that may not exist.
  assert.notEqual(loginRegistry(['${{ inputs.registry }}'], {}), '');
});

test('the job that mirrors the inventory acquires the images it mirrors', () => {
  // `docker buildx imagetools create` puts nothing in the local daemon, so it
  // does not look like an acquisition — but the manifest READ spends the source
  // registry's quota exactly as a pull does, and this is the only way the
  // mirroring job touches an image at all. Excusing it would exempt, by
  // accident, the one job that exists to read Docker Hub.
  const f = findingsFor('mirror-images.yml:mirror', realScan());
  assert.ok(
    f.acquisitions.some((a) => a.includes('imagetools')),
    `expected an imagetools acquisition, got ${JSON.stringify(f.acquisitions)}`,
  );
  // And it knows WHICH images: the inventory lives in lib/mirror.mjs and reaches
  // the command through a loop variable, so without the imported-binding hop R2
  // would have no ref to check and would pass on any login at all.
  assert.ok(
    [...f.refs].some((r) => isDockerHubRef(r)),
    `expected the mirrored inventory's Docker Hub refs, got ${JSON.stringify([...f.refs])}`,
  );
});

// ---------------------------------------------------------------------------
// The primitives.
// ---------------------------------------------------------------------------

test('a ref with no registry host is Docker Hub', () => {
  assert.equal(isDockerHubRef('golang:1.26'), true);
  assert.equal(isDockerHubRef('clickhouse/clickhouse-server:26.5'), true);
  assert.equal(isDockerHubRef('ghcr.io/tsouza/cerberus:1.13.2'), false);
  assert.equal(isDockerHubRef('localhost:5000/x'), false);
  assert.equal(isDockerHubRef('registry.k8s.io/pause:3.9'), false);
});

test('a compose file argument is not mistaken for an image ref', () => {
  // `compose-pull-images.mjs docker-compose.yml` and
  // `pull-images.mjs golang:1.26` are the same argv shape; only the token tells
  // them apart, and a `.yml` read as an image would be judged as Docker Hub.
  assert.equal(looksLikeImageRef('docker-compose.yml'), false);
  assert.equal(looksLikeImageRef('golang:1.26'), true);
  assert.equal(looksLikeImageRef('clickhouse/clickhouse-server:26.5-alpine'), true);
  assert.equal(looksLikeImageRef('--quiet'), false);
  assert.equal(looksLikeImageRef('$CH_IMAGE'), false);
});

test('an unresolvable variable yields null rather than a wrong ref', () => {
  assert.equal(expandVars('$CH_IMAGE', { CH_IMAGE: 'golang:1.26' }), 'golang:1.26');
  assert.equal(expandVars('{{K3S_IMAGE}}', { K3S_IMAGE: 'rancher/k3s:v1' }), 'rancher/k3s:v1');
  assert.equal(expandVars('$MISSING', {}), null);
  // A GitHub expression is never statically known; guessing at one is how a
  // gate invents a violation.
  assert.equal(expandVars('${{ inputs.cerberus_image }}', {}), null);
});

test('a command line splits on every separator that starts a new command', () => {
  assert.deepEqual(splitCommands('docker pull a && docker run b'), ['docker pull a', 'docker run b']);
  assert.deepEqual(splitCommands('# just a comment\ndocker pull a'), ['docker pull a']);
  assert.deepEqual(splitCommands('docker \\\n  pull a'), ['docker    pull a']);
  assert.deepEqual(tokenise('just _pull-retry "$CH_IMAGE"'), ['just', '_pull-retry', '$CH_IMAGE']);
});

test('a workflow parses into its jobs and their steps', () => {
  const { jobs } = parseWorkflow(readFileSync(join(workflows, 'e2e.yml'), 'utf8'));
  const bench = jobs.find((j) => j.name === 'startup-bench');
  assert.ok(bench, 'startup-bench did not parse out of e2e.yml');
  assert.equal(bench.env.CH_IMAGE?.startsWith('clickhouse/clickhouse-server:'), true);
  assert.ok(bench.steps.length > 5, `only ${bench.steps.length} steps parsed`);
});

test('a comment between two steps does not leak into the previous step body', () => {
  // A YAML comment sits at the OUTER indentation, so a block-scalar reader that
  // emits blank lines on sight splices the next step's prose into the previous
  // step's script — and prose naming `just e2e-up` in backticks then resolves as
  // a recipe call with a stray backtick attached.
  const results = realScan();
  const unresolved = results.flatMap((r) => r.findings.unresolved);
  assert.deepEqual(unresolved.filter((u) => u.includes('`')), unresolved.filter(() => false));
});
