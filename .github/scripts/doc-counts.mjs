// doc-counts.mjs — the assert-from-source doc-count gate.
//
// Prose in the docs states integer counts that DESCRIBE source structures:
// "N forbid-skip checks", "N-layer test map". Those literals rot the moment
// the structure they describe grows or shrinks. This gate derives each count
// LIVE from the source of truth and asserts the doc-stated integer equals it,
// so a count can never silently drift. It is assert-from-source, NOT a pinned
// literal (which would just relocate the staleness into a second place).
//
// The assertions:
//
//   1. forbid-skip CHECK count — the canonical number of discipline scans is
//      the number of entries in the CHECKS registry in
//      .github/scripts/forbid-skip.mjs (today: t-skip, soft-assert,
//      should-skip, escape-hatch, feature-discipline = 5). The gate
//      asserts every "N ... checks/scans/patterns" claim in
//      docs/forbid-skip.md matches that live registry size.
//
//   2. test-layer count — the canonical number of test layers is the count of
//      DISTINCT integer layer numbers across the `### Layer N` subsection
//      headings in docs/test-strategy.md (1..14 today, ignoring the a/b/c/d
//      sub-letters = 14). The gate asserts every "N-layer test map" claim in
//      CLAUDE.md (and any prose layer-count claim in test-strategy.md /
//      README.md) matches that live heading count.
//
//   3. compat parity counts stay in ONE place — the per-head passed/total is
//      generated into compatibility/parity-baseline.json by the compatibility
//      run. Two hand-written descriptions used to copy it: compat-ratchet.mjs's
//      header floors and docs/compatibility.md's roster table. Comparing three
//      statements of one fact only ever moved the work — a corpus-adding PR had
//      to hand-carry two files, and two such PRs conflicted in files neither
//      changed the meaning of, which is how the table drifted below the
//      baseline three corpus moves running (#1686 → #1717 → #1746). Both sites
//      now state the counts BY REFERENCE, so this assertion inverts: neither
//      may write a baseline integer down as its own standalone number, and each
//      must still name compatibility/parity-baseline.json so a reader can reach
//      the numbers it stopped printing. The .mjs is scanned through its `//`
//      comment prose only, because a code literal (an exit code, an array
//      index) restates nothing.
//
//   4. forbid-skip workflow callers — every `CHECK:` value any workflow hands
//      forbid-skip.mjs must name a live registry entry (or `all`). A stale
//      caller is not cosmetic drift: the invocation exits 1, and since the
//      compatibility lane's `gate` is `needs:` for all three required
//      compatibility heads, it reds every PR in the repository until fixed.
//
//   5. surface-parity "Coverage at a glance" — docs/coverage.md publishes a
//      four-column per-head table (symbols probed / supported / intentionally
//      rejected / wrong-rejected). Every cell is a tally over the `class` field
//      of test/surface-parity/inventory.json, folded exactly the way
//      scripts/gen-coverage.py folds it for the per-symbol tables
//      (parity-accept + wrong-accept -> supported, parity-reject ->
//      intentionally rejected, wrong-reject -> wrong-rejected). The gate
//      re-derives all sixteen cells from the ledger, so the headline
//      "wrong-rejections" figure a reader acts on cannot be hand-typed.
//
//   6. rejection-parity shape divergences — the surface-parity ledger is
//      SYMBOL-level, so its wrong-rejection column says nothing about which
//      argument SHAPES lower. That second measurement is the `class:
//      "divergence"` rows of test/rejection-parity/catalogue/*.json, and
//      docs/coverage.md states their count next to the symbol-level zero so
//      neither number can be read as the other. The gate counts the live rows
//      and asserts the stated integer equals them.
//
// Robustness: each count is parsed from the actual structure (switch arms /
// markdown headings), never from a string match on the prose it validates, so
// a doc edit can only make the gate go green by matching reality.
//
// Usage:
//   node .github/scripts/doc-counts.mjs              run every assertion
//   node .github/scripts/doc-counts.mjs --self-test  prove each assertion
//                                                     FAILS on a deliberate
//                                                     mismatch (meta-test)
//
// Exit codes: 0 = every doc count matches source, 1 = a drift was found (or a
// self-test that should have failed did not).

import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import process from 'node:process';
import { error, notice, log } from './lib/gh.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

const FORBID_SKIP_MJS = join(HERE, 'forbid-skip.mjs');
const FORBID_SKIP_DOC = join(REPO, 'docs', 'forbid-skip.md');
const TEST_STRATEGY_DOC = join(REPO, 'docs', 'test-strategy.md');
const CLAUDE_DOC = join(REPO, 'CLAUDE.md');
const README_DOC = join(REPO, 'README.md');
const COMPAT_RATCHET_MJS = join(HERE, 'compat-ratchet.mjs');
const COMPAT_DOC = join(REPO, 'docs', 'compatibility.md');
const PARITY_BASELINE = join(REPO, 'compatibility', 'parity-baseline.json');
const WORKFLOWS_DIR = join(REPO, '.github', 'workflows');

// The repo-relative path a hand-written parity description must point at
// instead of copying its numbers. It is the string the sites are checked to
// contain, so it is spelled exactly as a reader would type it into `git show`.
const PARITY_BASELINE_REF = 'compatibility/parity-baseline.json';

