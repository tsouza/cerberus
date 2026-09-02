// lane-harness.mjs — the set of repository files a workflow lane EXECUTES,
// DERIVED from the workflow rather than hand-listed.
//
// Why this exists
// ---------------
// A diff-scoped lane (see `lib/scope-gate.mjs`) selects the legs whose scope a
// change touches. That is only sound while changes to the lane's OWN machinery
// select everything: a file the runner executes has matrix-wide reach, so a PR
// editing it must run the whole matrix or the lane reports green over work its
// selection never covered.
//
// `mutation-phases.mjs` expressed that set as a hand-written `HARNESS_PATHS`
// array, and it rotted exactly the way a hand-written list of paths rots. When
// `mutant-memory-guard.mjs` landed (cerberus #2919 / #2921) it was wired into
// every leg's `go test -exec`, where it decides each mutant's fate — and it was
// never added to the array. A PR editing only the guard, including the one that
// resized its hold (#2947), therefore moved every leg's numbers while selecting
// NO leg (cerberus #2948). `lib/gh.mjs`, `go-module-fetch.mjs`,
// `lib/registry.mjs` and the `setup-go` composite action were missing for the
// same reason: nobody remembered to append them.
//
// So the array stops being written. The closure below is computed from the
// artefacts that already state what the lane runs — the workflow's own `node`
// invocations and `uses: ./.github/actions/…` steps, then the module graph
// reachable from those entry points. Adding a step, an import, or a spawned
// helper extends the harness set in the same commit that adds it, with nothing
// to remember.
//
// What it deliberately does NOT cover
// -----------------------------------
//   - Data the lane READS rather than executes (a tool's config file, `go.mod`).
//     No source scan can find those; the caller declares them alongside this
//     closure, and that short list is reviewable precisely because it is short.
//   - `*.test.mjs` suites. A unit test of a harness script is not run by the
//     lane and cannot change a single mutant's verdict; it is reachable from no
//     entry point, so it never enters the closure. Selecting the full matrix on
//     a test-only edit would spend every leg to prove nothing.
//
// This module is pure: every input is a file path under `root`, and the result
// is a sorted array of repo-relative paths.
//
// node: builtins only — no npm deps, no setup-node needed.

import { existsSync, readFileSync } from 'node:fs';
import { posix } from 'node:path';
import { fileURLToPath } from 'node:url';

