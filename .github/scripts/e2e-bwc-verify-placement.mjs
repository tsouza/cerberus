// e2e-bwc-verify-placement.mjs — placement assertions for the bundled-ClickHouse
// ("bwc") e2e lane. Run by `just e2e-bwc-verify <scenario>` AFTER
// `just e2e-seed-rolling` has landed data into the bundled ClickHouse that the
// chart deployed with clickhouse.bundled.enabled=true.
//
// Three scenarios (cerberus issue #3075), selected by SCENARIO, share this ONE
// checker rather than three parallel ones — only the expected disk/policy
// shape and the aging step differ:
//
//   object-storage — the chart's PRE-#3075 mode (hotVolume.enabled=false),
//               unchanged. Backed by in-cluster MinIO.
//   hot-only  — clickhouse.bundled.hotVolume.enabled=true + objectStorage.
//               enabled=false. No object-store disk/secret/env at all.
//   hot-cold  — the chart's DEFAULT mode since #3075 (hotVolume.enabled=true,
//               objectStorage stays on). Fresh rows land on the local hot
//               disk; rows aged past schema.tierAfter are asserted to move
//               onto the cold (object-store) disk after forcing a merge.
//
// It PROVES the data tier actually lives where the scenario claims — not just
// that the chart rendered — by asserting independent facts against the live
// stack:
//
//   1. STORAGE POLICY STAMPED. Every MergeTree table cerberus auto-created in
//      the otel database carries SETTINGS ... storage_policy='<expected>'
//      (the chart's cerberus.bundled.apply -> schema.storagePolicy wiring).
//   2. PARTS ON THE EXPECTED DISK(S). system.parts.disk_name for the active
//      parts matches the scenario, never the local `default` disk.
//   3. hot-only ONLY: no object-store disk exists in system.disks at all, and
//      no object-store credential env reaches the ClickHouse container.
//   4. hot-cold ONLY: aging rows past schema.tierAfter and forcing a merge
//      moves their parts from the hot disk onto the cold (object-store) disk.
//   5. object-storage / hot-cold: the MinIO bucket has objects under the
//      disk's path after the seed — polled, because the part-object writes
//      lag the insert by a beat. Asserted NON-EMPTY (>0), never an exact
//      count: batch object GC is async and a stray marker object is benign.
//
// Env contract:
//   SCENARIO      object-storage | hot-only | hot-cold (default object-storage)
//   NAMESPACE     k8s namespace                 (default cerberus)
//   DATABASE      ClickHouse database           (default otel)
//   CH_USER       ClickHouse user               (default cerberus)
//   CH_PASSWORD   ClickHouse password           (default cerberus)
//   BUCKET        object-store bucket            (default cerberus-bwc)
//   STORAGE_POLICY expected policy name         (default: scenario-derived)
//   MC_IMAGE      minio/mc image for bucket ls  (default the pinned RELEASE)
//   POLL_SECONDS  bucket-non-empty poll budget  (default 30)
//   TIER_AFTER_SECONDS  hot-cold aging window, must match the scenario
//                       overlay's schema.tierAfter (default 30)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log, capture } from './lib/gh.mjs';
import { makeKubectl, clickhousePodName } from './lib/bwc-k8s.mjs';

const SCENARIO = process.env.SCENARIO || 'object-storage';
const NS = process.env.NAMESPACE || 'cerberus';
const DB = process.env.DATABASE || 'otel';
const CH_USER = process.env.CH_USER || 'cerberus';
const CH_PASSWORD = process.env.CH_PASSWORD || 'cerberus';
const BUCKET = process.env.BUCKET || 'cerberus-bwc';
const MC_IMAGE = process.env.MC_IMAGE || 'minio/mc:RELEASE.2025-08-13T08-35-41Z';
const POLL_SECONDS = Number(process.env.POLL_SECONDS || '30');
const TIER_AFTER_SECONDS = Number(process.env.TIER_AFTER_SECONDS || '30');

const SCENARIO_POLICY = {
  'object-storage': 'bwc_object_store',
  'hot-only': 'bwc_hot_only',
  'hot-cold': 'bwc_hot_cold',
};
if (!(SCENARIO in SCENARIO_POLICY)) {
  error(`unknown SCENARIO=${SCENARIO} (expected one of ${Object.keys(SCENARIO_POLICY).join(', ')})`);
  process.exit(1);
}
const STORAGE_POLICY = process.env.STORAGE_POLICY || SCENARIO_POLICY[SCENARIO];

