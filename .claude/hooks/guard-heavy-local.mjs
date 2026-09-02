#!/usr/bin/env node
// PreToolUse hook for Bash. Blocks two classes of heavy local run that belong
// to CI, not to this machine.
//
//   1. MUTATION TESTING (`just mutate`, `just mutate-pkg`, a direct `gremlins`
//      invocation, or `mutation-run.mjs`).
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
//   2. GOLDEN REGENERATION (`just update-golden`, `just update-cardinality-baseline`).
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

import { readFileSync } from 'node:fs';

const ALLOW = 0;
const BLOCK = 2;

const OVERRIDE_ENV = 'CERBERUS_ALLOW_HEAVY_LOCAL';

// Command wrappers that may precede the real command on the line. Mirrors
// guard-git.mjs so `rtk just mutate` and friends are still seen.
const COMMAND_WRAPPERS = new Set(['rtk', 'proxy', 'command', 'sudo', 'time', 'nice', 'env', 'timeout']);

function readPayload() {
  try {
    return JSON.parse(readFileSync(0, 'utf8'));
  } catch {
    return null;
  }
}

// splitSegments — break a shell line into independently-executed segments so a
// guarded command buried in an `&&` chain is still seen. Quoting is not
// modelled; a rare false positive costs one explanatory message, a false
// negative costs the machine.
function splitSegments(command) {
  return command.split(/&&|\|\||[;\n|]/g);
}

// words — the segment's words with leading wrappers and `VAR=value` prefixes
// stripped, so `env FOO=1 rtk just mutate` reduces to ['just','mutate'].
function words(segment) {
  const out = segment.trim().split(/\s+/).filter(Boolean);
  let i = 0;
  while (i < out.length && (COMMAND_WRAPPERS.has(out[i]) || /^[A-Za-z_][A-Za-z0-9_]*=/.test(out[i]))) i++;
  return out.slice(i);
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

// Recipes whose whole purpose is a mutation run.
const MUTATION_RECIPES = new Set(['mutate', 'mutate-pkg']);
// Recipes that regenerate a checked-in generated artefact.
const GOLDEN_RECIPES = new Set(['update-golden', 'update-cardinality-baseline']);

function classify(segment) {
  const w = words(segment);
  if (w.length === 0) return null;

  const recipe = justRecipe(w);
  if (recipe && MUTATION_RECIPES.has(recipe)) return 'mutation';
  if (recipe && GOLDEN_RECIPES.has(recipe)) return 'golden';

  // A direct gremlins invocation, however it is spelled on PATH.
  const cmd = w[0];
  if (cmd === 'gremlins' || cmd.endsWith('/gremlins')) return 'mutation';

  // The lane's own runner, invoked directly.
  if (w.some((a) => a.includes('mutation-run.mjs'))) return 'mutation';

  return null;
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

  const kinds = new Set();
  for (const segment of splitSegments(command)) {
    const kind = classify(segment);
    if (kind) kinds.add(kind);
  }
  if (kinds.size === 0) return ALLOW;
  if (overridden) return ALLOW;

  if (kinds.has('mutation')) return block(MUTATION_MESSAGE);
  return block(GOLDEN_MESSAGE);
}

process.exit(main());
