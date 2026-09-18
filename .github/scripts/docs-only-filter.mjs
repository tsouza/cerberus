// docs-only-filter.mjs — the two halves of the `docs_only` decision behind
// `./.github/actions/docs-only`, the composite every workflow with a docs-only
// short-circuit uses (ci.yml, compatibility.yml, chdb.yml,
// schema-integration.yml, strict-scan.yml, agpl-oracle.yml).
//
// The set of paths a pull request can touch without making any lane relevant
// is declared ONCE, in `.github/ci-lanes.json`'s
// `impact_selection.known_nonimpact_globs` (quickstart-canary.mjs's `select`
// step reads the same list). Each of the six workflows used to carry its own
// ten-line copy of that list as a `dorny/paths-filter` block, and the copies
// had already drifted from the registry (`**/*.mdx`, `NOTICE*` and the
// unsuffixed `AGENTS*` / `CHANGELOG*` / `CLAUDE*` forms were in the
// registry only). This script renders the filter from the registry, so the
// six workflows and the canary cannot disagree.
//
// MODE=filters   render the paths-filter `filters:` input: a `code` key that
//                matches every changed file (`**`) MINUS every registry glob
//                (`!<glob>`), which under `predicate-quantifier: every` is
//                "any changed file that is not documentation". Written to
//                $GITHUB_OUTPUT as the multi-line `filters` output.
// MODE=compute   turn the filter's verdict into `docs_only`: `true` only on a
//                pull_request whose `code` filter came back `false`. Every
//                other event (push, schedule, dispatch, merge_group) is
//                `false`, so the heavy jobs run — the safe default for any
//                event whose diff is not a clean two-commit range.
//
// Env:
//   MODE            filters | compute (required)
//   CI_LANE_REGISTRY registry path (default .github/ci-lanes.json) — filters
//   EVENT_NAME      github.event_name — compute
//   CODE_CHANGED    the paths-filter `code` output ('true' / 'false' / '') — compute
//   GITHUB_OUTPUT   where the outputs go; logged when absent.

import { appendFileSync } from 'node:fs';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';

import { DEFAULT_REGISTRY_PATH, loadRegistry } from './ci-lane-contract.mjs';
import { error, log, setOutput } from './lib/gh.mjs';

// FILTER_KEY is the one paths-filter key the action reads back.
export const FILTER_KEY = 'code';

// filtersYAML — the `filters:` input for dorny/paths-filter, from the
// registry's non-impact globs.
export function filtersYAML(nonimpactGlobs) {
  if (!Array.isArray(nonimpactGlobs) || nonimpactGlobs.length === 0) {
    throw new Error('the registry declares no known_nonimpact_globs; the docs-only filter would match nothing');
  }
  const lines = [`${FILTER_KEY}:`, "  - '**'"];
  for (const glob of nonimpactGlobs) {
    if (glob.includes("'") || glob.includes('\n')) throw new Error(`unrenderable non-impact glob ${JSON.stringify(glob)}`);
    lines.push(`  - '!${glob}'`);
  }
  return `${lines.join('\n')}\n`;
}

// docsOnly — the verdict. Only a pull request can be docs-only, and only when
// the filter positively said no code file changed.
export function docsOnly({ eventName, codeChanged }) {
  return eventName === 'pull_request' && codeChanged === 'false';
}

// setMultilineOutput — a heredoc-delimited output, the runner's form for a
// value with newlines.
function setMultilineOutput(name, value) {
  const file = process.env.GITHUB_OUTPUT;
  if (!file) {
    log(`${name}<<EOF\n${value}EOF`);
    return;
  }
  appendFileSync(file, `${name}<<DOCS_ONLY_FILTERS_EOF\n${value}DOCS_ONLY_FILTERS_EOF\n`);
}

function main() {
  const mode = process.env.MODE;
  if (mode === 'filters') {
    const registry = loadRegistry(process.env.CI_LANE_REGISTRY || DEFAULT_REGISTRY_PATH, { root: process.cwd() });
    setMultilineOutput('filters', filtersYAML(registry.impact_selection.known_nonimpact_globs));
    return;
  }
  if (mode === 'compute') {
    const verdict = docsOnly({ eventName: process.env.EVENT_NAME ?? '', codeChanged: process.env.CODE_CHANGED ?? '' });
    setOutput('docs_only', verdict ? 'true' : 'false');
    return;
  }
  throw new Error(`MODE must be filters or compute; got ${JSON.stringify(mode)}`);
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (e) {
    error(`docs-only-filter: ${e.message}`);
    process.exit(1);
  }
}
