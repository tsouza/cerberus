// compat-ratchet.mjs — parity-regression RATCHET for the required
// compatibility/{prometheus,loki,tempo} checks.
//
// The three harnesses are SCORED: they accumulate per-case results,
// write compat-score.json plus the per-case roster compat-cases.json,
// and exit 0 even when a case diverges, so the harness step alone
// reddens the job only on infrastructure breakage. This script is the
// gate. It reads one head's run artefacts and the committed baseline in
// compatibility/parity-baseline.json, and FAILS the job on any parity
// movement the baseline does not already record.
//
// It gates on per-case IDENTITY, not on a count. A count cannot tell a
// swap from a steady state: one case regressing while a different one
// starts passing leaves passed/total untouched, so an aggregate floor
// reports green while parity got worse for a real query. The baseline
// therefore carries `heads.<head>.cases` — the exact, sorted roster of
// corpus case IDs — and every case is checked by name.
//
// The committed baseline records FULL parity for every head: passed ==
// total == cases.length. That is the project's contract, not an
// accident of the current numbers — a diff against a reference backend
// is a bug to fix at the source, so there is no shape in this file that
// can record a divergence as acceptable. The floors are prometheus
// 738/738, loki 116/116, tempo 49/49; doc-counts.mjs asserts those
// three numbers against compatibility/parity-baseline.json, so a corpus
// change cannot leave this comment behind.
//
// This is NOT an allow-list. An allow-list names the cases you are
// permitted to fail; the roster names the cases that must pass, and
// every entry on it is an obligation rather than an exemption.
//
// The four verdicts, all of them fatal:
//
//   REGRESSED   a case the baseline records as passing now diverges.
//               The regression to fix.
//   VANISHED    a case the baseline records is absent from the run.
//               A divergence must never be retired by deleting or
//               renaming its case, and corpus coverage must never
//               shrink silently, so a disappearance is a failure that
//               names the missing IDs rather than a quiet pass.
//   ARRIVED-FAILING
//               a case not in the baseline diverged on arrival. It is
//               new to the corpus and cerberus does not match the
//               reference on it — a real parity bug the corpus change
//               just surfaced. It is NOT absorbed as "not previously
//               passing, so fine": that reasoning is exactly the
//               allow-list this project refuses, and it would let a
//               corpus refresh import known-bad behaviour under a
//               green check.
//   UNRECORDED  a case not in the baseline passed. Nothing is broken,
//               but until it is written into the baseline no future
//               run is gated on it, so the ratchet has not actually
//               ratcheted. Recording it is a same-PR edit.
//
// Why this can't flake: case IDs are built by each harness from the
// case's static corpus identity (query text, endpoint, suite, lane) and
// never from wall-clock-derived values, so a corpus that did not change
// yields a byte-identical roster. Pass/fail per case comes from the
// same success predicate that feeds compat-score.json, over
// canonical-key-sorted result sets compared with absolute + relative
// epsilon (1e-9) against a deterministic seed. There is no float,
// timing or ordering surface left to jitter.
//
// Moving the baseline: when the corpus legitimately grows, or a case is
// deliberately renamed or retired, edit heads.<head> in
// compatibility/parity-baseline.json in the SAME PR. The run's
// compat-cases.json (uploaded as a job artefact) is the authoritative
// new roster — take the IDs from it rather than hand-editing, so the
// committed list cannot drift from what the harness actually ran.
//
// Env contract:
//   HEAD      head name: prometheus | loki | tempo (selects the baseline
//             entry, must match the cases file's own head field, and
//             labels the messages).
//   SCORE     path to that head's compat-score.json (the run's score).
//   CASES     path to that head's compat-cases.json (the run's per-case
//             roster).
//   BASELINE  path to the committed baseline JSON
//             (default: compatibility/parity-baseline.json).
//
// Exit codes:
//   0  every baseline case ran and passed, and the run introduced no
//      case the baseline does not record.
//   1  a regression / vanished / arrived-failing / unrecorded case, OR
//      the baseline, score or cases file is missing, malformed, or
//      internally inconsistent (a missing artefact means the harness
//      never produced one — that is itself a hard failure the separate
//      "Fail job" step also re-raises; failing here keeps the ratchet
//      honest rather than silently passing on absent data).

import { existsSync, readFileSync } from 'node:fs';
import process from 'node:process';
import { error, log } from './lib/gh.mjs';

const DEFAULT_BASELINE = 'compatibility/parity-baseline.json';

// maxListedCases caps how many case IDs a verdict prints inline. The
// full set is always in the run's compat-cases.json artefact; the log
// is for orientation, and a 700-line error block orients nobody.
const maxListedCases = 25;

