// crawl-surface-inventory-guard.mjs — the PR-time content ratchet over the
// Grafana crawl surface inventories,
// test/e2e/playwright/crawl/grafana-surface-inventory.<stack>.json.
//
// # Why this exists
//
// #1567 marked ~28 generated baselines `-merge` in .gitattributes so a LOCAL
// `git merge` refuses to silently blend two branches that each insert a record
// at a nearby offset (the #1422 corruption class: record boundaries shift, a
// name ends up paired with the next record's values, and the result still
// parses as valid JSON with a plausible length while every entry in the
// blended region is wrong). #1568 then established EMPIRICALLY that GitHub's
// server-side merge does not honour that attribute at all — so the attribute
// alone protects nothing on the path that actually lands code.
//
// What made that survivable everywhere else is that nearly every `-merge`
// artefact carries a required, content-exact regenerate-and-diff ratchet:
// TestCardinalityRatchet / TestSolverDecisionRatchet / TestScaleWallPin
// (`perf-guards`), TestCatalogueIsRegenerable + the surface-parity inventory
// tests (`check`), compat-ratchet.mjs (`compatibility/*`),
// coverage-summary.mjs (`coverage`), the Tier-0 migration goldens (`lint`).
//
// These two files were the one family left without one (#1674). They are
// exercised at full depth only by the release-gated `dashboard` (k3d) lane,
// which runs its real work on the MERGE commit rather than blocking the PR;
// on an ordinary PR `compose-smoke` walks only the `lean: true` subset of the
// `.compose.json` rows (SWEEP_DEPTH=lean), and only when its own scope
// triggers (internal/chsql, internal/api, internal/chclient, cmd/cerberus).
// So a blended merge of either file could reach `main` with no PR-time check
// having read the merged bytes.
//
// # Why a content-exact check is possible here at all
//
// The inventory content IS a live crawl result, so nothing offline can
// re-derive WHICH surfaces belong in it. But the committed FORM is not a live
// result: lib.ts's marshalInventory (test/e2e/playwright/crawl/lib.ts) renders
// every inventory through one canonical serialization — top-level keys
// {doc, stack, surfaces} in that order, rows {url, lean} in that order, rows
// sorted by byCodepoint on `url`, two-space indent, trailing newline — and
// crawl.spec.ts writes the file with exactly that function. The canonical form
// is therefore a PURE function of the file's own content, which means this
// gate can rebuild it offline and demand byte equality.
//
// That is what makes this a ratchet rather than a shape heuristic: it is not a
// sample of properties that happen to hold, it is the complete set of bytes
// the generator could have emitted for this content. Anything else — a row out
// of order, a duplicate row, a row missing `lean`, a row carrying an extra
// field, a reordered key, a re-indent, a dropped trailing newline, a
// JSON-duplicate key that JSON.parse silently collapsed — changes the bytes and
// fails here.
//
// generated-baseline-structural-guard.mjs deliberately excluded these two
// files when it was written: their row order had been checked by hand against
// a plain lexical sort and did not match, so folding them into that guard's
// generic sorted/unique check would have encoded a false invariant. That
// finding was true of the crawl as it then behaved. #1863 fixed the crawl's
// non-determinism (localeCompare → byCodepoint, and order-independent
// canonical folding), #1539/#1467 bootstrapped the k3d file and #1826 gave
// compose a CI regeneration path; both committed files are now exactly
// marshalInventory's output, verified against the real bytes. This gate is the
// last member of that epic (#1467).
//
// # What this gate CANNOT see — stated plainly, because it matters
//
// A `lean` boolean paired with the WRONG `url`, where the result is still
// sorted, still unique, and still well-shaped, is invisible here. `lean` is a
// live crawl property with no offline source of truth, so no PR-time check can
// derive it; a blend that swapped two rows' `lean` bits produces bytes this
// gate accepts. That residue is caught on-stack by diffInventory (lib.ts) —
// the compose-smoke lean crawl and the k3d `dashboard` full crawl — and
// nowhere else. This gate closes the FORM, not the last bit of the content;
// pretending otherwise is how a ratchet becomes a rubber stamp.
//
// Env:
//   CRAWL_DIR  directory holding the inventories
//              (default: test/e2e/playwright/crawl)
//
// Usage:
//   node .github/scripts/crawl-surface-inventory-guard.mjs
//
// Exit codes:
//   0  every discovered inventory is byte-identical to its canonical form.
//   1  a violation, an unreadable/unparseable file, or no inventory found at
//      all (a guard that reads nothing checks nothing).

