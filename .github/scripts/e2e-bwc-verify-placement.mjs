// e2e-bwc-verify-placement.mjs — placement assertions for the bundled-ClickHouse
// ("bwc") object-storage e2e lane. Run by `just e2e-bwc-verify` AFTER
// `just e2e-seed-rolling` has landed data into the bundled ClickHouse that the
// chart deployed with clickhouse.bundled.enabled=true pointed at in-cluster
// MinIO.
//
// It PROVES the data tier actually lives on object storage — not just that the
// chart rendered — by asserting three independent facts against the live stack:
//
//   1. STORAGE POLICY STAMPED. Every MergeTree table cerberus auto-created in
//      the otel database carries SETTINGS ... storage_policy='bwc_object_store'
//      (the chart's cerberus.bundled.apply -> schema.storagePolicy wiring).
//   2. PARTS ON THE OBJECT-STORE DISK. system.parts.disk_name for the active
//      parts is the bwc object/cache disk (bwc_object_cache fronting
//      bwc_object_disk), never the local `default` disk.
//   3. BUCKET NON-EMPTY. The MinIO bucket has objects under the disk's path
//      after the seed — polled, because the part-object writes lag the insert
//      by a beat. We assert NON-EMPTY (>0), never an exact count: batch object
//      GC is async and a stray marker object is benign.
//
// Env contract:
//   NAMESPACE     k8s namespace                 (default cerberus)
//   DATABASE      ClickHouse database           (default otel)
//   CH_USER       ClickHouse user               (default cerberus)
//   CH_PASSWORD   ClickHouse password           (default cerberus)
//   BUCKET        object-store bucket            (default cerberus-bwc)
//   STORAGE_POLICY expected policy name         (default bwc_object_store)
//   MC_IMAGE      minio/mc image for bucket ls  (default the pinned RELEASE)
//   POLL_SECONDS  bucket-non-empty poll budget  (default 30)
//
// Exit 0 = every assertion passed; 1 = any failed (with ::error:: annotation).

import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { error, notice, log, capture } from './lib/gh.mjs';

const NS = process.env.NAMESPACE || 'cerberus';
const DB = process.env.DATABASE || 'otel';
const CH_USER = process.env.CH_USER || 'cerberus';
const CH_PASSWORD = process.env.CH_PASSWORD || 'cerberus';
const BUCKET = process.env.BUCKET || 'cerberus-bwc';
const STORAGE_POLICY = process.env.STORAGE_POLICY || 'bwc_object_store';
const MC_IMAGE = process.env.MC_IMAGE || 'minio/mc:RELEASE.2025-08-13T08-35-41Z';
const POLL_SECONDS = Number(process.env.POLL_SECONDS || '30');

// The disks the chart's storage XML defines: an s3 object disk and the local
// cache disk fronting it. A part's disk_name is the cache disk; what matters is
// it is NEVER the built-in `default` (local) disk.
const BWC_DISKS = new Set(['bwc_object_cache', 'bwc_object_disk']);

// The OTel tables cerberus's own auto-create DDL manages and stamps with the
// storage policy. The collector's clickhouseexporter may ALSO create tables of
// its own (e.g. otel_metrics_exponential_histogram, a name cerberus's DDL does
// not use); those are outside cerberus's schema-stamping contract, so the
// explicit-SETTINGS check is scoped to this canonical set. Physical placement
// of EVERY table's data is still asserted comprehensively via system.parts.
const CANONICAL_TABLES = new Set([
  'otel_logs',
  'otel_traces',
  'otel_traces_trace_id_ts',
  'otel_metrics_gauge',
  'otel_metrics_sum',
  'otel_metrics_histogram',
  'otel_metrics_exp_histogram',
  'otel_metrics_summary',
]);

function kubectl(args, opts = {}) {
  return capture('kubectl', ['-n', NS, ...args], opts);
}

// Resolve the bundled ClickHouse pod by the chart's immutable selector label.
function clickhousePod() {
  const res = kubectl([
    'get', 'pod',
    '-l', 'app.kubernetes.io/component=clickhouse',
    '-o', 'jsonpath={.items[0].metadata.name}',
  ]);
  const name = res.stdout.trim();
  if (res.status !== 0 || !name) {
    error(`could not resolve bundled ClickHouse pod in namespace ${NS}: ${res.stderr.trim()}`);
    process.exit(1);
  }
  return name;
}

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

function main() {
  const pod = clickhousePod();
  log(`bwc placement verify: namespace=${NS} pod=${pod} db=${DB} bucket=${BUCKET} policy=${STORAGE_POLICY}`);
  let failures = 0;

  // ---- 0. The chart's storage XML rendered: the bwc disks exist. ----
  const disks = chQuery(pod, 'SELECT name FROM system.disks ORDER BY name')
    .split('\n').map((s) => s.trim()).filter(Boolean);
  log(`system.disks: ${disks.join(', ')}`);
  for (const need of ['bwc_object_disk', 'bwc_object_cache']) {
    if (!disks.includes(need)) {
      error(`disk ${need} missing from system.disks — storage XML did not render the object tier`);
      failures++;
    }
  }

  // ---- 1. storage_policy stamped on every MergeTree table. ----
  const tables = chQuery(
    pod,
    `SELECT name FROM system.tables WHERE database='${DB}' AND engine LIKE '%MergeTree%' ORDER BY name`,
  ).split('\n').map((s) => s.trim()).filter(Boolean);
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
  if (canonicalPresent.length > 0 && stampedCount === canonicalPresent.length) {
    notice(`storage_policy='${STORAGE_POLICY}' stamped on all ${stampedCount} cerberus OTel tables`);
  }

  // ---- 2. active parts live on the object-store disk, not `default`. ----
  // The seed's initial insert ran synchronously before this verify, so active
  // parts already exist.
  const diskNames = chQuery(
    pod,
    `SELECT DISTINCT disk_name FROM system.parts WHERE database='${DB}' AND active`,
  ).split('\n').map((s) => s.trim()).filter(Boolean);
  log(`active part disk_name set: ${diskNames.join(', ') || '(none)'}`);
  if (diskNames.length === 0) {
    error('no active parts found — the seed did not write data, cannot prove placement');
    failures++;
  }
  for (const d of diskNames) {
    if (d === 'default') {
      error('found active parts on the `default` (local) disk — data is NOT on object storage');
      failures++;
    } else if (!BWC_DISKS.has(d)) {
      error(`unexpected disk_name for active parts: ${d} (expected one of ${[...BWC_DISKS].join(', ')})`);
      failures++;
    }
  }
  if (diskNames.length > 0 && diskNames.every((d) => BWC_DISKS.has(d))) {
    notice(`all active parts on the object-store tier: ${diskNames.join(', ')}`);
  }

  return failures;
}

// Bucket-non-empty check, polled. Runs a throwaway minio/mc pod in-cluster so
// it needs no host-side aws/mc and works identically locally and on CI.
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

const chFailures = main();
const bucketFailures = await bucketNonEmpty();
const total = chFailures + bucketFailures;
if (total > 0) {
  error(`bwc placement verify FAILED with ${total} assertion failure(s)`);
  process.exit(1);
}
log('bwc placement verify PASSED: schema stamped, parts on object store, bucket non-empty.');
process.exit(0);
