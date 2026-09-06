// e2e-bwc-verify-mode-toggle.mjs — mode-toggle migration-safety assertion for
// the bundled-ClickHouse ("bwc") e2e lane (cerberus issue #3082). Run by
// `just e2e-bwc-verify-mode-toggle` AFTER `just e2e-bwc-toggle-mode` has
// helm-upgraded an ALREADY-POPULATED bwc_object_store cluster into hot-cold
// (bwc_hot_cold) WITHOUT pinning `hotVolume.enabled: false` — the exact
// real-world hazard docs/helm-clickhouse.md's "Upgrading into the hot/cold
// default" section describes.
//
// This proves the object-store -> hot-cold direction of cerberus issue
// #3075/#3076's per-mode storage-policy names (bwc_object_store /
// bwc_hot_only / bwc_hot_cold) against a real, already-populated cluster —
// the one direction the hot/cold-by-default upgrade hazard actually
// exercises. That the three names never collide with each other in the
// first place (so no OTHER pairwise toggle could silently succeed) is a
// separate, render-level claim already covered by chart-render-assert.mjs's
// per-mode `<bwc_*>` literal checks (run in the chart-validate job), not
// re-proven here.
//
// PROVES the failure is the SPECIFIC "unknown storage policy" class
// ClickHouse itself raises at startup (error code 478, UNKNOWN_POLICY) when
// a table's persisted `storage_policy` setting no longer resolves against the
// server's current `storage_configuration` — never merely:
//   - a generic non-ready / crash-looping pod (that could be any bug),
//   - a plain non-zero `helm upgrade` exit or a `--wait` timeout,
//   - or (the one failure mode this whole mechanism exists to prevent) a
//     SILENT success that quietly starts serving a cluster whose parts don't
//     actually live on the policy its metadata now claims.
// A test that only checked "the pod never became ready" would not actually
// prove #3075/#3076's claim — it would pass identically for an unrelated
// crash. So this script requires the exact ClickHouse exception text AND its
// error code before calling the hazard confirmed.
//
// Requires clickhouse.bundled.configOverrides to have turned on
// `<logger><console>1</console></logger>`
// (cerberus-values-bwc-mode-toggle.yaml) — the ONLY reason this exception is
// visible to `kubectl logs` at all: ClickHouse's shipped default only ever
// writes it to a log FILE inside the container.
//
// Env contract:
//   NAMESPACE        k8s namespace                            (default cerberus)
//   POLL_SECONDS     total polling budget for the failure      (default 180)
//   EXPECTED_POLICY  the OLD policy name the upgrade must reject
//                    (default bwc_object_store, matching the object-storage
//                    baseline `just e2e-bwc-toggle-mode` upgrades FROM)
//
// Exit 0 = the exact UNKNOWN_POLICY failure was observed and the pod never
// became Ready; 1 = anything else (with ::error:: annotation).
//
// THE RACE THIS SCRIPT MUST NOT LOSE (cerberus issue #3109's second symptom):
// `just e2e-bwc-toggle-mode`'s `helm upgrade` call returns as soon as the API
// server has accepted the new manifests — it does NOT wait for the
// StatefulSet controller to notice the changed `checksum/config` pod-template
// annotation and actually replace `cerberus-clickhouse-0` (that replacement
// alone — graceful shutdown of the OLD (still-healthy) process, pod
// termination, a brand-new Pod object, container start — reliably takes
// several seconds on a real cluster). This script starts polling within
// milliseconds of that `helm upgrade` returning, so its very first
// `podReady` read is virtually guaranteed to observe the PRE-upgrade pod —
// which has been happily Ready since the object-storage install — long
// before the controller has even begun rolling it. Reading readiness before
// the rollout has started is not evidence of anything: it was true before
// the toggle and says nothing about the toggle's outcome. So this script
// pins the pod's `metadata.uid` at the moment polling begins and treats
// `podReady` as meaningful ONLY once a pod with a DIFFERENT uid — i.e. one
// the controller actually created IN RESPONSE to the mode-toggling upgrade —
// exists. Confirmed live: this exact race is why the leg has failed on
// EVERY run since it first reached CI (dispatch run 33983571851, the very
// first execution right after cerberus issue #3082/#3085 merged) — it is
// not a regression in the storage-policy naming mechanism itself, which
// `helm template` against both scenarios' rendered storage.xml (and a
// bare-ClickHouse-26.3 repro of the same policy-rename shape) both confirm
// still throws UNKNOWN_POLICY exactly as designed.

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log, capture } from './lib/gh.mjs';
import { makeKubectl, clickhousePodName } from './lib/k8s.mjs';

const NS = process.env.NAMESPACE || 'cerberus';
const POLL_SECONDS = Number(process.env.POLL_SECONDS || '180');
const EXPECTED_POLICY = process.env.EXPECTED_POLICY || 'bwc_object_store';
// One iteration of the poll loop below sleeps this long between attempts —
// named so the "why 5s" isn't a bare literal at the call site (CLAUDE.md
// invariant 13).
const pollIntervalMs = 5000;

// Same namespaced kubectl runner + pod lookup e2e-bwc-verify-placement.mjs
// (and e2e-datashard-verify.mjs) use, factored into lib/k8s.mjs so no two
// copies drift apart.
const kubectl = makeKubectl(capture, NS);

