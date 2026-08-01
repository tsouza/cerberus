// issue-label.test.mjs — node:test guard for the deterministic issue labeler.
//
// Runs on the CHEAP forbid-skip lane (`node --test`) — no setup-node, no
// deps, no substrate — alongside the other discipline self-tests.
//
// A labeler that silently labels nothing is indistinguishable from no
// labeler at all, so the assertions below are POSITIVE throughout:
//
//   1. every AREA arm is reachable — each distinct `area/*` label the
//      mapping tables can produce is proven to come out of a real cerberus
//      issue title/body, and the loop is driven by the tables themselves so
//      a newly added area with no exercised case FAILS the test rather than
//      landing dark;
//   2. every TYPE arm is reachable — bug / test / performance / refactor /
//      documentation each classified from a real title, plus the
//      Conventional-Commit delegation to `pr-type-label.mjs`;
//   3. the decision layer is additive, capped, and idempotent;
//   4. the CLI exits 0 on a real fixture AND exits non-zero on each vacuity
//      shape it exists to catch (zero issues, unfetched body, an issue no
//      rule classifies). A gate never shown to fail is not a gate.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  APPLICABLE_TYPE_LABELS,
  AREA_SECONDARY_MIN_PATHS,
  HEAD_KEYWORD_TO_AREA,
  MAX_AREA_LABELS,
  MAX_TYPE_LABELS,
  PATH_PREFIX_TO_AREA,
  PRODUCTION_PATH_WEIGHT,
  TITLE_PREFIX_TO_AREA,
  TYPE_SIGNALS,
  TYPE_TIEBREAK_ORDER,
  areaForPath,
  areaForTitle,
  areaForTitlePrefix,
  assertBodyFetched,
  assertTablesUsable,
  citedPaths,
  inferAreas,
  inferType,
  labelsForIssue,
  rankPathAreas,
  scoreSignals,
} from './issue-label.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const CLI = join(HERE, 'issue-label.mjs');

function runCLI(env, args = []) {
  return spawnSync(process.execPath, [CLI, ...args], {
    encoding: 'utf8',
    env: { ...process.env, ...env },
  });
}

function fixtureFile(name, issues) {
  const dir = mkdtempSync(join(tmpdir(), 'issue-label-'));
  const file = join(dir, name);
  writeFileSync(file, JSON.stringify(issues));
  return file;
}

// ---------------------------------------------------------------------------
// Area: path resolution
// ---------------------------------------------------------------------------

test('areaForPath resolves by LONGEST prefix, not first match', () => {
  // `test/spec` and `test/spec/promql` are both in the table; the deeper one
  // has to win or every fixture issue would land on area/ci.
  assert.equal(areaForPath('test/spec/promql/rate.txtar'), 'area/promql');
  assert.equal(areaForPath('test/spec/strictscan_integration_test.go'), 'area/ci');
  // `internal/api` and `internal/api/prom` both resolve to the HTTP surface.
  assert.equal(areaForPath('internal/api/prom/handler.go'), 'area/api');
  assert.equal(areaForPath('internal/promql/lower.go'), 'area/promql');
});

test('areaForPath strips a trailing file:line cite and a leading ./', () => {
  assert.equal(areaForPath('internal/promql/lower.go:364'), 'area/promql');
  assert.equal(areaForPath('.github/workflows/e2e.yml:680-682'), 'area/ci');
  assert.equal(areaForPath('./internal/chsql/emit.go'), 'area/chsql');
});

test('areaForPath returns empty for an unmapped subtree', () => {
  assert.equal(areaForPath('docs/benchmarks.md'), '');
  assert.equal(areaForPath('README.md'), '');
  assert.equal(areaForPath('internal/'), '');
  // A prefix must match on a path BOUNDARY: `internal/chplanner` is not chplan.
  assert.equal(areaForPath('internal/chplanner/x.go'), '');
});

test('citedPaths extracts distinct repo-rooted paths from real issue prose', () => {
  const body = [
    'The **instant-mode** siblings — `internal/promql/histogram_quantile.go:577` —',
    'apply the bound emitted in `internal/chsql/range_bucket_fanout.go`.',
    'Also see internal/promql/histogram_quantile.go:884 (same file, second cite).',
    'Prose about internal state machines must not parse as a path.',
  ].join('\n');
  const got = citedPaths(body);
  // The `:577` / `:884` line cites are NOT part of the path, and the two
  // cites of the same file collapse to one entry.
  assert.deepEqual(got.sort(), [
    'internal/chsql/range_bucket_fanout.go',
    'internal/promql/histogram_quantile.go',
  ]);
  // "internal state machines" is prose, not a path.
  assert.ok(!got.some((p) => p.includes('state')));
});

