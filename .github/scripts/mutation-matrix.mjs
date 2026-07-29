// mutation-matrix.mjs — decides WHICH mutation phases run, and emits them as
// the `mutate` job's strategy.matrix (mutation.yml).
//
// Why this exists
// ---------------
// The `mutation` lane used to be all-or-nothing on a pull request: a job-level
// `if:` skipped the entire matrix unless the event was a push, a schedule, a
// dispatch, or a `release/*` PR, and the required `mutation` aggregator read the
// skipped matrix as a green pass-through. A PR could therefore drop
// internal/chplan's efficacy below its 95% floor, merge with `mutation` green,
// and only surface on push-to-main — or, as happened in the v1.13.2 cycle, on
// the release PR, where it blocks the release instead of the change that caused
// it. That is a hollow green: a required check reporting on work it never did.
//
// The answer is neither "skip" nor "run all ~15 legs on every PR" (the matrix
// cost the skip existed to avoid). It is to run the legs whose SCOPE the PR
// actually changed. A PR editing internal/chplan runs phase1. A PR editing only
// docs runs nothing and the aggregator passes through honestly, because there
// was nothing in this lane's scope to check. Push / schedule / dispatch and
// release PRs still sweep the FULL matrix, so no leg's floor is ever load-bearing
// on some PR happening to touch it.
//
// Three modes (env MODE, or argv[2]; default `verify`):
//   - verify : assert the PHASES table is internally sound — unique phase names,
//              every `scope` an existing directory, every `exclude_files`
//              pattern compiling AND staying inside RE2 (no lookaround, no
//              backreferences: Go's regexp rejects them, and round 11 of the
//              traceql split burned a full CI cycle discovering that mid-run).
//              Runs in milliseconds, installs nothing.
//   - emit   : run the same assertions, select the phases this event needs, and
//              write `matrix=<json>` + `has_phases=true|false` to $GITHUB_OUTPUT.
//              emit re-runs verify internally, so it can never ship a matrix
//              built from a table that would have failed the check.
//   - dump   : write the validated PHASES table to stdout as JSON, and nothing
//              else. This is the table's machine-readable face: Go's
//              test/regression leg-partition test reads it to check that every
//              mutable file under a mutated package is claimed by exactly one
//              leg — an invariant no single leg's regex can express, and which
//              therefore has to be checked against the whole table at once.
//
// Env:
//   MODE          `emit` | `verify` | `dump` (also argv[2]); default `verify`.
//   EVENT_NAME    github.event_name.
//   HEAD_REF      github.head_ref (empty off pull_request).
//   BASE_SHA      (emit, PR) github.event.pull_request.base.sha.
//   HEAD_SHA      (emit, PR) github.event.pull_request.head.sha.
//   GITHUB_OUTPUT (emit) runner file the matrix JSON is appended to.
//
// Exit: 0 clean / matrix emitted; 1 on a table violation, a coverage gap, or a
// bad MODE.
//
// node: builtins only — no npm deps, no setup-node needed.

import { statSync } from 'node:fs';

import { error, log, notice, setOutput } from './lib/gh.mjs';
import { changedPaths, matchesAny, normalise, runsFullLane, underPrefix } from './lib/scope-gate.mjs';
import { HARNESS_PATHS, PHASES } from './mutation-phases.mjs';

