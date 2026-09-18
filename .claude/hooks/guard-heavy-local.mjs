#!/usr/bin/env node
// PreToolUse hook for Bash. Blocks two classes of heavy local run that belong
// to CI, not to this machine.
//
//   1. MUTATION TESTING (any `just mutate*` recipe, a direct `gremlins`
//      invocation, or `node …/mutation-run.mjs`).
//
//      A mutation run deliberately executes mutants, and a mutant that inverts
//      a loop advance never terminates while allocating per iteration. Run
//      uncapped on a developer box that is also hosting the agent session, a
//      single such mutant exhausts the machine. Measured on 2026-09-02:
//
//        05:01:05  Killed process (logpattern.test)  anon-rss: 26,398,156 kB
//        07:37:49  Killed process (MainThread)       anon-rss: 25,073,352 kB
//
//      Both were `constraint=CONSTRAINT_NONE` — a SYSTEM-WIDE out-of-memory, so
//      the kernel chose victims across the whole box and killed the Claude Code
//      session itself. Three agents were lost mid-task, twice.
//
//      This is not a hypothetical the leash protects against: bounding a
//      mutant's memory is the open work in cerberus issue #2919, and the leash
//      derivation that lets a runaway outlive its budget is #2903 / #2910. The
//      lane is the place to run this; a laptop is not.
//
//   2. GOLDEN REGENERATION (`just update-golden`, `just migration-golden`,
//      `just update-parity-ledgers`, any `just update-*-baseline`).
//
//      Regenerating locally drifts against CI: the goldens are generated with a
//      pinned toolchain, pinned build tags and `libchdb.so`, and the
//      `update-golden.yml` workflow pushes one trusted patch back onto the
//      branch. A local regen that differs by a formatting nuance or a stale
//      chdb is silently wrong, and every generated path is marked `-merge` in
//      `.gitattributes` precisely because a wrong one still parses.
//
// ESCAPE HATCH — deliberate, narrow, and it makes you say so.
//
//   CERBERUS_ALLOW_HEAVY_LOCAL=1 lifts the block for one command. Use it only
//   when CI genuinely cannot answer the question (debugging the workflow
//   itself), and for mutation ALSO cap the memory, e.g.
//
//     systemd-run --scope --user -p MemoryMax=2G -p MemorySwapMax=0 <cmd>
//
//   A cgroup kill is contained: the 07:31 run in the log above was
//   `constraint=CONSTRAINT_MEMCG` and harmed nothing, while the uncapped ones
//   took down the host.
//
// WHAT COUNTS AS RUNNING IT. The line is tokenised like a shell (see
// shell-words.mjs): quoted strings and heredoc bodies are data, `bash -c`
// payloads and `xargs` tails are commands, and only the command position of
// each segment is classified. A commit message that mentions `gremlins`, or
// a grep for `mutation-run.mjs`, is not a mutation run; `bash -c "just
// mutate"` and `echo pkg | xargs gremlins unleash` are. The recipe families
// are read from `just --summary`, so a new `mutate-*` or `update-*-baseline`
// recipe is guarded without editing this file.

import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

import { segments } from './shell-words.mjs';

const ALLOW = 0;
const BLOCK = 2;

const OVERRIDE_ENV = 'CERBERUS_ALLOW_HEAVY_LOCAL';

function readPayload() {
  try {
    return JSON.parse(readFileSync(0, 'utf8'));
  } catch {
    return null;
  }
}

// A `just` recipe invocation, ignoring just's own flags that take a value.
const JUST_OPTS_WITH_VALUE = new Set(['-f', '--justfile', '-d', '--working-directory', '--set']);

function justRecipe(w) {
  if (w[0] !== 'just') return null;
  let i = 1;
  while (i < w.length) {
    const a = w[i];
    if (JUST_OPTS_WITH_VALUE.has(a)) { i += 2; continue; }
    if (a.startsWith('-')) { i += 1; continue; }
    return a;
  }
  return null;
}

// The recipe families, by NAME SHAPE rather than by a hand list: every
// `mutate*` recipe is a gremlins run (`mutate`, `mutate-pkg`, `mutate-chdb` —
// the last was the heaviest run in the repo and the old list did not name it),
// and every recipe that rewrites a checked-in generated artefact is one of
// `update-golden`, `migration-golden`, `update-parity-ledgers` or an
// `update-*-baseline`. The live set comes from `just --summary` in the
// session's project directory; when `just` cannot answer, RECIPE_FALLBACK is
// the last known set, so the guard never degrades to "nothing is heavy".
const MUTATION_RECIPE = /^mutate(-|$)/;
const GOLDEN_RECIPE = /^(update-golden|migration-golden|update-parity-ledgers|update-.+-baseline)$/;
const RECIPE_FALLBACK = [
  'mutate', 'mutate-chdb', 'mutate-pkg',
  'migration-golden', 'update-cardinality-baseline', 'update-golden',
  'update-metadata-query-size-baseline', 'update-nightly-perf-baseline',
  'update-parity-enrolment-baseline', 'update-parity-ledgers', 'update-perf-smoke-baseline',
  'update-scale-wall-baseline', 'update-solver-decision-baseline',
];

