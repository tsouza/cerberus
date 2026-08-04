// generated-baseline-structural-guard.mjs — cheap, fast structural sanity
// check over a subset of the `-merge`-marked generated baselines listed in
// .gitattributes.
//
// # Why this exists
//
// #1567 marked ~28 generated baselines `-merge` in .gitattributes so a LOCAL
// `git merge` refuses to silently blend two branches that each insert a
// record at a nearby offset (the #1422 corruption class: the record
// boundaries shift, a name ends up paired with the next record's values, and
// the result still parses as valid JSON with a plausible length while every
// entry in the blended region is wrong — see .gitattributes' own header
// comment for the full shape).
//
// #1568 asked whether GitHub's SERVER-SIDE merge (the "Update branch"
// button, `gh pr merge --squash`, the mergeability precomputation that
// decides whether a PR shows conflict-free) honours that attribute at all —
// `-merge` is a built-in driver, but nothing had verified GitHub's merge
// service actually reads .gitattributes rather than always falling back to a
// plain line-based text merge.
//
// It does not. Verified empirically 2026-08-04: two throwaway branches in
// this repo, each inserting one new record at a different, non-overlapping
// line offset into test/perf/solver-decision-baseline.json off the same
// base commit, were reported `"mergeable":"MERGEABLE"` by the GitHub API
// (`gh pr view --json mergeable,mergeStateStatus`) for a throwaway PR
// between them — while a LOCAL `git merge` of the identical branch pair
// refused with "Cannot merge binary files" / `CONFLICT (content)`, exactly
// as .gitattributes documents. The throwaway PR was closed unmerged and
// both branches deleted immediately after; see issue #1568 for the full
// experiment writeup.
//
// # What actually protects these files today
//
// Auditing every `-merge` path found that nearly all of them already carry a
// REQUIRED, PR-blocking, content-EXACT validator that regenerates the
// artefact from source and diffs it (byte-for-byte, or full-struct
// equality, keyed per record) against the committed file:
//   - TestCardinalityRatchet / TestSolverDecisionRatchet / TestScaleWallPin
//     (test/perf, the `perf-guards` required job),
//   - TestCatalogueIsRegenerable + the surface-parity inventory tests
//     (test/rejection-parity, test/surface-parity — plain `go test`, the
//     `check` required job),
//   - compat-ratchet.mjs (the `compatibility/*` required jobs),
//   - coverage-summary.mjs (the `coverage` required job),
//   - the Tier-0 migration goldens, via
//     `go test -tags=migration ./test/e2e/migration/tiers/tier0-offline/...`
//     (the `lint` required job — see ci.yml's "Run the Tier-0 migration
//     scenarios (explain-golden drift)" step).
// Those are the STRONG defence #1568 asked for ("re-runs the generator on
// the merge commit and diffs it against the committed file") — they were
// already there for most of the list.
//
// The residual gap is procedural, not a missing validator: with branch
// protection's "require branches to be up to date before merging" OFF
// (`strict: false` as of this writing — confirmed via
// `gh api repos/tsouza/cerberus/branches/main/protection`), a stale PR's
// squash-merge computes its diff against a `main` that has since moved
// WITHOUT ever re-running the checks above against the resulting content —
// so a corrupted blend of one of these files can, in principle, land on
// `main` with no CI run ever having seen the merged bytes. Turning
// `strict: true` on closes that window for every file the list above
// covers (it forces the "Update branch" step, which re-runs every required
// check — including all of the above — against the exact content that will
// land) and is the recommended follow-up. It is a branch-protection admin
// setting, not a code change, so this script does not attempt it — see the
// PR that added this file for the explicit recommendation to the
// maintainer.
//
// One family was found to have WEAKER coverage on an ordinary PR:
// test/e2e/playwright/crawl/grafana-surface-inventory.{compose,k3d}.json are
// exercised at full depth only by the release-gated `dashboard` (k3d) lane,
// which runs on the merge commit rather than blocking the PR; `compose-smoke`
// only walks the `lean: true` subset of the `.compose.json` file's rows when
// its own scope triggers. That gap is tracked separately (see the PR body /
// the follow-up issue it filed) rather than guessed at here — the file's
// row order was checked by hand and is NOT a simple lexical sort (it groups
// by base path in crawl-discovery order), so folding it into this guard's
// generic sorted/unique check would have been a false invariant.
//
// # What THIS script adds
//
// A fast, always-on, dependency-free structural pre-filter over the subset
// of `-merge` files whose invariants (unique key, sorted order, and — where
// the shape is genuinely uniform — one consistent field set) were verified
// by hand against the committed content (see TARGETS below). It catches the
// SHAPE-LEVEL symptom of a #1422-class blend (a duplicate or out-of-order
// key) in milliseconds, before the heavier per-file ratchets run, as a
// second, independent signal — deliberately not a replacement for the
// content-exact ratchets above, since a structural check cannot tell a
// value that is merely out of place from one that is subtly wrong.
//
// Usage:
//   node .github/scripts/generated-baseline-structural-guard.mjs
//   node .github/scripts/generated-baseline-structural-guard.mjs --self-test
//
// Exit codes:
//   0  every configured target is well-formed (unique, sorted, and — where
//      configured — one consistent field set).
//   1  a structural violation, or an unreadable/unparseable file.

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import process from 'node:process';