test('rankPathAreas weights production code above harness citations', () => {
  // Issue #1465's real citation set: one production file against a workflow
  // and a spec harness. Unweighted, area/ci would out-count area/solver 2:1
  // and the issue would be filed against CI instead of the solver.
  const body = [
    '`internal/solver/executor_realch_integration_test.go` is the only lane;',
    '`.github/workflows/strict-scan.yml` runs it, and',
    '`test/spec/strictscan_integration_test.go` is its harness.',
  ].join('\n');
  const ranked = rankPathAreas(body);
  assert.equal(ranked[0].area, 'area/solver');
  assert.equal(ranked[0].weight, PRODUCTION_PATH_WEIGHT);
  assert.equal(ranked[1].area, 'area/ci');
  assert.equal(ranked[1].paths, 2);
});

test('rankPathAreas is order-independent (name tiebreak on equal score)', () => {
  const a = rankPathAreas('`internal/chsql/emit.go` then `internal/chplan/node.go`');
  const b = rankPathAreas('`internal/chplan/node.go` then `internal/chsql/emit.go`');
  assert.deepEqual(
    a.map((r) => r.area),
    b.map((r) => r.area),
  );
  assert.deepEqual(
    a.map((r) => r.area),
    ['area/chplan', 'area/chsql'],
  );
});

// ---------------------------------------------------------------------------
// Area: title resolution
// ---------------------------------------------------------------------------

test('areaForTitlePrefix reads the leading <prefix>: token, including compounds', () => {
  assert.equal(areaForTitlePrefix('promql: subquery inner is not evaluated'), 'area/promql');
  assert.equal(areaForTitlePrefix('chsql/tempo: the trace_id_ts envelope builder'), 'area/chsql');
  assert.equal(areaForTitlePrefix('e2e seed: rolling re-seeder only deletes'), 'area/e2e');
  assert.equal(areaForTitlePrefix('cmd/cerberus: run() is untestable'), 'area/api');
  // `prom:` is the HTTP head, `promql:` the language — the repo uses both.
  assert.equal(areaForTitlePrefix('prom: matrixFromCursor buffers every row'), 'area/api');
});

test('areaForTitlePrefix yields nothing for a TYPE-only prefix or no prefix', () => {
  assert.equal(areaForTitlePrefix('perf: cardinality ratchet has no /series row'), '');
  assert.equal(areaForTitlePrefix('docs: tidy'), '');
  assert.equal(areaForTitlePrefix('Route B is never exercised'), '');
});

test('areaForTitle falls back to a head keyword only when there is no prefix', () => {
  assert.equal(areaForTitle('The TraceQL property generator was never widened'), 'area/traceql');
  assert.equal(areaForTitle('No e2e ever drives the Tempo gRPC streaming path'), 'area/traceql');
  assert.equal(areaForTitle('Loki /tail WebSockets permanently occupy the budget'), 'area/logql');
  // "reference Prometheus" means PromQL semantics in this repo, not the HTTP head.
  assert.equal(areaForTitle('info() returns base series that reference Prometheus drops'), 'area/promql');
  // An explicit prefix outranks a keyword later in the same title.
  assert.equal(areaForTitle('chsql: Tempo metrics zero-fill renders the Inner subquery twice'), 'area/chsql');
});

// ---------------------------------------------------------------------------
// Area: every arm is reachable
// ---------------------------------------------------------------------------

