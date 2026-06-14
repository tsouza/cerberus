// promql-surface-gate.mjs — reference-backed full-surface PromQL
// rejection-completeness gate (#106).
//
// The bug this closes: the surface-parity oracle modelled the reference
// verdict for experimental PromQL functions with a hardcoded
// `if fn.Experimental { ref = reject }` stand-in. That assumption only
// holds for a reference started WITHOUT
// --enable-feature=promql-experimental-functions. With the flag ON the
// reference ACCEPTS every experimental function it implements, so an
// UNIMPLEMENTED function cerberus rejected could silently masquerade as a
// "parity rejection" instead of surfacing as a genuine coverage gap.
//
// This gate replaces the stand-in with a REAL flag-enabled reference:
// prom/prometheus (pinned tag) started with
// --enable-feature=promql-experimental-functions, queried over HTTP
// /api/v1/query. The verdict is parse + type validation, which is
// data-independent — a well-typed accepted expression returns 2xx even
// with no series seeded, a parse/type error returns 4xx — so no seeding is
// required and the reference posture is a pure function of the grammar
// surface under the experimental-functions flag.
//
// What it does, all against the live reference:
//
//   1. ARTIFACT PARITY — re-probe every PromQL fn + aggregator symbol in
//      the inventory and assert the live reference verdict equals the
//      pinned test/surface-parity/promql-reference-verdicts.json (the
//      Docker-free in-process ratchet reads this artifact, so drift here
//      means the artifact is stale; in REGENERATE mode the artifact is
//      rewritten instead). This also fails if the inventory carries a
//      parser.Functions symbol absent from the artifact (a fork bump that
//      grew the surface) — no symbol may be uncovered.
//
//   2. SILENT-GAP GATE — for every parser.Functions symbol whose live
//      reference verdict is ACCEPT (2xx) and whose cerberus verdict
//      (the in-process parse->lower->optimize->emit runtime verdict
//      recorded in inventory.json — the same pipeline the HTTP handler
//      runs) is REJECT (4xx), the symbol is a coverage gap. The gate
//      FAILS on any such gap that is NOT already a recorded wrong-reject
//      in the committed inventory ledger. A NEW gap (an experimental fn
//      cerberus silently fails to lower, or a fork bump that adds an
//      accepted symbol cerberus doesn't handle) turns the gate red. The
//      ledger is the VISIBLE burndown surface, not an escape hatch: a
//      recorded wrong-reject is pinned by the inventory ratchet
//      (TestWrongRejectionsAreRatcheted) + a declared showcase
//      parity-rejection panel (cross-checked in step 3), so it can never
//      be silent.
//
//   3. SHOWCASE CROSS-CHECK — every showcase-promql panel whose declared
//      outcome is a rejection (title contains "parity rejection", or the
//      description states cerberus rejects/gates the query) is checked
//      against the flag-enabled reference. The contract: a declared-
//      rejection panel whose function-call query the flag-enabled
//      reference ACCEPTS is only legitimate if that function is RECORDED
//      as a wrong-reject in the inventory ledger (a visible, ratcheted,
//      deliberate flag-OFF-parity gate). A declared-rejection panel the
//      reference accepts whose function is NOT in the ledger is a misfiled
//      gap (a real fn cerberus should implement, hidden behind an "error"
//      label) and FAILS loudly. Panels referencing NO parser function
//      (bare-selector rejections like the query_range resolution cap,
//      whose rejection is a range-window-grid mechanism an instant
//      /api/v1/query oracle structurally cannot exercise) are out of scope
//      for this function-surface gate and skipped.
//
//   4. RUNTIME COVERAGE — the inventory's "covered" notion is runtime, not
//      parse-only: a symbol counts as covered iff cerberus's recorded
//      verdict is accept (it actually evaluates -> 2xx). This is already
//      how the inventory's `cerberus` field is computed (full pipeline),
//      so the gate asserts the invariant rather than re-deriving it.
//
// Env contract:
//   PROM_IMAGE     reference image (default prom/prometheus:v3.11.3)
//   REF_PORT       host port for the flag-enabled reference (default 39090)
//   INVENTORY      path to inventory.json
//                  (default test/surface-parity/inventory.json)
//   ARTIFACT       path to promql-reference-verdicts.json
//                  (default test/surface-parity/promql-reference-verdicts.json)
//   SHOWCASE       path to showcase-promql.json
//                  (default test/e2e/grafana/compose/dashboards/showcase-promql.json)
//   REGENERATE     when "1", rewrite ARTIFACT from the live reference and
//                  exit 0 (artifact-regeneration mode); otherwise verify.
//   KEEP_REF       when "1", leave the reference container running on exit
//                  (local debugging).
//
// Exit codes: 0 = all checks pass (or regenerate completed), 1 = any gate
// failure / infra error.
//
// Imports only node: builtins + lib/gh.mjs (no npm deps, no setup-node).

