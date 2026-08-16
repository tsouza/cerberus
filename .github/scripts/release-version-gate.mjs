// release-version-gate.mjs — on a push to main (a merged release PR), decide
// whether the APP needs publishing: does Chart.yaml's `appVersion:` name a
// version that does not yet have a complete immutable public release?
//
// This is the app-side twin of chart-publish.mjs's `version-gate` (which gates
// the chart on its OWN `version:` line vs the OCI registry). Together they make
// the publish-on-merge release pipeline idempotent: a merge that bumped neither
// line is a complete no-op (nothing publishes, no tag is cut), exactly the
// safety property a non-version PR (like the one introducing this script) needs.
//
// A tag alone is not completion. Publication can fail after the tag is pushed
// but before the release becomes public. The public immutable release, with
// the exact five-asset roster and content digests, is the completion marker.
// A rerun therefore repairs a missing/draft/incomplete release even when its
// tag already exists; only the complete public state is a no-op.
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
//                    is_latest=true|false      — highest stable release line
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
const RELEASE_DIGEST_RE = /^sha256:[0-9a-f]{64}$/;

export function expectedReleaseAssetNames(appVersion) {
  return [
    `cerberus_${appVersion}_darwin_amd64.tar.gz`,
    `cerberus_${appVersion}_darwin_arm64.tar.gz`,
    `cerberus_${appVersion}_linux_amd64.tar.gz`,
    `cerberus_${appVersion}_linux_arm64.tar.gz`,
    'checksums.txt',
  ];
}

export function completedRelease(document, { appVersion, tag }) {
  if (document === null || typeof document !== 'object' || Array.isArray(document)) return false;
  if (
    document.draft !== false ||
    document.immutable !== true ||
    document.tag_name !== tag ||
    document.prerelease !== appVersion.includes('-') ||
    !Array.isArray(document.assets)
  ) {
    return false;
  }
  const expected = expectedReleaseAssetNames(appVersion);
  const actual = new Map();
  for (const asset of document.assets) {
    const name = String(asset?.name ?? '');
    if (name === '' || actual.has(name)) return false;
    if (!Number.isSafeInteger(asset.size) || asset.size <= 0) return false;
    if (!RELEASE_DIGEST_RE.test(asset.digest ?? '')) return false;
    actual.set(name, asset);
  }
  return actual.size === expected.length && expected.every((name) => actual.has(name));
}

export function decide(appVersion, existingTags, releaseComplete = undefined) {
  const tag = `v${appVersion}`;
  const exists = existingTags.includes(tag);
  const publish = releaseComplete === undefined ? !exists : !releaseComplete;
  return {
    publish,
    version: appVersion,
    tag,
    isLatest: isHighestStable(appVersion, existingTags),
    retry: exists && publish,
  };
}

function stableVersion(value) {
  const match = String(value).match(/^(\d+)\.(\d+)\.(\d+)$/);
  if (!match) return null;
  return match.slice(1).map(Number);
}

function compareStable(left, right) {
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] !== right[index]) return left[index] - right[index];
  }
  return 0;
}