import { readdirSync, readFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';

/** Default location of the crawl suite's committed inventories. */
const DEFAULT_CRAWL_DIR = 'test/e2e/playwright/crawl';

/** Filename shape: grafana-surface-inventory.<stack>.json. */
const INVENTORY_PREFIX = 'grafana-surface-inventory.';
const INVENTORY_SUFFIX = '.json';

/**
 * The crawl's seed surface. lib.ts defines the lean plan as "the root page,
 * the nav links harvested from it, and one representative per drilldown app",
 * so the root is lean by construction on every stack — a lean set that has
 * lost it has lost the page the whole lean walk starts from.
 */
const ROOT_SURFACE = '/';

/** marshalInventory's indent (lib.ts: JSON.stringify(…, null, 2)). */
const CANONICAL_INDENT = 2;

/** The exact field set of an inventory row, in the order it is emitted. */
const ROW_KEYS = ['url', 'lean'];

/**
 * Order two strings by UTF-16 code unit — the ordering JS `<` gives. This
 * MIRRORS byCodepoint in test/e2e/playwright/crawl/lib.ts, which the crawl
 * uses for everything its OUTPUT order depends on (localeCompare is not
 * reproducible across locales or ICU versions). The two definitions must stay
 * identical; if lib.ts's canonical form ever changes, this gate goes red on
 * the next regenerated file, which is the intended signal rather than a
 * surprise.
 */
export function byCodepoint(a, b) {
  if (a < b) return -1;
  return a > b ? 1 : 0;
}

/**
 * Render the canonical serialized form of an inventory — the bytes
 * marshalInventory (lib.ts) would have written for this content.
 *
 * Every field is REBUILT rather than passed through, which is what gives the
 * byte comparison its reach: an extra field, a missing field, a reordered key
 * or an unexpected top-level entry cannot survive the rebuild, so it shows up
 * as a byte difference instead of round-tripping unnoticed.
 */
export function canonicalize(inv) {
  const surfaces = [...inv.surfaces]
    .sort((a, b) => byCodepoint(a.url, b.url))
    .map(({ url, lean }) => ({ url, lean }));
  const doc = { doc: inv.doc, stack: inv.stack, surfaces };
  return `${JSON.stringify(doc, null, CANONICAL_INDENT)}\n`;
}

/**
 * Check one committed inventory. Returns a list of human-readable problems;
 * an empty list means the file is byte-identical to its canonical form.
 *
 * `filename` is used both for the label and as the source of truth for which
 * stack the file claims to pin — the field and the name are two independent
 * statements of the same fact, so they can contradict each other.
 */
export function checkInventory({ filename, raw }) {
  const problems = [];
  const label = filename;

  let inv;
  try {
    inv = JSON.parse(raw);
  } catch (e) {
    return [`${label}: is not parseable JSON — ${e.message}`];
  }

  if (inv === null || typeof inv !== 'object' || Array.isArray(inv)) {
    return [`${label}: expected a JSON object at the top level, got ${Array.isArray(inv) ? 'an array' : typeof inv}`];
  }

  const stackFromName = filename.slice(
    INVENTORY_PREFIX.length,
    filename.length - INVENTORY_SUFFIX.length,
  );

  if (typeof inv.doc !== 'string' || inv.doc === '') {
    problems.push(`${label}: "doc" must be a non-empty string documenting the regen convention`);
  }
  if (typeof inv.stack !== 'string' || inv.stack === '') {
    problems.push(`${label}: "stack" must be a non-empty string`);
  } else if (inv.stack !== stackFromName) {
    problems.push(
      `${label}: pins stack ${JSON.stringify(inv.stack)} but its filename says ` +
        `${JSON.stringify(stackFromName)} — the file and the field disagree about which stack this inventory ` +
        'describes, which is what a merge that crossed two stacks\' files produces.',
    );
  }
  if (!Array.isArray(inv.surfaces)) {
    problems.push(`${label}: "surfaces" must be an array, got ${typeof inv.surfaces}`);
    return problems;
  }

  // Row shape. Checked before order and before the byte comparison: a
  // malformed row makes both of those report something misleading.
  inv.surfaces.forEach((row, i) => {
    if (row === null || typeof row !== 'object' || Array.isArray(row)) {
      problems.push(`${label}: surfaces[${i}] is not an object`);
      return;
    }
    const keys = Object.keys(row);
    if (keys.length !== ROW_KEYS.length || keys.some((k, j) => k !== ROW_KEYS[j])) {
      problems.push(
        `${label}: surfaces[${i}] has fields ${JSON.stringify(keys)}, expected exactly ` +
          `${JSON.stringify(ROW_KEYS)} in that order. A blend that spliced one row's trailing fields onto ` +
          'another\'s leading fields produces exactly this.',
      );
    }
    if (typeof row.url !== 'string' || row.url === '') {
      problems.push(`${label}: surfaces[${i}].url must be a non-empty string`);
    }
    if (typeof row.lean !== 'boolean') {
      problems.push(
        `${label}: surfaces[${i}].lean must be a boolean, got ${JSON.stringify(row.lean)}`,
      );
    }
  });
  if (problems.length > 0) return problems;

  const urls = inv.surfaces.map((r) => r.url);

  const firstSeenAt = new Map();
  urls.forEach((u, i) => {
    if (firstSeenAt.has(u)) {
      problems.push(
        `${label}: duplicate url ${JSON.stringify(u)} at index ${firstSeenAt.get(u)} and ${i} — ` +
          'the inventory pins a SET of surfaces, so a repeated url means two branches inserted the same row.',
      );
    } else {
      firstSeenAt.set(u, i);
    }
  });

  for (let i = 1; i < urls.length; i++) {
    if (byCodepoint(urls[i - 1], urls[i]) >= 0) {
      problems.push(
        `${label}: out of canonical order at index ${i} — ${JSON.stringify(urls[i - 1])} then ` +
          `${JSON.stringify(urls[i])}. marshalInventory emits rows sorted by byCodepoint on url; a merge that ` +
          'silently interleaved two branches\' insertions (the #1422 shape) lands a row next to the wrong ' +
          'neighbour and breaks exactly this.',
      );
    }
  }

  const root = inv.surfaces.find((r) => r.url === ROOT_SURFACE);
  if (root === undefined) {
    problems.push(
      `${label}: no ${JSON.stringify(ROOT_SURFACE)} surface — the crawl's BFS is seeded from the root page, ` +
        'so an inventory without it does not describe a reachable surface set.',
    );
  } else if (root.lean !== true) {
    problems.push(
      `${label}: the ${JSON.stringify(ROOT_SURFACE)} surface is not lean. The lean (per-PR) plan is defined as ` +
        'the root page plus the nav links harvested from it plus one representative per drilldown app, so a ' +
        'non-lean root would leave the lean crawl with nothing to walk from.',
    );
  }

  if (problems.length > 0) return problems;

  const canonical = canonicalize(inv);
  if (canonical !== raw) {
    problems.push(
      `${label}: does NOT match its canonical serialized form. This file is generated — regenerate it rather ` +
        'than hand-editing it:\n' +
        `  CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=${stackFromName} npx playwright test crawl/crawl.spec.ts\n` +
        '  (or dispatch the e2e workflow with update_crawl_inventory=true)\n' +
        'The canonical form is marshalInventory in test/e2e/playwright/crawl/lib.ts: keys {doc, stack, ' +
        `surfaces}, rows {url, lean}, rows sorted by byCodepoint on url, ${CANONICAL_INDENT}-space indent, ` +
        `trailing newline. Committed ${raw.length} byte(s), canonical ${canonical.length} byte(s); first ` +
        `difference at byte ${firstDifference(raw, canonical)}.`,
    );
  }

  return problems;
}

/** Byte offset of the first difference between two strings, for the message. */
function firstDifference(a, b) {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    if (a[i] !== b[i]) return i;
  }
  return n;
}