// Constructs Go's regexp (RE2) rejects outright. gremlins passes exclude_files
// straight to Go, so a JS-valid pattern using any of these compiles fine here
// and then errors the leg out at run time — which is how round 11's `(?!…)`
// slipped through review and cost a CI cycle. Checked as source text because a
// compiled JS RegExp has already accepted them.
const NON_RE2_CONSTRUCTS = [
  { re: /\(\?=/, what: 'lookahead `(?=`' },
  { re: /\(\?!/, what: 'negative lookahead `(?!`' },
  { re: /\(\?<=/, what: 'lookbehind `(?<=`' },
  { re: /\(\?<!/, what: 'negative lookbehind `(?<!`' },
  { re: /\\[1-9]/, what: 'a backreference `\\1`' },
];

// tableViolations — everything wrong with the PHASES table, as strings.
export function tableViolations(phases) {
  const problems = [];
  const seen = new Set();

  for (const p of phases) {
    const name = p.phase;
    if (!name) {
      problems.push(`a phase entry has no \`phase\` name: ${JSON.stringify(p)}`);
      continue;
    }
    if (seen.has(name)) {
      problems.push(
        `duplicate phase name "${name}" — phase names become artifact names (gremlins-<phase>) and ` +
          `check names, so a collision silently overwrites one leg's report with another's.`,
      );
    }
    seen.add(name);

    if (!p.scope) {
      problems.push(`phase "${name}" has no \`scope\` package.`);
    } else if (!isDirectory(normalise(p.scope))) {
      problems.push(
        `phase "${name}" scopes "${p.scope}", which is not a directory in this tree. A stale scope ` +
          `makes gremlins mutate nothing and report a vacuous 100% efficacy.`,
      );
    }

    if (!Number.isInteger(p.efficacy) || p.efficacy < 0 || p.efficacy > 100) {
      problems.push(`phase "${name}" has a non-percentage \`efficacy\`: ${JSON.stringify(p.efficacy)}`);
    }
    if (!Number.isInteger(p.workers) || p.workers < 0) {
      problems.push(`phase "${name}" has a negative or non-integer \`workers\`: ${JSON.stringify(p.workers)}`);
    }

    if (p.exclude_files !== undefined) {
      problems.push(...excludeViolations(name, p.exclude_files));
    }
  }

  return problems;
}

function excludeViolations(name, pattern) {
  const problems = [];
  if (typeof pattern !== 'string' || pattern === '') {
    problems.push(`phase "${name}" has an empty \`exclude_files\` — omit the key instead of excluding nothing.`);
    return problems;
  }
  for (const { re, what } of NON_RE2_CONSTRUCTS) {
    if (re.test(pattern)) {
      problems.push(
        `phase "${name}" \`exclude_files\` uses ${what}, which Go's RE2 engine rejects. gremlins would ` +
          `error the leg out at run time, after the checkout and toolchain setup have already been paid for.`,
      );
    }
  }
  try {
    new RegExp(pattern);
  } catch (e) {
    problems.push(`phase "${name}" \`exclude_files\` is not a valid regular expression: ${e.message}`);
  }
  return problems;
}

function isDirectory(path) {
  try {
    return statSync(path).isDirectory();
  } catch {
    return false;
  }
}

// phaseClaims — does this phase mutate `path`? True when the path lies under the
// phase's scope AND its SCOPE-RELATIVE form is not excluded — the same two-step
// gremlins itself applies, so the selection can never claim a file the run would
// then skip.
//
// Deliberately not restricted to non-test `.go` files. gremlins mutates only
// production Go, but a leg's efficacy is `killed / (killed + lived)` over the
// package's OWN tests: editing a `_test.go` or a testdata fixture moves the
// number without touching a mutable line. Selecting on any changed path under
// the scope over-selects a little (a `lexer_test.go` edit also claims the leg
// whose exclude names `lexer.go`) and under-selects never, which is the correct
// direction for a gate.
export function phaseClaims(phase, path) {
  if (!underPrefix(path, phase.scope)) return false;
  if (!phase.exclude_files) return true;
  const rel = normalise(path).slice(normalise(phase.scope).length + 1);
  return !new RegExp(phase.exclude_files).test(rel);
}

// selectPhases — the phases this event must run, plus the reason, plus any
// changed path that fell inside a scope while claiming NO leg.
//
// A path in that last bucket is a real coverage gap: it sits in a package the
// mutation lane owns, yet every leg's exclude regex skips it, so no leg mutates
// it on ANY event. Reported rather than shrugged off — silently dropping it
// would let a leg partition drift into leaving a file permanently unmutated.
export function selectPhases({ phases, harnessPaths, eventName, headRef, changed }) {
  if (runsFullLane({ eventName, headRef })) {
    return { phases, reason: `event "${eventName}" always runs the full matrix`, gaps: [] };
  }
  if (changed === null) {
    return { phases, reason: 'the changed-path set could not be computed', gaps: [] };
  }

  const paths = [...changed];
  const harnessHit = paths.filter((p) => matchesAny(p, harnessPaths));
  if (harnessHit.length > 0) {
    return {
      phases,
      reason: `the lane's own harness changed (${harnessHit.join(', ')})`,
      gaps: [],
    };
  }

  const selected = phases.filter((phase) => paths.some((p) => phaseClaims(phase, p)));
  const gaps = paths.filter(
    (p) => phases.some((phase) => underPrefix(p, phase.scope)) && !phases.some((phase) => phaseClaims(phase, p)),
  );

  return {
    phases: selected,
    reason:
      selected.length > 0
        ? `${selected.length} of ${phases.length} phases scope a changed path`
        : 'no changed path falls in any phase scope',
    gaps,
  };
}

// ---------------------------------------------------------------------------
// driver
// ---------------------------------------------------------------------------

function main() {
  const mode = (process.env.MODE || process.argv[2] || 'verify').trim();
  if (mode !== 'verify' && mode !== 'emit' && mode !== 'dump') {
    error(`mutation-matrix: MODE must be "verify", "emit" or "dump" (got "${mode}")`);
    process.exit(1);
  }

  const problems = tableViolations(PHASES);
  for (const p of problems) error(`mutation-matrix: ${p}`);
  if (problems.length > 0) process.exit(1);

  // dump puts the table on stdout and NOTHING else, because a consumer parses
  // that stream as JSON. Validation still runs first: a dump of a table that
  // would have failed verify is worse than no dump at all.
  if (mode === 'dump') {
    process.stdout.write(`${JSON.stringify(PHASES, null, 2)}\n`);

    return;
  }

  log(`mutation-matrix: ${PHASES.length} phases, table sound.`);

  if (mode === 'verify') return;

  const eventName = (process.env.EVENT_NAME || '').trim();
  const headRef = (process.env.HEAD_REF || '').trim();
  const changed = runsFullLane({ eventName, headRef })
    ? null
    : changedPaths({ baseSha: process.env.BASE_SHA, headSha: process.env.HEAD_SHA });

  const { phases, reason, gaps } = selectPhases({
    phases: PHASES,
    harnessPaths: HARNESS_PATHS,
    eventName,
    headRef,
    changed,
  });

  for (const gap of gaps) {
    error(
      `mutation-matrix: "${gap}" lies inside a mutation phase scope but every leg's exclude_files skips it — ` +
        `no event mutates that file. Claim it in a leg (usually the scope's catch-all) or move it out of scope.`,
    );
  }
  if (gaps.length > 0) process.exit(1);

  notice(
    `mutation-matrix: running ${phases.length}/${PHASES.length} phases — ${reason}` +
      (phases.length > 0 ? ` (${phases.map((p) => p.phase).join(', ')})` : ''),
  );

  // `matrix` is the whole strategy.matrix object (mirrors compose-smoke-setup),
  // so mutation.yml interpolates it as `matrix: ${{ fromJSON(...) }}` rather
  // than reaching into a bare array. `has_phases` is emitted separately because
  // a job-level `if:` cannot introspect a matrix: without it, a SKIPPED `mutate`
  // is indistinguishable to the aggregator from a selection that was legitimately
  // empty — which is precisely the ambiguity this lane is being fixed to remove.
  setOutput('matrix', JSON.stringify({ include: phases }));
  setOutput('has_phases', String(phases.length > 0));
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