// The disks the chart's storage XML can define, by scenario. A part's
// disk_name for the object-store tier is the CACHE disk (bwc_object_cache),
// never the raw bwc_object_disk. What matters everywhere is it is NEVER the
// built-in `default` (local) disk — that would mean the storage XML never
// applied at all.
const SCENARIO_DISKS = {
  'object-storage': new Set(['bwc_object_cache', 'bwc_object_disk']),
  'hot-only': new Set(['bwc_hot_disk']),
  // hot-cold: both are legitimate depending on whether a part has aged past
  // tierAfter yet. The pre-aging / post-aging assertions below narrow this
  // further to prove BOTH states actually occur.
  'hot-cold': new Set(['bwc_hot_disk', 'bwc_object_cache', 'bwc_object_disk']),
};
const EXPECTED_DISKS = SCENARIO_DISKS[SCENARIO];

// The OTel tables cerberus's own auto-create DDL manages and stamps with the
// storage policy. These names MUST match the ones the upstream
// clickhouseexporter writes — cerberus's DDL borrows the exporter's templates
// but fills in its own table-name defaults, so any drift between the two means
// cerberus reads a table the exporter never created and silently returns
// nothing. The storage-policy check is scoped to this canonical set; the DRIFT
// GUARD below then fails the run if the collector created any `otel_metrics_*`
// base table outside it. Physical placement of EVERY table's data is still
// asserted comprehensively via system.parts.
const CANONICAL_TABLES = new Set([
  'otel_logs',
  'otel_traces',
  'otel_traces_trace_id_ts',
  'otel_metrics_gauge',
  'otel_metrics_sum',
  'otel_metrics_histogram',
  'otel_metrics_exponential_histogram',
  'otel_metrics_summary',
]);

// Namespaced kubectl runner + pod lookup, shared with
// e2e-bwc-verify-mode-toggle.mjs via lib/bwc-k8s.mjs.
const kubectl = makeKubectl(capture, NS);
const clickhousePod = () => clickhousePodName(kubectl, NS);

// Run a ClickHouse query inside the CH pod as the dedicated cerberus user.
function chQuery(pod, sql) {
  const res = kubectl([
    'exec', pod, '--',
    'clickhouse-client',
    '--user', CH_USER,
    '--password', CH_PASSWORD,
    '--database', DB,
    '--query', sql,
  ]);
  if (res.status !== 0) {
    error(`clickhouse query failed: ${sql}\n${res.stderr.trim()}`);
    process.exit(1);
  }
  return res.stdout.trim();
}

function lines(s) {
  return s.split('\n').map((x) => x.trim()).filter(Boolean);
}

function activePartDisks(pod) {
  return new Set(lines(chQuery(pod, `SELECT DISTINCT disk_name FROM system.parts WHERE database='${DB}' AND active`)));
}

