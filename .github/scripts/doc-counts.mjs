// doc-counts.mjs — the assert-from-source doc-count gate.
//
// Prose in the docs states integer counts that DESCRIBE source structures:
// "N forbid-skip checks", "N-layer test map". Those literals rot the moment
// the structure they describe grows or shrinks. This gate derives each count
// LIVE from the source of truth and asserts the doc-stated integer equals it,
// so a count can never silently drift. It is assert-from-source, NOT a pinned
// literal (which would just relocate the staleness into a second place).
//
// Five assertions:
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
//   4. compat parity ROSTER TABLE — docs/compatibility.md prints the same
//      per-head passed/total as a markdown table ("| <head> | <p> / <t> |"). It
//      is the number a reader sees first, and until this assertion existed
//      nothing compared it to anything: it drifted below the baseline across
//      three consecutive corpus moves (#1686 → #1717 → #1746 left it reading
//      120, 124 and 127 against baselines of 124, 126 and 129) because each PR
//      updated the header floor the gate DID check and hand-carried the table.
//      The gate asserts exactly one table row per head, each matching the
//      baseline, so the two statements of the same fact cannot separate again.
//
//   5. forbid-skip workflow callers — every `CHECK:` value any workflow hands
//      forbid-skip.mjs must name a live registry entry (or `all`). A stale
//      caller is not cosmetic drift: the invocation exits 1, and since the
//      compatibility lane's `gate` is `needs:` for all three required
//      compatibility heads, it reds every PR in the repository until fixed.
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

// parityTableClaims — the passed/total docs/compatibility.md's roster table
// prints for each head, one entry per matching row. The head name is matched as
// a WHOLE table cell (`|` … `|`) rather than as a substring, because `tempo` is
// a prefix of `tempo-grpc` and a substring match would read the wrong row for
// one of them and report two claims for the other. Returns every match per head
// for the same reason parityFloorClaims does: zero rows means the table stopped
// stating a head at all, which is a drift the gate must catch rather than skip.
export function parityTableClaims(src, heads) {
  const out = {};
  for (const head of heads) {
    const re = new RegExp(`^\\|\\s*${head}\\s*\\|\\s*(\\d+)\\s*/\\s*(\\d+)\\s*\\|`, 'gm');
    out[head] = [...src.matchAll(re)].map((m) => ({ passed: Number(m[1]), total: Number(m[2]) }));
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

// assertParityRestatement compares one restatement of the per-head parity
// numbers against the committed baseline that owns them. `extract` pulls the
// claims out of `source`; the rest is the same comparison for every restatement,
// so a second place that prints the numbers costs one call rather than a second
// copy of this loop. Exactly one claim per head: a second one is a
// contradiction, and none means the restatement stopped stating what the gate
// validates.
function assertParityRestatement({ label, sourcePath, sourceName, extract, oncePerHeadWhy }) {
  const baseline = JSON.parse(readFileSync(PARITY_BASELINE, 'utf8'));
  const heads = Object.keys(baseline.heads ?? {});
  if (heads.length === 0) {
    error(`${label}: compatibility/parity-baseline.json declares no heads to validate against`);
    return false;
  }
  const claims = extract(readFileSync(sourcePath, 'utf8'), heads);
  let ok = true;
  for (const head of heads) {
    const { passed, total } = baseline.heads[head];
    const found = claims[head];
    if (found.length !== 1) {
      error(
        `${label}: ${sourceName} states ${found.length} "${head} <passed>/<total>" entries, ` +
          `expected exactly 1 — ${oncePerHeadWhy}`,
        { file: sourceName },
      );
      ok = false;
      continue;
    }
    if (found[0].passed !== passed || found[0].total !== total) {
      error(
        `${label}: ${sourceName} claims ${head} ${found[0].passed}/${found[0].total} but ` +
          `compatibility/parity-baseline.json has ${passed}/${total}`,
        { file: sourceName },
      );
      ok = false;
    }
  }
  return ok;
}

// assertParityFloors compares the floors stated in compat-ratchet.mjs's header
// against the committed baseline it describes.
function assertParityFloors() {
  return assertParityRestatement({
    label: 'parity-floor',
    sourcePath: COMPAT_RATCHET_MJS,
    sourceName: '.github/scripts/compat-ratchet.mjs',
    extract: parityFloorClaims,
    oncePerHeadWhy:
      'README.md and docs/test-strategy.md send readers there for the canonical ' +
      "numbers, so the header must state each head's floor once",
  });
}

// assertParityTable compares docs/compatibility.md's roster table against the
// same baseline. It is a separate assertion rather than a second pattern on the
// floor one because the failure it catches is different: the header floor moved
// with every corpus change and the table did not, so the two numbers describing
// one fact were free to disagree.
function assertParityTable() {
  return assertParityRestatement({
    label: 'parity-table',
    sourcePath: COMPAT_DOC,
    sourceName: 'docs/compatibility.md',
    extract: parityTableClaims,
    oncePerHeadWhy:
      'the roster table is the numbers a reader meets first, and it prints one ' +
      'row per head',
  });
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
  const parityTableOk = assertParityTable();

  const callers = forbidSkipCallers(readWorkflows());
  log(
    `forbid-skip workflow callers (live): ${callers.length} ` +
      `[${callers.map((c) => `${c.name}:${c.check}`).join(', ')}]`,
  );
  const callersOk = assertForbidSkipCallers(fsNames, callers);

  if (forbidOk && layerOk && parityOk && parityTableOk && callersOk) {
    notice(
      `doc-counts: all doc-stated counts match source ` +
        `(forbid-skip=${fsCount}, test-layers=${layerCount}, parity floors and the ` +
        `docs/compatibility.md roster table both match the baseline, ` +
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

  // 6. The roster-table extractor reads a markdown row, does NOT confuse a head
  //    whose name prefixes another's, and rejects a row that drifted.
  const tableHeads = ['prometheus', 'tempo', 'tempo-grpc'];
  const table = [
    '| head       | passed/total |',
    '| ---------- | ------------ |',
    '| prometheus | 748 / 748    |',
    '| tempo      | 68 / 68      |',
    '| tempo-grpc | 65 / 65      |',
  ].join('\n');
  const tableClaims = parityTableClaims(table, tableHeads);
  check(
    'table extractor reads a markdown roster row',
    tableClaims.prometheus.length === 1 && tableClaims.prometheus[0].passed === 748,
  );
  check(
    'table extractor keeps tempo and tempo-grpc apart',
    tableClaims.tempo.length === 1 &&
      tableClaims.tempo[0].total === 68 &&
      tableClaims['tempo-grpc'].length === 1 &&
      tableClaims['tempo-grpc'][0].total === 65,
  );
  const staleTable = tableClaims.prometheus[0].passed !== 747;
  check('table gate would REJECT a row claiming 747/747 against a 748/748 baseline', staleTable);
  check(
    'table gate would REJECT a table that dropped a head row entirely',
    parityTableClaims('| head | passed/total |', tableHeads).tempo.length === 0,
  );
  // The REAL table must match the REAL baseline — the assertion the gate runs.
  check('real docs/compatibility.md roster table matches compatibility/parity-baseline.json', assertParityTable());

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