import { error, log, notice } from './lib/gh.mjs';

// get() descends a path of object keys. An empty path returns the root
// value unchanged, which is what the two top-level-array targets need.
function get(obj, path) {
  return path.reduce((o, k) => (o == null ? o : o[k]), obj);
}

// checkArray is the core structural assertion over an array of items —
// either plain strings (a roster, e.g. a compat case list) when `key` is
// null, or objects keyed by `key`. Returns a list of problem strings; an
// empty list means clean.
export function checkArray({ label, items, key, uniformShape }) {
  const problems = [];
  const values = key == null ? items : items.map((it) => it?.[key]);

  values.forEach((v, i) => {
    if (v === undefined) {
      problems.push(`${label}[${i}]: missing key ${JSON.stringify(key)}`);
    }
  });

  const firstSeenAt = new Map();
  values.forEach((v, i) => {
    if (firstSeenAt.has(v)) {
      problems.push(
        `${label}: duplicate ${key ?? 'value'} ${JSON.stringify(v)} at index ${firstSeenAt.get(v)} and ${i}`,
      );
    } else {
      firstSeenAt.set(v, i);
    }
  });

  for (let i = 1; i < values.length; i++) {
    if (values[i] < values[i - 1]) {
      problems.push(
        `${label}: out of sorted order at index ${i} — ${JSON.stringify(values[i - 1])} then ` +
          `${JSON.stringify(values[i])}. A merge that silently interleaved two branches' insertions into this ` +
          'array (the #1422 shape) produces exactly this: a record landing next to the wrong neighbour.',
      );
    }
  }

  if (uniformShape && key != null) {
    const shapes = new Set(items.filter((it) => it && typeof it === 'object').map((it) => JSON.stringify(Object.keys(it).sort())));
    if (shapes.size > 1) {
      problems.push(
        `${label}: ${shapes.size} distinct field sets across records — expected exactly one. A blended merge ` +
          'that spliced one record\'s trailing fields onto another\'s leading fields produces a record whose ' +
          'key set does not match its neighbours.',
      );
    }
  }

  return problems;
}

// checkSortedLines is the text-file variant: non-comment, non-blank lines
// must be unique and lexically sorted.
export function checkSortedLines({ label, text, commentPrefix = '#' }) {
  const lines = text.split('\n').filter((l) => l.trim() !== '' && !l.trimStart().startsWith(commentPrefix));
  return checkArray({ label, items: lines, key: null, uniformShape: false });
}

// checkSortedKeys is the dict variant: an object's OWN keys (in the order
// JSON.parse preserved them, i.e. source order) must be sorted. Values are
// not otherwise inspected here — the file's own regenerate-and-diff test is
// the content-exact authority for what a value should be.
export function checkSortedKeys({ label, obj }) {
  const keys = Object.keys(obj);
  const problems = [];
  for (let i = 1; i < keys.length; i++) {
    if (keys[i] < keys[i - 1]) {
      problems.push(
        `${label}: keys out of sorted order at index ${i} — ${JSON.stringify(keys[i - 1])} then ` +
          `${JSON.stringify(keys[i])}`,
      );
    }
  }
  return problems;
}

