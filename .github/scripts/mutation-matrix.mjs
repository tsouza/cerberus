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
// The answer is neither "skip" nor "run all 30 legs on every PR" (the matrix
// cost the skip existed to avoid). It is to run the legs whose SCOPE the PR
// actually changed. A PR editing internal/chplan runs phase1. A PR editing only
// docs runs nothing and the aggregator passes through honestly, because there
// was nothing in this lane's scope to check. Push / schedule / dispatch and
// release PRs still sweep the FULL matrix, so no leg's floor is ever load-bearing
// on some PR happening to touch it.
//
// Three modes (env MODE, or argv[2]; default `verify`):
//   - verify : assert the PHASES table is internally sound — unique phase names,
//              every `scope` an existing directory, every `exclude_files` /
//              `include_files` pattern compiling AND staying inside RE2 (no
//              lookaround, no backreferences: Go's regexp rejects them, and
//              round 11 of the traceql split burned a full CI cycle discovering
//              that mid-run), and every `include_files` allowlist still matching
//              exactly the files it names in the real tree. Runs in
//              milliseconds, installs nothing.
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
//   BASE_SHA      (emit, PR/merge group) base commit used for the changed-path projection.
//   HEAD_SHA      (emit, PR/merge group) candidate commit used for the changed-path projection.
//   GITHUB_OUTPUT (emit) runner file the matrix JSON is appended to.
//
// Exit: 0 clean / matrix emitted; 1 on a table violation, a coverage gap, or a
// bad MODE.
//
// node: builtins only — no npm deps, no setup-node needed.

import { execFileSync } from 'node:child_process';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

import { error, git, log, notice, setOutput } from './lib/gh.mjs';
import { changedPaths, matchesAny, normalise, runsFullLane, underPrefix } from './lib/scope-gate.mjs';
import { HARNESS_PATHS, MUTATION_PRODUCTION_GLOBS, PHASES } from './mutation-phases.mjs';

export const MUTATION_LANE_ID = 'quality.mutation';
export const MUTATION_REGISTRY_PATH = '.github/ci-lanes.json';
export const MUTATION_MIN_EFFICACY = 95;
const MUTATION_SEMANTIC_HARNESS_PATHS = new Set([MUTATION_REGISTRY_PATH]);

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

