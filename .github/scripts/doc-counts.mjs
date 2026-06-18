// doc-counts.mjs — the assert-from-source doc-count gate.
//
// Prose in the docs states integer counts that DESCRIBE source structures:
// "N forbid-skip checks", "N-layer test map". Those literals rot the moment
// the structure they describe grows or shrinks. This gate derives each count
// LIVE from the source of truth and asserts the doc-stated integer equals it,
// so a count can never silently drift. It is assert-from-source, NOT a pinned
// literal (which would just relocate the staleness into a second place).
//
// Two assertions:
//
//   1. forbid-skip CHECK count — the canonical number of discipline scans is
//      the number of `case '<name>':` arms actually dispatched by the CHECK
//      switch in .github/scripts/forbid-skip.mjs (today: t-skip,
//      not-implemented, soft-assert, should-skip, escape-hatch = 5). The gate
//      asserts every "N ... checks/scans/patterns" claim in
//      docs/forbid-skip.md matches that live arm count.
//
//   2. test-layer count — the canonical number of test layers is the count of
//      DISTINCT integer layer numbers across the `### Layer N` subsection
//      headings in docs/test-strategy.md (1..13 today, ignoring the a/b/c/d
//      sub-letters = 13). The gate asserts every "N-layer test map" claim in
//      CLAUDE.md (and any prose layer-count claim in test-strategy.md /
//      README.md) matches that live heading count.
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

import { readFileSync } from 'node:fs';
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

// --- source-count derivations (the "from source" half) ---------------------

// countForbidSkipChecks — the live number of discipline scans is the number of
// `case '<name>':` arms in forbid-skip.mjs's CHECK switch. We parse the case
// labels (not a hardcoded list) so adding/removing a scan moves the count
// automatically. The trailing `default:` arm is the error path, not a scan.
export function countForbidSkipChecks(src) {
  const names = [];
  // Match `case 'name': {` — single-quoted string label arms only.
  const re = /\bcase\s+'([a-z][a-z0-9-]*)'\s*:/g;
  let m;
  while ((m = re.exec(src)) !== null) {
    names.push(m[1]);
  }
  return { count: names.length, names };
}

// countTestLayers — the live number of test layers is the count of DISTINCT
// integer layer numbers across the `### Layer N[sub] — title` headings in
// test-strategy.md. Sub-lettered headings (2a, 2b, 6d, 7b) collapse to their
// integer, so 1,2a,2b,3..13 -> {1..13} -> 13.
export function countTestLayers(src) {
  const ints = new Set();
  const re = /^###\s+Layer\s+(\d+)[a-z]?\b/gm;
  let m;
  while ((m = re.exec(src)) !== null) {
    ints.add(Number(m[1]));
  }
  return { count: ints.size, ints: [...ints].sort((a, b) => a - b) };
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

  if (forbidOk && layerOk) {
    notice(
      `doc-counts: all doc-stated counts match source ` +
        `(forbid-skip=${fsCount}, test-layers=${layerCount})`,
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

  // 1. The deriver counts real switch arms, not the default error path.
  const fakeForbid = [
    'switch (CHECK) {',
    "  case 'a': { break; }",
    "  case 'b': { break; }",
    "  case 'c': { break; }",
    '  default: process.exit(1);',
    '}',
  ].join('\n');
  const { count: fakeCount, names } = countForbidSkipChecks(fakeForbid);
  check('forbid-skip deriver counts 3 arms from a 3-arm switch', fakeCount === 3);
  check('forbid-skip deriver ignores the default arm', !names.includes('default'));

  // The REAL forbid-skip.mjs must derive exactly 5.
  const realForbid = readFileSync(FORBID_SKIP_MJS, 'utf8');
  check('real forbid-skip.mjs derives 5 CHECK arms', countForbidSkipChecks(realForbid).count === 5);

  // 2. A doc that claims the WRONG forbid-skip count must be REJECTED.
  const draftDoc = 'The gate has **6** patterns total.';
  const claims6 = extractClaims(draftDoc, FORBID_SKIP_CLAIM_PATTERNS);
  check('forbid-skip claim extractor finds the "6 patterns" claim', claims6.some((c) => c.value === 6));
  check(
    'forbid-skip gate would REJECT a doc claiming 6 against source 5',
    claims6.some((c) => c.value !== 5),
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

  // The REAL test-strategy.md must derive exactly 13.
  const realStrategy = readFileSync(TEST_STRATEGY_DOC, 'utf8');
  check('real test-strategy.md derives 13 layers', countTestLayers(realStrategy).count === 13);

  // 4. A doc claiming the WRONG layer count must be REJECTED.
  const staleClaude = 'See the canonical 12-layer test map for details.';
  const layerClaims12 = extractClaims(staleClaude, TEST_LAYER_CLAIM_PATTERNS);
  check('layer claim extractor finds the "12-layer test map" claim', layerClaims12.some((c) => c.value === 12));
  check(
    'layer gate would REJECT a doc claiming 12 against source 13',
    layerClaims12.some((c) => c.value !== 13),
  );
  const fixedClaude = 'See the canonical 13-layer test map for details.';
  const layerClaims13 = extractClaims(fixedClaude, TEST_LAYER_CLAIM_PATTERNS);
  check(
    'layer gate would ACCEPT a doc claiming the real 13',
    layerClaims13.length > 0 && layerClaims13.every((c) => c.value === 13),
  );

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