// TARGETS — every entry's shape was checked by hand against the committed
// file before being added here (see the file doc above for the ones left
// out and why). `arrayPath` / `dictPath` are lists of keys to descend from
// the parsed JSON root; an empty array means "the root itself".
export const TARGETS = [
  {
    path: 'test/perf/cardinality-baseline.json',
    kind: 'array',
    arrayPath: [],
    key: 'fixture',
    uniformShape: true,
  },
  {
    path: 'test/perf/solver-decision-baseline.json',
    kind: 'array',
    arrayPath: [],
    key: 'query',
    uniformShape: true,
  },
  {
    path: 'test/rejection-parity/catalogue.json',
    kind: 'array',
    arrayPath: ['entries'],
    key: 'site',
    // Entries carry optional fields (e.g. `trigger_query`, `endpoint`) —
    // three distinct field sets are legitimate in the committed file today.
    uniformShape: false,
  },
  {
    path: 'test/e2e/grafana/ql-inventory/promql-feature-inventory.json',
    kind: 'array',
    arrayPath: ['rows'],
    key: 'id',
    // Some rows carry an optional `experimental` flag.
    uniformShape: false,
  },
  {
    path: 'test/e2e/grafana/ql-inventory/logql-feature-inventory.json',
    kind: 'array',
    arrayPath: ['rows'],
    key: 'id',
    uniformShape: true,
  },
  {
    path: 'test/e2e/grafana/ql-inventory/traceql-feature-inventory.json',
    kind: 'array',
    arrayPath: ['rows'],
    key: 'id',
    uniformShape: true,
  },
  {
    path: 'compatibility/parity-baseline.json',
    kind: 'array',
    arrayPath: ['heads', 'prometheus', 'cases'],
    key: null,
    uniformShape: false,
  },
  {
    path: 'compatibility/parity-baseline.json',
    kind: 'array',
    arrayPath: ['heads', 'loki', 'cases'],
    key: null,
    uniformShape: false,
  },
  {
    path: 'compatibility/parity-baseline.json',
    kind: 'array',
    arrayPath: ['heads', 'tempo', 'cases'],
    key: null,
    uniformShape: false,
  },
  {
    path: 'compatibility/loki/upstream-skip-baseline.txt',
    kind: 'lines',
  },
  {
    path: 'test/surface-parity/promql-reference-verdicts.json',
    kind: 'dict-keys',
    dictPath: ['verdicts'],
  },
];

export function runTarget(t, { readFile = (p) => readFileSync(p, 'utf8') } = {}) {
  if (t.kind === 'lines') {
    const text = readFile(t.path);
    return checkSortedLines({ label: t.path, text });
  }
  if (t.kind === 'dict-keys') {
    const doc = JSON.parse(readFile(t.path));
    const obj = get(doc, t.dictPath);
    const label = `${t.path}#${t.dictPath.join('.')}`;
    if (obj == null || typeof obj !== 'object' || Array.isArray(obj)) {
      return [`${label}: expected an object, got ${Array.isArray(obj) ? 'array' : typeof obj}`];
    }
    return checkSortedKeys({ label, obj });
  }
  // 'array'
  const doc = JSON.parse(readFile(t.path));
  const items = get(doc, t.arrayPath);
  const label = `${t.path}#${t.arrayPath.join('.') || '(top)'}`;
  if (!Array.isArray(items)) {
    return [`${label}: expected an array, got ${typeof items}`];
  }
  return checkArray({ label, items, key: t.key, uniformShape: t.uniformShape });
}

function main() {
  const problems = [];
  for (const t of TARGETS) {
    try {
      problems.push(...runTarget(t));
    } catch (e) {
      problems.push(`${t.path}: ${e.message}`);
    }
  }

  if (problems.length > 0) {
    for (const p of problems) error(p, { title: 'generated-baseline-structural-guard' });
    log(
      `\n${problems.length} structural violation(s) across ${TARGETS.length} check(s). This shape is exactly ` +
        'what a merge that silently blended two branches\' insertions into one of these generated baselines ' +
        'produces (see #1568) — regenerate the artefact from source rather than hand-editing it.',
    );
    process.exit(1);
  }

  notice(
    `generated-baseline-structural-guard: ${TARGETS.length} check(s) clean (unique, sorted, and shape-consistent ` +
      'where asserted).',
  );
}

