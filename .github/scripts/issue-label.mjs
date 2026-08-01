// issue-label.mjs — deterministic issue labeler for `issue-label.yml`.
//
// PRs are labeled by `pr-label.yml` / `pr-type-label.mjs` from the
// Conventional-Commit prefix in the PR title. ISSUES had no equivalent, so
// 62 of 100 open issues carried zero labels. This module is the issue-side
// counterpart, and it deliberately REUSES `pr-type-label.mjs`'s
// `labelsForTitle()` rather than restating the type table: a second,
// drifting copy of that mapping is the failure mode this file exists to
// avoid.
//
// Two independent inference passes run over every issue:
//
//   1. AREA — from the file paths cited in the issue body. Cerberus issues
//      cite exact `file:line` (`internal/promql/lower.go:364`), so the
//      dominant cited subtree names the area. Longest prefix wins, and the
//      title's own `<head>:` prefix (`promql:`, `chsql:`, `loki:` …) is a
//      first-class second source. See PATH_PREFIX_TO_AREA / TITLE_PREFIX_TO_AREA.
//
//   2. TYPE — the Conventional-Commit prefix when the title carries one
//      (`perf:`, `ci:`, `docs:`, `test:` …), delegated to
//      `labelsForTitle()`. Otherwise a scored scan of curated phrase
//      signals over the title first, then the body: wrong-answer /
//      divergence / silent empty -> bug, missing coverage / hollow test /
//      mutation survivor -> test, unbounded resource / scan cost / fan-out
//      -> performance, duplication / half-finished mechanical change ->
//      refactor, stale or contradictory prose -> documentation. See
//      TYPE_SIGNALS.
//
// Both passes are PURE, DETERMINISTIC functions of (title, body) — no LLM,
// no network guess, no heuristic that depends on wall-clock or ordering.
// The same issue always yields the same labels.
//
// The apply step is ADDITIVE and IDEMPOTENT: it only ever POSTs labels the
// issue is missing, never removes or replaces one a human set, and caps the
// per-issue totals (MAX_AREA_LABELS area labels, MAX_TYPE_LABELS type
// labels) counting the labels already present. Re-running on an
// already-labeled issue is a no-op.
//
// ANTI-VACUITY. A labeler that silently labels nothing is worse than none,
// so this module asserts positively rather than trusting a green exit:
//   - the mapping tables are non-empty at module load (assertTablesUsable);
//   - every issue processed must have had its body actually fetched (a
//     missing `body` KEY is a fetch failure and fails the run; a genuinely
//     empty body is reported, not silently treated as "no signal");
//   - in backfill mode the run FAILS if it processed zero issues, or if it
//     applied zero labels while unlabeled issues remain;
//   - every issue that the rules cannot classify is reported by number in
//     an ::error:: — never silently skipped.
//
// ENV CONTRACT
//   GITHUB_TOKEN     (required, unless ISSUE_LABEL_DRY_RUN=1 and a fixture
//                    file is supplied) — token with `issues: write`.
//   GITHUB_REPOSITORY(required) — `owner/repo`.
//   ISSUE_LABEL_MODE (required) — `event` | `backfill`.
//                      event    : label the single issue in the webhook
//                                 payload at $GITHUB_EVENT_PATH.
//                      backfill : walk every OPEN issue and apply missing
//                                 labels.
//   GITHUB_EVENT_PATH(required in `event` mode) — webhook payload JSON.
//   ISSUE_LABEL_DRY_RUN (optional) — `1`/`true` computes and reports the
//                    labels but applies nothing. Used for the PR evidence
//                    run and for local development.
//   ISSUE_LABEL_FIXTURE (optional, dry-run only) — path to a JSON array of
//                    `{number, title, body, labels}` to read INSTEAD of the
//                    API. Lets the dry run be reproduced offline.
//   GITHUB_API_URL   (optional) — API base; defaults to https://api.github.com.
//
// Exits 0 on success, 1 on any failure (unclassifiable issue, vacuous run,
// API error, unfetched body).
//
// Dependency-light by design: `node:` builtins + global fetch only.
//
// argv `--check-tables` runs the mapping-table assertion and exits.

import process from 'node:process';
import { readFileSync } from 'node:fs';