// The per-head fields the baseline owns. `passed` and `total` are the two
// integers a prose site used to restate; `cases` is the roster itself and is
// not a count.
const PARITY_BASELINE_FIELDS = ['passed', 'total'];
const COVERAGE_DOC = join(REPO, 'docs', 'coverage.md');
const SURFACE_INVENTORY = join(REPO, 'test', 'surface-parity', 'inventory.json');
const REJECTION_CATALOGUE_DIR = join(REPO, 'test', 'rejection-parity', 'catalogue');
const DIVERGENCE_CEILING = join(REPO, 'test', 'rejection-parity', 'divergence-ceiling.json');

// The dispatch mode that runs every registered scan; a legal CHECK value that is
// deliberately not a registry entry. Mirrors `ALL` in forbid-skip.mjs.
const FORBID_SKIP_ALL_MODE = 'all';

// --- source-count derivations (the "from source" half) ---------------------

// countForbidSkipChecks — the live number of discipline scans is the number of
// entries in forbid-skip.mjs's CHECKS registry. We parse the registry keys (not
// a hardcoded list) so adding/removing a scan moves the count automatically.
// `all` is a dispatch mode over the registry, not an entry in it, so it does not
// count.
export function countForbidSkipChecks(src) {
  const names = [];
  // Match a registry entry — `  'name': () => {` at the object's own indent.
  const re = /^ {2}'([a-z][a-z0-9-]*)':\s*\(\)\s*=>\s*\{/gm;
  let m;
  while ((m = re.exec(src)) !== null) {
    names.push(m[1]);
  }
  return { count: names.length, names };
}

// forbidSkipCallers — every `CHECK: <value>` a workflow hands forbid-skip.mjs.
// A caller naming a scan the registry does not define is a HARD error at runtime
// (`unknown CHECK`), and because the compatibility lane's `gate` is `needs:` for
// all three required compatibility heads, one stale caller reds every PR in the
// repository. That is exactly how #1538's scan removal shipped: ci.yml dropped
// its step, compatibility.yml kept a copy. Parsing the callers back out of the
// YAML closes the loop — a removed scan now fails HERE, on the PR that removes
// it, naming the file still asking for it.
export function forbidSkipCallers(workflows) {
  const callers = [];
  // A `run: node .github/scripts/forbid-skip.mjs` step followed by its `env:`
  // block; the CHECK value is the next `CHECK:` within that step.
  const re = /forbid-skip\.mjs[^\n]*\n(?:[^\n]*\n){0,3}?\s*CHECK:\s*([a-z][a-z0-9-]*)/g;
  for (const { path, name, src } of workflows) {
    let m;
    while ((m = re.exec(src)) !== null) {
      callers.push({ check: m[1], path, name });
    }
  }
  return callers;
}

// countTestLayers — the live number of test layers is the count of DISTINCT
// integer layer numbers across the `### Layer N[sub] — title` headings in
// test-strategy.md. Sub-lettered headings (2a, 2b, 6d, 7b) collapse to their
// integer, so 1,2a,2b,3..14 -> {1..14} -> 14.
export function countTestLayers(src) {
  const ints = new Set();
  const re = /^###\s+Layer\s+(\d+)[a-z]?\b/gm;
  let m;
  while ((m = re.exec(src)) !== null) {
    ints.add(Number(m[1]));
  }
  return { count: ints.size, ints: [...ints].sort((a, b) => a - b) };
}

// parityBaselineIntegers — every integer compatibility/parity-baseline.json
// states for a head: its `passed` and its `total`. These are the values a
// hand-written prose site must not copy. Keyed by value so a hit can name
// which head's number was written down (two heads can legitimately share a
// size, hence a list).
export function parityBaselineIntegers(baseline) {
  const out = new Map();
  for (const [head, entry] of Object.entries(baseline?.heads ?? {})) {
    for (const field of PARITY_BASELINE_FIELDS) {
      const value = entry?.[field];
      if (typeof value !== 'number') continue;
      if (!out.has(value)) out.set(value, []);
      out.get(value).push(`${head}.${field}`);
    }
  }
  return out;
}

// commentProse — the human-readable half of a .mjs source: its `//` comment
// text with the markers stripped, one output line per input line so a hit
// still reports the source line number. Code literals (exit codes, array
// indices) restate nothing, so the scan below reads prose only and cannot
// red the build over an unrelated `return 1`.
export function commentProse(src) {
  return src
    .split('\n')
    .map((line) => {
      const m = /^[ \t]*\/\/[ \t]?(.*)$/.exec(line);
      return m ? m[1] : '';
    })
    .join('\n');
}

// A standalone decimal integer: not glued to a word, a `#issue` reference, a
// dotted version, a path segment or a percentage. That exclusion is what
// keeps `#1714`, `24.x` and `1e-9` from reading as counts.
const PARITY_INTEGER_TOKEN = /(?<![\w.#/-])(\d+)(?![\w.%/-])/g;

// parityIntegerRestatements — every place `text` writes one of the baseline's
// per-head integers down as its own standalone number. A hit IS the drift
// this gate exists to prevent: the count belongs to the generated baseline,
// and a second hand-typed copy is what made two independent corpus PRs
// conflict over a file neither of them changed the meaning of.
export function parityIntegerRestatements(text, values) {
  const found = [];
  text.split('\n').forEach((line, i) => {
    for (const m of line.matchAll(PARITY_INTEGER_TOKEN)) {
      const value = Number(m[1]);
      if (values.has(value)) {
        found.push({ line: i + 1, value, owners: values.get(value), text: line.trim() });
      }
    }
  });
  return found;
}

// The heads the surface-parity ledger probes, in the order docs/coverage.md
// tabulates them, plus the aggregate row that closes the table.
const SURFACE_HEADS = ['promql', 'logql', 'traceql'];
const SURFACE_TOTAL_ROW = 'total';

// The four glance-table columns, keyed by the tally field and carrying the
// column header the doc prints, so a mismatch names the cell a reader would
// look at rather than an internal field name.
const GLANCE_COLUMNS = [
  ['probed', 'Symbols probed'],
  ['supported', 'Supported (incl. experimental)'],
  ['parityRejected', 'Intentionally rejected (parity)'],
  ['wrongRejected', 'Wrong-rejected symbols'],
];

// How a ledger class folds into a glance column. This mirrors the translation
// scripts/gen-coverage.py performs for the per-symbol tables: `wrong-accept` is
// cerberus accepting a shape the bare-call probe's reference rejects (range() /
// step() driven outside a query context), which is supported surface for a
// reader, not a gap.
const SURFACE_CLASS_COLUMN = {
  'parity-accept': 'supported',
  'wrong-accept': 'supported',
  'parity-reject': 'parityRejected',
  'wrong-reject': 'wrongRejected',
};

// surfaceParityTotals — the live per-head tallies behind docs/coverage.md's
// "Coverage at a glance" table, derived from the pinned ledger. An unknown head
// or class throws rather than silently landing in no column: a ledger that grew
// a fourth verdict must not be able to shrink the wrong-rejection cell by
// falling off the end of this map.
export function surfaceParityTotals(inventory) {
  const blank = () => ({ probed: 0, supported: 0, parityRejected: 0, wrongRejected: 0 });
  const out = { [SURFACE_TOTAL_ROW]: blank() };
  for (const head of SURFACE_HEADS) out[head] = blank();
  for (const entry of inventory.entries ?? []) {
    const row = out[entry.head];
    if (!row || entry.head === SURFACE_TOTAL_ROW) {
      throw new Error(`surface-parity inventory carries head "${entry.head}", which docs/coverage.md does not tabulate`);
    }
    const column = SURFACE_CLASS_COLUMN[entry.class];
    if (!column) {
      throw new Error(`surface-parity inventory carries class "${entry.class}", which maps to no glance column`);
    }
    for (const key of ['probed', column]) {
      row[key] += 1;
      out[SURFACE_TOTAL_ROW][key] += 1;
    }
  }
  return out;
}

// glanceTableRows — the four integers docs/coverage.md prints per head. Cells
// are matched with their optional `**` bold markers and the padding
// markdownlint's aligned-table style inserts, so reformatting the table cannot
// hide a cell from the gate.
export function glanceTableRows(src) {
  const rows = {};
  const cell = String.raw`\s*\*{0,2}(\d+)\*{0,2}\s*`;
  const re = new RegExp(
    String.raw`^\|\s*\*{0,2}(promql|logql|traceql|total)\*{0,2}\s*\|${cell}\|${cell}\|${cell}\|${cell}\|`,
    'gim',
  );
  let m;
  while ((m = re.exec(src)) !== null) {
    const row = {};
    GLANCE_COLUMNS.forEach(([key], i) => {
      row[key] = Number(m[i + 2]);
    });
    rows[m[1].toLowerCase()] = row;
  }
  return rows;
}

// countShapeDivergences — the live number of `class: "divergence"` rows across
// the rejection-parity catalogue shards: argument shapes the reference backend
// answers and cerberus rejects. This is the SHAPE-level wrong-rejection count,
// and it is a different measurement from the ledger's symbol-level one.
export function countShapeDivergences(shards) {
  let count = 0;
  const byShard = {};
  for (const { name, json } of shards) {
    const n = (json.entries ?? []).filter((e) => e.class === 'divergence').length;
    if (n > 0) byShard[name] = n;
    count += n;
  }
  return { count, byShard };
}

function readCatalogueShards() {
  return readdirSync(REJECTION_CATALOGUE_DIR)
    .filter((f) => f.endsWith('.json'))
    .sort()
    .map((f) => ({
      name: `test/rejection-parity/catalogue/${f}`,
      json: JSON.parse(readFileSync(join(REJECTION_CATALOGUE_DIR, f), 'utf8')),
    }));
}

// --- doc-claim extraction (the "doc-stated integer" half) ------------------

// extractClaims — find every prose integer that claims this count, returning
// { value, context } for each so a mismatch can be reported precisely. The
// regexes target the specific claim shapes, not any bare number, so unrelated
// integers in the doc are ignored.
function extractClaims(src, patterns) {
  const claims = [];
  for (const re of patterns) {
    const g = new RegExp(re.source, re.flags.includes('g') ? re.flags : `${re.flags}g`);
    let m;
    while ((m = g.exec(src)) !== null) {
      claims.push({ value: Number(m[1]), context: m[0].trim() });
    }
  }
  return claims;
}

// forbid-skip doc claims: the canonical count is the number of CHECK SCANS
// the gate dispatches, so the gate keys on the scan/check vocabulary
// ("N checks", "N scans", "N CHECK categories", "N discipline scans") and on
// an explicit "scan/check count [is|:] N". It deliberately does NOT match a
// bare "N patterns" — the doc legitimately distinguishes the 7 regex pattern
// ROWS from the 5 dispatched CHECK scans, so "pattern" is ambiguous; only the
// scan/check count is the source-derived invariant. A "patterns total" claim
// IS matched (that was the historical stale-count phrasing).
const FORBID_SKIP_CLAIM_PATTERNS = [
  /(?:scan|check|pattern)\s+count[^.\n]*?[:*\s]\**(\d+)\**/i,
  /\**(\d+)\**\s+(?:active\s+)?(?:CHECK\s+)?(?:checks?|scans?|categories|discipline scans?)\b/i,
  /\**(\d+)\**\s+patterns?\s+total\b/i,
];

// shape-divergence doc claims: docs/coverage.md must state the catalogue's live
// `divergence` count in the same breath as the ledger's symbol-level zero, so a
// reader cannot take one number for the other.
const SHAPE_DIVERGENCE_CLAIM_PATTERNS = [
  /\**(\d+)\**\s+open\s+argument-shape\s+divergences?/i,
];

// test-layer doc claims: "N-layer test map", "N layers", "tested in N layers".
const TEST_LAYER_CLAIM_PATTERNS = [
  /\**(\d+)\**[-\s]layer\s+test\s+map/i,
  /tested\s+in\s+(?:a\s+)?\**(\d+)\**[-\s]layer/i,
  /tested\s+in\s+\**(\d+)\**\s+layers\b/i,
];

// --- assertion driver -------------------------------------------------------

function assertClaims({ label, expected, docs, patterns }) {
  let ok = true;
  let totalClaims = 0;
  for (const { path, name } of docs) {
    let text;
    try {
      text = readFileSync(path, 'utf8');
    } catch (e) {
      error(`${label}: cannot read ${name}: ${e.message}`);
      ok = false;
      continue;
    }
    const claims = extractClaims(text, patterns);
    for (const c of claims) {
      totalClaims += 1;
      if (c.value !== expected) {
        error(
          `${label}: ${name} claims ${c.value} but source has ${expected} ` +
            `(claim: "${c.context}")`,
          { file: name },
        );
        ok = false;
      }
    }
  }
  if (totalClaims === 0) {
    error(
      `${label}: found ZERO matching count claims to validate — the claim ` +
        `wording drifted out from under the gate, which is itself a failure`,
    );
    ok = false;
  }
  return ok;
}

// assertParityByReference is the surviving half of what used to be two
// stale-number comparisons. Both hand-written sites now state the parity
// numbers BY REFERENCE, so the assertion inverts: neither may restate a
// baseline integer, and each must still name the baseline so a reader can
// reach the numbers it stopped printing.
//
// `report` is the failure sink so the self-test can prove rejection without
// posting an ::error:: annotation on an otherwise-green job.
export function assertParityByReference(baseline, sites, report = error) {
  const values = parityBaselineIntegers(baseline);
  if (values.size === 0) {
    report('parity-by-reference: compatibility/parity-baseline.json states no per-head counts to protect');
    return false;
  }
  let ok = true;
  for (const { name, src, proseOnly } of sites) {
    for (const hit of parityIntegerRestatements(proseOnly ? commentProse(src) : src, values)) {
      report(
        `parity-by-reference: ${name}:${hit.line} writes ${hit.value}, which is ` +
          `${hit.owners.join(' / ')} in ${PARITY_BASELINE_REF} — state it by reference. ` +
          `A hand-typed copy of a generated count is what makes two unrelated ` +
          `corpus-adding PRs conflict here: "${hit.text}"`,
        { file: name, line: hit.line },
      );
      ok = false;
    }
    if (!src.includes(PARITY_BASELINE_REF)) {
      report(
        `parity-by-reference: ${name} no longer names ${PARITY_BASELINE_REF} — ` +
          `it describes the parity contract without pointing at the numbers, ` +
          `which leaves a reader nowhere to go`,
        { file: name },
      );
      ok = false;
    }
  }
  return ok;
}

// compareGlance diffs the doc's published table against the live ledger tallies,
// cell by cell. A row the doc stopped printing is a failure in its own right —
// the headline coverage number vanishing is exactly the drift this catches.
// `report` is the failure sink, so the self-test can prove rejection without
// posting an ::error:: annotation on a green job.
export function compareGlance(live, rows, report = error) {
  let ok = true;
  for (const head of [...SURFACE_HEADS, SURFACE_TOTAL_ROW]) {
    const row = rows[head];
    if (!row) {
      report(
        `surface-parity-glance: docs/coverage.md prints no "${head}" row in the ` +
          `"Coverage at a glance" table — the published coverage figures must state ` +
          `one row per head plus the total`,
        { file: 'docs/coverage.md' },
      );
      ok = false;
      continue;
    }
    for (const [key, column] of GLANCE_COLUMNS) {
      if (row[key] !== live[head][key]) {
        report(
          `surface-parity-glance: docs/coverage.md says ${head} "${column}" = ${row[key]} but ` +
            `test/surface-parity/inventory.json tallies ${live[head][key]}`,
          { file: 'docs/coverage.md' },
        );
        ok = false;
      }
    }
  }
  return ok;
}

function assertSurfaceParityGlance() {
  const live = surfaceParityTotals(JSON.parse(readFileSync(SURFACE_INVENTORY, 'utf8')));
  return compareGlance(live, glanceTableRows(readFileSync(COVERAGE_DOC, 'utf8')), error);
}

// readParityReferenceSites loads the two hand-written descriptions of the
// parity gate. `proseOnly` marks the .mjs, whose numbers live in comments —
// its code literals are exit codes, not restatements.
function readParityReferenceSites() {
  return [
    {
      name: '.github/scripts/compat-ratchet.mjs',
      src: readFileSync(COMPAT_RATCHET_MJS, 'utf8'),
      proseOnly: true,
    },
    { name: 'docs/compatibility.md', src: readFileSync(COMPAT_DOC, 'utf8'), proseOnly: false },
  ];
}

// assertForbidSkipCallers checks every workflow `CHECK:` value against the live
// registry. `all` is the dispatch-over-the-registry mode, so it is always valid;
// anything else must name a registered scan.
// `report` is the failure sink — the real run annotates via `error()`, the
// self-test passes a no-op so proving the gate REJECTS a stale caller does not
// post an ::error:: annotation on an otherwise-green job.
export function assertForbidSkipCallers(names, callers, report = error) {
  const known = new Set([...names, FORBID_SKIP_ALL_MODE]);
  let ok = true;
  for (const { check, name } of callers) {
    if (!known.has(check)) {
      report(
        `forbid-skip-callers: ${name} passes CHECK="${check}", which forbid-skip.mjs ` +
          `does not define (live scans: ${names.join(', ')}; plus "${FORBID_SKIP_ALL_MODE}"). ` +
          `That invocation exits 1 at runtime — remove the step or point it at a live scan`,
        { file: name },
      );
      ok = false;
    }
  }
  if (callers.length === 0) {
    report(
      'forbid-skip-callers: found ZERO forbid-skip.mjs invocations across ' +
        '.github/workflows — the discipline gate is not wired into CI at all',
    );
    ok = false;
  }
  return ok;
}

function readWorkflows() {
  return readdirSync(WORKFLOWS_DIR)
    .filter((f) => f.endsWith('.yml') || f.endsWith('.yaml'))
    .sort()
    .map((f) => ({
      path: join(WORKFLOWS_DIR, f),
      name: `.github/workflows/${f}`,
      src: readFileSync(join(WORKFLOWS_DIR, f), 'utf8'),
    }));
}

function runAssertions() {
  const forbidSrc = readFileSync(FORBID_SKIP_MJS, 'utf8');
  const { count: fsCount, names: fsNames } = countForbidSkipChecks(forbidSrc);
  log(`forbid-skip CHECK arms (live): ${fsCount} [${fsNames.join(', ')}]`);

  const strategySrc = readFileSync(TEST_STRATEGY_DOC, 'utf8');
  const { count: layerCount, ints: layerInts } = countTestLayers(strategySrc);
  log(`test-strategy Layer integers (live): ${layerCount} [${layerInts.join(', ')}]`);

  const forbidOk = assertClaims({
    label: 'forbid-skip-count',
    expected: fsCount,
    docs: [{ path: FORBID_SKIP_DOC, name: 'docs/forbid-skip.md' }],
    patterns: FORBID_SKIP_CLAIM_PATTERNS,
  });

  const layerOk = assertClaims({
    label: 'test-layer-count',
    expected: layerCount,
    docs: [
      { path: CLAUDE_DOC, name: 'CLAUDE.md' },
      { path: TEST_STRATEGY_DOC, name: 'docs/test-strategy.md' },
      { path: README_DOC, name: 'README.md' },
    ],
    patterns: TEST_LAYER_CLAIM_PATTERNS,
  });

  const parityBaseline = JSON.parse(readFileSync(PARITY_BASELINE, 'utf8'));
  const parityValues = parityBaselineIntegers(parityBaseline);
  log(
    `parity-baseline per-head integers (live): ` +
      `[${[...parityValues].map(([v, owners]) => `${owners.join('/')}=${v}`).join(', ')}]`,
  );
  const parityOk = assertParityByReference(parityBaseline, readParityReferenceSites());

  const glanceOk = assertSurfaceParityGlance();

  const { count: divCount, byShard } = countShapeDivergences(readCatalogueShards());
  log(
    `rejection-parity divergence rows (live): ${divCount} ` +
      `[${Object.entries(byShard).map(([n, c]) => `${n}:${c}`).join(', ')}]`,
  );
  const divergenceOk = assertClaims({
    label: 'shape-divergence-count',
    expected: divCount,
    docs: [{ path: COVERAGE_DOC, name: 'docs/coverage.md' }],
    patterns: SHAPE_DIVERGENCE_CLAIM_PATTERNS,
  });

  const callers = forbidSkipCallers(readWorkflows());
  log(
    `forbid-skip workflow callers (live): ${callers.length} ` +
      `[${callers.map((c) => `${c.name}:${c.check}`).join(', ')}]`,
  );
  const callersOk = assertForbidSkipCallers(fsNames, callers);

  if (forbidOk && layerOk && parityOk && glanceOk && divergenceOk && callersOk) {
    notice(
      `doc-counts: all doc-stated counts match source ` +
        `(forbid-skip=${fsCount}, test-layers=${layerCount}, ` +
        `compat-ratchet.mjs and docs/compatibility.md state the per-head parity ` +
        `counts by reference to ${PARITY_BASELINE_REF} rather than restating them, ` +
        `the coverage glance table matches the surface-parity ledger, ` +
        `shape-divergences=${divCount}, ` +
        `${callers.length} workflow CHECK callers all name a live scan)`,
    );
    return 0;
  }
  return 1;
}

// --- self-test: prove each assertion FAILS on a deliberate mismatch ---------

// Each case feeds the count-derivers / claim-extractors a deliberately-drifted
// input and asserts the comparison reports a mismatch. If a mutation slips
// through (the gate would NOT catch the drift), the self-test fails loudly.
function selfTest() {
  let failures = 0;
  const check = (name, cond) => {
    if (cond) {
      log(`  ok   ${name}`);
    } else {
      error(`  FAIL ${name}`);
      failures += 1;
    }
  };

  // 1. The deriver counts real registry entries, not the dispatch machinery.
  const fakeForbid = [
    'const CHECKS = {',
    "  'a': () => { fail('a'); },",
    "  'b': () => { fail('b'); },",
    "  'c': () => { fail('c'); },",
    '};',
    "if (CHECK === ALL) { for (const [name, scan] of Object.entries(CHECKS)) { scan(); } }",
    "else if (Object.hasOwn(CHECKS, CHECK)) { CHECKS[CHECK](); }",
  ].join('\n');
  const { count: fakeCount, names } = countForbidSkipChecks(fakeForbid);
  check('forbid-skip deriver counts 3 entries from a 3-entry registry', fakeCount === 3);
  check('forbid-skip deriver ignores the all-mode dispatch', !names.includes(FORBID_SKIP_ALL_MODE));

  // The REAL forbid-skip.mjs must derive exactly 5 (not-implemented removed in #1538).
  const realForbid = readFileSync(FORBID_SKIP_MJS, 'utf8');
  check('real forbid-skip.mjs derives 5 CHECK scans', countForbidSkipChecks(realForbid).count === 5);

  // 1b. A workflow caller naming a scan the registry does not define must be
  // REJECTED — the #1538 failure mode, where compatibility.yml kept asking for
  // a deleted scan and reded every PR's compatibility heads.
  const staleWorkflow = [
    {
      path: 'fake',
      name: '.github/workflows/fake.yml',
      src: [
        '      - name: Reject a deleted scan',
        '        run: node .github/scripts/forbid-skip.mjs',
        '        env:',
        '          CHECK: not-implemented',
      ].join('\n'),
    },
  ];
  const staleCallers = forbidSkipCallers(staleWorkflow);
  check(
    'caller parser extracts CHECK from a workflow step',
    staleCallers.length === 1 && staleCallers[0].check === 'not-implemented',
  );
  check(
    'caller gate would REJECT a workflow asking for a deleted scan',
    !assertForbidSkipCallers(['t-skip', 'soft-assert'], staleCallers, () => {}),
  );
  check(
    'caller gate ACCEPTS the all-mode dispatch value',
    assertForbidSkipCallers(
      ['t-skip'],
      [{ check: FORBID_SKIP_ALL_MODE, name: 'fake.yml' }],
      () => {},
    ),
  );

  // 2. A doc that claims the WRONG forbid-skip count must be REJECTED.
  const draftDoc = 'The gate has **7** patterns total.';
  const claims7 = extractClaims(draftDoc, FORBID_SKIP_CLAIM_PATTERNS);
  check('forbid-skip claim extractor finds the "7 patterns" claim', claims7.some((c) => c.value === 7));
  check(
    'forbid-skip gate would REJECT a doc claiming 7 against source 5',
    claims7.some((c) => c.value !== 5),
  );
  // And ACCEPT the corrected wording.
  const fixedDoc = 'The gate has **5** CHECK categories total.';
  const claims5 = extractClaims(fixedDoc, FORBID_SKIP_CLAIM_PATTERNS);
  check(
    'forbid-skip gate would ACCEPT a doc claiming the real 5',
    claims5.length > 0 && claims5.every((c) => c.value === 5),
  );

  // 3. The layer deriver collapses sub-letters to distinct integers.
  const fakeStrategy = [
    '### Layer 1 — a',
    '### Layer 2a — b',
    '### Layer 2b — c',
    '### Layer 3 — d',
  ].join('\n');
  const { count: fakeLayers, ints } = countTestLayers(fakeStrategy);
  check('layer deriver collapses 1,2a,2b,3 to 3 distinct integers', fakeLayers === 3);
  check('layer deriver yields the integers [1,2,3]', ints.join(',') === '1,2,3');

  // The REAL test-strategy.md must derive exactly this many layers. Pinning it
  // is the tripwire that catches countTestLayers silently breaking (returning 0,
  // or double-counting the a/b/c sub-letters). Adding a test layer is therefore
  // a deliberate one-line bump here, not an accident.
  const realLayers = 14;
  const staleLayers = realLayers - 1;
  const realStrategy = readFileSync(TEST_STRATEGY_DOC, 'utf8');
  check(`real test-strategy.md derives ${realLayers} layers`, countTestLayers(realStrategy).count === realLayers);

  // 4. A doc claiming the WRONG layer count must be REJECTED.
  const staleClaude = `See the canonical ${staleLayers}-layer test map for details.`;
  const staleLayerClaims = extractClaims(staleClaude, TEST_LAYER_CLAIM_PATTERNS);
  check(
    `layer claim extractor finds the "${staleLayers}-layer test map" claim`,
    staleLayerClaims.some((c) => c.value === staleLayers),
  );
  check(
    `layer gate would REJECT a doc claiming ${staleLayers} against source ${realLayers}`,
    staleLayerClaims.some((c) => c.value !== realLayers),
  );
  const fixedClaude = `See the canonical ${realLayers}-layer test map for details.`;
  const realLayerClaims = extractClaims(fixedClaude, TEST_LAYER_CLAIM_PATTERNS);
  check(
    `layer gate would ACCEPT a doc claiming the real ${realLayers}`,
    realLayerClaims.length > 0 && realLayerClaims.every((c) => c.value === realLayers),
  );

  // 5. The parity numbers live in ONE place. The gate is an absence check, so
  //    the mutation it must catch is a hand-typed count creeping back into a
  //    prose site — the restatement that made every corpus-adding PR conflict
  //    in two files it did not change the meaning of.
  const fakeBaseline = {
    heads: {
      prometheus: { passed: 788, total: 788 },
      tempo: { passed: 72, total: 72 },
    },
  };
  const fakeValues = parityBaselineIntegers(fakeBaseline);
  check(
    'baseline deriver keys each per-head passed/total by value and names its owner',
    fakeValues.get(788)?.join(',') === 'prometheus.passed,prometheus.total' && fakeValues.has(72),
  );
  check(
    'comment-prose reader keeps // text and drops code literals',
    commentProse(['// the roster is 788 cases', 'const n = 788;'].join('\n')).trim() ===
      'the roster is 788 cases',
  );
  const restated = parityIntegerRestatements('the roster is 788 cases', fakeValues);
  check(
    'restatement scanner flags a prose line that writes a baseline count',
    restated.length === 1 && restated[0].value === 788 && restated[0].owners.includes('prometheus.total'),
  );
  check(
    'restatement scanner does NOT flag an issue reference or a version that shares the digits',
    parityIntegerRestatements(`see #788 and v1.788 and 78%`, fakeValues).length === 0,
  );
  const byReferenceSrc = `read heads.<head> out of ${PARITY_BASELINE_REF} for the counts`;
  check(
    'parity gate ACCEPTS a site that points at the baseline and states no count',
    assertParityByReference(
      fakeBaseline,
      [{ name: 'fake.md', src: byReferenceSrc, proseOnly: false }],
      () => {},
    ),
  );
  check(
    'parity gate REJECTS a site that restates a baseline count in prose',
    !assertParityByReference(
      fakeBaseline,
      [{ name: 'fake.md', src: `${byReferenceSrc}\nprometheus passes 788 cases`, proseOnly: false }],
      () => {},
    ),
  );
  check(
    'parity gate REJECTS a site that stopped naming the baseline at all',
    !assertParityByReference(
      fakeBaseline,
      [{ name: 'fake.md', src: 'every head reaches full parity.', proseOnly: false }],
      () => {},
    ),
  );
  check(
    'parity gate ignores a code literal in a proseOnly site but still reads its comments',
    assertParityByReference(
      fakeBaseline,
      [{ name: 'fake.mjs', src: `// see ${PARITY_BASELINE_REF}\nprocess.exit(72);`, proseOnly: true }],
      () => {},
    ) &&
      !assertParityByReference(
        fakeBaseline,
        [{ name: 'fake.mjs', src: `// see ${PARITY_BASELINE_REF}, tempo is 72`, proseOnly: true }],
        () => {},
      ),
  );
  check(
    'parity gate REJECTS a baseline that states no per-head counts to protect',
    !assertParityByReference({ heads: {} }, [{ name: 'fake.md', src: byReferenceSrc }], () => {}),
  );
  // The REAL sites must hold — the assertion the gate runs.
  check(
    'real compat-ratchet.mjs and docs/compatibility.md state the parity counts by reference',
    assertParityByReference(
      JSON.parse(readFileSync(PARITY_BASELINE, 'utf8')),
      readParityReferenceSites(),
      () => {},
    ),
  );

  // 6. The glance tally folds the ledger's four classes into the doc's four
  //    columns, and the comparison rejects a hand-typed wrong-rejection cell —
  //    the drift that let docs/coverage.md publish a zero the catalogue
  //    contradicted.
  const fakeInventory = {
    entries: [
      { head: 'promql', class: 'parity-accept' },
      { head: 'promql', class: 'wrong-accept' },
      { head: 'promql', class: 'parity-reject' },
      { head: 'promql', class: 'wrong-reject' },
      { head: 'logql', class: 'parity-accept' },
      { head: 'traceql', class: 'parity-accept' },
    ],
  };
  const fakeTotals = surfaceParityTotals(fakeInventory);
  check(
    'glance tally folds wrong-accept into Supported and wrong-reject into its own column',
    fakeTotals.promql.probed === 4 &&
      fakeTotals.promql.supported === 2 &&
      fakeTotals.promql.parityRejected === 1 &&
      fakeTotals.promql.wrongRejected === 1,
  );
  check(
    'glance tally sums the total row across all three heads',
    fakeTotals.total.probed === 6 && fakeTotals.total.supported === 4,
  );
  let unknownClassRejected = false;
  try {
    surfaceParityTotals({ entries: [{ head: 'promql', class: 'novel-verdict' }] });
  } catch {
    unknownClassRejected = true;
  }
  check('glance tally THROWS on a ledger class it cannot place in a column', unknownClassRejected);

  const glanceDoc = [
    '| Head      | Symbols probed | Supported (incl. experimental) | Intentionally rejected (parity) | Wrong-rejected symbols |',
    '| --------- | -------------- | ------------------------------ | ------------------------------- | ---------------------- |',
    '| PromQL    | 4              | 2                              | 1                               | 0                      |',
    '| LogQL     | 1              | 1                              | 0                               | 0                      |',
    '| TraceQL   | 1              | 1                              | 0                               | 0                      |',
    '| **Total** | **6**          | **4**                          | **1**                           | **0**                  |',
  ].join('\n');
  const glanceRows = glanceTableRows(glanceDoc);
  check(
    'glance row parser reads a bolded, markdownlint-padded total row',
    glanceRows.total !== undefined && glanceRows.total.probed === 6 && glanceRows.total.supported === 4,
  );
  check(
    'glance gate would REJECT a doc printing 0 wrong-rejections against a ledger with 1',
    !compareGlance(fakeTotals, glanceRows, () => {}),
  );
  const honestDoc = [
    '| Head      | Symbols probed | Supported (incl. experimental) | Intentionally rejected (parity) | Wrong-rejected symbols |',
    '| --------- | -------------- | ------------------------------ | ------------------------------- | ---------------------- |',
    '| PromQL    | 4              | 2                              | 1                               | 1                      |',
    '| LogQL     | 1              | 1                              | 0                               | 0                      |',
    '| TraceQL   | 1              | 1                              | 0                               | 0                      |',
    '| **Total** | **6**          | **4**                          | **1**                           | **1**                  |',
  ].join('\n');
  check(
    'glance gate would ACCEPT the corrected wrong-rejection cells',
    compareGlance(fakeTotals, glanceTableRows(honestDoc), () => {}),
  );
  check(
    'glance gate would REJECT a doc that dropped the table entirely',
    !compareGlance(fakeTotals, {}, () => {}),
  );
  // The REAL ledger must match the REAL doc — the assertion the gate runs.
  check('real docs/coverage.md glance table matches test/surface-parity/inventory.json', assertSurfaceParityGlance());

  // 7. The shape-divergence count is a SECOND measurement, and the doc must
  //    state it rather than let the symbol-level zero stand in for it.
  const fakeShards = [
    { name: 'a.json', json: { entries: [{ class: 'divergence' }, { class: 'rejection' }] } },
    { name: 'b.json', json: { entries: [{ class: 'internal' }] } },
    { name: 'c.json', json: { entries: [{ class: 'divergence' }, { class: 'divergence' }] } },
  ];
  const fakeDiv = countShapeDivergences(fakeShards);
  check('divergence deriver counts only `divergence` rows across shards', fakeDiv.count === 3);
  check('divergence deriver omits shards with no divergence rows', !('b.json' in fakeDiv.byShard));
  const staleCoverage = 'the catalogue records **0** open argument-shape divergences today.';
  const staleDivClaims = extractClaims(staleCoverage, SHAPE_DIVERGENCE_CLAIM_PATTERNS);
  check(
    'divergence claim extractor finds the stated count',
    staleDivClaims.length === 1 && staleDivClaims[0].value === 0,
  );
  check(
    'divergence gate would REJECT a doc claiming 0 against a catalogue with 3',
    staleDivClaims.some((c) => c.value !== fakeDiv.count),
  );
  // The REAL catalogue must derive a non-zero count that respects the ratchet's
  // ceiling. Both bounds are tripwires on the deriver itself: a silently-broken
  // reader returns 0 (and would let the doc reprint the false zero), and a
  // double-counting one exceeds the ceiling the Go ratchet enforces.
  const realDiv = countShapeDivergences(readCatalogueShards()).count;
  const ceiling = JSON.parse(readFileSync(DIVERGENCE_CEILING, 'utf8')).max_entries;
  check('real rejection-parity catalogue derives a non-zero divergence count', realDiv > 0);
  check(`real divergence count ${realDiv} is within the ratchet ceiling ${ceiling}`, realDiv <= ceiling);

  if (failures === 0) {
    notice(`doc-counts --self-test: all ${'meta-assertions'} passed`);
    return 0;
  }
  error(`doc-counts --self-test: ${failures} meta-assertion(s) failed`);
  return 1;
}

const mode = process.argv[2] || '';
const code = mode === '--self-test' ? selfTest() : runAssertions();
process.exit(code);