function selfTest() {
  // Clean: uniform, sorted, unique.
  assert.deepEqual(
    checkArray({ label: 't', items: [{ id: 'a' }, { id: 'b' }], key: 'id', uniformShape: true }),
    [],
  );

  // Duplicate key.
  const dup = checkArray({ label: 't', items: [{ id: 'a' }, { id: 'a' }], key: 'id', uniformShape: false });
  assert.equal(dup.length, 1);
  assert.match(dup[0], /duplicate id/);

  // Out of order — the #1422 shape: an insertion landed at the wrong offset.
  const unsorted = checkArray({ label: 't', items: [{ id: 'b' }, { id: 'a' }], key: 'id', uniformShape: false });
  assert.equal(unsorted.length, 1);
  assert.match(unsorted[0], /out of sorted order/);

  // Non-uniform shape, asserted -> flagged.
  const nonUniform = checkArray({
    label: 't',
    items: [{ id: 'a', x: 1 }, { id: 'b' }],
    key: 'id',
    uniformShape: true,
  });
  assert.equal(nonUniform.length, 1);
  assert.match(nonUniform[0], /distinct field sets/);

  // Non-uniform shape, NOT asserted -> tolerated (catalogue.json's optional fields).
  assert.deepEqual(
    checkArray({ label: 't', items: [{ id: 'a', x: 1 }, { id: 'b' }], key: 'id', uniformShape: false }),
    [],
  );

  // Plain-string array (a compat case roster) — key is null, values compare directly.
  assert.deepEqual(checkArray({ label: 't', items: ['a', 'b', 'c'], key: null, uniformShape: false }), []);
  const dupStr = checkArray({ label: 't', items: ['a', 'a'], key: null, uniformShape: false });
  assert.equal(dupStr.length, 1);

  // Sorted lines, comments and blanks ignored.
  assert.deepEqual(checkSortedLines({ label: 't', text: '# comment\n\na\nb\n' }), []);
  const badLines = checkSortedLines({ label: 't', text: 'b\na\n' });
  assert.equal(badLines.length, 1);

  // Sorted dict keys.
  assert.deepEqual(checkSortedKeys({ label: 't', obj: { a: 1, b: 2 } }), []);
  const badKeys = checkSortedKeys({ label: 't', obj: { b: 1, a: 2 } });
  assert.equal(badKeys.length, 1);

  // runTarget dispatches on `kind` correctly, over an injected in-memory
  // file — no real files touched by this half of the self-test.
  const files = {
    'a.json': JSON.stringify([{ id: 'a' }, { id: 'b' }]),
    'b.json': JSON.stringify({ rows: [{ id: 'a' }, { id: 'z' }, { id: 'm' }] }),
    'c.txt': 'b\na\n',
    'd.json': JSON.stringify({ verdicts: { b: 1, a: 2 } }),
  };
  const readFile = (p) => {
    if (!(p in files)) throw new Error(`unexpected read: ${p}`);
    return files[p];
  };
  assert.deepEqual(
    runTarget({ path: 'a.json', kind: 'array', arrayPath: [], key: 'id', uniformShape: true }, { readFile }),
    [],
  );
  assert.equal(
    runTarget({ path: 'b.json', kind: 'array', arrayPath: ['rows'], key: 'id', uniformShape: false }, { readFile })
      .length,
    1,
  );
  assert.equal(runTarget({ path: 'c.txt', kind: 'lines' }, { readFile }).length, 1);
  assert.equal(
    runTarget({ path: 'd.json', kind: 'dict-keys', dictPath: ['verdicts'] }, { readFile }).length,
    1,
  );

  // Every configured TARGET is currently clean against the real committed
  // files in this tree — the guard itself must not be a standing failure.
  for (const t of TARGETS) {
    const problems = runTarget(t);
    assert.deepEqual(problems, [], `TARGETS entry ${t.path} is not currently clean: ${problems.join('; ')}`);
  }

  log('generated-baseline-structural-guard self-test: OK');
}

if (process.argv.includes('--self-test')) {
  selfTest();
} else {
  main();
}
