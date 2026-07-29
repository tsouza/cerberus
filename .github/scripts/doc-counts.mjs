// doc-counts.mjs — the assert-from-source doc-count gate.
//
// Prose in the docs states integer counts that DESCRIBE source structures:
// "N forbid-skip checks", "N-layer test map". Those literals rot the moment
// the structure they describe grows or shrinks. This gate derives each count
// LIVE from the source of truth and asserts the doc-stated integer equals it,
// so a count can never silently drift. It is assert-from-source, NOT a pinned
// literal (which would just relocate the staleness into a second place).
//
// Three assertions:
//
//   1. forbid-skip CHECK count — the canonical number of discipline scans is
//      the number of `case '<name>':` arms actually dispatched by the CHECK
//      switch in .github/scripts/forbid-skip.mjs (today: t-skip,
//      not-implemented, soft-assert, should-skip, escape-hatch,
//      feature-discipline = 6). The gate
//      asserts every "N ... checks/scans/patterns" claim in
//      docs/forbid-skip.md matches that live arm count.
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
const COMPAT_RATCHET_MJS = join(HERE, 'compat-ratchet.mjs');
const PARITY_BASELINE = join(REPO, 'compatibility', 'parity-baseline.json');

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

  if (forbidOk && layerOk && parityOk) {
    notice(
      `doc-counts: all doc-stated counts match source ` +
        `(forbid-skip=${fsCount}, test-layers=${layerCount}, parity floors match the baseline)`,
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

  // The REAL forbid-skip.mjs must derive exactly 6.
  const realForbid = readFileSync(FORBID_SKIP_MJS, 'utf8');
  check('real forbid-skip.mjs derives 6 CHECK arms', countForbidSkipChecks(realForbid).count === 6);

  // 2. A doc that claims the WRONG forbid-skip count must be REJECTED.
  const draftDoc = 'The gate has **7** patterns total.';
  const claims7 = extractClaims(draftDoc, FORBID_SKIP_CLAIM_PATTERNS);
  check('forbid-skip claim extractor finds the "7 patterns" claim', claims7.some((c) => c.value === 7));
  check(
    'forbid-skip gate would REJECT a doc claiming 7 against source 6',
    claims7.some((c) => c.value !== 6),
  );
  // And ACCEPT the corrected wording.
  const fixedDoc = 'The gate has **6** CHECK categories total.';
  const claims6 = extractClaims(fixedDoc, FORBID_SKIP_CLAIM_PATTERNS);
  check(
    'forbid-skip gate would ACCEPT a doc claiming the real 6',
    claims6.length > 0 && claims6.every((c) => c.value === 6),
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
