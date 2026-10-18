// rejection-parity-classification.mjs — names every rejection-parity catalogue
// entry that still needs a manual classification, and says how to classify it.
//
// `just update-parity-ledgers` regenerates test/rejection-parity/catalogue/.
// A new rejection site, or an `internal` entry whose evidence moved, comes out
// of that regeneration with an empty `class`. Classifying it is a human
// decision, so the regeneration does not fail on it: the curation gate is
// `TestCatalogueEntriesAreClassified` in `go test ./test/rejection-parity/`,
// which keeps refusing the entry until someone classifies it. This script is
// the regeneration's report of that pending work. It runs last in the recipe,
// so a local run and every update-golden.yml leg print the same list.
//
// Environment inputs:
//
//   CATALOGUE_DIR        (optional) the catalogue shard directory. Default
//                        test/rejection-parity/catalogue.
//   GITHUB_STEP_SUMMARY  (optional) set by the Actions runner; the report is
//                        appended to the job summary too.
//
// Exit 0 whether or not entries need classification. Exit 1 when the
// catalogue cannot be read, holds no shard, or a shard is not a valid
// catalogue document.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { appendStepSummary, error, log } from './lib/gh.mjs';
import { DEFAULT_CATALOGUE_DIR, mergeShards, readCatalogueShards } from './lib/rejection-catalogue.mjs';

/**
 * The classes an entry may carry, with the fields each one requires. Mirrors
 * the `Class*` constants in test/rejection-parity/catalogue.go;
 * rejection-parity-classification.test.mjs fails when the two sets differ.
 */
export const CLASSES = {
  rejection:
    'reachable from a parseable query that the reference backend also rejects; ' +
    'add "trigger_query" (and "endpoint" when the head default does not apply)',
  internal: 'not reachable from a wire query; add "rationale"',
  divergence:
    'rejected here but answered by the reference backend on purpose; add "trigger_query", ' +
    '"tracking_issue" (an open issue) and "since" (YYYY-MM-DD)',
};

/** The Go test that refuses an unclassified entry. */
export const CLASSIFICATION_GATE = 'TestCatalogueEntriesAreClassified';

/** The source file whose `Entry` doc comment is the field contract. */
const ENTRY_CONTRACT = 'test/rejection-parity/catalogue.go';

/**
 * Every entry of every shard under `dir` whose class is not one of CLASSES,
 * as `{ shard, site, head, message, class }`, in shard then entry order.
 * `shard` is `dir`-joined, so a repository-relative `dir` yields
 * repository-relative paths. Throws on an unreadable or malformed catalogue.
 */
export function unclassifiedEntries(dir) {
  const out = [];
  for (const shard of readCatalogueShards(dir)) {
    for (const entry of mergeShards([shard]).entries) {
      const cls = entry?.class ?? '';
      if (Object.hasOwn(CLASSES, cls)) continue;
      out.push({
        shard: shard.path,
        site: String(entry?.site ?? ''),
        head: String(entry?.head ?? ''),
        message: String(entry?.message ?? ''),
        class: String(cls),
      });
    }
  }
  return out;
}

/**
 * The report for `entries` as plain-text lines: which entries need a class,
 * the file each lives in, what each class requires, and how to verify.
 * Empty input yields an empty array.
 */
export function reportLines(entries) {
  if (entries.length === 0) return [];
  const lines = [
    `rejection-parity: ${entries.length} catalogue entr${entries.length === 1 ? 'y needs' : 'ies need'} ` +
      `a manual classification; ${CLASSIFICATION_GATE} fails until ${entries.length === 1 ? 'it is' : 'they are'} classified:`,
  ];
  for (const e of entries) {
    lines.push(`  - ${e.site} (${e.head}): "${e.message}"`);
    lines.push(`    in ${e.shard}`);
  }
  lines.push('Classify each by editing its entry in that file and setting "class" to one of:');
  for (const [cls, requires] of Object.entries(CLASSES)) {
    lines.push(`  ${cls.padEnd(10)}  ${requires}`);
  }
  lines.push(`The field contract is the Entry doc comment in ${ENTRY_CONTRACT}.`);
  lines.push(
    `Then verify: go test -count=1 -run '^${CLASSIFICATION_GATE}$' ./test/rejection-parity/ ` +
      'and the whole package, go test -count=1 ./test/rejection-parity/',
  );
  return lines;
}

/** The same report as job-summary markdown. Empty input yields ''. */
export function summaryMarkdown(entries) {
  if (entries.length === 0) return '';
  const md = [
    '### Rejection-parity entries need classification',
    '',
    `\`${CLASSIFICATION_GATE}\` fails until each entry below is classified by hand.`,
    '',
    '| Site | Head | Message | File |',
    '| ---- | ---- | ------- | ---- |',
  ];
  const cell = (v) => v.replace(/\|/g, '\\|');
  for (const e of entries) {
    md.push(`| \`${cell(e.site)}\` | ${cell(e.head)} | ${cell(e.message)} | \`${cell(e.shard)}\` |`);
  }
  md.push('', 'Set `"class"` in each entry to one of:', '');
  for (const [cls, requires] of Object.entries(CLASSES)) {
    md.push(`- \`${cls}\`: ${requires}`);
  }
  md.push(
    '',
    `The field contract is the \`Entry\` doc comment in \`${ENTRY_CONTRACT}\`. ` +
      'Verify with `go test -count=1 ./test/rejection-parity/`.',
    '',
  );
  return md.join('\n');
}

export function main(env = process.env) {
  const dir = env.CATALOGUE_DIR || DEFAULT_CATALOGUE_DIR;
  let entries;
  try {
    entries = unclassifiedEntries(dir);
  } catch (err) {
    error(`rejection-parity-classification: cannot read the catalogue at ${dir}: ${err.message}`);
    return 1;
  }
  if (entries.length === 0) {
    log('rejection-parity: every catalogue entry is classified.');
    return 0;
  }
  for (const line of reportLines(entries)) log(line);
  appendStepSummary(summaryMarkdown(entries), { quiet: true });
  return 0;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  process.exit(main());
}
