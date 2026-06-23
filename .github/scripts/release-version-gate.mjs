// release-version-gate.mjs — on a push to main (a merged release PR), decide
// whether the APP needs publishing: does Chart.yaml's `appVersion:` name a
// version that has NOT yet been released as a `v<appVersion>` git tag?
//
// This is the app-side twin of chart-publish.mjs's `version-gate` (which gates
// the chart on its OWN `version:` line vs the OCI registry). Together they make
// the publish-on-merge release pipeline idempotent: a merge that bumped neither
// line is a complete no-op (nothing publishes, no tag is cut), exactly the
// safety property a non-version PR (like the one introducing this script) needs.
//
// Why git tags, not the OCI/registry: the app release identity IS the `vX.Y.Z`
// git tag — goreleaser derives `{{ .Version }}` from it, and the immutable
// GitHub release is keyed on it. So "already released" == "the tag exists".
// (The chart's identity, by contrast, is the OCI artifact, so chart-publish.mjs
// probes the registry; the two gates intentionally use different oracles.)
//
// The gate is fail-safe by omission: if `v<appVersion>` already exists we set
// publish=false. We only set publish=true for a genuinely new, un-tagged
// appVersion. A prerelease appVersion (e.g. 1.5.0-rc.1) is handled the same way
// — the tag is `v1.5.0-rc.1` and the existence check is identical.
//
// Env contract (the single source of truth):
//   CHART_DIR      path to the chart dir (default: deploy/helm/cerberus).
//                  Chart.yaml's `appVersion:` line is the app version.
//   GITHUB_OUTPUT  (runner-provided) step-output sink. Writes:
//                    publish=true|false  — does the app need a new release?
//                    version=<appVersion>      — the bare appVersion (no `v`)
//                    tag=v<appVersion>         — the git tag to create/publish
//
// Subcommand (argv[2]):
//   app-version-gate   run the gate (default if omitted)
//
// argv `--self-test` runs the in-process assertion suite and exits.
//
// Imports only node: builtins. Run with `node .github/scripts/release-version-gate.mjs app-version-gate`.

import { readFileSync, appendFileSync } from 'node:fs';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import process from 'node:process';

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus';

