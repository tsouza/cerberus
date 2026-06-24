// chart-publish.mjs — non-trivial Helm-chart publish step logic, lifted out of
// the workflow YAML per the project's inline-YAML-discipline rule.
//
// One module, three subcommands (argv[2]):
//
//   version-gate
//     Compare the local Chart.yaml `version:` against the latest chart tag
//     already published to the OCI registry. Exits 0 and sets the
//     `publish=true|false` step output. When the local version already exists
//     remotely, publish=false (an app-only `v*` tag must NOT republish an
//     unchanged chart). Otherwise publish=true.
//
//   push
//     Run `helm push <tgz> oci://<repo>`, parse the pushed digest out of helm's
//     stderr, and set the `digest` + `ref` step outputs for the downstream
//     cosign-sign / attest steps.
//
//   ah-metadata
//     Idempotently push the artifacthub-repo.yml as the special Artifact Hub
//     OCI artifact via `oras`.
//
// argv `--self-test` runs the in-process assertion suite (pins the pure
// `notFoundError` / `decideFromProbe` boundary, incl. the maintenance-line
// "older than latest but absent → publish" case) and exits.
//
// Env contract (documented here, the single source of truth):
//   CHART_DIR        path to the chart dir (default: deploy/helm/cerberus)
//   OCI_REPO         oci://… target WITHOUT the chart name
//                    (e.g. oci://ghcr.io/tsouza/cerberus/charts)
//   CHART_NAME       chart name (default: cerberus)
//   CHART_TGZ        (push) path to the packaged .tgz
//   GITHUB_OUTPUT    (provided by the runner) step-output sink
//
// Imports only node: builtins. Run with `node .github/scripts/chart-publish.mjs <cmd>`.

import { spawnSync } from 'node:child_process';
import { readFileSync, appendFileSync } from 'node:fs';
import { join } from 'node:path';
import process from 'node:process';

const CHART_DIR = process.env.CHART_DIR || 'deploy/helm/cerberus';
const OCI_REPO = process.env.OCI_REPO || 'oci://ghcr.io/tsouza/cerberus/charts';
const CHART_NAME = process.env.CHART_NAME || 'cerberus';

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

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { encoding: 'utf8', ...opts });
  return r;
}

// Read the `version:` field from Chart.yaml without a YAML dependency: the
// field is a top-level `version: <semver>` line.
function chartVersion() {
  const raw = readFileSync(join(CHART_DIR, 'Chart.yaml'), 'utf8');
  const m = raw.match(/^version:\s*["']?([^"'\s]+)["']?\s*$/m);
  if (!m) {
    ghError(`could not find a top-level version: in ${CHART_DIR}/Chart.yaml`);
    process.exit(1);
  }
  return m[1];
}

// The OCI ref for a tag is `ghcr.io/.../charts/<name>:<version>`. helm/oras
// accept the bare host/path form (no oci:// scheme) for `show`/`manifest`.
function ociHostPath() {
  return `${OCI_REPO.replace(/^oci:\/\//, '')}/${CHART_NAME}`;
}

// notFoundError — does a `helm show chart` failure mean a DEFINITIVE "this
// version is not in the registry" (so it's a new version → publish), as opposed
// to a transient/auth/registry error (which must fail CLOSED rather than risk a
// wrong publish)? Pure string classification — exported so the self-test pins
// the exact boundary.
export function notFoundError(stderr) {
  return /not found|manifest unknown|MANIFEST_UNKNOWN|NAME_UNKNOWN|no such|404|not.*exist/i.test(stderr || '');
}

// decideFromProbe — the pure chart gate. Given the `helm show chart` probe
// result ({ status, stderr }), return { publish } | { error }. publish is true
// ONLY when the probe DEFINITIVELY reports the chart version is absent from the
// registry (status !== 0 AND a not-found-shaped stderr); false when the probe
// succeeds (version already published); { error } when the probe failed for an
// indeterminate reason (the caller must fail closed). This is the chart-side
// twin of the app gate's tag-absent rule: publish iff the target artifact does
// not already exist, which is idempotent and works for main, maintenance, and
// re-runs alike. Pure: no I/O, no process.exit — so the self-test is exact.
export function decideFromProbe({ status, stderr }) {
  if (status === 0) return { publish: false };
  if (notFoundError(stderr)) return { publish: true };
  return { error: true };
}