function main() {
  const pod = clickhousePod();
  log(`bwc placement verify: scenario=${SCENARIO} namespace=${NS} pod=${pod} db=${DB} bucket=${BUCKET} policy=${STORAGE_POLICY}`);
  let failures = 0;

  // ---- 0. system.disks matches the scenario's storage XML shape. ----
  const disks = lines(chQuery(pod, 'SELECT name FROM system.disks ORDER BY name'));
  log(`system.disks: ${disks.join(', ')}`);
  for (const need of EXPECTED_DISKS) {
    // hot-cold's EXPECTED_DISKS includes both tiers' disks even though only
    // one XML disk set is unconditionally required per SIGNAL; the storage
    // XML itself always renders both bwc_hot_disk AND bwc_object_cache in
    // hot-cold mode, so both must exist regardless of what has aged yet.
    if (!disks.includes(need)) {
      error(`disk ${need} missing from system.disks — storage XML did not render the expected ${SCENARIO} tier`);
      failures++;
    }
  }
  if (SCENARIO === 'hot-only') {
    for (const forbidden of ['bwc_object_disk', 'bwc_object_cache']) {
      if (disks.includes(forbidden)) {
        error(`hot-only mode: disk ${forbidden} rendered — object-store tier must NOT exist at all in hot-only mode`);
        failures++;
      }
    }
    const env = kubectl(['exec', pod, '--', 'env']).stdout;
    for (const credEnv of ['S3_ACCESS_KEY_ID', 'GCS_ACCESS_KEY_ID', 'AZURE_ACCOUNT_NAME']) {
      if (env.includes(credEnv)) {
        error(`hot-only mode: object-store credential env ${credEnv} present in the ClickHouse container — must be absent`);
        failures++;
      }
    }
    const secrets = kubectl(['get', 'secret', '-o', 'name']).stdout;
    if (/object-store/.test(secrets)) {
      error(`hot-only mode: an object-store Secret exists (${secrets.match(/\S*object-store\S*/)?.[0]}) — must not be rendered`);
      failures++;
    }
  }

  // ---- 1. storage_policy stamped on every MergeTree table. ----
  const tables = lines(chQuery(
    pod,
    `SELECT name FROM system.tables WHERE database='${DB}' AND engine LIKE '%MergeTree%' ORDER BY name`,
  ));
  if (tables.length === 0) {
    error(`no MergeTree tables found in database ${DB} — schema was not auto-created`);
    failures++;
  } else {
    log(`MergeTree tables in ${DB}: ${tables.join(', ')}`);
  }
  // clickhouse-client escapes the single quotes in SHOW CREATE output (\'), so
  // strip backslashes and tolerate optional spaces before matching.
  const needle = new RegExp(`storage_policy\\s*=\\s*'${STORAGE_POLICY}'`);
  const canonicalPresent = tables.filter((t) => CANONICAL_TABLES.has(t));
  let stampedCount = 0;
  for (const t of canonicalPresent) {
    const ddl = chQuery(pod, `SHOW CREATE TABLE ${DB}.\`${t}\``).replace(/\\/g, '');
    if (needle.test(ddl)) {
      stampedCount++;
    } else {
      error(`cerberus table ${DB}.${t} is NOT stamped with storage_policy='${STORAGE_POLICY}'`);
      failures++;
    }
  }
  const nonCanonical = tables.filter((t) => !CANONICAL_TABLES.has(t));
  if (nonCanonical.length > 0) {
    log(`non-cerberus MergeTree tables (placement asserted via system.parts, not SHOW CREATE): ${nonCanonical.join(', ')}`);
  }
  // DRIFT GUARD. The collector's clickhouseexporter owns the physical
  // OTel-metrics schema; cerberus must READ exactly the tables it creates. Any
  // `otel_metrics_*` base table the collector created that is NOT in the
  // canonical set cerberus reads means cerberus's schema-name defaults have
  // drifted from the exporter's — cerberus would query a table that does not
  // exist (or an empty one it created itself) and silently return nothing, so
  // the run must fail here rather than let that reach a datasource.
  const drift = nonCanonical.filter((t) => /^otel_metrics_[a-z_]+$/.test(t));
  if (drift.length > 0) {
    error(
      `schema drift: the OTel collector created metrics table(s) cerberus does not read: ${drift.join(', ')} — ` +
        `cerberus's schema-name defaults have drifted from the clickhouseexporter's. ` +
        `Reconcile internal/schema + internal/schema/ddl with the exporter's table names.`,
    );
    failures += drift.length;
  }
  if (canonicalPresent.length > 0 && stampedCount === canonicalPresent.length) {
    notice(`storage_policy='${STORAGE_POLICY}' stamped on all ${stampedCount} cerberus OTel tables`);
  }

  // ---- 2. active parts live on the expected disk(s), never `default`. ----
  // The seed's initial insert ran synchronously before this verify, so active
  // parts already exist.
  const diskNames = [...activePartDisks(pod)];
  log(`active part disk_name set (pre-aging): ${diskNames.join(', ') || '(none)'}`);
  if (diskNames.length === 0) {
    error('no active parts found — the seed did not write data, cannot prove placement');
    failures++;
  }
  const validDiskNames = SCENARIO === 'hot-cold' ? SCENARIO_DISKS['hot-cold'] : EXPECTED_DISKS;
  for (const d of diskNames) {
    if (d === 'default') {
      error('found active parts on the `default` (local) disk — data is NOT on the expected storage tier');
      failures++;
    } else if (!validDiskNames.has(d)) {
      error(`unexpected disk_name for active parts: ${d} (expected one of ${[...validDiskNames].join(', ')})`);
      failures++;
    }
  }
  if (SCENARIO === 'hot-only' && diskNames.length > 0 && diskNames.every((d) => d === 'bwc_hot_disk')) {
    notice('all active parts on the local hot disk (bwc_hot_disk) — hot-only mode confirmed');
  }
  if (SCENARIO === 'object-storage' && diskNames.length > 0 && diskNames.every((d) => EXPECTED_DISKS.has(d))) {
    notice(`all active parts on the object-store tier: ${diskNames.join(', ')}`);
  }
  if (SCENARIO === 'hot-cold') {
    if (diskNames.includes('bwc_hot_disk')) {
      notice('freshly-seeded rows are on the hot disk (bwc_hot_disk), as expected before aging past tierAfter');
    } else {
      error('hot-cold mode: no freshly-seeded active parts found on the hot disk (bwc_hot_disk) — new inserts should land there first');
      failures++;
    }
  }

  return failures;
}