function ghError(msg) {
  process.stdout.write(`::error::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function ghNotice(msg) {
  process.stdout.write(`::notice::${String(msg).replace(/\r?\n/g, '%0A')}\n`);
}

function setOutput(name, value) {
  const out = process.env.GITHUB_OUTPUT;
  if (out) appendFileSync(out, `${name}=${value}\n`);
  else process.stdout.write(`[output] ${name}=${value}\n`);
}

// ---------------------------------------------------------------------------
// pure helpers (exported for the self-test — no I/O, no git, no process.exit)
// ---------------------------------------------------------------------------

// parseAppVersion — pull the top-level `appVersion:` from a Chart.yaml body.
// Quoted ("1.4.0") or bare; SemVer-shaped including a prerelease/build suffix.
// Throws on a missing / malformed field so the gate fails loud rather than
// silently publishing the wrong version.
export function parseAppVersion(chartYaml) {
  const m = chartYaml.match(/^appVersion:\s*["']?([^"'\s]+)["']?\s*$/m);
  if (!m) {
    throw new Error('could not find a top-level appVersion: in Chart.yaml');
  }
  return m[1];
}

// decide — the pure gate. Given the chart's appVersion and the set of existing
// `v*` git tags, return { publish, version, tag }. publish is true ONLY when
// the `v<appVersion>` tag does not already exist. Pure: same inputs, same
// output, no side effects — so the self-test pins the exact boundary.
export function decide(appVersion, existingTags) {
  const tag = `v${appVersion}`;
  const exists = existingTags.includes(tag);
  return { publish: !exists, version: appVersion, tag };
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

// listVTags — every `v*` git tag in the repo. The release job checks out with
// fetch-depth: 0 so the tag list is complete. A git failure is fatal (we must
// not guess that a tag is absent and wrongly publish).
function listVTags() {
  const r = spawnSync('git', ['tag', '-l', 'v*'], { encoding: 'utf8' });
  if (r.status !== 0) {
    ghError(`git tag -l v* failed (exit ${r.status}): ${(r.stderr || '').trim()}`);
    process.exit(1);
  }
  return (r.stdout || '')
    .split('\n')
    .map((s) => s.trim())
    .filter(Boolean);
}

function appVersionGate() {
  let appVersion;
  try {
    appVersion = parseAppVersion(readFileSync(join(CHART_DIR, 'Chart.yaml'), 'utf8'));
  } catch (e) {
    ghError(`${e.message} (in ${CHART_DIR}/Chart.yaml)`);
    process.exit(1);
  }
  const { publish, version, tag } = decide(appVersion, listVTags());
  if (publish) {
    ghNotice(`appVersion ${version} not yet released (no ${tag} tag) — will tag + publish the app`);
  } else {
    ghNotice(`appVersion ${version} already released at ${tag} — skipping app publish`);
  }
  setOutput('publish', String(publish));
  setOutput('version', version);
  setOutput('tag', tag);
  process.exit(0);
}

// ---------------------------------------------------------------------------
// self-test
// ---------------------------------------------------------------------------

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };

  // parseAppVersion: quoted, bare, prerelease, and the missing-field error.
  assert(parseAppVersion('appVersion: "1.4.0"\n') === '1.4.0', 'quoted appVersion');
  assert(parseAppVersion('appVersion: 1.4.0\n') === '1.4.0', 'bare appVersion');
  assert(parseAppVersion("appVersion: '1.5.0-rc.1'\n") === '1.5.0-rc.1', 'prerelease appVersion');
  assert(
    parseAppVersion('name: cerberus\nversion: 0.6.3\nappVersion: "2.0.0"\n') === '2.0.0',
    'appVersion among other keys',
  );
  let threw = false;
  try {
    parseAppVersion('name: cerberus\nversion: 0.6.3\n');
  } catch {
    threw = true;
  }
  assert(threw, 'missing appVersion must throw');

  // decide: unchanged appVersion already tagged -> no publish. THIS is the
  // safety case the version-introducing PR relies on: appVersion 1.4.0 with
  // v1.4.0 already present must NOT publish.
  let d = decide('1.4.0', ['v1.2.1', 'v1.3.0', 'v1.4.0']);
  assert(d.publish === false, 'already-tagged appVersion must not publish');
  assert(d.version === '1.4.0' && d.tag === 'v1.4.0', 'version/tag outputs on no-op');

  // decide: a newly-bumped appVersion with no matching tag -> publish.
  d = decide('1.5.0', ['v1.2.1', 'v1.3.0', 'v1.4.0']);
  assert(d.publish === true, 'new appVersion must publish');
  assert(d.tag === 'v1.5.0', 'tag derived as v<appVersion>');

  // decide: empty tag set (fresh repo) -> publish.
  d = decide('1.0.0', []);
  assert(d.publish === true, 'no tags yet -> publish');

  // decide: prerelease tag existence is exact (v1.5.0 present must NOT mask
  // the un-tagged prerelease v1.5.0-rc.1).
  d = decide('1.5.0-rc.1', ['v1.5.0']);
  assert(d.publish === true, 'prerelease not masked by the stable tag');
  d = decide('1.5.0-rc.1', ['v1.5.0-rc.1']);
  assert(d.publish === false, 'already-tagged prerelease must not republish');

  ghNotice('release-version-gate --self-test: all assertions passed');
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}

const cmd = process.argv[2];
switch (cmd) {
  case undefined:
  case 'app-version-gate':
    appVersionGate();
    break;
  default:
    ghError(`unknown subcommand: ${cmd} — expected app-version-gate (or no argument)`);
    process.exit(1);
}