function justRecipes(cwd) {
  try {
    const out = execFileSync('just', ['--summary'], { cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] });
    const names = out.trim().split(/\s+/).filter(Boolean);
    if (names.length > 0) return names;
  } catch {
    // `just` absent, or no Justfile under cwd: fall through to the fallback.
  }
  return RECIPE_FALLBACK;
}

export function recipeSets(names) {
  return {
    mutation: new Set(names.filter((n) => MUTATION_RECIPE.test(n))),
    golden: new Set(names.filter((n) => GOLDEN_RECIPE.test(n))),
  };
}

// classify — one executed segment (wrappers already stripped, `bash -c` and
// `xargs` already unwrapped by shell-words.mjs). Only the COMMAND position
// decides: a word that merely mentions a runner's file name — a grep for it,
// a commit message quoting it — is data, not an execution.
export function classify(w, sets) {
  if (w.length === 0) return null;

  const recipe = justRecipe(w);
  if (recipe && sets.mutation.has(recipe)) return 'mutation';
  if (recipe && sets.golden.has(recipe)) return 'golden';

  const cmd = w[0];
  // A direct gremlins invocation, however it is spelled on PATH.
  if (cmd === 'gremlins' || cmd.endsWith('/gremlins')) return 'mutation';

  // The lane's own runner, invoked directly (`node .github/scripts/mutation-run.mjs`).
  const isNode = cmd === 'node' || cmd.endsWith('/node');
  const script = isNode ? w.slice(1).find((a) => !a.startsWith('-')) : cmd;
  if (script && /(^|\/)mutation-run\.mjs$/.test(script)) return 'mutation';

  return null;
}

export function kindsOf(command, sets) {
  const kinds = new Set();
  for (const w of segments(command)) {
    const kind = classify(w, sets);
    if (kind) kinds.add(kind);
  }
  return kinds;
}

function block(lines) {
  process.stderr.write(`${lines.join('\n')}\n`);
  return BLOCK;
}

const MUTATION_MESSAGE = [
  'guard-heavy-local: refusing to run mutation testing on this machine.',
  '',
  'A mutant that never terminates allocates until the kernel intervenes. On',
  '2026-09-02 this took 26 GB on a 31 GB host and triggered a SYSTEM-WIDE OOM',
  '(constraint=CONSTRAINT_NONE), killing the Claude Code session and three',
  'agents mid-task — twice. Bounding a mutant\'s memory is still open work',
  '(cerberus #2919); the leash that lets a runaway outlive its budget is #2903 /',
  '#2910. Until those land, the CI lane is the only safe place to run this.',
  '',
  'Use instead:',
  '  - the `mutation` lane on a PR, which shards the matrix across runners;',
  '  - `gh run view <id> --log` to read a leg\'s survivors, rather than',
  '    reproducing the whole leg locally.',
  '',
  'If you genuinely must run it here (e.g. debugging the runner itself), cap it:',
  '  CERBERUS_ALLOW_HEAVY_LOCAL=1 systemd-run --scope --user \\',
  '    -p MemoryMax=2G -p MemorySwapMax=0 <command>',
];

const GOLDEN_MESSAGE = [
  'guard-heavy-local: refusing to regenerate goldens on this machine.',
  '',
  'Goldens are generated with a pinned toolchain, pinned build tags and',
  'libchdb.so, and `update-golden.yml` pushes one trusted patch back onto the',
  'branch. A local regen that differs by a stale chdb or a formatting nuance is',
  'silently wrong — every generated path is marked `-merge` in .gitattributes',
  'precisely because a wrong one still parses.',
  '',
  'Use instead:',
  '  gh workflow run update-golden.yml -f shards=<shard...> -f branch=<branch>',
  '',
  'Then FREEZE the branch until it publishes: the workflow pins the dispatched',
  'TARGET_SHA and refuses if the head has moved, so pushing meanwhile throws the',
  'whole run away.',
  '',
  'Override (only when debugging the workflow itself):',
  '  CERBERUS_ALLOW_HEAVY_LOCAL=1 <command>',
];

function main() {
  const payload = readPayload();
  if (!payload || payload.tool_name !== 'Bash') return ALLOW;

  const command = payload?.tool_input?.command;
  if (typeof command !== 'string' || command.length === 0) return ALLOW;

  // The override is read from the hook's own environment, so a caller that
  // exports it for the session, or prefixes it on the line, both work.
  const overridden = process.env[OVERRIDE_ENV] === '1' || /\bCERBERUS_ALLOW_HEAVY_LOCAL=1\b/.test(command);

  const cwd = payload.cwd && typeof payload.cwd === 'string' ? payload.cwd : process.cwd();
  const kinds = kindsOf(command, recipeSets(justRecipes(cwd)));
  if (kinds.size === 0) return ALLOW;
  if (overridden) return ALLOW;

  if (kinds.has('mutation')) return block(MUTATION_MESSAGE);
  return block(GOLDEN_MESSAGE);
}

// Only dispatch when run as the hook — the node:test suite imports the pure
// classifier without firing it.
if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  process.exit(main());
}
