// Tests for the `docker login` retry classifier.
//
// What is actually at stake here is the ORDER of the questions, not the
// patterns. Three of the four classes can match the same output — Docker Hub
// reports a quota refusal through BuildKit as `unexpected status from HEAD
// request … 429`, which also reads as a transport fault, and its anonymous-limit
// body says "unauthenticated", one letter-shuffle away from the credential
// list. Get the order wrong and each mistake is silent in a different way: a
// quota refusal retried four more times spends the quota it was refused for, a
// credential rejection retried turns a clear error into a slow one, and a
// handshake blip classed as fatal is the exact failure (run 30724446834) that
// shipped the image mirror inert.
//
// So every case below is written as a precedence question, on the real output
// strings the toolchain emits.

import assert from 'node:assert/strict';
import test from 'node:test';

import {
  classifyLoginFailure,
  downgradeToMirrorOnly,
  isDockerHubLogin,
  onTransportFaultFail,
  onTransportFaultMirrorOnly,
} from './registry-login.mjs';

// The output that killed the mirror workflow's first real run: the /v2/
// handshake timed out before anything was pulled.
const handshakeTimeout =
  'Error response from daemon: Get "https://registry-1.docker.io/v2/": context deadline exceeded';

test('a handshake that timed out is transient, so it is retried', () => {
  assert.equal(classifyLoginFailure(handshakeTimeout), 'transient');
});

test('a quota refusal is never transient, even when it also names a transport fault', () => {
  // BuildKit's shape: the 429 arrives INSIDE a phrase that is on the transient
  // list. Asking the transient question first would retry it, and every extra
  // attempt spends more of the window that is already exhausted.
  const buildkit429 =
    'unexpected status from HEAD request to https://registry-1.docker.io/v2/: 429 Too Many Requests';
  assert.equal(classifyLoginFailure(buildkit429), 'rate-limit');

  // Docker Hub's own prose body. "unauthenticated" must not be read as a
  // credential rejection: the credential is fine, the counter is not.
  const hubBody =
    'toomanyrequests: You have reached your unauthenticated pull rate limit. ' +
    'https://www.docker.com/increase-rate-limit';
  assert.equal(classifyLoginFailure(hubBody), 'rate-limit');
});

test('a rejected credential is not retried, even alongside a transport fault', () => {
  assert.equal(
    classifyLoginFailure('Error response from daemon: Get "https://registry-1.docker.io/v2/": unauthorized: incorrect username or password'),
    'credential',
  );
  assert.equal(classifyLoginFailure('denied: requested access to the resource is denied'), 'credential');
  assert.equal(classifyLoginFailure('unauthorized: authentication required'), 'credential');

  // A registry that answered 401 did reach us, so the transport is not the
  // problem no matter what else the output mentions. Reordering these two
  // branches would spend five attempts on a secret that is simply wrong.
  assert.equal(
    classifyLoginFailure(`401 Unauthorized\n${handshakeTimeout}`),
    'credential',
    'a credential rejection must win over a transport fault named in the same output',
  );
});

test('an error off every list is fatal rather than retried', () => {
  // Retrying an unrecognised failure is how a real, reproducible error gets
  // reported five times and diagnosed as flake.
  assert.equal(classifyLoginFailure('docker: command not found'), 'fatal');
  assert.equal(classifyLoginFailure(''), 'fatal');
  assert.equal(classifyLoginFailure(undefined), 'fatal');
});

// ---------------------------------------------------------------------------
// The downgrade to mirror-only (issue #1933).
//
// A spent transport-fault budget on DOCKER HUB is the one failure the GHCR
// mirror was built to survive, and until #1933 it was the one that guaranteed
// the lane could not run: `compatibility/promql-surface` died on run
// 31148765992 without issuing a query, while every image it needed sat in a
// mirror nothing had contacted. So the verdict that used to end the job now
// downgrades it — for that verdict, on that registry, unless the step opted out.
//
// Each of the three conditions is tested by DENYING it, because the risk here is
// not a downgrade that fails to happen (that is the old, visible behaviour) but
// one that happens where it must not: a job that carries on without the registry
// it was going to publish to, or one that "falls back" to the registry it just
// failed to reach.
// ---------------------------------------------------------------------------

const hubTransient = { registry: '', verdict: 'transient', onTransportFault: onTransportFaultMirrorOnly };

test('an unreachable Docker Hub downgrades the job instead of killing it', () => {
  assert.equal(downgradeToMirrorOnly(hubTransient), true);
  // The default is the downgrade: a consuming job says nothing at all, which is
  // what keeps a NEW image-acquiring lane from having to know the rule exists.
  assert.equal(downgradeToMirrorOnly({ ...hubTransient, onTransportFault: '' }), true);
});

test('only a transport fault downgrades — a quota or a bad credential still fails', () => {
  // A mirror cannot decrement a rolling quota window and cannot fix a wrong
  // password. Downgrading on either would run the job in a degraded mode for a
  // reason the degradation does not address, and hide a plain misconfiguration
  // behind an image-not-mirrored error five steps later.
  for (const verdict of ['rate-limit', 'credential', 'fatal']) {
    assert.equal(downgradeToMirrorOnly({ ...hubTransient, verdict }), false, `${verdict} must not downgrade`);
  }
});

test('only Docker Hub downgrades — a mirror that is down cannot stand in for itself', () => {
  // "Acquire everything from the GHCR mirror" is not a recovery from "ghcr.io is
  // unreachable"; it is the same failure with a longer path to it.
  assert.equal(downgradeToMirrorOnly({ ...hubTransient, registry: 'ghcr.io' }), false);
  // Every spelling `docker login` accepts for Docker Hub still downgrades, so a
  // step that names the host does not silently lose the behaviour.
  for (const spelling of ['docker.io', 'index.docker.io', 'registry-1.docker.io', 'DOCKER.IO', ' ']) {
    assert.equal(isDockerHubLogin(spelling), true, `${JSON.stringify(spelling)} is Docker Hub`);
    assert.equal(downgradeToMirrorOnly({ ...hubTransient, registry: spelling }), true);
  }
  assert.equal(isDockerHubLogin('ghcr.io'), false);
});

test('a step that publishes to Docker Hub opts out and still fails', () => {
  // `release.yml` pushes half of an atomic dual-registry release to Docker Hub
  // and `mirror-images.yml` READS the inventory from it. For both, Docker Hub is
  // the thing itself rather than a fallback, so continuing without it would
  // report success over work that did not happen.
  assert.equal(downgradeToMirrorOnly({ ...hubTransient, onTransportFault: onTransportFaultFail }), false);
});
