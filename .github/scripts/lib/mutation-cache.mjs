// mutation-cache.mjs — content-keyed verdict cache for one `mutation` leg.
//
// A leg's verdict is a deterministic function of its inputs, so it is cached
// by the CONTENT of those inputs and never by commit SHA, branch or time. This
// module holds the pure half: which inputs make up the key, how the key is
// derived from them, what a stored entry looks like, and when an entry may be
// believed. .github/scripts/mutation-cache.mjs collects the inputs from a
// checkout and drives the workflow steps.
//
// KEY INPUT CLASSES (KEY_INPUT_CLASSES below; the key refuses an input object
// that lacks any of them, so a class cannot be dropped silently):
//
//   toolchain      `go version` plus the Go environment knobs that change what
//                  a test binary is (CGO_ENABLED, GOEXPERIMENT, GOFLAGS).
//   gremlins       the gremlins module path, the fork ref the workflow
//                  installs, and the commit that ref resolves to.
//   phaseRow       the leg's matrix row (scope, efficacy, workers,
//                  exclude_files, anything added later) WITHOUT `diff_ref`,
//                  which is a commit SHA; the content it selects is `scopeDiff`.
//   runnerEnv      the workflow-level bounds mutation-run.mjs reads
//                  (MUTANT_TIMEOUT_MIN / MUTANT_TIMEOUT_MAX).
//   scopeDiff      for a changed-line leg, the diff of the scope against its
//                  merge base: that diff is what selects the mutants. Empty for
//                  a full-phase leg, so a pull request's full leg and main's
//                  full leg over identical content share one entry.
//   runnerScripts  every script the leg executes to reach its verdict
//                  (mutation-run.mjs, mutant-memory-guard.mjs,
//                  gremlins-threshold.mjs, the cache itself) and, transitively,
//                  every local module they import.
//   goModule       go.mod, go.sum and .gremlins.yaml; go.sum pins the content
//                  of every third-party module in the closure.
//   packages       every file in the directory of every main-module package in
//                  `go list -deps -test <scope>/...` — sources, tests, embeds,
//                  cgo, and any data file sitting beside them — plus each
//                  package's testdata/ tree.
//   dataRoots      files the tests read from outside their own package: every
//                  all-literal relative path (`"../x"` or
//                  `filepath.Join("..", "..", "test", "spec")`) written in any
//                  Go file of the closure, resolved against that package's
//                  directory and hashed recursively. A test that builds a path
//                  to repository data at runtime from anything but literals is
//                  outside what this discovers; write such paths as literals.
//
// TIMED-OUT MUTANTS. `RUN TIMED OUT` and `TIMED OUT` are the only outcomes
// that depend on how fast the runner was, not only on the inputs above. An
// entry is written only when its verdict is PROVEN STABLE under every
// re-timing: the gate is evaluated with every timed-out mutant (both kinds)
// scored as KILLED and again as LIVED, and those two extremes bound every
// assignment in between (a mutant leaving the ratio as unadjudicated lies
// between them too). If both extremes give the same pass/fail as the
// report itself, no runner speed can flip the verdict, and the entry is stored;
// otherwise the leg is not cached and runs again next time. The stored report
// keeps its real counts, and the threshold step re-gates it on a hit.
//
// A report that still holds a RUNNABLE mutant (the run was interrupted) or a
// status the threshold gate does not know is never cached.
//
// AN ENTRY IS BELIEVED ONLY AFTER VALIDATION. validateEntry() recomputes the
// entry's digest over everything it carries, requires its key to equal the key
// computed from this checkout, and re-applies the timing-stability rule. A
// missing, unparsable, tampered, mismatched or unstable entry is a MISS and the
// leg runs gremlins; it is never a pass.

import { createHash } from 'node:crypto';

import { attemptedEfficacy, countMutationStatuses, minCompletedMutants } from '../gremlins-threshold.mjs';