function fail(message, props) {
  error(message, props);
  process.exit(1);
}

// listCases renders IDs for a log line, one per row, truncated at
// maxListedCases so a wholesale corpus change stays readable.
export function listCases(ids) {
  const shown = ids.slice(0, maxListedCases).map((id) => `    - ${id}`);
  if (ids.length > maxListedCases) {
    shown.push(`    … and ${ids.length - maxListedCases} more (see the compat-cases.json artefact)`);
  }
  return shown.join('\n');
}

// baselineRoster validates a baseline head entry and returns its case
// IDs as a Set. The entry has to be self-consistent before it can be
// trusted to gate anything: a `passed` that disagrees with the roster
// length would let a hand-edit quietly shrink what is being checked.
export function baselineRoster(entry, headName) {
  if (!entry || typeof entry.passed !== 'number' || typeof entry.total !== 'number') {
    return { err: `expected heads.${headName}.{passed,total} as numbers` };
  }
  if (!Array.isArray(entry.cases)) {
    return { err: `expected heads.${headName}.cases as an array of case IDs` };
  }
  const ids = new Set();
  for (const id of entry.cases) {
    if (typeof id !== 'string' || id.trim() === '') {
      return { err: `heads.${headName}.cases contains a non-string or empty case ID` };
    }
    if (ids.has(id)) {
      return { err: `heads.${headName}.cases lists ${JSON.stringify(id)} twice` };
    }
    ids.add(id);
  }
  const sorted = [...entry.cases].sort();
  if (sorted.some((id, i) => id !== entry.cases[i])) {
    return {
      err:
        `heads.${headName}.cases is not sorted — the roster is committed in sorted order so a ` +
        `baseline diff shows exactly which cases moved`,
    };
  }
  if (entry.cases.length !== entry.passed) {
    return {
      err:
        `heads.${headName} says passed=${entry.passed} but lists ${entry.cases.length} case(s); ` +
        `the roster IS the passing set, so the two must agree`,
    };
  }
  if (entry.passed !== entry.total) {
    return {
      err:
        `heads.${headName} says passed=${entry.passed} of total=${entry.total} — the committed ` +
        `baseline records FULL parity for every head, because a divergence is a bug to fix at ` +
        `the source, never a state to record as acceptable`,
    };
  }
  return { ids };
}

// runRoster validates the run's compat-cases.json and returns its cases
// as an id -> passed Map.
export function runRoster(doc, headName) {
  if (!doc || typeof doc !== 'object') {
    return { err: 'expected a JSON object' };
  }
  if (doc.head !== headName) {
    return {
      err: `head field is ${JSON.stringify(doc.head)} but this job is gating '${headName}' — the score and cases artefacts were crossed over between heads`,
    };
  }
  if (!Array.isArray(doc.cases)) {
    return { err: 'expected a `cases` array of {id, passed} objects' };
  }
  const cases = new Map();
  for (const c of doc.cases) {
    if (!c || typeof c.id !== 'string' || c.id.trim() === '' || typeof c.passed !== 'boolean') {
      return { err: 'every entry must be {id: non-empty string, passed: boolean}' };
    }
    if (cases.has(c.id)) {
      return { err: `case ID ${JSON.stringify(c.id)} appears twice` };
    }
    cases.set(c.id, c.passed);
  }
  return { cases };
}

// verdicts partitions the run against the baseline roster. Every
// non-empty bucket is a failure; they are separated because the fix
// differs — a regression is fixed in the engine, an unrecorded case in
// the baseline file.
export function verdicts(baselineIds, runCases) {
  const regressed = [];
  const vanished = [];
  const arrivedFailing = [];
  const unrecorded = [];
  for (const id of baselineIds) {
    if (!runCases.has(id)) {
      vanished.push(id);
    } else if (!runCases.get(id)) {
      regressed.push(id);
    }
  }
  for (const [id, passed] of runCases) {
    if (baselineIds.has(id)) continue;
    (passed ? unrecorded : arrivedFailing).push(id);
  }
  return {
    regressed: regressed.sort(),
    vanished: vanished.sort(),
    arrivedFailing: arrivedFailing.sort(),
    unrecorded: unrecorded.sort(),
  };
}