export function isHighestStable(appVersion, existingTags) {
  const candidate = stableVersion(appVersion);
  if (!candidate) return false;
  return !existingTags
    .map((tag) => stableVersion(String(tag).replace(/^v/, '')))
    .filter(Boolean)
    .some((version) => compareStable(version, candidate) > 0);
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

function releaseForTag(tag) {
  const result = spawnSync(
    'gh',
    ['api', `repos/{owner}/{repo}/releases/tags/${tag}`],
    { encoding: 'utf8' },
  );
  if (result.status === 0) {
    try {
      return JSON.parse(result.stdout || '');
    } catch (cause) {
      throw new Error(`release ${tag} returned malformed JSON: ${cause.message}`);
    }
  }
  const output = `${result.stdout || ''}\n${result.stderr || ''}`;
  if (/(?:\b404\b|not found)/i.test(output)) return null;
  throw new Error(
    `could not determine whether release ${tag} is complete (exit ${result.status}): ` +
      output.trim(),
  );
}

function appVersionGate() {
  let appVersion;
  try {
    appVersion = parseAppVersion(readFileSync(join(CHART_DIR, 'Chart.yaml'), 'utf8'));
  } catch (e) {
    ghError(`${e.message} (in ${CHART_DIR}/Chart.yaml)`);
    process.exit(1);
  }
  const tags = listVTags();
  const tag = `v${appVersion}`;
  let releaseComplete = false;
  try {
    releaseComplete = tags.includes(tag) && completedRelease(releaseForTag(tag), { appVersion, tag });
  } catch (cause) {
    ghError(cause instanceof Error ? cause.message : String(cause));
    process.exit(1);
  }
  const { publish, version, isLatest, retry } = decide(appVersion, tags, releaseComplete);
  if (publish) {
    ghNotice(
      retry
        ? `appVersion ${version} has a tag but no complete immutable release — will repair publication`
        : `appVersion ${version} is unpublished — will qualify and publish the app`,
    );
  } else {
    ghNotice(`appVersion ${version} has a complete immutable release at ${tag} — skipping app publish`);
  }
  setOutput('publish', String(publish));
  setOutput('version', version);
  setOutput('tag', tag);
  setOutput('is_latest', String(isLatest));
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
  assert(d.isLatest === true, 'new highest stable appVersion owns shared latest resources');

  // decide: MAINTENANCE-LINE hotfix. appVersion 1.4.1 is OLDER than the latest
  // tagged release v1.5.0, but the v1.4.1 tag itself is absent -> publish. This
  // is the case a "newer-than-latest-tag" gate would WRONGLY skip; the gate is
  // tag-absent, not newest-wins, so a backport off release/1.4.x still ships.
  d = decide('1.4.1', ['v1.2.1', 'v1.3.0', 'v1.4.0', 'v1.5.0']);
  assert(d.publish === true, 'tag-absent maintenance hotfix older than latest must still publish');
  assert(d.tag === 'v1.4.1', 'maintenance tag derived as v<appVersion>');
  assert(d.isLatest === false, 'maintenance backport must not own shared latest resources');

  // decide: the same maintenance hotfix re-run AFTER its tag landed -> no-op
  // (idempotency on the maintenance path, identical to the main path).
  d = decide('1.4.1', ['v1.2.1', 'v1.3.0', 'v1.4.0', 'v1.4.1', 'v1.5.0']);
  assert(d.publish === false, 're-run of an already-tagged maintenance hotfix must not republish');

  // decide: empty tag set (fresh repo) -> publish.
  d = decide('1.0.0', []);
  assert(d.publish === true, 'no tags yet -> publish');

  // decide: prerelease tag existence is exact (v1.5.0 present must NOT mask
  // the un-tagged prerelease v1.5.0-rc.1).
  d = decide('1.5.0-rc.1', ['v1.5.0']);
  assert(d.publish === true, 'prerelease not masked by the stable tag');
  assert(d.isLatest === false, 'prerelease must not own shared latest resources');
  d = decide('1.5.0-rc.1', ['v1.5.0-rc.1']);
  assert(d.publish === false, 'already-tagged prerelease must not republish');

  const release = {
    draft: false,
    immutable: true,
    prerelease: false,
    tag_name: 'v1.5.0',
    assets: expectedReleaseAssetNames('1.5.0').map((name) => ({
      name,
      size: 1,
      digest: `sha256:${'a'.repeat(64)}`,
    })),
  };
  assert(
    completedRelease(release, { appVersion: '1.5.0', tag: 'v1.5.0' }),
    'exact immutable public release is complete',
  );
  d = decide('1.5.0', ['v1.5.0'], true);
  assert(d.publish === false && d.retry === false, 'complete public release is the no-op marker');
  for (const incomplete of [
    { ...release, draft: true },
    { ...release, immutable: false },
    { ...release, assets: release.assets.slice(1) },
    { ...release, assets: release.assets.map((asset, index) => index === 0 ? { ...asset, digest: null } : asset) },
  ]) {
    assert(
      !completedRelease(incomplete, { appVersion: '1.5.0', tag: 'v1.5.0' }),
      'draft, mutable, missing-asset, and digestless releases remain incomplete',
    );
  }
  d = decide('1.5.0', ['v1.5.0'], false);
  assert(d.publish === true && d.retry === true, 'an early tag cannot suppress publication repair');

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