// One real (title, body) case per distinct `area/*` the tables can emit. The
// test below is driven by the TABLES, so adding an area without adding a case
// here fails — a dark arm is a mapping that was never shown to work.
const AREA_ARM_CASES = Object.freeze({
  'area/promql': ['promql: histogram_quantile agg path sums raw bucket counts', '`internal/promql/lower.go:364`'],
  'area/logql': ['loki: | unpack swallows the parser-error labels', '`internal/logql/lsyntax/parse.go:12`'],
  'area/traceql': ['traceql: 7 LIVED CONDITIONALS_NEGATION mutants', '`internal/traceql/lower.go:88`'],
  'area/chplan': ['Slice-invariance registration stalled', '`internal/chplan/sliceinvariant.go`'],
  'area/optimizer': ['The transpose rule fires twice', '`internal/optimizer/transpose.go:40`'],
  'area/solver': ['Route B is never exercised', '`internal/solver/executor.go`'],
  'area/engine': ['engine: the fused subquery reduction design', '`internal/engine/pipeline.go`'],
  'area/chsql': ['chsql: metrics_compare emits the cohort seed scan three times', '`internal/chsql/metrics_compare.go`'],
  'area/chclient': ['chclient: columnar decoder discriminates by column name', '`internal/chclient/decode.go`'],
  'area/schema': ['schema: the override map drops a column', '`internal/schema/override.go`'],
  'area/api': ['prom: /api/v1/parse_query returns a Go type string', '`internal/api/prom/handler.go:251`'],
  'area/observability': ['telemetry: gRPC RPCs are invisible', '`internal/telemetry/middleware.go`'],
  'area/config': ['config: the env override loses precedence', '`internal/config/load.go`'],
  'area/ci': ['ci: the required coverage check short-circuits', '`.github/workflows/coverage.yml:31`'],
  'area/e2e': ['The k3d Grafana surface inventory is empty', '`test/e2e/grafana/dashboards/cerberus.json`'],
  'area/parser': ['parser: the upstream bump changed a node shape', 'no path cited here'],
});

test('every area arm in the mapping tables is reachable from a real issue', () => {
  const reachable = new Set([...Object.values(PATH_PREFIX_TO_AREA), ...Object.values(TITLE_PREFIX_TO_AREA), ...Object.values(HEAD_KEYWORD_TO_AREA)]);
  assert.ok(reachable.size > 0, 'the area tables produce no labels at all');

  const missingCases = [...reachable].filter((a) => !(a in AREA_ARM_CASES));
  assert.deepEqual(missingCases, [], `area(s) with no exercised case — add one to AREA_ARM_CASES: ${missingCases}`);

  for (const [area, [title, body]] of Object.entries(AREA_ARM_CASES)) {
    const got = inferAreas(title, body);
    assert.ok(got.includes(area), `${area}: inferAreas(${JSON.stringify(title)}) = [${got}]`);
  }
});

test('inferAreas caps the label count and gates secondary areas on evidence', () => {
  // A single passing mention of a second package earns nothing …
  const single = inferAreas('promql: histogram_quantile agg path', '`internal/chsql/histogram_quantile.go:12`');
  assert.deepEqual(single, ['area/promql']);

  // … two distinct files in that subtree do.
  const double = inferAreas(
    'promql: histogram_quantile agg path',
    '`internal/chsql/histogram_quantile.go:12` and `internal/chsql/emit_node.go:44`',
  );
  assert.deepEqual(double, ['area/promql', 'area/chsql']);
  assert.equal(AREA_SECONDARY_MIN_PATHS, 2);

  // Three candidate areas still yield at most MAX_AREA_LABELS.
  const many = inferAreas(
    'tempo: cross-cutting',
    ['`internal/api/tempo/a.go` `internal/api/tempo/b.go`', '`internal/chsql/a.go` `internal/chsql/b.go`'].join(' '),
  );
  assert.equal(many.length, MAX_AREA_LABELS);
  assert.deepEqual(many, ['area/traceql', 'area/api']);
});

test('inferAreas is deterministic — the same input always yields the same order', () => {
  const title = 'tempo: SearchMetrics.InspectedTraces means different things';
  const body = '`internal/api/tempo/search.go:10` `internal/api/tempo/grpc/server.go:20`';
  const first = inferAreas(title, body);
  for (let i = 0; i < 5; i++) assert.deepEqual(inferAreas(title, body), first);
});

// ---------------------------------------------------------------------------
// Type: every arm is reachable
// ---------------------------------------------------------------------------

test('inferType delegates a Conventional-Commit prefix to pr-type-label', () => {
  // These four are the CC prefixes cerberus issues actually use. The mapping
  // lives in pr-type-label.mjs and is REUSED, never restated here.
  assert.equal(inferType('perf: cardinality ratchet has no /series row', ''), 'performance');
  assert.equal(inferType('ci: the required coverage check short-circuits', ''), 'ci');
  assert.equal(inferType('docs: tidy the engine page', ''), 'documentation');
  assert.equal(inferType('test: add a traceql fixture', ''), 'test');
  assert.equal(inferType('feat: add holt-winters', ''), 'enhancement');
});