function versionGate() {
  const version = chartVersion();
  const ref = `${ociHostPath()}:${version}`;
  // `helm show chart oci://…:<version>` succeeds iff that chart version is
  // already published. A failure (most commonly "not found") means this is a
  // new version → publish. A maintenance hotfix whose chart version is OLDER
  // than the latest published chart still publishes here, because the gate keys
  // on absence-of-this-version, not newest-wins.
  const r = run('helm', ['show', 'chart', `${OCI_REPO}/${CHART_NAME}`, '--version', version]);
  const stderr = `${r.stdout || ''}\n${r.stderr || ''}`;
  const d = decideFromProbe({ status: r.status, stderr });
  if (d.error) {
    // Fail CLOSED: a transient/auth/registry error must NOT default to
    // publish=true (that risks republishing — or wrongly skipping — on a flaky
    // registry). Abort.
    ghError(`version-gate could not determine whether ${ref} exists (exit ${r.status}); failing closed rather than risk a wrong publish. stderr:\n${stderr.trim()}`);
    process.exit(1);
  }
  if (d.publish) {
    ghNotice(`chart ${CHART_NAME} ${version} not yet published — will publish`);
    setOutput('publish', 'true');
  } else {
    ghNotice(`chart ${CHART_NAME} ${version} already published at ${ref} — skipping publish`);
    setOutput('publish', 'false');
  }
  setOutput('version', version);
  process.exit(0);
}

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };

  // notFoundError: definitive not-found shapes classify as absent.
  assert(notFoundError('Error: chart not found'), 'helm not-found');
  assert(notFoundError('manifest unknown'), 'oci manifest unknown');
  assert(notFoundError('MANIFEST_UNKNOWN: ...'), 'oci MANIFEST_UNKNOWN code');
  assert(notFoundError('failed: 404 Not Found'), '404');
  assert(!notFoundError('Error: unauthorized: authentication required'), 'auth error is NOT not-found');
  assert(!notFoundError('connection reset by peer'), 'network error is NOT not-found');
  assert(!notFoundError(''), 'empty stderr is NOT not-found');

  // decideFromProbe: published version (probe ok) -> no publish.
  let d = decideFromProbe({ status: 0, stderr: '' });
  assert(d.publish === false, 'probe success means already published -> no publish');

  // decideFromProbe: definitive absent -> publish. THIS is the main, the
  // maintenance, and the fresh-chart case all at once — the version simply is
  // not in the registry yet.
  d = decideFromProbe({ status: 1, stderr: 'Error: cerberus:0.6.4 not found' });
  assert(d.publish === true, 'definitively absent version must publish');

  // decideFromProbe: MAINTENANCE chart hotfix. A chart version OLDER than the
  // latest published one (e.g. 0.6.3 backport while 0.7.0 is the newest) is
  // still absent at THIS exact version -> publish. The "newest-wins" trap a
  // greater-than comparison would fall into is structurally impossible here.
  d = decideFromProbe({ status: 1, stderr: 'manifest unknown' });
  assert(d.publish === true, 'tag-absent maintenance chart hotfix older than latest must still publish');

  // decideFromProbe: indeterminate probe error -> fail closed (no publish flag).
  d = decideFromProbe({ status: 1, stderr: 'unauthorized: authentication required' });
  assert(d.error === true && d.publish === undefined, 'indeterminate error must fail closed');

  ghNotice('chart-publish version-gate --self-test: all assertions passed');
}

function push() {
  const tgz = process.env.CHART_TGZ;
  if (!tgz) {
    ghError('CHART_TGZ is required for the push subcommand');
    process.exit(1);
  }
  const r = run('helm', ['push', tgz, OCI_REPO]);
  // helm prints the pushed Digest + Pushed ref to stderr.
  const combined = `${r.stdout || ''}\n${r.stderr || ''}`;
  process.stdout.write(combined + '\n');
  if (r.status !== 0) {
    ghError(`helm push failed (exit ${r.status})`);
    process.exit(1);
  }
  const digestMatch = combined.match(/Digest:\s*(sha256:[0-9a-f]+)/i);
  if (!digestMatch) {
    ghError('could not parse a sha256 Digest from helm push output');
    process.exit(1);
  }
  const digest = digestMatch[1];
  const ref = `${ociHostPath()}@${digest}`;
  setOutput('digest', digest);
  setOutput('ref', ref);
  ghNotice(`pushed ${ref}`);
  process.exit(0);
}

function ahMetadata() {
  const ref = `${ociHostPath()}:artifacthub.io`;
  const meta = join(CHART_DIR, 'artifacthub-repo.yml');
  const r = run('oras', [
    'push',
    ref,
    '--config',
    '/dev/null:application/vnd.cncf.artifacthub.config.v1+yaml',
    `${meta}:application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml`,
  ]);
  process.stdout.write(`${r.stdout || ''}\n${r.stderr || ''}\n`);
  if (r.status !== 0) {
    ghError(`oras push of Artifact Hub metadata failed (exit ${r.status})`);
    process.exit(1);
  }
  ghNotice(`pushed Artifact Hub metadata to ${ref}`);
  process.exit(0);
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}

const cmd = process.argv[2];
switch (cmd) {
  case 'version-gate':
    versionGate();
    break;
  case 'push':
    push();
    break;
  case 'ah-metadata':
    ahMetadata();
    break;
  default:
    ghError(`unknown subcommand: ${cmd || '(none)'} — expected version-gate | push | ah-metadata`);
    process.exit(1);
}