// The exact class of failure #3075/#3076 promises: ClickHouse's own
// UNKNOWN_POLICY exception (error code 478) naming the policy a persisted
// table references that the CURRENT storage_configuration no longer defines.
// Both the human-readable class ("Unknown storage policy") AND the error
// code/name are required in the same match, so a coincidental substring in
// an unrelated log line can never satisfy this by accident.
const FAILURE_RE = /Unknown storage policy `([^`]+)`[\s\S]*?\(UNKNOWN_POLICY\)/;

// Scan both the CURRENT container's logs (whatever attempt is live or most
// recently terminated) and the PREVIOUS one, so this is robust to exactly
// where in a CrashLoopBackOff cycle the poll lands: the container that just
// logged the exception may already have exited (making it "previous") by the
// time this runs, or still be the "current" one.
function scanLogsForFailure(pod) {
  for (const extra of [[], ['--previous']]) {
    const res = kubectl(['logs', pod, '-c', 'clickhouse', '--tail=-1', ...extra]);
    if (res.status !== 0) continue; // container not started yet / no previous instance yet
    const m = FAILURE_RE.exec(res.stdout);
    if (m) return m;
  }
  return null;
}

function containerStatusField(pod, field) {
  const res = kubectl([
    'get', 'pod', pod,
    '-o', `jsonpath={.status.containerStatuses[?(@.name=="clickhouse")].${field}}`,
  ]);
  return res.stdout.trim();
}

function podReady(pod) {
  return containerStatusField(pod, 'ready') === 'true';
}

function restartCount(pod) {
  return Number(containerStatusField(pod, 'restartCount') || '0');
}

// The Pod object's own `metadata.uid` — stable for the lifetime of ONE Pod
// object, and guaranteed different on any replacement Pod the StatefulSet
// controller creates under the same name. Empty during the brief gap between
// the old Pod being deleted and the new one being admitted; callers treat
// that as "no pod to read yet" rather than a new/old determination.
function podUID(pod) {
  const res = kubectl(['get', 'pod', pod, '-o', 'jsonpath={.metadata.uid}']);
  return res.stdout.trim();
}

async function main() {
  const pod = clickhousePodName(kubectl, NS);
  const preToggleUID = podUID(pod);
  log(
    `mode-toggle verify: namespace=${NS} pod=${pod} pre-toggle-uid=${preToggleUID || '<none>'} ` +
      `expected-rejected-policy=${EXPECTED_POLICY} poll-budget=${POLL_SECONDS}s`,
  );

  const deadline = Date.now() + POLL_SECONDS * 1000;
  let match = null;
  let rolled = false; // has a pod DISTINCT from the pre-toggle one appeared yet?
  while (Date.now() < deadline) {
    const currentUID = podUID(pod);
    // Until the StatefulSet controller has actually replaced the pod, its
    // readiness describes the PRE-toggle install, not the toggle's outcome —
    // see the module doc's "THE RACE THIS SCRIPT MUST NOT LOSE" section.
    // currentUID is empty during the brief delete/recreate gap; that is
    // itself evidence a replacement is underway, so it also counts as
    // "rolled" once preToggleUID was non-empty.
    if (!rolled) {
      if (currentUID && currentUID === preToggleUID) {
        await sleep(pollIntervalMs);
        continue;
      }
      rolled = true;
      log(`mode-toggle verify: rollout detected (pod uid changed from ${preToggleUID || '<none>'} to ` +
        `${currentUID || '<none> (mid-recreate)'}) — readiness now proves something`);
    }
    if (currentUID && podReady(pod)) {
      error(
        `bundled ClickHouse pod ${pod} became Ready after the mode-toggling upgrade — the hazard did NOT ` +
          `reproduce (this would mean cerberus issue #3075/#3076's per-mode storage-policy names silently ` +
          'stopped protecting an already-populated cluster)',
      );
      process.exit(1);
    }
    match = scanLogsForFailure(pod);
    if (match) break;
    await sleep(pollIntervalMs);
  }

  if (!rolled) {
    error(
      `bundled ClickHouse pod ${pod} was NEVER replaced within ${POLL_SECONDS}s (uid stayed ${preToggleUID}) — ` +
        'the mode-toggling helm upgrade apparently never changed the pod template (checksum/config annotation), ' +
        'so this run proves nothing about the storage-policy hazard either way',
    );
    process.exit(1);
  }

  if (!match) {
    error(
      `bundled ClickHouse pod ${pod} never logged the expected "Unknown storage policy ... (UNKNOWN_POLICY)" ` +
        `exception within ${POLL_SECONDS}s (restartCount=${restartCount(pod)}, ready=${podReady(pod)}) — a ` +
        'non-ready or crash-looping pod alone does NOT prove the claim; the specific ClickHouse ' +
        'startup-validation failure must actually be observed',
    );
    process.exit(1);
  }

  const rejectedPolicy = match[1];
  if (rejectedPolicy !== EXPECTED_POLICY) {
    error(
      `ClickHouse rejected storage policy '${rejectedPolicy}', expected '${EXPECTED_POLICY}' — wrong policy ` +
        'name for this leg; investigate before trusting this as the mode-toggle hazard',
    );
    process.exit(1);
  }
  // Re-check readiness after the match: guards the (unexpected in practice,
  // but asserted anyway per "never call this a silent success") race where
  // the matched log line came from an earlier crash and a later, unrelated
  // attempt has since come up healthy.
  if (podReady(pod)) {
    error(
      `ClickHouse logged the UNKNOWN_POLICY failure but pod ${pod} is now Ready — the hazard did not actually ` +
        'block startup',
    );
    process.exit(1);
  }

  notice(
    'bundled ClickHouse correctly REFUSED the mode-toggling upgrade with its own startup validation: ' +
      `Unknown storage policy \`${rejectedPolicy}\` (UNKNOWN_POLICY) — restartCount=${restartCount(pod)}, pod ` +
      'never became Ready. Migration-safety mechanism (cerberus issue #3075/#3076) confirmed against a real ' +
      'ClickHouse.',
  );
}

await main();