test('inferType classifies a wrong answer / divergence / silent empty as bug', () => {
  assert.equal(inferType('PromQL: `le` matchers on a classic-histogram selector are unsatisfiable (always empty)', ''), 'bug');
  assert.equal(inferType('TraceQL || mixing a span-level and a trace-level arm returns ClickHouse code 258', ''), 'bug');
  assert.equal(inferType('ReanchorRange ignores RangeWindow.Offset, so route B under-scans', ''), 'bug');
  assert.equal(inferType('LogQL bare | json does not flatten nested objects', ''), 'bug');
  assert.equal(inferType('Tempo exemplar failures degrade silently at four sites', ''), 'bug');
  assert.equal(inferType('prom: /api/v1/query_exemplars 400s on any selector', ''), 'bug');
});

test('inferType classifies missing coverage / hollow test / mutation survivor as test', () => {
  assert.equal(inferType('traceql: 7 LIVED CONDITIONALS_NEGATION mutants in lower.go', ''), 'test');
  assert.equal(inferType('logql/structured_metadata_selector_unchanged is an inert fixture — it cannot fail', ''), 'test');
  assert.equal(inferType('chsql: emitSearchTraceLimit had zero golden or round-trip coverage', ''), 'test');
  assert.equal(inferType('prom: summary-metric short-circuit has no handler-level test', ''), 'test');
  assert.equal(inferType('The TraceQL property generator was never widened past its first sweep', ''), 'test');
});

test('inferType classifies an unbounded resource / scan cost / fan-out as performance', () => {
  assert.equal(inferType('tempo: metrics grid has no per-step density bound', ''), 'performance');
  assert.equal(inferType('chsql: metrics_compare emits the cohort seed scan three times', ''), 'performance');
  assert.equal(inferType('prom: matrixFromCursor buffers every row before emitting', ''), 'performance');
  assert.equal(inferType('The regex-name fan-out is unbounded', ''), 'performance');
});

test('inferType classifies duplication / half-finished mechanical change as refactor', () => {
  assert.equal(inferType('chsql/tempo: the trace_id_ts envelope builder is duplicated across two packages', ''), 'refactor');
  assert.equal(inferType('compat: the compat-<QL> rename stopped half-way', ''), 'refactor');
  assert.equal(inferType('logql: log-stream projection emits a meaningless placeholder column', ''), 'refactor');
});

test('inferType classifies stale or contradictory prose as documentation', () => {
  assert.equal(inferType('docs/benchmarks.md contradicts itself on the set-op residual', ''), 'documentation');
  assert.equal(inferType('Tempo handler docstring asserts the opposite of the code it documents', ''), 'documentation');
  assert.equal(inferType('count_values rejection message names milestone M1.7, which no longer exists', ''), 'documentation');
  assert.equal(inferType('engine: the design exists only in a merged PR body', ''), 'documentation');
});

test('every type in TYPE_SIGNALS is an applicable label and is covered by the tiebreak order', () => {
  for (const type of Object.keys(TYPE_SIGNALS)) {
    assert.ok(APPLICABLE_TYPE_LABELS.includes(type), `${type} is not an applicable label`);
    assert.ok(TYPE_TIEBREAK_ORDER.includes(type), `${type} has no tiebreak rank`);
  }
  for (const type of TYPE_TIEBREAK_ORDER) {
    assert.ok(type in TYPE_SIGNALS, `${type} ranks in the tiebreak but has no signals`);
  }
});

test('the TITLE decides when it carries any signal; the body is the fallback only', () => {
  // A precise title beats a long body that brushes other signals in passing.
  const title = 'loki: /loki/api/v1/patterns emits level:"" on every cluster';
  const body = 'See `docs/loki-patterns-impl-plan.md` — the doc comment claims otherwise.';
  assert.ok(scoreSignals(title).bug > 0, 'the title must carry a bug signal');
  assert.equal(inferType(title, body), 'bug');

  // A title with no signal at all hands the decision to the body.
  const silent = 'The error-reason taxonomy needs a consumer';
  assert.equal(
    Object.values(scoreSignals(silent)).reduce((a, b) => a + b, 0),
    0,
    'this title is meant to carry no signal',
  );
  assert.equal(inferType(silent, 'The dashboard has no test and no fixture asserting it.'), 'test');
});

