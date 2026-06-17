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

function versionGate() {
  const version = chartVersion();
  const ref = `${ociHostPath()}:${version}`;
  // `helm show chart oci://…:<version>` succeeds iff that chart version is
  // already published. A failure (most commonly "not found") means this is a
  // new version → publish.
  const r = run('helm', ['show', 'chart', `${OCI_REPO}/${CHART_NAME}`, '--version', version]);
  if (r.status === 0) {
    ghNotice(`chart ${CHART_NAME} ${version} already published at ${ref} — skipping publish`);
    setOutput('publish', 'false');
  } else {
    // Fail CLOSED: only a DEFINITIVE not-found means "new version → publish".
    // A transient/auth/registry error must NOT default to publish=true (that
    // risks republishing — or wrongly skipping — on a flaky registry). Abort.
    const stderr = `${r.stdout || ''}\n${r.stderr || ''}`;
    const notFound = /not found|manifest unknown|MANIFEST_UNKNOWN|NAME_UNKNOWN|no such|404|not.*exist/i.test(stderr);
    if (!notFound) {
      ghError(`version-gate could not determine whether ${ref} exists (exit ${r.status}); failing closed rather than risk a wrong publish. stderr:\n${stderr.trim()}`);
      process.exit(1);
    }
    ghNotice(`chart ${CHART_NAME} ${version} not yet published — will publish`);
    setOutput('publish', 'true');
  }
  setOutput('version', version);
  process.exit(0);
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