import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { error, notice, log, capture } from './lib/gh.mjs';

const PROM_IMAGE = process.env.PROM_IMAGE || 'prom/prometheus:v3.11.3';
const REF_PORT = process.env.REF_PORT || '39090';
const INVENTORY = process.env.INVENTORY || 'test/surface-parity/inventory.json';
const ARTIFACT = process.env.ARTIFACT || 'test/surface-parity/promql-reference-verdicts.json';
const SHOWCASE =
  process.env.SHOWCASE || 'test/e2e/grafana/compose/dashboards/showcase-promql.json';
const REGENERATE = process.env.REGENERATE === '1';
const KEEP_REF = process.env.KEEP_REF === '1';

const REF_CONTAINER = 'cerberus-promql-surface-ref';
// A fixed instant; the verdict is data-independent so the exact value is
// irrelevant — it only needs to be a valid evaluation timestamp.
const PROBE_TIME = '1700000000';
const READY_TIMEOUT_MS = 60_000;
const READY_POLL_MS = 1_000;

function die(msg) {
  error(msg);
  teardown();
  process.exit(1);
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// startReference launches the flag-enabled reference Prometheus and waits
// for /-/ready. The reference scrapes nothing and needs no fixture — the
// surface verdict is parse + type validation only.
function startReference() {
  capture('docker', ['rm', '-f', REF_CONTAINER]);
  const promYml = [
    'global:',
    '  scrape_interval: 15s',
    'scrape_configs: []',
    '',
  ].join('\n');
  const cfgPath = '/tmp/promql-surface-ref.yml';
  writeFileSync(cfgPath, promYml);
  const run = capture('docker', [
    'run', '-d', '--name', REF_CONTAINER,
    '-p', `${REF_PORT}:9090`,
    '-v', `${cfgPath}:/etc/prometheus/prometheus.yml:ro`,
    PROM_IMAGE,
    '--config.file=/etc/prometheus/prometheus.yml',
    '--storage.tsdb.path=/prometheus',
    '--web.enable-remote-write-receiver',
    '--enable-feature=promql-experimental-functions',
    '--web.listen-address=:9090',
  ]);
  if (run.status !== 0) {
    die(`failed to start reference container: ${run.stderr.trim() || run.stdout.trim()}`);
  }
}

function teardown() {
  if (KEEP_REF) {
    log(`KEEP_REF=1: leaving ${REF_CONTAINER} running on :${REF_PORT}`);
    return;
  }
  capture('docker', ['rm', '-f', REF_CONTAINER]);
}

async function waitReady() {
  const url = `http://localhost:${REF_PORT}/-/ready`;
  const deadline = Date.now() + READY_TIMEOUT_MS;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) return;
    } catch {
      // not up yet
    }
    await sleep(READY_POLL_MS);
  }
  die(`reference Prometheus did not become ready on :${REF_PORT} within ${READY_TIMEOUT_MS}ms`);
}

// referenceVerdict returns "accept" (HTTP 2xx) or "reject" (4xx) for a
// probe against the flag-enabled reference.
async function referenceVerdict(probe) {
  const url = `http://localhost:${REF_PORT}/api/v1/query`;
  const body = new URLSearchParams({ query: probe, time: PROBE_TIME });
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body,
  });
  return res.ok ? 'accept' : 'reject';
}

function loadJSON(path) {
  try {
    return JSON.parse(readFileSync(path, 'utf8'));
  } catch (e) {
    die(`cannot read/parse ${path}: ${e.message}`);
  }
}

// promSymbols returns the PromQL fn + aggregator inventory entries (the
// experimental surface lives entirely here; binary-ops / modifiers are
// core PromQL and not in the artifact).
function promSymbols(inv) {
  return inv.entries.filter(
    (e) => e.head === 'promql' && (e.kind === 'function' || e.kind === 'aggregator'),
  );
}

// committedWrongRejects returns the set of promql symbols recorded as
// wrong-reject in the committed inventory ledger — the visible burndown
// surface the silent-gap gate measures NEW gaps against.
function committedWrongRejects(inv) {
  const s = new Set();
  for (const e of inv.entries) {
    if (e.head === 'promql' && e.class === 'wrong-reject') s.add(e.symbol);
  }
  return s;
}