// `node <script>` as a workflow actually spells it: bare, quoted, or rooted at
// `$GITHUB_WORKSPACE` — the form a COMPOSITE action must use, because its own
// `run:` steps inherit the caller's working directory rather than the repo
// root. Anchoring on the `node` verb is what keeps a script a comment merely
// NAMES out of the closure: `mutation.yml`'s header points at
// `compose-smoke-matrix.mjs` as a shape to compare against, and that file is
// not part of this lane.
const NODE_INVOCATION =
  /\bnode\s+["']?(?:\$\{?GITHUB_WORKSPACE\}?\/|\$\{\{\s*github\.workspace\s*\}\}\/)?(\.github\/scripts\/[A-Za-z0-9._/-]+\.mjs)/g;

// A local composite action step: `uses: ./.github/actions/<name>`. Its manifest
// is `<name>/action.yml`, and the manifest's own `node` invocations are entry
// points of the lane exactly as the workflow's are.
const LOCAL_ACTION_USE = /\buses:\s*(\.\/\.github\/actions\/[A-Za-z0-9._/-]+)/g;

// An entry point this module does NOT model: a checked-in script run through an
// interpreter other than `node`, which has no `.mjs` graph to walk. Nothing in
// any lane wired to this module uses one today, and a derivation that quietly
// ignored the first one would be back to under-reporting the harness — the
// failure that made the hand-written list wrong. So it FAILS instead, naming
// the file, and whoever adds the step extends the model in the same commit.
// `shell: bash` and friends do not match: a path under a scripts directory must
// follow the interpreter.
const UNMODELLED_INVOCATION =
  /\b(?:ba|z)?sh\s+["']?(?:\$\{?GITHUB_WORKSPACE\}?\/|\$\{\{\s*github\.workspace\s*\}\}\/)?((?:\.github\/)?scripts\/[A-Za-z0-9._/-]+)/g;

// A module reference whose ENTIRE string literal is a path ending in `.mjs`.
// That covers both edge kinds a script uses to reach another script: the
// `import … from './lib/gh.mjs'` specifier, and the bare
// `'mutant-memory-guard.mjs'` that `mutation-run.mjs` resolves against its own
// directory to hand to `go test -exec`. Requiring the whole literal to be the
// path is what excludes prose that merely names a script — an `::error::`
// message or a thrown `Error` string — from the graph.
const MODULE_LITERAL = /(['"`])([A-Za-z0-9._/-]+\.mjs)\1/g;

// The repository root, derived from this module's own location
// (`<root>/.github/scripts/lib/lane-harness.mjs`) rather than from
// `process.cwd()`, so the closure is the same whichever directory a caller,
// a test, or a workflow step happens to run from.
export const REPO_ROOT = fileURLToPath(new URL('../../../', import.meta.url));

function stripDotSlash(p) {
  return String(p ?? '').replace(/^\.\//, '');
}

// resolveReference — where a `.mjs` literal found inside `fromPath` points.
// A literal that already starts at `.github/` is repo-relative (that is how a
// workflow names a script); anything else is resolved against the referring
// file's own directory, which is how both ESM specifiers and the guard's
// `dirname(import.meta.url)`-relative spawn path behave.
export function resolveReference(fromPath, reference) {
  if (reference.startsWith('.github/')) return reference;
  return posix.normalize(posix.join(posix.dirname(fromPath), reference));
}

// laneHarnessClosure — every repo file the lane at `workflow` executes.
//
// The workflow itself, each local composite action manifest it uses, every
// script those invoke with `node`, and the transitive `.mjs` graph reachable
// from them. Sorted, so two callers reading the same tree get byte-identical
// answers.
//
// A `node`-invoked script that does not exist is a broken workflow and throws:
// silently dropping an entry point would put this module back in the business
// of under-reporting the harness. A transitive literal that resolves to no file
// is simply not an edge and is skipped.
export function laneHarnessClosure({
  workflow,
  root = REPO_ROOT,
  readFile = (p) => readFileSync(posix.join(root, p), 'utf8'),
  fileExists = (p) => existsSync(posix.join(root, p)),
}) {
  const harness = new Set();
  const manifests = [stripDotSlash(workflow)];
  const roots = [];

  for (const manifest of manifests) {
    if (harness.has(manifest)) continue;
    harness.add(manifest);
    const text = readFile(manifest);
    for (const [, action] of text.matchAll(LOCAL_ACTION_USE)) {
      const actionManifest = posix.join(stripDotSlash(action), 'action.yml');
      if (!harness.has(actionManifest) && fileExists(actionManifest)) manifests.push(actionManifest);
    }
    for (const [, script] of text.matchAll(NODE_INVOCATION)) {
      if (!fileExists(script)) {
        throw new Error(`lane-harness: ${manifest} runs "${script}", which does not exist`);
      }
      roots.push(script);
    }
    for (const [, script] of text.matchAll(UNMODELLED_INVOCATION)) {
      throw new Error(
        `lane-harness: ${manifest} runs "${script}" through a shell, an entry-point form this module ` +
          'does not model — teach lane-harness.mjs to walk it rather than leaving it out of the harness set',
      );
    }
  }

  const queue = [...roots];
  while (queue.length > 0) {
    const script = queue.shift();
    if (harness.has(script)) continue;
    harness.add(script);
    const source = readFile(script);
    for (const [, , reference] of source.matchAll(MODULE_LITERAL)) {
      const target = resolveReference(script, reference);
      if (!harness.has(target) && fileExists(target)) queue.push(target);
    }
  }

  return [...harness].sort();
}