import { error, notice, log } from './lib/gh.mjs';
import { labelsForTitle } from './pr-type-label.mjs';

// ---------------------------------------------------------------------------
// Area mapping
// ---------------------------------------------------------------------------

// Repository subtree -> `area/*` label. Longest matching prefix wins, so
// `test/spec/promql` beats `test/spec` and `internal/api/prom` is reachable
// even though `internal/api` is also listed.
//
// Deliberately unmapped (no honest area label exists, and inventing one per
// subtree would make the label set noise): `docs/` — the `documentation`
// TYPE label already carries that, and a docs path is usually cited as
// evidence about some other area's code.
export const PATH_PREFIX_TO_AREA = Object.freeze({
  // Query heads: parse + lowering.
  'internal/promql': 'area/promql',
  'internal/logql': 'area/logql',
  'internal/drain': 'area/logql', // Drain miner backs LogQL `pattern`.
  'internal/traceql': 'area/traceql',

  // Shared pipeline.
  'internal/chplan': 'area/chplan',
  'internal/optimizer': 'area/optimizer',
  'internal/solver': 'area/solver',
  'internal/engine': 'area/engine',
  'internal/chsql': 'area/chsql',
  'internal/chclient': 'area/chclient',
  'internal/schema': 'area/schema',

  // HTTP surface.
  'internal/api': 'area/api',
  'cmd/cerberus': 'area/api',

  // Cross-cutting runtime.
  'internal/telemetry': 'area/observability',
  'internal/config': 'area/config',

  // Fixtures inherit the head they exercise.
  'test/spec/promql': 'area/promql',
  'test/spec/logql': 'area/logql',
  'test/spec/traceql': 'area/traceql',
  'test/spec': 'area/ci',

  // Differential harnesses inherit the language they diff.
  'compatibility/prometheus': 'area/promql',
  'compatibility/loki': 'area/logql',
  'compatibility/tempo': 'area/traceql',

  // Substrate lanes.
  'test/e2e': 'area/e2e',
  'test/perf': 'area/ci',
  'test/property': 'area/ci',
  'test/regression': 'area/ci',
  '.github/workflows': 'area/ci',
  '.github/scripts': 'area/ci',
  '.github/actions': 'area/ci',
  charts: 'area/ci',
});

// The `<prefix>:` token cerberus issue titles open with -> `area/*`. This is
// the AUTHOR's own classification, so it is treated as evidence at least as
// strong as a path citation.
//
// `prom` / `loki` / `tempo` name the HTTP heads; `promql` / `logql` /
// `traceql` name the query languages. The repo uses both, and the
// distinction is real — `prom: matrixFromCursor buffers every row` is an
// `internal/api/prom` issue, `promql: subquery inner …` is a lowering issue.
export const TITLE_PREFIX_TO_AREA = Object.freeze({
  promql: 'area/promql',
  logql: 'area/logql',
  traceql: 'area/traceql',
  prom: 'area/api',
  prometheus: 'area/api',
  loki: 'area/logql',
  tempo: 'area/traceql',
  api: 'area/api',
  chsql: 'area/chsql',
  chplan: 'area/chplan',
  chclient: 'area/chclient',
  optimizer: 'area/optimizer',
  solver: 'area/solver',
  engine: 'area/engine',
  schema: 'area/schema',
  parser: 'area/parser',
  config: 'area/config',
  telemetry: 'area/observability',
  observability: 'area/observability',
  ci: 'area/ci',
  e2e: 'area/e2e',
  spec: 'area/ci',
  compat: 'area/ci',
  compatibility: 'area/ci',
  mutation: 'area/ci',
  coverage: 'area/ci',
  cmd: 'area/api',
});

// A head/language name appearing anywhere in the title, used when the title
// carries no `<prefix>:` token at all ("TraceQL spanset aggregates report …",
// "No e2e ever drives the Tempo gRPC streaming path"). `prometheus` maps to
// the PromQL head rather than the HTTP surface because cerberus titles say
// "reference Prometheus" when they mean PromQL semantics.
export const HEAD_KEYWORD_TO_AREA = Object.freeze({
  promql: 'area/promql',
  prometheus: 'area/promql',
  logql: 'area/logql',
  loki: 'area/logql',
  traceql: 'area/traceql',
  tempo: 'area/traceql',
});

