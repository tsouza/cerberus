#!/usr/bin/env node
/**
 * Crawl surface-inventory PURITY gate (tsouza/cerberus#1872).
 *
 * The surface-inventory pin is only meaningful if a crawled surface is
 * a pure function of the Grafana APPLICATION — same app, same
 * surfaces. Three axes had leaked into it before this gate existed,
 * each closed by its own patch and each followed by another: the
 * seeded dataset (log field names, dashboard tags, span attribute
 * names, detected severity levels) and the order pages happened to be
 * visited. This gate replaces "patch axis five" with a property the
 * committed bytes either have or do not:
 *
 *   DATA PURITY — every interaction fragment in every committed
 *      inventory keys a value that is either the representative
 *      placeholder, or a member of a CLOSED option set declared for
 *      that control. A control earns a verbatim key two ways, both
 *      checked: its discovery key already embeds the vocabulary
 *      (`radio[Grid|Rows]`, `tabs[a|b|c]` — the value half says
 *      nothing the key half has not), or
 *      test/e2e/playwright/crawl/control-vocabularies.json names it.
 *      Nothing else may appear. A dataset column added tomorrow
 *      therefore cannot change the pin: there is no literal in it that
 *      came from a query result.
 *
 * This is the seed half of the epic's equivalence property, decided
 * offline on the committed artefact instead of by crawling twice: a
 * different dataset can only reach the pin through a fragment value,
 * and after this gate there is no fragment value it could reach.
 *
 * The VISIT-ORDER half is not decidable here and this gate does not
 * pretend otherwise. Re-sorting the committed rows and finding them
 * unchanged would prove nothing — the marshaller sorts, so that
 * assertion cannot fail and would be coverage in name only. Order
 * independence is a property of the crawl's fold (a canonical's
 * representative is picked from the candidate SET, and lean membership
 * is unioned across candidates rather than read off the fold winner),
 * and it is pinned by the browser-free "crawl: canonicalization pins"
 * specs, which the same CI job runs alongside this gate.
 *
 * The declarations are read from the SAME manifest the crawler
 * enforces at gesture-planning time (interactions.ts), so this gate
 * cannot certify a pin the crawler would never produce. The crawler
 * additionally re-checks each declared set against the live app on
 * every run, which is what keeps a declaration honest.
 *
 * Env:
 *   CRAWL_DIR — directory holding the inventories and the manifest.
 *               Defaults to test/e2e/playwright/crawl relative to the
 *               repository root.
 *
 * Exit 1 on any violation, with the offending file, row and reason.
 */

import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const INVENTORY_PREFIX = 'grafana-surface-inventory.';
const INVENTORY_SUFFIX = '.json';
const MANIFEST_FILENAME = 'control-vocabularies.json';
const DEFAULT_CRAWL_DIR = join(
  'test',
  'e2e',
  'playwright',
  'crawl',
);

/**
 * An interaction fragment, as `interactionStateKey` mints it:
 * `<canonical>#<control-key>=<state-value>`. The control key is
 * `<kind>` or `<kind>[<identity>]`, optionally carrying
 * assignUniqueKeys' `#<n>` disambiguation suffix. The identity is
 * matched greedily so a key that legitimately contains `=` — the
 * adhoc-filter builder's `adhoc[+ label = value]` — splits at the
 * right place rather than at its first `=`.
 */