export const CACHE_SCHEMA = 'cerberus-mutation-leg-cache/v1';

export const KEY_INPUT_CLASSES = Object.freeze([
  'toolchain',
  'gremlins',
  'phaseRow',
  'runnerEnv',
  'scopeDiff',
  'runnerScripts',
  'goModule',
  'packages',
  'dataRoots',
]);

// The phase-row field that names a commit rather than content.
export const PHASE_ROW_COMMIT_FIELD = 'diff_ref';

const runnableStatus = 'RUNNABLE';
const hexDigestPattern = /^[0-9a-f]{64}$/;
const runUrlPattern = /^https:\/\/github\.com\/[^/\s]+\/[^/\s]+\/actions\/runs\/[0-9]+(\/attempts\/[0-9]+)?$/;

export function sha256(data) {
  return createHash('sha256').update(data).digest('hex');
}

// canonicalJson serialises with object keys sorted at every depth, so two
// equal values always hash equally regardless of insertion order.
export function canonicalJson(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(',')}]`;
  if (value !== null && typeof value === 'object') {
    const keys = Object.keys(value).sort();
    return `{${keys.map((k) => `${JSON.stringify(k)}:${canonicalJson(value[k])}`).join(',')}}`;
  }
  if (value === undefined) throw new Error('canonicalJson: undefined is not serialisable');
  return JSON.stringify(value);
}

// phaseRowForKey drops the commit-valued field; see PHASE_ROW_COMMIT_FIELD.
export function phaseRowForKey(row) {
  if (row === null || typeof row !== 'object' || Array.isArray(row)) {
    throw new Error('phase row must be an object');
  }
  const { [PHASE_ROW_COMMIT_FIELD]: _commit, ...rest } = row;
  return rest;
}

export function legCacheKey(inputs) {
  if (inputs === null || typeof inputs !== 'object') throw new Error('key inputs must be an object');
  const extra = Object.keys(inputs).filter((k) => !KEY_INPUT_CLASSES.includes(k));
  if (extra.length > 0) throw new Error(`unknown key input class(es): ${extra.join(', ')}`);
  for (const cls of KEY_INPUT_CLASSES) {
    if (inputs[cls] === undefined) throw new Error(`key input class "${cls}" is missing`);
  }
  if (Object.hasOwn(inputs.phaseRow, PHASE_ROW_COMMIT_FIELD)) {
    throw new Error(`phaseRow must not carry ${PHASE_ROW_COMMIT_FIELD}; pass phaseRowForKey(row)`);
  }
  return sha256(canonicalJson({ schema: CACHE_SCHEMA, ...inputs }));
}

function gatePasses(counts, threshold) {
  if (counts.killed + counts.lived < minCompletedMutants) return false;
  return attemptedEfficacy(counts) >= threshold;
}

// timingStability answers whether a report's verdict against `threshold` can
// change under any re-timing of its timed-out mutants. See the header.
export function timingStability(report, threshold) {
  if (!Number.isFinite(threshold)) return { stable: false, reason: 'threshold is not a number' };
  const counts = countMutationStatuses(report);
  if (counts.unknown.size > 0) return { stable: false, reason: 'report carries an unknown mutation status' };
  const runnable = (report?.files ?? []).some((f) => (f?.mutations ?? []).some((m) => m?.status === runnableStatus));
  if (runnable) return { stable: false, reason: 'report carries RUNNABLE mutants (the run was interrupted)' };
  if (counts.total === 0) return { stable: false, reason: 'report carries no per-mutant records' };
  const timedOut = counts.timedOut + counts.runTimedOut;
  const actual = gatePasses(counts, threshold);
  if (timedOut === 0) return { stable: true, pass: actual, reason: 'no timed-out mutants' };
  const base = { ...counts, timedOut: 0, runTimedOut: 0 };
  const allKilled = gatePasses({ ...base, killed: counts.killed + timedOut }, threshold);
  const allLived = gatePasses({ ...base, lived: counts.lived + timedOut }, threshold);
  if (allKilled === actual && allLived === actual) {
    return { stable: true, pass: actual, reason: `verdict holds with all ${timedOut} timed-out mutant(s) killed or lived` };
  }
  return {
    stable: false,
    pass: actual,
    reason: `${timedOut} timed-out mutant(s) could flip the verdict under a different runner speed`,
  };
}

// survivors lists the LIVED mutants a report carries, for the hit log.
export function survivors(report) {
  const out = [];
  for (const file of report?.files ?? []) {
    for (const m of file?.mutations ?? []) {
      if (m?.status === 'LIVED') out.push(`${file.file_name}:${m.line}:${m.column} ${m.type}`);
    }
  }
  return out;
}

function entryDigest({ schema, key, phase, threshold, sourceRunUrl, report }) {
  return sha256(canonicalJson({ schema, key, phase, threshold, sourceRunUrl, report }));
}

export function buildEntry({ key, phase, threshold, sourceRunUrl, report }) {
  const body = { schema: CACHE_SCHEMA, key, phase, threshold, sourceRunUrl, report };
  return { ...body, digest: entryDigest(body) };
}

// validateEntry returns { ok: true, entry } or { ok: false, reason }. Anything
// it cannot positively verify is a miss.
export function validateEntry(raw, { key, phase, threshold }) {
  let entry;
  try {
    entry = typeof raw === 'string' ? JSON.parse(raw) : raw;
  } catch (cause) {
    return { ok: false, reason: `entry is not JSON: ${cause.message}` };
  }
  if (entry === null || typeof entry !== 'object') return { ok: false, reason: 'entry is not an object' };
  if (entry.schema !== CACHE_SCHEMA) return { ok: false, reason: `entry schema ${JSON.stringify(entry.schema)}` };
  if (!hexDigestPattern.test(String(entry.key))) return { ok: false, reason: 'entry key is malformed' };
  if (entry.key !== key) return { ok: false, reason: 'entry key does not match this leg' };
  if (entry.phase !== phase) return { ok: false, reason: 'entry phase does not match this leg' };
  if (entry.threshold !== threshold) return { ok: false, reason: 'entry threshold does not match this leg' };
  if (!runUrlPattern.test(String(entry.sourceRunUrl))) return { ok: false, reason: 'entry source run URL is malformed' };
  if (entry.report === null || typeof entry.report !== 'object') return { ok: false, reason: 'entry has no report' };
  if (entry.digest !== entryDigest(entry)) return { ok: false, reason: 'entry digest does not match its content' };
  const stability = timingStability(entry.report, threshold);
  if (!stability.stable) return { ok: false, reason: `entry verdict is not timing-stable: ${stability.reason}` };
  return { ok: true, entry };
}

// PROVENANCE: every leg records where its verdict came from, and the
// `mutation` aggregator checks the record of every selected phase. A cache
// record carries the entry itself so the aggregator re-verifies it instead of
// trusting the leg's word.
export const PROVENANCE_SOURCES = Object.freeze(['run', 'cache']);

export function validateProvenance(record, { phase, threshold, entry }) {
  if (record === null || typeof record !== 'object') return 'provenance is not an object';
  if (record.phase !== phase) return `provenance phase ${JSON.stringify(record.phase)} is not ${phase}`;
  if (!PROVENANCE_SOURCES.includes(record.source)) return `provenance source ${JSON.stringify(record.source)}`;
  if (!hexDigestPattern.test(String(record.key))) return 'provenance key is malformed';
  if (record.source === 'run') return null;
  if (entry === undefined) return 'cache hit carries no entry to verify';
  const verdict = validateEntry(entry, { key: record.key, phase, threshold });
  if (!verdict.ok) return `cache entry fails validation: ${verdict.reason}`;
  if (record.digest !== entry.digest) return 'provenance digest does not name the carried entry';
  return null;
}