// A path citation is any repo-rooted path in the body. The leading
// alternation pins the roots that exist in this repo so prose like
// "internal state/machine" cannot masquerade as a path.
const PATH_CITATION =
  /(?:^|[\s`(['"<|>,])((?:internal|cmd|test|docs|compatibility|charts|deploy|examples|tools|\.github)\/[A-Za-z0-9_.-]+(?:\/[A-Za-z0-9_.-]+)*)/g;

// The title's leading `<prefix>:` token. Cerberus issue titles are
// consistently `promql: …` / `chsql/tempo: …` / `e2e seed: …`, so the
// prefix may itself be a compound — every `/`- or space-separated token in
// it is looked up.
const TITLE_PREFIX = /^([A-Za-z0-9_./ -]{1,40}?):\s/;

// At most this many `area/*` labels on one issue, counting any a human
// already applied. Two lets a genuinely cross-cutting issue name both ends
// (`ci` + `e2e`, `promql` + `chsql`) without turning the label set into a
// tag cloud.
export const MAX_AREA_LABELS = 2;

// A SECONDARY area (anything past the highest-ranked one) must be cited by
// at least this many DISTINCT paths. One passing mention of another
// package is not evidence that the issue lives there; two files in the same
// subtree is.
export const AREA_SECONDARY_MIN_PATHS = 2;

// A citation of PRODUCTION code counts this much when ranking areas; a test,
// fixture, harness, or workflow path counts 1. An issue's area is a property
// of where the code under discussion LIVES, and a bug in one production
// package is routinely evidenced by several test/CI files that merely
// observe it — without this weight the harness out-votes the subject
// (`internal/solver` losing to `.github/workflows` + `test/spec`).
export const PRODUCTION_PATH_WEIGHT = 3;

const PRODUCTION_ROOTS = Object.freeze(['internal/', 'cmd/']);

function pathWeight(path) {
  return PRODUCTION_ROOTS.some((r) => path.startsWith(r)) ? PRODUCTION_PATH_WEIGHT : 1;
}

// areaForPath resolves one repo-rooted path to its `area/*` label by
// longest matching prefix, or '' when no prefix matches.
export function areaForPath(path) {
  const clean = String(path ?? '')
    .replace(/^\.\//, '')
    .replace(/:\d+.*$/, ''); // strip a trailing `:364` / `:680-682` cite
  let best = '';
  let bestLen = 0;
  for (const [prefix, area] of Object.entries(PATH_PREFIX_TO_AREA)) {
    if (clean !== prefix && !clean.startsWith(prefix + '/')) continue;
    if (prefix.length > bestLen) {
      best = area;
      bestLen = prefix.length;
    }
  }
  return best;
}

// citedPaths returns the DISTINCT repo-rooted paths a body mentions.
export function citedPaths(body) {
  const out = new Set();
  const text = String(body ?? '');
  PATH_CITATION.lastIndex = 0;
  let m;
  while ((m = PATH_CITATION.exec(text)) !== null) out.add(m[1]);
  return [...out];
}

// rankPathAreas returns [{ area, paths, weight }] sorted by weighted score
// descending, then distinct-path count, then area name ascending. The name
// tiebreak keeps the result independent of the order paths happen to appear
// in the body. `paths` (the UNweighted distinct count) is what the
// AREA_SECONDARY_MIN_PATHS bar is measured against.
export function rankPathAreas(body) {
  const byArea = new Map();
  for (const p of citedPaths(body)) {
    const area = areaForPath(p);
    if (!area) continue;
    const acc = byArea.get(area) ?? { paths: 0, weight: 0 };
    acc.paths += 1;
    acc.weight += pathWeight(p);
    byArea.set(area, acc);
  }
  return [...byArea.entries()]
    .map(([area, acc]) => ({ area, ...acc }))
    .sort((a, b) => b.weight - a.weight || b.paths - a.paths || a.area.localeCompare(b.area));
}

// areaForTitlePrefix maps a title's leading `<prefix>:` token to an area,
// or '' when the title has no prefix or the prefix names no area (`perf:`
// is a TYPE, not an area). A compound prefix resolves on its first
// area-bearing token, so `chsql/tempo:` -> area/chsql.
export function areaForTitlePrefix(title) {
  const m = String(title ?? '').match(TITLE_PREFIX);
  if (!m) return '';
  for (const token of m[1].split(/[/\s-]+/)) {
    const area = TITLE_PREFIX_TO_AREA[token.toLowerCase()];
    if (area) return area;
  }
  return '';
}

// areaForTitle resolves the title's own claim about the area: the explicit
// `<prefix>:` token first, then — only when there is none — a head/language
// name mentioned anywhere in the title.
export function areaForTitle(title) {
  const prefixArea = areaForTitlePrefix(title);
  if (prefixArea) return prefixArea;
  for (const [keyword, area] of Object.entries(HEAD_KEYWORD_TO_AREA)) {
    if (new RegExp(`\\b${keyword}\\b`, 'i').test(String(title ?? ''))) return area;
  }
  return '';
}

// inferAreas returns the ordered `area/*` labels an issue should carry.
// The title's own claim (the author's classification) ranks first when
// present; path-derived areas fill the remaining slots in rank order, with
// every SECONDARY area required to clear AREA_SECONDARY_MIN_PATHS.
export function inferAreas(title, body) {
  const out = [];
  const titleArea = areaForTitle(title);
  if (titleArea) out.push(titleArea);

  const ranked = rankPathAreas(body);
  for (const { area, paths } of ranked) {
    if (out.length >= MAX_AREA_LABELS) break;
    if (out.includes(area)) continue;
    // The first area overall stands on a single citation; every one after
    // it must clear the secondary bar.
    if (out.length > 0 && paths < AREA_SECONDARY_MIN_PATHS) continue;
    out.push(area);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Type mapping
// ---------------------------------------------------------------------------

// Curated phrase signals per TYPE label. Phrases, not bare words: cerberus
// issue bodies are long and technical, so a loose word list ("error",
// "slow") would fire on every issue and the scoring would be noise. Every
// entry below is a phrase this repo's issues actually use to state the
// class of defect.
export const TYPE_SIGNALS = Object.freeze({
  // Wrong answer / divergence / silent empty.
  bug: Object.freeze([
    /\bwrong[- ]answer\b/i,
    /\breturns? (?:the )?wrong\b/i,
    /\bwrong(?:ly)? (?:rejected?|accepted?|reported?|bound|labell?ed|classified)\b/i,
    /\bincorrect(?:ly)?\b/i,
    /\bdiverge(?:s|nce|nt)?\b/i,
    /\bmismatch(?:es|ed)?\b/i,
    /\b(?:means|reports?) different things\b/i,
    /\bignores?\b/i,
    /\bsilent(?:ly)?\b/i,
    /\binvisible to\b/i,
    /\bswallows?\b/i,
    /\b(?:always|permanently) empty\b/i,
    /\bwhere reference \w+ is empty\b/i,
    /\breference (?:prometheus|loki|tempo) (?:accepts|rejects|drops|returns|is)\b/i,
    /\bthe opposite of\b/i,
    /\bunsatisfiable\b/i,
    /\boff[- ]by[- ]one\b/i,
    /\bpanics?\b/i,
    /\bcode 2\d\d\b/,
    /\b(?:400|401|403|404|409|422|429|500|502|503)s?\b/,
    /\bnever fires?\b/i,
    /\binvert(?:s|ed|ing)\b/i,
    /\bstarv(?:es|ing|ation)\b/i,
    /\bregression\b/i,
    /\bis false\b/i,
    /\bdefends the wrong\b/i,
    /\bdrift(?:ed|s)? (?:out of|apart)\b/i,
    /\bunder-?scans?\b/i,
    /\bdoes(?:n't| not)\b/i,
    /\b(?:is|are|was|were) not\b/i,
    /,\s*not\s+\w/,
    /:\s*""/, // an emitted empty value, e.g. `level:""` on every cluster
  ]),

  // Missing coverage / hollow test / mutation survivor.
  test: Object.freeze([
    /\bcoverage gap\b/i,
    /\b(?:zero|no|missing)\b[^.\n]{0,40}\bcoverage\b/i,
    /\bhas no\b[^.\n]{0,40}\b(?:test|tests|fixture|lane|assertion)\b/i,
    /\bcannot fail\b/i,
    /\binert\b/i,
    /\bhollow\b/i,
    /\buntest(?:ed|able)\b/i,
    /\bunasserted\b/i,
    /\bnever (?:asserted|exercised|tested|covered|widened)\b/i,
    /\bLIVED\b/,
    /\bmutants?\b/i,
    /\bmutation survivor\b/i,
    /\bis only ever tested\b/i,
    /\bno (?:e2e|differential lane|test ever)\b/i,
    /\bcoverage signal\b/i,
  ]),

  // Unbounded resource / scan cost / fan-out.
  performance: Object.freeze([
    /\bunbounded\b/i,
    /\bunguarded\b/i,
    /\bno\b[^.\n]{0,30}\bbound\b/i,
    /\bscan cost\b/i,
    /\bfan-?out\b/i,
    /\bover-?scans?\b/i,
    /\bmemory cap\b/i,
    /\bOOM\b/,
    /\bbuffers every row\b/i,
    /\bemits? the\b[^.\n]{0,60}\b(?:twice|three times)\b/i,
    /\brenders? the\b[^.\n]{0,60}\b(?:twice|three times)\b/i,
    /\bdouble[- ]render\b/i,
    /\bdensity bound\b/i,
    /\bcardinality ratchet\b/i,
    /\bwidens the bound\b/i,
  ]),

  // Duplication / dead code / half-finished mechanical change.
  refactor: Object.freeze([
    /\bduplicat(?:ed|ion|es)\b/i,
    /\bcopy[- ]pasted?\b/i,
    /\bcopied to\b/i,
    /\bredundant\b/i,
    /\brenames?\b|\brenamed\b/i,
    /\bstopped half-?way\b/i,
    /\bplaceholder\b/i,
    /\bmeaningless\b/i,
    /\bhand-(?:rolled|writes|written)\b/i,
    /\bhard-?coded\b/i,
    /\bnot configurable\b/i,
  ]),

  // Stale, contradictory, or absent prose.
  documentation: Object.freeze([
    /\bdocstring\b/i,
    /\bdoc comment\b/i,
    // Prose that states the opposite of the code it describes: the defect is
    // in the prose, so this must outweigh the wrong-answer reading of
    // "asserts the opposite of".
    /\b(?:docstring|doc comment|comment|prose|message|caveat)\b[^.\n]{0,40}\b(?:asserts?|claims?|says?|states?|names?)\b/i,
    /\bcontradicts?\b/i,
    /\bstale (?:comment|doc|prose|caveat)\b/i,
    /\bpresent tense\b/i,
    /\bno longer exists\b/i,
    /\bexists only in a merged PR body\b/i,
    /\bdocs\/[a-z0-9-]+\.md\b/i,
    /\bREADME\b/,
    /\bmisleading prose\b/i,
  ]),
});

// Deterministic tiebreak when two types score equally. A wrong answer is
// the most actionable classification, so it wins ties; `documentation` is
// last because almost every issue quotes some prose.
export const TYPE_TIEBREAK_ORDER = Object.freeze(['bug', 'performance', 'test', 'refactor', 'documentation']);

// scoreSignals returns { type: matches } for ONE text. Each distinct pattern
// contributes at most once, so a text that repeats a single phrase twenty
// times cannot outvote one that states four different symptoms.
export function scoreSignals(text) {
  const s = String(text ?? '');
  const scores = {};
  for (const [type, patterns] of Object.entries(TYPE_SIGNALS)) {
    scores[type] = patterns.reduce((n, re) => n + (re.test(s) ? 1 : 0), 0);
  }
  return scores;
}

function bestType(scores) {
  let best = '';
  let bestScore = 0;
  for (const type of TYPE_TIEBREAK_ORDER) {
    const s = scores[type] ?? 0;
    if (s > bestScore) {
      best = type;
      bestScore = s;
    }
  }
  return best;
}

// inferType returns the single TYPE label an issue should carry, or '' when
// nothing matched.
//
// Precedence, strongest evidence first:
//   1. a Conventional-Commit title prefix (`perf:`, `ci:`, `docs:`, `test:`)
//      — authoritative, and delegated to `pr-type-label.mjs` so the type
//      table has exactly one definition in the repo;
//   2. the TITLE's signal scores — the author's own one-line statement of
//      the defect class;
//   3. the BODY's signal scores — consulted only when the title is silent,
//      because a long technical body inevitably brushes several signals in
//      passing and would otherwise drown out a precise title.
export function inferType(title, body) {
  const fromCC = labelsForTitle(title);
  if (fromCC.length > 0) return fromCC[0];

  const fromTitle = bestType(scoreSignals(title));
  if (fromTitle) return fromTitle;
  return bestType(scoreSignals(body));
}

// ---------------------------------------------------------------------------
// Per-issue decision
// ---------------------------------------------------------------------------

const AREA_PREFIX = 'area/';

// At most one TYPE label is applied per issue. Multiple type labels make
// the "what kind of work is this" question unanswerable.
export const MAX_TYPE_LABELS = 1;

// Type labels this module may apply. Anything outside the set is a mapping
// bug, not a label to invent on the fly.
export const APPLICABLE_TYPE_LABELS = Object.freeze([
  'bug',
  'test',
  'performance',
  'documentation',
  'enhancement',
  'refactor',
  'ci',
  'chore',
  'build',
  'security',
  'revert',
  'dependencies',
  'release',
]);

// labelsForIssue computes the FULL decision for one issue:
//   { areas, type, missing, skipped, unclassified }
// `missing` is the additive delta to POST; `skipped` explains any computed
// label that was withheld because the caps were already met by labels a
// human set. `unclassified` is true when neither pass produced anything.
export function labelsForIssue({ title, body, labels }) {
  const have = new Set((labels ?? []).map((l) => (typeof l === 'string' ? l : l.name)));
  const haveAreas = [...have].filter((l) => l.startsWith(AREA_PREFIX));
  const haveTypes = [...have].filter((l) => APPLICABLE_TYPE_LABELS.includes(l));

  const areas = inferAreas(title, body);
  const type = inferType(title, body);

  const missing = [];
  const skipped = [];

  let areaBudget = MAX_AREA_LABELS - haveAreas.length;
  for (const area of areas) {
    if (have.has(area)) continue;
    if (areaBudget <= 0) {
      skipped.push(`${area} (area cap ${MAX_AREA_LABELS} already met by: ${haveAreas.join(', ')})`);
      continue;
    }
    missing.push(area);
    areaBudget--;
  }

  if (type && !have.has(type)) {
    if (haveTypes.length >= MAX_TYPE_LABELS) {
      skipped.push(`${type} (type cap ${MAX_TYPE_LABELS} already met by: ${haveTypes.join(', ')})`);
    } else {
      missing.push(type);
    }
  }

  return {
    areas,
    type,
    missing,
    skipped,
    unclassified: areas.length === 0 && type === '',
  };
}

// ---------------------------------------------------------------------------
// Anti-vacuity guards
// ---------------------------------------------------------------------------

// assertTablesUsable fails the run if a mapping table has been emptied —
// the shape in which this labeler would degrade to a green no-op.
export function assertTablesUsable() {
  const problems = [];
  if (Object.keys(PATH_PREFIX_TO_AREA).length === 0) problems.push('PATH_PREFIX_TO_AREA is empty');
  if (Object.keys(TITLE_PREFIX_TO_AREA).length === 0) problems.push('TITLE_PREFIX_TO_AREA is empty');
  for (const [type, patterns] of Object.entries(TYPE_SIGNALS)) {
    if (patterns.length === 0) problems.push(`TYPE_SIGNALS.${type} is empty`);
  }
  if (Object.keys(TYPE_SIGNALS).length === 0) problems.push('TYPE_SIGNALS is empty');
  return problems;
}

// assertBodyFetched distinguishes "the API never gave us a body" (a fetch
// failure that would make every issue look signal-free) from "the author
// wrote no body" (real, and reported). A missing KEY is the former.
export function assertBodyFetched(issue) {
  if (!Object.prototype.hasOwnProperty.call(issue, 'body')) {
    return `issue #${issue.number}: no \`body\` field in the payload — the body was never fetched`;
  }
  return '';
}

// ---------------------------------------------------------------------------
// GitHub REST (node: builtins + global fetch only)
// ---------------------------------------------------------------------------

const DEFAULT_API_URL = 'https://api.github.com';
const PER_PAGE = 100;

function apiHeaders(token) {
  return {
    accept: 'application/vnd.github+json',
    authorization: `Bearer ${token}`,
    'x-github-api-version': '2022-11-28',
    'user-agent': 'cerberus-issue-label',
  };
}

async function ghJSON(url, token, init = {}) {
  const res = await fetch(url, { ...init, headers: { ...apiHeaders(token), ...(init.headers ?? {}) } });
  if (!res.ok) {
    throw new Error(`${init.method ?? 'GET'} ${url} -> ${res.status} ${res.statusText}: ${await res.text()}`);
  }
  return res.status === 204 ? null : res.json();
}

// listOpenIssues paginates /issues?state=open, dropping the pull requests
// GitHub folds into that endpoint.
async function listOpenIssues(api, repo, token) {
  const out = [];
  for (let page = 1; ; page++) {
    const url = `${api}/repos/${repo}/issues?state=open&per_page=${PER_PAGE}&page=${page}`;
    const batch = await ghJSON(url, token);
    if (!Array.isArray(batch)) throw new Error(`unexpected non-array response from ${url}`);
    for (const it of batch) {
      if (it.pull_request) continue;
      out.push(it);
    }
    if (batch.length < PER_PAGE) break;
  }
  return out;
}

async function addLabels(api, repo, token, number, labels) {
  await ghJSON(`${api}/repos/${repo}/issues/${number}/labels`, token, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ labels }),
  });
}

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

function truthy(v) {
  return v === '1' || String(v).toLowerCase() === 'true';
}

async function main() {
  const tableProblems = assertTablesUsable();
  if (tableProblems.length > 0) {
    error(`issue-label mapping tables are unusable:\n  - ${tableProblems.join('\n  - ')}`, {
      title: 'issue-label: vacuous mapping',
    });
    process.exit(1);
  }

  const mode = process.env.ISSUE_LABEL_MODE ?? '';
  if (mode !== 'event' && mode !== 'backfill') {
    error(`ISSUE_LABEL_MODE must be 'event' or 'backfill' (got ${JSON.stringify(mode)})`);
    process.exit(1);
  }
  const dryRun = truthy(process.env.ISSUE_LABEL_DRY_RUN);
  const api = process.env.GITHUB_API_URL || DEFAULT_API_URL;
  const repo = process.env.GITHUB_REPOSITORY ?? '';
  const token = process.env.GITHUB_TOKEN ?? '';
  const fixture = process.env.ISSUE_LABEL_FIXTURE ?? '';

  if (!fixture && !repo) {
    error('GITHUB_REPOSITORY is required (owner/repo)');
    process.exit(1);
  }
  if (!fixture && !token) {
    error('GITHUB_TOKEN is required');
    process.exit(1);
  }

  // Gather the issues this run is responsible for.
  let issues;
  if (fixture) {
    if (!dryRun) {
      error('ISSUE_LABEL_FIXTURE is a dry-run-only input; set ISSUE_LABEL_DRY_RUN=1');
      process.exit(1);
    }
    issues = JSON.parse(readFileSync(fixture, 'utf8'));
  } else if (mode === 'event') {
    const eventPath = process.env.GITHUB_EVENT_PATH ?? '';
    if (!eventPath) {
      error('GITHUB_EVENT_PATH is required in event mode');
      process.exit(1);
    }
    const payload = JSON.parse(readFileSync(eventPath, 'utf8'));
    if (!payload.issue) {
      error('event payload carries no `issue` object');
      process.exit(1);
    }
    issues = [payload.issue];
  } else {
    issues = await listOpenIssues(api, repo, token);
  }

  if (!Array.isArray(issues)) {
    error('the issue set is not an array — nothing to process');
    process.exit(1);
  }

  // ANTI-VACUITY: a run that saw no issues at all is a broken run, not a
  // clean one. In event mode the payload always carries exactly one.
  if (issues.length === 0) {
    error(`issue-label (${mode}) processed ZERO issues — the fetch returned nothing`, {
      title: 'issue-label: vacuous run',
    });
    process.exit(1);
  }

  const unfetched = [];
  const unclassified = [];
  const emptyBodies = [];
  let applied = 0;
  let touched = 0;
  let alreadyComplete = 0;
  let unlabeledRemaining = 0;

  for (const issue of issues) {
    const problem = assertBodyFetched(issue);
    if (problem) {
      unfetched.push(problem);
      continue;
    }
    const number = issue.number;
    const title = issue.title ?? '';
    const body = issue.body ?? '';
    const have = (issue.labels ?? []).map((l) => (typeof l === 'string' ? l : l.name));
    if (body.trim() === '') emptyBodies.push(number);

    const decision = labelsForIssue({ title, body, labels: issue.labels });

    if (decision.unclassified) {
      unclassified.push(`#${number} — ${title}`);
      if (have.length === 0) unlabeledRemaining++;
      continue;
    }

    const detail =
      `areas=[${decision.areas.join(', ') || '-'}] type=${decision.type || '-'}` +
      (decision.skipped.length > 0 ? ` withheld=[${decision.skipped.join('; ')}]` : '');

    if (decision.missing.length === 0) {
      alreadyComplete++;
      notice(`#${number}: already carries its computed labels — ${detail}`);
      continue;
    }

    if (dryRun) {
      notice(`#${number} DRY-RUN would add [${decision.missing.join(', ')}] — ${detail} — ${title}`);
    } else {
      await addLabels(api, repo, token, number, decision.missing);
      notice(`#${number} labeled [${decision.missing.join(', ')}] — ${detail} — ${title}`);
    }
    applied += decision.missing.length;
    touched++;
  }

  const failures = [];

  if (unfetched.length > 0) {
    failures.push(`${unfetched.length} issue(s) had no body fetched:\n  - ${unfetched.join('\n  - ')}`);
  }

  // ANTI-VACUITY: an unclassifiable issue is REPORTED, never silently
  // skipped — a growing residue means the mapping is too narrow.
  if (unclassified.length > 0) {
    failures.push(
      `${unclassified.length} issue(s) matched no area and no type rule — widen the mapping ` +
        `(PATH_PREFIX_TO_AREA / TITLE_PREFIX_TO_AREA / TYPE_SIGNALS):\n  - ${unclassified.join('\n  - ')}`,
    );
  }

  // ANTI-VACUITY: in backfill mode, applying nothing while unlabeled issues
  // remain is exactly the hollow-green shape this gate exists to prevent.
  if (mode === 'backfill' && !dryRun && applied === 0 && unlabeledRemaining > 0) {
    failures.push(
      `backfill applied ZERO labels while ${unlabeledRemaining} issue(s) remain unlabeled — ` +
        `the labeler is inert`,
    );
  }

  if (emptyBodies.length > 0) {
    notice(`${emptyBodies.length} issue(s) have an empty body (title-only inference): ${emptyBodies.join(', ')}`);
  }

  log(
    `issue-label (${mode}${dryRun ? ', dry-run' : ''}): ${issues.length} issue(s) scanned, ` +
      `${touched} issue(s) ${dryRun ? 'would be' : ''} labeled, ${applied} label(s) ` +
      `${dryRun ? 'pending' : 'applied'}, ${alreadyComplete} already complete, ` +
      `${unclassified.length} unclassified.`,
  );

  if (failures.length > 0) {
    for (const f of failures) error(f, { title: 'issue-label' });
    process.exit(1);
  }

  notice(
    `issue-label (${mode}${dryRun ? ', dry-run' : ''}) complete: ${touched}/${issues.length} issue(s) ` +
      `${dryRun ? 'would receive' : 'received'} ${applied} label(s).`,
  );
}

// `--check-tables` is this module's self-test. It is NOT spelled
// `--self-test` like its sibling: importing `pr-type-label.mjs` runs that
// module's top level, whose own `--self-test` branch would consume the flag
// and exit before any of this file's code ran.
if (process.argv.includes('--check-tables')) {
  const problems = assertTablesUsable();
  if (problems.length > 0) {
    error(`issue-label --check-tables: ${problems.join('; ')}`);
    process.exit(1);
  }
  notice('issue-label --check-tables: mapping tables usable');
  process.exit(0);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main().catch((e) => {
    error(String(e?.stack ?? e), { title: 'issue-label' });
    process.exit(1);
  });
}