// promFnNames returns the set of bare parser function/aggregator names
// ("rate", "limitk", ...) from the inventory symbols, used to detect which
// function a showcase expr invokes.
function promFnNames(inv) {
  const names = new Set();
  for (const e of inv.entries) {
    if (e.head !== 'promql') continue;
    if (e.kind === 'function' && e.symbol.startsWith('fn:')) names.add(e.symbol.slice('fn:'.length));
    if (e.kind === 'aggregator' && e.symbol.startsWith('agg:')) names.add(e.symbol.slice('agg:'.length));
  }
  return names;
}

// invokedFn returns the parser function/aggregator name a showcase expr
// invokes as a call (`name(...)`), or "" if the expr is a bare selector /
// modifier with no function call (e.g. `up`, `up @ start()`).
function invokedFn(expr, fnNames) {
  const m = /([a-zA-Z_][a-zA-Z0-9_]*)\s*\(/.exec(expr);
  if (m && fnNames.has(m[1])) return m[1];
  return '';
}

// showcaseRejectionExprs walks the showcase dashboard and returns the
// distinct target exprs of panels whose DECLARED outcome is a rejection:
// the title carries "parity rejection", or the description states cerberus
// rejects/gates the query.
function showcaseRejectionExprs(dash) {
  const exprs = new Set();
  const isRejectionPanel = (p) => {
    const title = String(p.title || '').toLowerCase();
    const desc = String(p.description || '').toLowerCase();
    if (title.includes('parity rejection')) return true;
    // Description-declared rejection: "cerberus rejects" / "gates it" /
    // "MUST reject". Kept narrow so an accept-panel that merely mentions
    // the word "reject" in prose isn't swept in.
    if (/cerberus rejects|gates it|must reject|reference .*rejects/.test(desc)) return true;
    return false;
  };
  const walk = (node) => {
    if (Array.isArray(node)) {
      node.forEach(walk);
      return;
    }
    if (node && typeof node === 'object') {
      if (isRejectionPanel(node) && Array.isArray(node.targets)) {
        for (const t of node.targets) {
          if (t && typeof t.expr === 'string' && t.expr.trim()) exprs.add(t.expr.trim());
        }
      }
      for (const v of Object.values(node)) walk(v);
    }
  };
  walk(dash);
  return [...exprs];
}

async function main() {
  const inv = loadJSON(INVENTORY);
  const symbols = promSymbols(inv);
  if (symbols.length === 0) {
    die(`${INVENTORY} carries no promql fn/aggregator entries`);
  }

  startReference();
  await waitReady();
  notice(`flag-enabled reference up: ${PROM_IMAGE} --enable-feature=promql-experimental-functions on :${REF_PORT}`);

  // Probe the live reference for every symbol, keyed by the inventory's
  // own probe (the domain-aware query the in-process prober synthesizes).
  const live = new Map(); // symbol -> "accept"|"reject"
  for (const e of symbols) {
    live.set(e.symbol, await referenceVerdict(e.probe));
  }

  // ---- REGENERATE mode: rewrite the artifact + exit. ----
  if (REGENERATE) {
    const verdicts = {};
    for (const sym of [...live.keys()].sort()) verdicts[sym] = live.get(sym);
    const out = {
      reference: `${PROM_IMAGE} --enable-feature=promql-experimental-functions`,
      oracle:
        'HTTP GET /api/v1/query verdict (2xx=accept, 4xx=reject); data-independent parse+type validation, no seeding required',
      generated_by:
        '.github/scripts/promql-surface-gate.mjs (regenerate mode), pinned + verified by test/surface-parity',
      verdicts,
    };
    writeFileSync(ARTIFACT, `${JSON.stringify(out, null, 2)}\n`);
    notice(`regenerated ${ARTIFACT} (${Object.keys(verdicts).length} symbols)`);
    teardown();
    process.exit(0);
  }

  let failed = false;

  // ---- 1. ARTIFACT PARITY ----
  const artifact = loadJSON(ARTIFACT);
  const pinned = artifact.verdicts || {};
  const artifactDrift = [];
  const uncovered = [];
  for (const e of symbols) {
    const liveV = live.get(e.symbol);
    const pinnedV = pinned[e.symbol];
    if (pinnedV === undefined) {
      uncovered.push(e.symbol);
    } else if (pinnedV !== liveV) {
      artifactDrift.push(`${e.symbol}: pinned=${pinnedV} live=${liveV}`);
    }
  }
  if (uncovered.length > 0) {
    error(
      `${ARTIFACT} is missing ${uncovered.length} parser symbol(s) present in the inventory ` +
        `(the pinned surface grew — regenerate with REGENERATE=1): ${uncovered.join(', ')}`,
    );
    failed = true;
  }
  if (artifactDrift.length > 0) {
    error(
      `${ARTIFACT} drifted from the live flag-enabled reference (regenerate with REGENERATE=1):\n  ` +
        artifactDrift.join('\n  '),
    );
    failed = true;
  }
  if (uncovered.length === 0 && artifactDrift.length === 0) {
    notice(`artifact parity OK: ${symbols.length} symbols match the live flag-enabled reference`);
  }

  // ---- 2. SILENT-GAP GATE ----
  const ledger = committedWrongRejects(inv);
  const newGaps = [];
  for (const e of symbols) {
    const refV = live.get(e.symbol);
    const cerV = e.cerberus; // in-process runtime verdict (== HTTP handler pipeline)
    if (refV === 'accept' && cerV === 'reject' && !ledger.has(e.symbol)) {
      newGaps.push(`${e.symbol} (probe: ${e.probe}; cerberus error: ${e.cerberus_error || 'n/a'})`);
    }
  }
  if (newGaps.length > 0) {
    error(
      `SURFACE GAP: ${newGaps.length} PromQL symbol(s) the flag-enabled reference ACCEPTS but ` +
        `cerberus REJECTS, not recorded as a wrong-reject in ${INVENTORY}. An unimplemented PromQL ` +
        `function is silently masquerading as a parity rejection. Implement the lowering, or (if a ` +
        `deliberate, declared rejection) regenerate the inventory + add a showcase parity-rejection ` +
        `panel:\n  ` +
        newGaps.join('\n  '),
    );
    failed = true;
  } else {
    notice(`silent-gap gate OK: no unrecorded cerberus-reject / reference-accept symbols`);
  }

  // ---- 3. SHOWCASE CROSS-CHECK ----
  const dash = loadJSON(SHOWCASE);
  const rejectionExprs = showcaseRejectionExprs(dash);
  const fnNames = promFnNames(inv);
  // Ledger keyed by bare fn name (strip the "fn:"/"agg:" prefix) so a
  // showcase expr's invoked function can be matched against it.
  const ledgerFns = new Set();
  for (const sym of ledger) {
    const i = sym.indexOf(':');
    ledgerFns.add(i >= 0 ? sym.slice(i + 1) : sym);
  }
  const misfiled = [];
  for (const expr of rejectionExprs) {
    const refV = await referenceVerdict(expr);
    if (refV !== 'accept') continue; // reference rejects too: a true parity rejection.
    const fn = invokedFn(expr, fnNames);
    if (fn === '') continue; // bare-selector rejection (e.g. resolution cap): out of scope.
    if (ledgerFns.has(fn)) continue; // recorded wrong-reject: a visible, declared gate.
    misfiled.push(`${expr} (invokes ${fn}, reference-accepted, NOT in the wrong-reject ledger)`);
  }
  if (misfiled.length > 0) {
    error(
      `SHOWCASE MISFILE: ${misfiled.length} showcase panel(s) declared as a rejection whose query the ` +
        `flag-enabled reference ACCEPTS and whose function is NOT a recorded wrong-reject — a real ` +
        `PromQL function hidden behind an "error" label. These are coverage gaps, not parity rejections:\n  ` +
        misfiled.join('\n  '),
    );
    failed = true;
  } else {
    notice(
      `showcase cross-check OK: every declared-rejection panel is either reference-rejected, a ` +
        `bare-selector (out-of-scope) rejection, or a recorded wrong-reject in the ledger ` +
        `(${rejectionExprs.length} panel(s) examined)`,
    );
  }

  // ---- 4. RUNTIME COVERAGE invariant ----
  // A symbol is "covered" iff cerberus's recorded verdict is accept (it
  // evaluates -> 2xx). Assert the inventory's cerberus field is a real
  // accept/reject (not parse-only), which the shape test also pins.
  for (const e of symbols) {
    if (e.cerberus !== 'accept' && e.cerberus !== 'reject') {
      error(`${e.symbol}: cerberus verdict ${JSON.stringify(e.cerberus)} is not a runtime accept/reject`);
      failed = true;
    }
  }

  teardown();
  process.exit(failed ? 1 : 0);
}

main().catch((e) => {
  die(`promql-surface-gate.mjs crashed: ${e && e.stack ? e.stack : e}`);
});