export function main() {
  const dir = process.env.CRAWL_DIR || DEFAULT_CRAWL_DIR;

  let names;
  try {
    names = readdirSync(dir)
      .filter((n) => n.startsWith(INVENTORY_PREFIX) && n.endsWith(INVENTORY_SUFFIX))
      .sort();
  } catch (e) {
    error(`crawl-surface-inventory-guard: cannot read ${dir} — ${e.message}`, {
      title: 'crawl-surface-inventory-guard',
    });
    process.exit(1);
  }

  // A guard that discovers nothing reports green while checking nothing —
  // the failure mode this whole family exists to close. Discovery is by
  // pattern rather than a hard-coded pair so a third stack is covered the day
  // it registers, with no list to forget to update.
  if (names.length === 0) {
    error(
      `crawl-surface-inventory-guard: no ${INVENTORY_PREFIX}*${INVENTORY_SUFFIX} file under ${dir} — the guard ` +
        'read nothing, so it checked nothing. Either the inventories moved and this gate must follow them, or ' +
        'CRAWL_DIR is pointed at the wrong tree.',
      { title: 'crawl-surface-inventory-guard' },
    );
    process.exit(1);
  }

  const problems = [];
  for (const name of names) {
    try {
      problems.push(
        ...checkInventory({ filename: name, raw: readFileSync(path.join(dir, name), 'utf8') }),
      );
    } catch (e) {
      problems.push(`${name}: ${e.message}`);
    }
  }

  if (problems.length > 0) {
    for (const p of problems) error(p, { title: 'crawl-surface-inventory-guard' });
    log(
      `\n${problems.length} violation(s) across ${names.length} crawl surface inventory file(s). These files are ` +
        '`-merge`-marked in .gitattributes, and #1568 established that GitHub\'s server-side merge ignores that ' +
        'attribute — so a blended merge reaches this gate looking like valid JSON. Regenerate from a real crawl ' +
        'rather than hand-editing.\n' +
        'Note what this gate does NOT prove: a `lean` boolean paired with the wrong `url`, still sorted and ' +
        'unique, passes here. `lean` is a live crawl property with no offline source of truth; that residue is ' +
        'caught on-stack by diffInventory in the compose-smoke lean crawl and the k3d `dashboard` full crawl.',
    );
    process.exit(1);
  }

  notice(
    `crawl-surface-inventory-guard: ${names.length} inventory file(s) byte-identical to their canonical form ` +
      `(${names.join(', ')}).`,
  );
}

if (import.meta.url === `file://${process.argv[1]}`) main();