test('inferType returns empty when nothing matches at all', () => {
  assert.equal(inferType('Thoughts on naming', 'Just a question about naming things.'), '');
  assert.equal(inferType('', ''), '');
  assert.equal(inferType(null, null), '');
});

// ---------------------------------------------------------------------------
// Decision layer: additive, capped, idempotent
// ---------------------------------------------------------------------------

test('labelsForIssue proposes the area + type delta for an unlabeled issue', () => {
  const d = labelsForIssue({
    title: 'promql: subquery inner is not evaluated on the subquery step grid',
    body: 'Anchors are missing — see `internal/promql/subquery.go:80`.',
    labels: [],
  });
  assert.deepEqual(d.missing, ['area/promql', 'bug']);
  assert.equal(d.unclassified, false);
  assert.deepEqual(d.skipped, []);
});

test('labelsForIssue is IDEMPOTENT — a second run on the result is a no-op', () => {
  const issue = {
    title: 'promql: subquery inner is not evaluated on the subquery step grid',
    body: '`internal/promql/subquery.go:80`',
    labels: [],
  };
  const first = labelsForIssue(issue);
  const second = labelsForIssue({ ...issue, labels: first.missing.map((name) => ({ name })) });
  assert.deepEqual(second.missing, []);
  const third = labelsForIssue({ ...issue, labels: second.missing.concat(first.missing).map((name) => ({ name })) });
  assert.deepEqual(third.missing, []);
});

test('labelsForIssue never proposes removing or replacing a label a human set', () => {
  const d = labelsForIssue({
    title: 'promql: subquery inner is not evaluated',
    body: 'Anchors are missing — see `internal/promql/subquery.go:80`.',
    labels: [{ name: 'good first issue' }, { name: 'help wanted' }],
  });
  // Only additions are ever returned, and the human labels are untouched.
  assert.deepEqual(d.missing, ['area/promql', 'bug']);
  assert.ok(!d.missing.includes('good first issue'));
});

test('labelsForIssue respects a human-set TYPE label instead of adding a second', () => {
  const d = labelsForIssue({
    title: 'promql: subquery inner is not evaluated',
    body: 'Anchors are missing — see `internal/promql/subquery.go:80`.',
    labels: [{ name: 'enhancement' }],
  });
  assert.deepEqual(d.missing, ['area/promql']);
  assert.equal(d.skipped.length, 1);
  assert.match(d.skipped[0], /^bug \(type cap 1 already met by: enhancement\)/);
  assert.equal(MAX_TYPE_LABELS, 1);
});

test('labelsForIssue respects a human-set AREA label when the cap is already met', () => {
  const body = '`internal/chsql/a.go` `internal/chsql/b.go`';
  const labels = [{ name: 'area/optimizer' }, { name: 'area/schema' }];
  const d = labelsForIssue({ title: 'promql: the step grid is not aligned', body, labels });
  assert.deepEqual(d.missing, ['bug']);
  assert.equal(d.skipped.length, 2, `expected both computed areas withheld, got ${d.skipped}`);
  for (const s of d.skipped) assert.match(s, /area cap 2 already met by: area\/optimizer, area\/schema/);
});

test('labelsForIssue flags an issue no rule can classify', () => {
  const d = labelsForIssue({ title: 'Thoughts on naming', body: 'Just a question.', labels: [] });
  assert.equal(d.unclassified, true);
  assert.deepEqual(d.missing, []);
});

// ---------------------------------------------------------------------------
// Anti-vacuity guards
// ---------------------------------------------------------------------------

test('assertTablesUsable passes on the live tables', () => {
  assert.deepEqual(assertTablesUsable(), []);
  assert.ok(Object.keys(PATH_PREFIX_TO_AREA).length > 0);
  assert.ok(Object.keys(TITLE_PREFIX_TO_AREA).length > 0);
});

test('assertBodyFetched separates "never fetched" from "author wrote nothing"', () => {
  assert.match(assertBodyFetched({ number: 7, title: 't' }), /never fetched/);
  assert.equal(assertBodyFetched({ number: 7, title: 't', body: null }), '');
  assert.equal(assertBodyFetched({ number: 7, title: 't', body: '' }), '');
});