function main() {
  const head = process.env.HEAD || '';
  const scorePath = process.env.SCORE || '';
  const casesPath = process.env.CASES || '';
  const baselinePath = process.env.BASELINE || DEFAULT_BASELINE;

  if (!head) {
    fail('compat-ratchet: HEAD env var is required (prometheus|loki|tempo)', {
      title: 'compat ratchet misconfigured',
    });
  }
  if (!scorePath) {
    fail('compat-ratchet: SCORE env var (path to compat-score.json) is required', {
      title: 'compat ratchet misconfigured',
    });
  }
  if (!casesPath) {
    fail('compat-ratchet: CASES env var (path to compat-cases.json) is required', {
      title: 'compat ratchet misconfigured',
    });
  }

  const readJson = (path, label) => {
    if (!existsSync(path)) {
      fail(
        `compat-ratchet: ${label} not found at ${path} — the harness must have ` +
          `failed before producing it; cannot verify parity against baseline`,
        { title: `compatibility/${head} ratchet: missing ${label}` },
      );
    }
    try {
      return JSON.parse(readFileSync(path, 'utf8'));
    } catch (e) {
      fail(`compat-ratchet: could not parse ${label} at ${path}: ${e.message}`, {
        title: `compatibility/${head} ratchet: malformed ${label}`,
      });
    }
    return undefined; // unreachable; fail() exits.
  };

  const baseline = readJson(baselinePath, 'baseline');
  const { ids: baselineIds, err: baselineErr } = baselineRoster(baseline?.heads?.[head], head);
  if (baselineErr) {
    fail(`compat-ratchet: bad baseline entry in ${baselinePath}: ${baselineErr}`, {
      title: `compatibility/${head} ratchet: bad baseline entry`,
    });
  }

  const score = readJson(scorePath, 'compat-score.json');
  if (typeof score.passed !== 'number' || typeof score.total !== 'number') {
    fail(`compat-ratchet: ${scorePath} is missing numeric passed/total fields`, {
      title: `compatibility/${head} ratchet: malformed score`,
    });
  }

  const casesDoc = readJson(casesPath, 'compat-cases.json');
  const { cases: runCases, err: runErr } = runRoster(casesDoc, head);
  if (runErr) {
    fail(`compat-ratchet: bad ${casesPath}: ${runErr}`, {
      title: `compatibility/${head} ratchet: malformed cases`,
    });
  }

  // The score and the roster come from the same run over the same
  // result slice, so they must tally. A mismatch means one of the two
  // artefacts is stale — gating on a stale roster would bless a run
  // nobody checked.
  const runPassed = [...runCases.values()].filter(Boolean).length;
  if (runCases.size !== score.total || runPassed !== score.passed) {
    fail(
      `compat-ratchet: ${casesPath} and ${scorePath} disagree about this run — ` +
        `roster has ${runPassed}/${runCases.size}, score says ${score.passed}/${score.total}. ` +
        `One of the two artefacts is stale; the ratchet will not gate on data it cannot reconcile.`,
      { title: `compatibility/${head} ratchet: score/roster mismatch` },
    );
  }

  const { regressed, vanished, arrivedFailing, unrecorded } = verdicts(baselineIds, runCases);

  if (regressed.length || vanished.length || arrivedFailing.length || unrecorded.length) {
    const lines = [`compatibility/${head} PARITY MOVED vs the committed baseline:`];
    if (regressed.length) {
      lines.push(
        `  REGRESSED — ${regressed.length} case(s) the baseline records as agreeing with the ` +
          `reference now diverge:\n${listCases(regressed)}`,
      );
    }
    if (vanished.length) {
      lines.push(
        `  VANISHED — ${vanished.length} baseline case(s) did not run at all (removed or renamed ` +
          `in the corpus). A case is not fixed by disappearing, and corpus coverage must not ` +
          `shrink silently:\n${listCases(vanished)}`,
      );
    }
    if (arrivedFailing.length) {
      lines.push(
        `  ARRIVED-FAILING — ${arrivedFailing.length} case(s) new to the corpus diverge from the ` +
          `reference on arrival. New is not an excuse: this is a real parity bug the corpus ` +
          `change surfaced:\n${listCases(arrivedFailing)}`,
      );
    }
    if (unrecorded.length) {
      lines.push(
        `  UNRECORDED — ${unrecorded.length} case(s) new to the corpus passed but are not in the ` +
          `baseline, so no future run is gated on them:\n${listCases(unrecorded)}`,
      );
    }
    lines.push(
      `Fix REGRESSED / ARRIVED-FAILING at the source — compatibility is the source of truth for ` +
        `the ${head} head, and there is no allow-list to park a divergence in. For a deliberate ` +
        `corpus change (VANISHED / UNRECORDED), replace heads.${head} in ${baselinePath} in the ` +
        `SAME PR using the run's compat-cases.json artefact as the new roster.`,
    );
    fail(lines.join('\n'), { title: `compatibility/${head} parity regression` });
  }

  log(
    `compat-ratchet: compatibility/${head} matches the committed baseline — all ` +
      `${baselineIds.size} case(s) ran and agreed with the reference, and the run introduced ` +
      `none the baseline does not record.`,
  );
  process.exit(0);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