// canonicalFilePattern — the ONE RE2 alternation this table uses to name a set
// of scope-relative mutable files: `^(stem|stem|…)\.go$`, stems sorted and
// regex-escaped. Both directions of the include/exclude translation go through
// it, so a hand-written `include_files` and the `exclude_files` resolvePhases
// derives from it are the same shape by construction rather than by review.
// Returns null for an empty set — "names nothing" has no honest pattern.
export function canonicalFilePattern(rels) {
  const stems = [];
  for (const rel of rels) {
    if (!rel.endsWith('.go')) return null;
    stems.push(rel.slice(0, -3).replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
  }
  if (stems.length === 0) return null;
  return `^(${stems.sort().join('|')})\\.go$`;
}

// includeTightnessViolations — an `include_files` allowlist must be exactly the
// canonical pattern for the files it actually claims, no more and no less.
//
// An allowlist fails differently from a denylist, and silently. A denylist that
// names a since-deleted file over-excludes nothing (the name matches no path);
// an allowlist that names one just claims fewer files than its doc comment
// promises. Worse, a RENAME moves that file's whole mutant population out of
// the curated leg and into the scope's catch-all with no signal at all — the
// leg's efficacy denominator shifts underneath it while every check stays
// green, because the exactly-one-owner invariant is still satisfied. Requiring
// the pattern to re-derive from the real directory makes the drift a PR-time
// failure that prints the pattern to paste. Caught `late_mat` in phase2-range
// on its first run — deleted by cerberus #2830, still named in the allowlist.
function includeTightnessViolations(name, phase, root) {
  const directory = normalise(phase.scope);
  const walkProblems = [];
  const files = walkMutableGoFiles(root, directory, walkProblems);
  if (walkProblems.length > 0) return walkProblems;
  const includeRe = new RegExp(phase.include_files);
  const claimed = files.map((file) => file.slice(directory.length + 1)).filter((rel) => includeRe.test(rel));
  const canonical = canonicalFilePattern(claimed);
  if (canonical === phase.include_files) return [];
  return [
    `phase "${name}" \`include_files\` is not the canonical allowlist of the files it actually claims in ` +
      `${phase.scope} — it names a file that no longer exists, or is written in a non-canonical shape. ` +
      `An allowlist that has drifted from the tree shrinks a leg silently. Expected: ` +
      (canonical === null ? '(it claims no mutable Go file at all)' : `'${canonical}'`),
  ];
}

// tableViolations — everything wrong with the PHASES table, as strings.
export function tableViolations(phases, root = process.cwd()) {
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
    } else if (p.efficacy < MUTATION_MIN_EFFICACY) {
      problems.push(
        `phase "${name}" has efficacy ${p.efficacy}, below the committed minimum ${MUTATION_MIN_EFFICACY}`,
      );
    }
    if (!Number.isInteger(p.workers) || p.workers < 0) {
      problems.push(`phase "${name}" has a negative or non-integer \`workers\`: ${JSON.stringify(p.workers)}`);
    }

    if (p.include_files !== undefined && p.exclude_files !== undefined) {
      problems.push(
        `phase "${name}" declares both \`include_files\` and \`exclude_files\` — a leg owns its file set ` +
          `one way or the other, never both; the combination is ambiguous about which one gremlins would honour.`,
      );
    }
    if (p.exclude_files !== undefined) {
      problems.push(...filePatternViolations(name, 'exclude_files', p.exclude_files));
    }
    if (p.include_files !== undefined) {
      const patternProblems = filePatternViolations(name, 'include_files', p.include_files);
      problems.push(...patternProblems);
      // Tightness needs a compiled pattern and a real directory to walk. When
      // either is already broken the message above is the actionable one.
      if (patternProblems.length === 0 && p.scope && isDirectory(normalise(p.scope))) {
        problems.push(...includeTightnessViolations(name, p, root));
      }
    }
  }

  return problems;
}