// ---------------------------------------------------------------------------
// CLI end-to-end — proves the gate both PASSES and FAILS
// ---------------------------------------------------------------------------

const DRY = { ISSUE_LABEL_MODE: 'backfill', ISSUE_LABEL_DRY_RUN: '1' };

test('CLI dry-run exits 0 and reports a per-issue line for a real fixture', () => {
  const file = fixtureFile('ok.json', [
    {
      number: 1477,
      title: 'PromQL: subquery inner is not evaluated on the subquery step grid',
      body: 'It diverges from reference Prometheus — `internal/promql/subquery.go:80`.',
      labels: [],
    },
    {
      number: 1445,
      title: 'chsql: metrics_compare emits the cohort seed scan three times',
      body: 'Seen in `internal/chsql/metrics_compare.go`.',
      labels: [],
    },
  ]);
  const res = runCLI({ ...DRY, ISSUE_LABEL_FIXTURE: file });
  assert.equal(res.status, 0, res.stdout + res.stderr);
  assert.match(res.stdout, /#1477 DRY-RUN would add \[area\/promql, bug\]/);
  assert.match(res.stdout, /#1445 DRY-RUN would add \[area\/chsql, performance\]/);
  assert.match(res.stdout, /2 issue\(s\) scanned, 2 issue\(s\) would be labeled, 4 label\(s\) pending/);
});

test('CLI reports zero pending labels for an already-labeled fixture (idempotent)', () => {
  const file = fixtureFile('done.json', [
    {
      number: 1477,
      title: 'PromQL: subquery inner is not evaluated on the subquery step grid',
      body: 'It diverges from reference Prometheus — `internal/promql/subquery.go:80`.',
      labels: [{ name: 'area/promql' }, { name: 'bug' }],
    },
  ]);
  const res = runCLI({ ...DRY, ISSUE_LABEL_FIXTURE: file });
  assert.equal(res.status, 0, res.stdout + res.stderr);
  assert.match(res.stdout, /#1477: already carries its computed labels/);
  assert.match(res.stdout, /0 label\(s\) pending, 1 already complete/);
});

test('CLI FAILS on a vacuous run — zero issues processed', () => {
  const file = fixtureFile('empty.json', []);
  const res = runCLI({ ...DRY, ISSUE_LABEL_FIXTURE: file });
  assert.equal(res.status, 1);
  assert.match(res.stdout, /::error[^:]*::issue-label \(backfill\) processed ZERO issues/);
});

test('CLI FAILS when a body was never fetched', () => {
  const file = fixtureFile('nobody.json', [{ number: 42, title: 'promql: something', labels: [] }]);
  const res = runCLI({ ...DRY, ISSUE_LABEL_FIXTURE: file });
  assert.equal(res.status, 1);
  assert.match(res.stdout, /no `body` field in the payload/);
});

test('CLI FAILS, naming the issue, when a rule classifies nothing', () => {
  const file = fixtureFile('residue.json', [{ number: 99, title: 'Thoughts on naming', body: 'A question.', labels: [] }]);
  const res = runCLI({ ...DRY, ISSUE_LABEL_FIXTURE: file });
  assert.equal(res.status, 1);
  assert.match(res.stdout, /#99 — Thoughts on naming/);
  assert.match(res.stdout, /widen the mapping/);
});

test('CLI rejects an unknown mode and a fixture outside dry-run', () => {
  const file = fixtureFile('ok2.json', [{ number: 1, title: 'promql: x', body: 'internal/promql/a.go', labels: [] }]);
  const bad = runCLI({ ISSUE_LABEL_MODE: 'sideways', ISSUE_LABEL_FIXTURE: file, ISSUE_LABEL_DRY_RUN: '1' });
  assert.equal(bad.status, 1);
  assert.match(bad.stdout, /ISSUE_LABEL_MODE must be/);

  const wet = runCLI({
    ISSUE_LABEL_MODE: 'backfill',
    ISSUE_LABEL_FIXTURE: file,
    GITHUB_REPOSITORY: 'o/r',
    GITHUB_TOKEN: 't',
  });
  assert.equal(wet.status, 1);
  assert.match(wet.stdout, /dry-run-only input/);
});

test('CLI --check-tables proves the mapping tables are usable', () => {
  const res = runCLI({}, ['--check-tables']);
  assert.equal(res.status, 0, res.stdout + res.stderr);
  assert.match(res.stdout, /mapping tables usable/);
});
