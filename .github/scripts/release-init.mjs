// release-init.mjs — establish the immutable identity and time window before
// any release-candidate byte is constructed.
//
// Environment:
//   GITHUB_SHA          exact source commit checked out by the init job.
//   GITHUB_RUN_ID       positive workflow run id.
//   GITHUB_RUN_ATTEMPT  positive workflow attempt.
//   GITHUB_EVENT_NAME   push or workflow_dispatch.
//   GITHUB_REF          exact triggering branch ref.
//   RELEASE_REQUESTED_SHA
//                       workflow_dispatch only: full main SHA the dry run must
//                       qualify. It must equal GITHUB_SHA.
//   GITHUB_OUTPUT       receives source_sha, source_tree, started_at_ms,
//                       correlation_nonce, run_id, run_attempt, and dry_run.
//
// Node builtins only. A dirty/wrong checkout, malformed run identity, or an
// ambiguous dry-run flag fails before the candidate build can start.

import { randomBytes } from 'node:crypto';
import process from 'node:process';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { capture, error, notice, setOutput } from './lib/gh.mjs';

const SHA_RE = /^[0-9a-f]{40}$/;
const NONCE_RE = /^[0-9a-f]{64}$/;
const POSITIVE_RE = /^[1-9][0-9]*$/;
const CORRELATION_NONCE_BYTES = 32;
const MAIN_REF = 'refs/heads/main';
const MAINTENANCE_REF_RE = /^refs\/heads\/release\/\d+\.\d+\.x$/;

export class ReleaseInitError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ReleaseInitError';
  }
}

function positive(value, label) {
  const text = String(value ?? '').trim();
  if (!POSITIVE_RE.test(text)) throw new ReleaseInitError(`${label} must be a positive integer`);
  const number = Number(text);
  if (!Number.isSafeInteger(number)) throw new ReleaseInitError(`${label} exceeds safe integer range`);
  return number;
}

export function createReleaseIdentity({
  sha,
  tree,
  runID,
  runAttempt,
  dryRun,
  startedAtMs,
  correlationNonce,
}) {
  if (!SHA_RE.test(sha ?? '')) throw new ReleaseInitError('source SHA must be 40 lowercase hex');
  if (!SHA_RE.test(tree ?? '')) throw new ReleaseInitError('source tree must be 40 lowercase hex');
  const id = positive(runID, 'run id');
  const attempt = positive(runAttempt, 'run attempt');
  const started = positive(startedAtMs, 'qualification start');
  if (dryRun !== 'true' && dryRun !== 'false') {
    throw new ReleaseInitError('RELEASE_DRY_RUN must be the literal true or false');
  }
  if (!NONCE_RE.test(correlationNonce ?? '')) {
    throw new ReleaseInitError('correlation nonce must be 64 lowercase hex');
  }
  return Object.freeze({
    source_sha: sha,
    source_tree: tree,
    started_at_ms: String(started),
    correlation_nonce: correlationNonce,
    run_id: String(id),
    run_attempt: String(attempt),
    dry_run: dryRun,
  });
}

export function releaseEntryPolicy({ eventName, ref, sha, requestedSHA }) {
  if (!SHA_RE.test(sha ?? '')) {
    throw new ReleaseInitError('trigger source SHA must be 40 lowercase hex');
  }
  if (eventName === 'workflow_dispatch') {
    if (ref !== MAIN_REF) {
      throw new ReleaseInitError('manual release qualification must target main');
    }
    if (!SHA_RE.test(requestedSHA ?? '')) {
      throw new ReleaseInitError('manual release qualification requires an exact full main SHA');
    }
    if (requestedSHA !== sha) {
      throw new ReleaseInitError(
        `manual qualification requested ${requestedSHA}, but the selected main ref resolved to ${sha}`,
      );
    }
    return Object.freeze({ dryRun: 'true', requestedSHA });
  }
  if (eventName === 'push') {
    if (ref !== MAIN_REF && !MAINTENANCE_REF_RE.test(ref ?? '')) {
      throw new ReleaseInitError(`release push has unsupported branch ref ${JSON.stringify(ref)}`);
    }
    if (String(requestedSHA ?? '').trim() !== '') {
      throw new ReleaseInitError('push release must not accept a manual requested SHA');
    }
    return Object.freeze({ dryRun: 'false', requestedSHA: null });
  }
  throw new ReleaseInitError(`unsupported release event ${JSON.stringify(eventName)}`);
}

function git(root, args) {
  const result = capture('git', args, { cwd: root });
  if (result.status !== 0) {
    throw new ReleaseInitError(`git ${args.join(' ')} failed: ${result.stderr.trim()}`);
  }
  return result.stdout.trim();
}

export function checkoutIdentity(expectedSHA, cwd = process.cwd()) {
  const root = resolve(git(cwd, ['rev-parse', '--show-toplevel']));
  if (root !== resolve(cwd)) throw new ReleaseInitError('release init must run at repository root');
  const sha = git(root, ['rev-parse', '--verify', 'HEAD^{commit}']);
  if (sha !== expectedSHA) {
    throw new ReleaseInitError(`checkout HEAD is ${sha}, want ${expectedSHA}`);
  }
  const status = capture('git', ['status', '--porcelain=v1', '--untracked-files=all'], {
    cwd: root,
  });
  if (status.status !== 0 || status.stdout !== '') {
    throw new ReleaseInitError(
      status.status === 0 ? 'release init checkout is not clean' : `git status failed: ${status.stderr.trim()}`,
    );
  }
  return { sha, tree: git(root, ['rev-parse', '--verify', 'HEAD^{tree}']) };
}

export function main(env = process.env) {
  const expectedSHA = String(env.GITHUB_SHA ?? '').trim();
  if (!SHA_RE.test(expectedSHA)) throw new ReleaseInitError('GITHUB_SHA must be 40 lowercase hex');
  const entry = releaseEntryPolicy({
    eventName: String(env.GITHUB_EVENT_NAME ?? '').trim(),
    ref: String(env.GITHUB_REF ?? '').trim(),
    sha: expectedSHA,
    requestedSHA: String(env.RELEASE_REQUESTED_SHA ?? '').trim(),
  });
  const source = checkoutIdentity(expectedSHA);
  const identity = createReleaseIdentity({
    sha: source.sha,
    tree: source.tree,
    runID: env.GITHUB_RUN_ID,
    runAttempt: env.GITHUB_RUN_ATTEMPT,
    dryRun: entry.dryRun,
    startedAtMs: Date.now(),
    correlationNonce: randomBytes(CORRELATION_NONCE_BYTES).toString('hex'),
  });
  for (const [name, value] of Object.entries(identity)) setOutput(name, value);
  notice(
    `release qualification initialized for ${identity.source_sha.slice(0, 12)} ` +
      `tree ${identity.source_tree.slice(0, 12)} dry_run=${identity.dry_run}`,
  );
  return identity;
}

const invokedDirectly =
  process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (cause) {
    error(cause instanceof Error ? cause.message : String(cause));
    process.exit(1);
  }
}