const FRAGMENT = /^([a-z-]+(?:\[[\s\S]*\])?(?:#\d+)?)=([\s\S]*)$/;

/** The `a|b|c` a `tabs[…]` / `radio[…]` key carries in itself. */
export function keyEmbeddedVocabulary(key) {
  const m = /^(?:tabs|radio)\[([\s\S]+)\](?:#\d+)?$/.exec(key);
  return m === null ? undefined : m[1].split('|');
}

/** Split a pinned surface URL into its canonical half and fragment. */
export function splitFragment(url) {
  const hash = url.indexOf('#');
  if (hash < 0) return undefined;
  return { canonical: url.slice(0, hash), fragment: url.slice(hash + 1) };
}

export function loadManifest(dir) {
  const raw = readFileSync(join(dir, MANIFEST_FILENAME), 'utf8');
  const parsed = JSON.parse(raw);
  const problems = [];
  if (
    typeof parsed.representativePlaceholder !== 'string' ||
    parsed.representativePlaceholder === ''
  ) {
    problems.push(
      `${MANIFEST_FILENAME}: representativePlaceholder must be a non-empty string`,
    );
  }
  for (const field of ['closedVocabularies', 'exhaustiveDataControls']) {
    if (!Array.isArray(parsed[field])) {
      problems.push(`${MANIFEST_FILENAME}: ${field} must be an array`);
    }
  }
  if (problems.length > 0) return { problems };

  const vocabularies = [];
  for (const [i, entry] of parsed.closedVocabularies.entries()) {
    const at = `${MANIFEST_FILENAME}: closedVocabularies[${i}]`;
    if (typeof entry.control !== 'string' || entry.control === '') {
      problems.push(`${at}: control must be a non-empty regex source string`);
      continue;
    }
    // A declaration without a rationale is an assertion nobody can
    // review — the whole mechanism is a claim about the app that a
    // human has to be able to check.
    if (typeof entry.rationale !== 'string' || entry.rationale.trim() === '') {
      problems.push(
        `${at} (${entry.control}): every declaration needs a rationale saying why this option set is app structure and not seeded data`,
      );
    }
    if (
      !Array.isArray(entry.values) ||
      entry.values.length === 0 ||
      entry.values.some((v) => typeof v !== 'string')
    ) {
      problems.push(
        `${at} (${entry.control}): values must be a non-empty array of option labels — a declaration with no closed set is exactly the hole #1872 closed`,
      );
      continue;
    }
    let control;
    try {
      control = new RegExp(entry.control);
    } catch (err) {
      problems.push(`${at}: control is not a valid regex — ${err.message}`);
      continue;
    }
    vocabularies.push({ control, values: entry.values });
  }
  for (const [i, entry] of parsed.exhaustiveDataControls.entries()) {
    const at = `${MANIFEST_FILENAME}: exhaustiveDataControls[${i}]`;
    if (typeof entry.control !== 'string' || entry.control === '') {
      problems.push(`${at}: control must be a non-empty regex source string`);
    }
    if (typeof entry.rationale !== 'string' || entry.rationale.trim() === '') {
      problems.push(`${at}: every entry needs a rationale`);
    }
  }
  return {
    problems,
    placeholder: parsed.representativePlaceholder,
    vocabularies,
  };
}

/** The closed set a control may key verbatim, or undefined. */
export function declaredVocabulary(key, vocabularies) {
  const declared = vocabularies.find((v) => v.control.test(key));
  if (declared !== undefined) return declared.values;
  return keyEmbeddedVocabulary(key);
}

/** Codepoint order — the crawl's own tie-breaker (lib.ts byCodepoint). */
export function byCodepoint(a, b) {
  if (a < b) return -1;
  return a > b ? 1 : 0;
}

/**
 * Every violation in one inventory. An empty array means no literal in
 * the pin came from a query result.
 */
export function checkInventory(file, inv, manifest) {
  const problems = [];
  if (!Array.isArray(inv.surfaces)) {
    return [`${file}: surfaces must be an array`];
  }

  for (const surface of inv.surfaces) {
    const url = surface?.url;
    if (typeof url !== 'string') continue; // shape is the guard's job
    const split = splitFragment(url);
    if (split === undefined) continue;
    const m = FRAGMENT.exec(split.fragment);
    if (m === null) {
      problems.push(
        `${file}: ${JSON.stringify(url)} has a fragment this gate cannot parse (${JSON.stringify(split.fragment)}) — an interaction state is written <canonical>#<control-key>=<value>`,
      );
      continue;
    }
    const [, key, value] = m;
    if (value === manifest.placeholder) continue;
    const vocabulary = declaredVocabulary(key, manifest.vocabularies);
    if (vocabulary === undefined) {
      problems.push(
        `${file}: ${JSON.stringify(url)} pins the literal option ${JSON.stringify(value)} for control ${JSON.stringify(key)}, which declares no closed vocabulary. ` +
          `An undeclared control's options come from a datasource query, so pinning one makes the inventory a function of the seeded dataset rather than of the app. ` +
          `Either the control's option set really is fixed by the pinned app version — declare it in ${MANIFEST_FILENAME} with a rationale, and the crawl will re-check the declaration against the live app on every run — or it is data, and the crawler must key it ${JSON.stringify(manifest.placeholder)}.`,
      );
      continue;
    }
    if (!vocabulary.includes(value)) {
      problems.push(
        `${file}: ${JSON.stringify(url)} pins option ${JSON.stringify(value)} for control ${JSON.stringify(key)}, which is outside that control's declared closed set [${vocabulary.map((v) => JSON.stringify(v)).join(', ')}]. ` +
          `Either the pinned app version gained an option (widen the declaration in ${MANIFEST_FILENAME} deliberately and regenerate) or the set is not closed and the declaration must go.`,
      );
    }
  }

  return problems;
}

export function run(dir) {
  const manifest = loadManifest(dir);
  if (manifest.problems.length > 0) return manifest.problems;

  const files = readdirSync(dir)
    .filter(
      (f) => f.startsWith(INVENTORY_PREFIX) && f.endsWith(INVENTORY_SUFFIX),
    )
    .sort(byCodepoint);
  if (files.length === 0) {
    return [
      `${dir}: found no ${INVENTORY_PREFIX}*${INVENTORY_SUFFIX} files — a gate that reads nothing checks nothing`,
    ];
  }

  const problems = [];
  for (const file of files) {
    let inv;
    try {
      inv = JSON.parse(readFileSync(join(dir, file), 'utf8'));
    } catch (err) {
      problems.push(`${file}: not parseable JSON — ${err.message}`);
      continue;
    }
    problems.push(...checkInventory(file, inv, manifest));
  }
  return { problems, files };
}

const isMain =
  process.argv[1] !== undefined &&
  import.meta.url === `file://${process.argv[1]}`;
if (isMain) {
  const dir = process.env.CRAWL_DIR ?? DEFAULT_CRAWL_DIR;
  const result = run(dir);
  const problems = Array.isArray(result) ? result : result.problems;
  for (const problem of problems) console.error(`::error::${problem}`);
  if (problems.length > 0) {
    console.error(
      `crawl surface-inventory purity: ${problems.length} violation(s)`,
    );
    process.exit(1);
  }
  const files = Array.isArray(result) ? [] : result.files;
  console.log(
    `::notice::crawl surface-inventory purity: ${files.length} inventory file(s) pin no literal outside a declared closed vocabulary`,
  );
}