function filePatternViolations(name, field, pattern) {
  const problems = [];
  if (typeof pattern !== 'string' || pattern === '') {
    problems.push(`phase "${name}" has an empty \`${field}\` — omit the key instead of matching nothing.`);
    return problems;
  }
  for (const { re, what } of NON_RE2_CONSTRUCTS) {
    if (re.test(pattern)) {
      problems.push(
        `phase "${name}" \`${field}\` uses ${what}, which Go's RE2 engine rejects. \`include_files\` is ` +
          `resolved into an \`exclude_files\` for gremlins (see resolvePhases), so this check applies to ` +
          `both fields identically — a non-RE2 construct would error the leg out at run time either way, ` +
          `after the checkout and toolchain setup have already been paid for.`,
      );
    }
  }
  try {
    new RegExp(pattern);
  } catch (e) {
    problems.push(`phase "${name}" \`${field}\` is not a valid regular expression: ${e.message}`);
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

// mutationPackageGlobs reads the mutation lane's independently declared source
// surface. Phase scopes are deliberately not consulted: if a complete phase is
// deleted, the registry still remembers the files that phase used to own.
export function mutationPackageGlobs(registry) {
  const problems = [];
  const lanes = Array.isArray(registry?.lanes) ? registry.lanes : [];
  const matches = lanes.filter((lane) => lane?.id === MUTATION_LANE_ID);
  if (matches.length !== 1) {
    problems.push(`registry must contain exactly one ${MUTATION_LANE_ID} lane (found ${matches.length})`);
    return { globs: [], problems };
  }
  const globs = matches[0].package_globs;
  if (!Array.isArray(globs) || globs.length === 0) {
    problems.push(`${MUTATION_LANE_ID}.package_globs must be a non-empty array`);
    return { globs: [], problems };
  }
  for (const glob of globs) {
    if (typeof glob !== 'string' || glob === '') {
      problems.push(`${MUTATION_LANE_ID}.package_globs contains an empty or non-string entry`);
      continue;
    }
    const stars = glob.indexOf('*');
    const invalidShape =
      /[?\[\]{}]/.test(glob) ||
      glob.startsWith('/') ||
      glob.split('/').some((segment) => segment === '' || segment === '.' || segment === '..') ||
      (stars !== -1 && (!glob.endsWith('/**') || glob.slice(0, -3).includes('*')));
    if (invalidShape) {
      problems.push(
        `${MUTATION_LANE_ID}.package_globs contains unsupported shape ${JSON.stringify(glob)}; ` +
          'only literal paths and directory/** globs are accepted',
      );
    }
  }
  if (problems.length === 0) {
    problems.push(...mutationProductionScopeViolations(globs));
  }
  return { globs, problems };
}

export function mutationProductionScopeViolations(globs, canonical = MUTATION_PRODUCTION_GLOBS) {
  const actual = [...new Set(globs.filter((glob) => glob.endsWith('/**')))].sort();
  const expected = [...new Set(canonical)].sort();
  if (JSON.stringify(actual) === JSON.stringify(expected)) return [];
  const missing = expected.filter((glob) => !actual.includes(glob));
  const extra = actual.filter((glob) => !expected.includes(glob));
  return [
    `${MUTATION_LANE_ID}.package_globs production scope differs from the canonical mutation surface` +
      (missing.length > 0 ? `; missing ${missing.join(', ')}` : '') +
      (extra.length > 0 ? `; extra ${extra.join(', ')}` : ''),
  ];
}

function sortedStrings(values) {
  return Array.isArray(values) ? [...new Set(values)].sort() : [];
}

function selectedObject(value, keys) {
  const out = {};
  for (const key of keys) out[key] = value?.[key] ?? null;
  return out;
}

// Project only fields that can change mutation ownership or execution. Editing
// another lane's metadata must not buy eighteen gremlins jobs, while changing
// this lane's package surface or entrypoint still exercises the full harness.
export function mutationRegistryProjection(source) {
  const registry = JSON.parse(source);
  const parsed = mutationPackageGlobs(registry);
  if (parsed.problems.length > 0) throw new Error(parsed.problems.join('; '));
  const lanes = registry.lanes.filter((lane) => lane?.id === MUTATION_LANE_ID);
  if (lanes.length !== 1) throw new Error(`expected exactly one ${MUTATION_LANE_ID} lane`);
  const lane = lanes[0];
  return {
    id: lane.id,
    owner: {
      workflow: lane.owner?.workflow ?? null,
      jobs: sortedStrings(lane.owner?.jobs),
      producer_jobs: sortedStrings(lane.owner?.producer_jobs),
      context_job: lane.owner?.context_job ?? null,
    },
    executions: sortedStrings(lane.executions),
    context: selectedObject(lane.context, ['match', 'name', 'protected']),
    recipes: sortedStrings(lane.recipes),
    command: lane.command ?? null,
    build_tags: sortedStrings(lane.build_tags),
    package_globs: sortedStrings(lane.package_globs),
    merge_posture: lane.merge_posture ?? null,
    main_posture: lane.main_posture ?? null,
    release_posture: lane.release_posture ?? null,
    applicability: selectedObject(lane.applicability, ['source', 'artifact']),
  };
}

function readRevisionFile(revision, path) {
  if (!/^[0-9a-f]{7,64}$/i.test(revision || '')) throw new Error(`invalid git revision for ${path}`);
  return execFileSync('git', ['show', `${revision}:${path}`], { encoding: 'utf8' });
}

export function mutationSemanticHarnessStatus({
  eventName,
  changed,
  baseSha,
  headSha,
  readAtRevision = readRevisionFile,
}) {
  const semanticPaths = [MUTATION_REGISTRY_PATH].filter((path) => changed?.has(path));
  if (semanticPaths.length === 0) return { changed: false, failed: false, paths: [] };
  if (eventName !== 'pull_request' && eventName !== 'merge_group') {
    return { changed: true, failed: false, paths: semanticPaths };
  }
  try {
    const paths = semanticPaths.filter((path) => {
      const before = mutationRegistryProjection(readAtRevision(baseSha, path));
      const after = mutationRegistryProjection(readAtRevision(headSha, path));
      return JSON.stringify(before) !== JSON.stringify(after);
    });
    return { changed: paths.length > 0, failed: false, paths };
  } catch (cause) {
    return { changed: false, failed: true, paths: semanticPaths, cause: cause.message };
  }
}

function globClaimsPath(glob, path) {
  if (!glob.endsWith('/**')) return normalise(path) === normalise(glob);
  return underPrefix(path, glob.slice(0, -3));
}

function registryClaimsPath(globs, path) {
  return globs.some((glob) => globClaimsPath(glob, path));
}

function walkMutableGoFiles(root, directory, problems) {
  const out = [];
  const absolute = join(root, directory);
  let entries;
  try {
    entries = readdirSync(absolute, { withFileTypes: true });
  } catch (e) {
    problems.push(`cannot enumerate mutation registry directory ${directory}: ${e.message}`);
    return out;
  }
  for (const entry of entries) {
    const child = join(directory, entry.name);
    if (entry.isDirectory()) {
      if (entry.name !== 'testdata') out.push(...walkMutableGoFiles(root, child, problems));
      continue;
    }
    if (entry.isFile() && entry.name.endsWith('.go') && !entry.name.endsWith('_test.go')) {
      out.push(normalise(child));
    }
  }
  return out;
}

export function registryMutableFiles({ registry, root = process.cwd() }) {
  const parsed = mutationPackageGlobs(registry);
  const problems = [...parsed.problems];
  const files = new Set();
  if (problems.length > 0) return { files, globs: parsed.globs, problems };
  for (const glob of parsed.globs) {
    if (glob.endsWith('/**')) {
      for (const file of walkMutableGoFiles(root, glob.slice(0, -3), problems)) files.add(file);
      continue;
    }
    if (glob.endsWith('.go') && !glob.endsWith('_test.go')) files.add(normalise(glob));
  }
  if (files.size === 0) problems.push(`${MUTATION_LANE_ID}.package_globs match no mutable Go files`);
  return { files, globs: parsed.globs, problems };
}

function phaseMutableFiles(phases, root, problems) {
  const files = new Set();
  for (const phase of phases) {
    if (typeof phase?.scope !== 'string' || phase.scope === '') continue;
    const directory = normalise(phase.scope);
    for (const file of walkMutableGoFiles(root, directory, problems)) files.add(file);
  }
  return files;
}

// ownershipViolations checks both directions against the registry-owned file
// universe: every owned source has exactly one phase, and no phase reaches
// source outside that universe.
export function ownershipViolations({ phases, registryFiles, root = process.cwd() }) {
  const problems = [];
  if (!Array.isArray(phases) || phases.length === 0) {
    return ['the mutation phase table is empty'];
  }
  const claimedFiles = phaseMutableFiles(phases, root, problems);
  const registryClaimCounts = new Map(phases.map((phase) => [phase, 0]));
  for (const file of registryFiles) {
    const ownerPhases = phases.filter((phase) => phaseClaims(phase, file));
    for (const phase of ownerPhases) registryClaimCounts.set(phase, registryClaimCounts.get(phase) + 1);
    const owners = ownerPhases.map((phase) => phase.phase);
    if (owners.length === 0) problems.push(`${file} is registry-owned but claimed by no mutation phase`);
    if (owners.length > 1) {
      problems.push(`${file} is registry-owned but claimed by ${owners.length} mutation phases (${owners.join(', ')})`);
    }
  }
  for (const file of claimedFiles) {
    const owners = phases.filter((phase) => phaseClaims(phase, file));
    if (owners.length > 0 && !registryFiles.has(file)) {
      problems.push(`${file} is claimed by mutation phase ${owners[0].phase} but is outside the registry surface`);
    }
  }
  for (const phase of phases) {
    if (registryClaimCounts.get(phase) === 0) {
      problems.push(`mutation phase ${phase.phase} claims zero registry-owned mutable Go files`);
    }
  }
  return problems;
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
  const rel = normalise(path).slice(normalise(phase.scope).length + 1);
  if (phase.include_files) return new RegExp(phase.include_files).test(rel);
  if (!phase.exclude_files) return true;
  return !new RegExp(phase.exclude_files).test(rel);
}

// resolvePhases — translate every `include_files` leg into an equivalent
// `exclude_files` gremlins (and the Go partition test's `dump` reader) can
// consume directly. RE2 has no way to express "match only this small set"
// without negative lookahead (NON_RE2_CONSTRUCTS bans it), so an include-based
// leg's real exclude pattern is computed by walking every mutable file
// actually in its scope and excluding whichever ones `include_files` does NOT
// match. The output never carries `include_files` — mutation.yml's
// `matrix.exclude_files` interpolation and the Go test's JSON parse both stay
// exactly as they are today.
//
// This is the one place a NEW file gets classified for free. Today, adding a
// file to a directory shared by an include-based leg and its siblings means
// walkMutableGoFiles sees it, it matches none of the static include patterns,
// so it lands in every include-based leg's DERIVED exclude — i.e. it is
// excluded from all of them — and falls through to whichever sibling in the
// group has no `include_files` (the scope's catch-all), the same way a file
// no exclude_files pattern names already falls through today. No hand edit
// needed anywhere, closing the gap that let a new chsql file collide across
// four legs until someone remembered to hand-patch three separate regexes.
export function resolvePhases(phases, root = process.cwd(), problems = []) {
  return phases.map((phase) => {
    if (!phase.include_files) return phase;
    const directory = normalise(phase.scope);
    const includeRe = new RegExp(phase.include_files);
    const excluded = [];
    for (const file of walkMutableGoFiles(root, directory, problems)) {
      const rel = file.slice(directory.length + 1);
      if (includeRe.test(rel)) continue;
      if (!rel.endsWith('.go')) {
        problems.push(`phase "${phase.phase}": derived exclude cannot express non-.go mutable file "${rel}"`);
        continue;
      }
      excluded.push(rel);
    }
    const { include_files: _includeFiles, ...rest } = phase;
    const derived = canonicalFilePattern(excluded);
    if (derived === null) return rest; // this leg's include already claims everything in scope
    return { ...rest, exclude_files: derived };
  });
}

// selectPhases — the phases this event must run, plus the reason, plus any
// changed path that fell inside a scope while claiming NO leg.
//
// A path in that last bucket is a real coverage gap: it sits in a package the
// mutation lane owns, yet every leg's exclude regex skips it, so no leg mutates
// it on ANY event. Reported rather than shrugged off — silently dropping it
// would let a leg partition drift into leaving a file permanently unmutated.
export function selectPhases({
  phases,
  harnessPaths,
  registryGlobs = [],
  eventName,
  headRef,
  changed,
  semanticHarness = { changed: false, failed: false, paths: [] },
}) {
  if (runsFullLane({ eventName, headRef })) {
    return { phases, reason: `event "${eventName}" always runs the full matrix`, gaps: [] };
  }
  if (changed === null) {
    return { phases, reason: 'the changed-path set could not be computed', gaps: [] };
  }
  if (semanticHarness.failed) {
    return {
      phases,
      reason: `a semantic harness projection could not be computed (${semanticHarness.cause || 'unknown error'})`,
      gaps: [],
    };
  }
  if (semanticHarness.changed) {
    return {
      phases,
      reason: `mutation-relevant harness material changed (${semanticHarness.paths.join(', ')})`,
      gaps: [],
    };
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
    (p) =>
      !MUTATION_SEMANTIC_HARNESS_PATHS.has(p) &&
      registryClaimsPath(registryGlobs, p) &&
      !phases.some((phase) => phaseClaims(phase, p)),
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

// changedLineProjection returns the exact merge-base ref and per-path added-line
// counts used by gremlins' native `--diff` filter. A malformed or unavailable
// projection is null: callers must run the selected phase in full.
export function changedLineProjection({ baseSha, headSha, runGit = git }) {
  const base = String(baseSha ?? '').trim();
  const head = String(headSha ?? '').trim();
  if (!/^[0-9a-f]{7,64}$/i.test(base) || !/^[0-9a-f]{7,64}$/i.test(head)) return null;

  const mergeBase = runGit(['merge-base', base, head]);
  const ref = String(mergeBase?.stdout ?? '').trim();
  if (mergeBase?.status !== 0 || !/^[0-9a-f]{40,64}$/i.test(ref)) return null;

  // `--no-renames` makes the NUL-delimited numstat grammar one fixed record
  // shape. A rename with content becomes delete + add, which safely treats the
  // new file as entirely changed rather than under-selecting its mutants.
  const diff = runGit(['diff', '--numstat', '-z', '--no-renames', ref, head]);
  if (diff?.status !== 0) return null;
  const additions = new Map();
  for (const record of String(diff.stdout ?? '').split('\0')) {
    if (record === '') continue;
    const fields = record.split('\t');
    if (fields.length !== 3 || fields[2] === '') return null;
    const [added, deleted, path] = fields;
    if ((added !== '-' && !/^[0-9]+$/.test(added)) || (deleted !== '-' && !/^[0-9]+$/.test(deleted))) {
      return null;
    }
    additions.set(normalise(path), added === '-' ? null : Number(added));
  }
  return { ref, additions };
}

function mutableProductionGo(path) {
  return path.endsWith('.go') && !path.endsWith('_test.go');
}

// addChangedLineRefs chooses incremental mutation independently per phase. A
// phase is incremental only when every changed path that selected it is a
// production Go file with at least one added line. Test, fixture, binary,
// harness, delete-only, and unknown changes deliberately keep the full phase:
// each can change the efficacy of mutants outside the edited lines.
export function addChangedLineRefs({ phases, changed, projection }) {
  return phases.map((phase) => {
    if (!(changed instanceof Set) || projection === null) return { ...phase, diff_ref: '' };
    const claimed = [...changed].filter((path) => phaseClaims(phase, path));
    const incremental =
      claimed.length > 0 &&
      claimed.every(
        (path) =>
          mutableProductionGo(path) &&
          Number.isSafeInteger(projection.additions.get(normalise(path))) &&
          projection.additions.get(normalise(path)) > 0,
      );
    return { ...phase, diff_ref: incremental ? projection.ref : '' };
  });
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

  let registry;
  try {
    registry = JSON.parse(readFileSync(MUTATION_REGISTRY_PATH, 'utf8'));
  } catch (e) {
    error(`mutation-matrix: cannot read ${MUTATION_REGISTRY_PATH}: ${e.message}`);
    process.exit(1);
  }
  const surface = registryMutableFiles({ registry });
  const problems = [...tableViolations(PHASES), ...surface.problems];
  if (problems.length === 0) {
    problems.push(...ownershipViolations({ phases: PHASES, registryFiles: surface.files }));
  }
  // resolvePhases only produces a well-formed table once PHASES itself is
  // sound (an include_files leg scoped to a non-directory, say, would make the
  // walk below meaningless) — but its own derivation can independently fail
  // (a non-.go mutable file the derived exclude cannot express), so its
  // problems are validated regardless of ownership state and folded into the
  // same fatal set.
  const resolvedPhases = resolvePhases(PHASES, process.cwd(), problems);
  for (const p of problems) error(`mutation-matrix: ${p}`);
  if (problems.length > 0) process.exit(1);

  // dump puts the table on stdout and NOTHING else, because a consumer parses
  // that stream as JSON. Validation still runs first: a dump of a table that
  // would have failed verify is worse than no dump at all. Resolved, not raw
  // PHASES: the Go partition test (and gremlins itself, via emit below) only
  // ever reads `exclude_files` — an include_files leg's derived equivalent
  // must reach both the same way a hand-written one would.
  if (mode === 'dump') {
    process.stdout.write(`${JSON.stringify(resolvedPhases, null, 2)}\n`);

    return;
  }

  log(`mutation-matrix: ${PHASES.length} phases, table sound.`);

  if (mode === 'verify') return;

  const eventName = (process.env.EVENT_NAME || '').trim();
  const headRef = (process.env.HEAD_REF || '').trim();
  const changed = runsFullLane({ eventName, headRef })
    ? null
    : changedPaths({ baseSha: process.env.BASE_SHA, headSha: process.env.HEAD_SHA });
  const semanticHarness =
    changed === null
      ? { changed: false, failed: false, paths: [] }
      : mutationSemanticHarnessStatus({
          eventName,
          changed,
          baseSha: process.env.BASE_SHA,
          headSha: process.env.HEAD_SHA,
        });

  // resolvedPhases, not PHASES: selectPhases' own claim-matching is identical
  // either way (resolvePhases changes how a leg's ownership is EXPRESSED, not
  // WHICH files it claims), but only the resolved form carries a real
  // `exclude_files` for the matrix JSON below to interpolate into gremlins'
  // own CLI invocation.
  const { phases, reason, gaps } = selectPhases({
    phases: resolvedPhases,
    harnessPaths: HARNESS_PATHS,
    registryGlobs: surface.globs,
    eventName,
    headRef,
    changed,
    semanticHarness,
  });

  for (const gap of gaps) {
    error(
      `mutation-matrix: "${gap}" lies inside a mutation phase scope but every leg's exclude_files skips it — ` +
        `no event mutates that file. Claim it in a leg (usually the scope's catch-all) or move it out of scope.`,
    );
  }
  if (gaps.length > 0) process.exit(1);

  const projection =
    changed === null
      ? null
      : changedLineProjection({
          baseSha: process.env.BASE_SHA,
          headSha: process.env.HEAD_SHA,
        });
  const matrixPhases = addChangedLineRefs({ phases, changed, projection });
  const incrementalCount = matrixPhases.filter((phase) => phase.diff_ref !== '').length;

  notice(
    `mutation-matrix: running ${phases.length}/${PHASES.length} phases — ${reason}; ` +
      `${incrementalCount} changed-line, ${matrixPhases.length - incrementalCount} full` +
      (phases.length > 0 ? ` (${phases.map((p) => p.phase).join(', ')})` : ''),
  );

  // `matrix` is the whole strategy.matrix object (mirrors compose-smoke-setup),
  // so mutation.yml interpolates it as `matrix: ${{ fromJSON(...) }}` rather
  // than reaching into a bare array. `has_phases` is emitted separately because
  // a job-level `if:` cannot introspect a matrix: without it, a SKIPPED `mutate`
  // is indistinguishable to the aggregator from a selection that was legitimately
  // empty — which is precisely the ambiguity this lane is being fixed to remove.
  setOutput('matrix', JSON.stringify({ include: matrixPhases }));
  setOutput('has_phases', String(phases.length > 0));
}

// Only dispatch when run as a script — importing for the unit test must not
// exit the test runner.
const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
