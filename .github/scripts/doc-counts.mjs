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
//   3. compat parity floors — README.md and docs/test-strategy.md route
//      readers to compat-ratchet.mjs's header for the canonical explanation of
//      the parity gate, and that header states each head's committed floor
//      ("prometheus 737/737"). The canonical numbers are the `heads` block of
//      compatibility/parity-baseline.json; the gate asserts the header states
//      exactly one floor per head and that each matches the baseline, so
//      bumping a floor cannot leave the explanation behind.
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
const PARITY_BASELINE = join(REPO, 'compatibility', 'parity-baseline.json');
const WORKFLOWS_DIR = join(REPO, '.github', 'workflows');
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

// parityFloorClaims — the floors compat-ratchet.mjs's header states, one entry
// per head name it is asked about. The source is line-comment prose, so a
// claim can wrap across `//` continuations ("prometheus 737/737, loki\n//
// 116/116"); comment markers are stripped and whitespace collapsed first so a
// wrapped claim reads the same as an unwrapped one. Returns every match per
// head, because "how many times is this claimed" is itself an assertion: zero
// means the wording drifted out from under the gate.
export function parityFloorClaims(src, heads) {
  const prose = src.replace(/^[ \t]*\/\/[ \t]?/gm, ' ').replace(/\s+/g, ' ');
  const out = {};
  for (const head of heads) {
    const re = new RegExp(`\\b${head}\\s+(\\d+)/(\\d+)\\b`, 'g');
    out[head] = [...prose.matchAll(re)].map((m) => ({ passed: Number(m[1]), total: Number(m[2]) }));
  }
  return out;
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

// assertParityFloors compares the floors stated in compat-ratchet.mjs's header
// against the committed baseline it describes. Exactly one claim per head: a
// second one is a contradiction, and none means the prose stopped stating what
// the gate validates.
function assertParityFloors() {
  const baseline = JSON.parse(readFileSync(PARITY_BASELINE, 'utf8'));
  const heads = Object.keys(baseline.heads ?? {});
  if (heads.length === 0) {
    error('parity-floor: compatibility/parity-baseline.json declares no heads to validate against');
    return false;
  }
  const claims = parityFloorClaims(readFileSync(COMPAT_RATCHET_MJS, 'utf8'), heads);
  let ok = true;
  for (const head of heads) {
    const { passed, total } = baseline.heads[head];
    const found = claims[head];
    if (found.length !== 1) {
      error(
        `parity-floor: compat-ratchet.mjs states ${found.length} "${head} <passed>/<total>" floors, ` +
          `expected exactly 1 — README.md and docs/test-strategy.md send readers there for the ` +
          `canonical numbers, so the header must state each head's floor once`,
        { file: '.github/scripts/compat-ratchet.mjs' },
      );
      ok = false;
      continue;
    }
    if (found[0].passed !== passed || found[0].total !== total) {
      error(
        `parity-floor: compat-ratchet.mjs claims ${head} ${found[0].passed}/${found[0].total} but ` +
          `compatibility/parity-baseline.json has ${passed}/${total}`,
        { file: '.github/scripts/compat-ratchet.mjs' },
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

  const parityOk = assertParityFloors();

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
        `(forbid-skip=${fsCount}, test-layers=${layerCount}, parity floors match the baseline, ` +
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

  // 5. The parity-floor extractor reads a wrapped comment claim, and the gate
  //    rejects both a drifted number and a claim that disappeared.
  const heads = ['prometheus', 'loki', 'tempo'];
  const wrapped = ['// floors are prometheus 718/718, loki', '// 116/116, tempo 48/48 — see the baseline.'].join('\n');
  const wrappedClaims = parityFloorClaims(wrapped, heads);
  check(
    'parity extractor reads a claim wrapped across // continuations',
    wrappedClaims.loki.length === 1 && wrappedClaims.loki[0].passed === 116 && wrappedClaims.loki[0].total === 116,
  );
  const driftedClaims = parityFloorClaims('// floors are prometheus 574/574.', heads);
  check(
    'parity gate would REJECT a header claiming prometheus 574/574 against a 718/718 baseline',
    driftedClaims.prometheus.length === 1 && driftedClaims.prometheus[0].passed !== 718,
  );
  check('parity gate would REJECT a header that states no floor at all', driftedClaims.tempo.length === 0);
  // The REAL header must match the REAL baseline — the assertion the gate runs.
  check('real compat-ratchet.mjs floors match compatibility/parity-baseline.json', assertParityFloors());

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