// ---- hot-cold ONLY: age past tierAfter, force a merge, assert the move. ----
async function verifyHotColdAging() {
  const pod = clickhousePod();
  const waitSeconds = TIER_AFTER_SECONDS + 15; // headroom past the tierAfter boundary
  notice(`hot-cold: waiting ${waitSeconds}s for the seeded rows to age past schema.tierAfter=${TIER_AFTER_SECONDS}s`);
  await sleep(waitSeconds * 1000);

  const tables = lines(chQuery(
    pod,
    `SELECT name FROM system.tables WHERE database='${DB}' AND engine LIKE '%MergeTree%' AND name IN ('${[...CANONICAL_TABLES].join("','")}') ORDER BY name`,
  ));
  for (const t of tables) {
    // OPTIMIZE ... FINAL forces a merge immediately rather than waiting for
    // ClickHouse's background merge scheduler, which re-evaluates every TTL
    // rule (including the TO VOLUME move) on the resulting part.
    kubectl(['exec', pod, '--', 'clickhouse-client', '--user', CH_USER, '--password', CH_PASSWORD, '--database', DB,
      '--query', `OPTIMIZE TABLE ${DB}.\`${t}\` FINAL`], { timeout: 120_000 });
  }

  const diskNames = [...activePartDisks(pod)];
  log(`active part disk_name set (post-aging, post-OPTIMIZE FINAL): ${diskNames.join(', ') || '(none)'}`);
  if (diskNames.includes('bwc_object_cache')) {
    notice('hot-cold: aged rows moved onto the cold (object-store) disk after crossing schema.tierAfter — TTL ... TO VOLUME confirmed');
    return 0;
  }
  error(`hot-cold: no active parts found on the cold disk (bwc_object_cache) after aging ${waitSeconds}s + OPTIMIZE ... FINAL — the TTL move rule did not fire`);
  return 1;
}

// Bucket-non-empty check, polled. Runs a throwaway minio/mc pod in-cluster so
// it needs no host-side aws/mc and works identically locally and on CI. Only
// meaningful for scenarios that actually use MinIO (object-storage, hot-cold).
async function bucketNonEmpty() {
  const podName = `mc-verify-${Date.now()}`;
  const deadline = Date.now() + POLL_SECONDS * 1000;
  let lastCount = -1;
  while (Date.now() < deadline) {
    const res = kubectl([
      'run', podName,
      '--image', MC_IMAGE,
      '--restart', 'Never',
      '--rm', '-i', '--quiet',
      '--command', '--',
      'sh', '-c',
      `mc alias set m http://minio:9000 minioadmin minioadmin >/dev/null 2>&1 && ` +
        `mc ls --recursive m/${BUCKET}/ | wc -l`,
    ], { timeout: 60_000 });
    const out = res.stdout.trim().split('\n').map((s) => s.trim()).filter(Boolean);
    const n = Number(out[out.length - 1] || 'NaN');
    if (Number.isFinite(n)) {
      lastCount = n;
      log(`mc ls m/${BUCKET}/ -> ${n} object(s)`);
      if (n > 0) {
        notice(`object store bucket ${BUCKET} is non-empty (${n} objects) — parts are on MinIO`);
        return 0;
      }
    } else {
      log(`mc ls attempt produced no count yet: ${res.stderr.trim()}`);
    }
    await sleep(3000);
  }
  error(`bucket ${BUCKET} stayed empty after ${POLL_SECONDS}s (last count=${lastCount}) — no objects on object storage`);
  return 1;
}

let total = main();
if (SCENARIO === 'hot-cold') {
  total += await verifyHotColdAging();
}
if (SCENARIO !== 'hot-only') {
  total += await bucketNonEmpty();
}
if (total > 0) {
  error(`bwc placement verify (scenario=${SCENARIO}) FAILED with ${total} assertion failure(s)`);
  process.exit(1);
}
log(`bwc placement verify (scenario=${SCENARIO}) PASSED.`);
process.exit(0);
