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
// line offset into one `-merge` path off the same
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
// so a corrupted blend of one of these files can land on `main` with no CI
// run ever having seen the merged bytes.
//
// `strict: true` would close that window for every file the list above covers
// (it forces the "Update branch" step, which re-runs every required check
// against the exact content that will land) at a price that is not worth
// paying: ~19 checks and ~40 minutes per PR, re-forced each time another PR
// lands first, which serialises merges and multiplies CI spend worst exactly
// when the repo is busiest. `.github/workflows/post-merge-drift.yml` closes it
// from the other side instead — on every push to `main` it regenerates the
// shards that merge implied and diffs them against what was committed, which
// is the one place the merged content exists (#1877).
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
import { readdirSync, readFileSync } from 'node:fs';
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

// SHARD_EXT is the suffix of a shard file inside a sharded target's directory.
const SHARD_EXT = '.json';

// runShardedTarget is the directory variant: the target names a directory of
// per-source-file shards (test/rejection-parity/catalogue/) rather than one
// file. Each shard gets the same within-file check as a whole-file target,
// PLUS a cross-shard uniqueness check on the key — sharding moves the #1422
// hazard, it does not remove it, and a key appearing in two shards is the one
// corruption a per-file check cannot see. Sorted order is asserted only WITHIN
// a shard: the shards partition the key space by source file, and their names
// (path separators folded to `__`) do not sort in the same order as the keys
// they hold.
function runShardedTarget(t, { readFile, readDir }) {
  const names = readDir(t.path).filter((n) => n.endsWith(SHARD_EXT)).sort();
  if (names.length === 0) {
    return [`${t.path}: holds no ${SHARD_EXT} shard — a sharded target that reads nothing checks nothing`];
  }

  const problems = [];
  const owner = new Map();
  for (const name of names) {
    const shardPath = `${t.path}/${name}`;
    const items = get(JSON.parse(readFile(shardPath)), t.arrayPath);
    const label = `${shardPath}#${t.arrayPath.join('.') || '(top)'}`;
    if (!Array.isArray(items)) {
      problems.push(`${label}: expected an array, got ${typeof items}`);
      continue;
    }
    problems.push(...checkArray({ label, items, key: t.key, uniformShape: t.uniformShape }));
    for (const item of items) {
      const value = item?.[t.key];
      if (owner.has(value)) {
        problems.push(
          `${t.path}: ${JSON.stringify(t.key)} ${JSON.stringify(value)} appears in both ` +
            `${owner.get(value)} and ${shardPath} — one entry has been copied across shards rather than moved`,
        );
      } else {
        owner.set(value, shardPath);
      }
    }
  }
  return problems;
}

// runRecordShardedTarget is the ONE-RECORD-PER-FILE directory variant: the
// target names a tree (test/perf/cardinality-baseline/) whose every .json file
// holds a single record object rather than an array.
//
// Uniqueness and sort order — the two things checkArray asserts about a flat
// array — do not survive the split, and pretending otherwise would leave a
// guard that asserts nothing. Uniqueness is now enforced by the filesystem: two
// records cannot share a path. Sort order does not exist: a directory has no
// order, and the shards' names ARE the keys, so `git ls-files` already returns
// them sorted with no invariant to check.
//
// What replaces them is an invariant the flat array could not express at all —
// a shard's `key` field must equal the path it is filed under. In an array a
// record's position carries no information, so nothing could contradict its
// key; in a tree the path is a second, independent statement of the same fact,
// and a record filed under someone else's name is exactly what a hand edit or a
// bad cherry-pick produces. The uniform-field-shape check carries over
// unchanged, applied ACROSS the tree: with one record per file, "every record
// has the same field set" is a cross-shard property.
function runRecordShardedTarget(t, { readFile, readDirRecursive }) {
  const rels = readDirRecursive(t.path).filter((n) => n.endsWith(SHARD_EXT)).sort();
  if (rels.length === 0) {
    return [`${t.path}: holds no ${SHARD_EXT} shard — a sharded target that reads nothing checks nothing`];
  }

  const problems = [];
  const shapes = new Map();
  for (const rel of rels) {
    const shardPath = `${t.path}/${rel}`;
    const record = JSON.parse(readFile(shardPath));
    if (record == null || typeof record !== 'object' || Array.isArray(record)) {
      problems.push(`${shardPath}: expected one record object, got ${Array.isArray(record) ? 'array' : typeof record}`);
      continue;
    }

    const want = rel.slice(0, -SHARD_EXT.length);
    const got = record[t.key];
    if (got !== want) {
      problems.push(
        `${shardPath}: ${JSON.stringify(t.key)} is ${JSON.stringify(got)} but the shard is filed as ` +
          `${JSON.stringify(want)}. A shard's path IS its key — a record filed under a name that is not ` +
          'its own is served to the ratchet as the fixture it is named after, not the one it describes.',
      );
    }

    if (t.uniformShape) {
      const shape = JSON.stringify(Object.keys(record).sort());
      if (!shapes.has(shape)) shapes.set(shape, shardPath);
    }
  }

  if (t.uniformShape && shapes.size > 1) {
    problems.push(
      `${t.path}: ${shapes.size} distinct field sets across shards — expected exactly one. Examples: ` +
        `${[...shapes.values()].slice(0, 2).join(', ')}. Every record in this tree is written by one ` +
        'generator from one struct, so a shard with a different field set was not written by it.',
    );
  }

  return problems;
}

// TARGETS — every entry's shape was checked by hand against the committed
// file before being added here (see the file doc above for the ones left
// out and why). `arrayPath` / `dictPath` are lists of keys to descend from
// the parsed JSON root; an empty array means "the root itself".
export const TARGETS = [
  {
    path: 'test/perf/cardinality-baseline',
    kind: 'record-shards',
    key: 'fixture',
    uniformShape: true,
  },
  {
    path: 'test/perf/solver-decision-baseline',
    kind: 'record-shards',
    key: 'query',
    uniformShape: true,
  },
  {
    path: 'test/rejection-parity/catalogue',
    kind: 'array-shards',
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

// readDirRecursiveSync lists every FILE under `dir`, as paths relative to it
// with `/` separators. The record-sharded perf baselines nest one level (the
// query head), so a flat readdir would see only directories.
function readDirRecursiveSync(dir, prefix = '') {
  const out = [];
  for (const de of readdirSync(`${dir}/${prefix}`.replace(/\/$/, ''), { withFileTypes: true })) {
    const rel = prefix === '' ? de.name : `${prefix}/${de.name}`;
    if (de.isDirectory()) {
      out.push(...readDirRecursiveSync(dir, rel));
    } else {
      out.push(rel);
    }
  }

  return out;
}

export function runTarget(
  t,
  { readFile = (p) => readFileSync(p, 'utf8'), readDir = readdirSync, readDirRecursive = readDirRecursiveSync } = {},
) {
  if (t.kind === 'array-shards') {
    return runShardedTarget(t, { readFile, readDir });
  }
  if (t.kind === 'record-shards') {
    return runRecordShardedTarget(t, { readFile, readDirRecursive });
  }
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

  // Non-uniform shape, NOT asserted -> tolerated (the catalogue's optional fields).
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

  // 'array-shards': clean across shards, then the two failure modes a sharded
  // artefact adds — a key duplicated across shards (which no per-file check
  // can see) and a directory that holds nothing at all.
  const shards = {
    'cat/a.go.json': JSON.stringify({ entries: [{ site: 'a.go:f#1' }, { site: 'a.go:g#2' }] }),
    'cat/b.go.json': JSON.stringify({ entries: [{ site: 'b.go:h#3' }] }),
  };
  const shardRead = (p) => {
    if (!(p in shards)) throw new Error(`unexpected read: ${p}`);
    return shards[p];
  };
  const shardTarget = {
    path: 'cat',
    kind: 'array-shards',
    arrayPath: ['entries'],
    key: 'site',
    uniformShape: true,
  };
  assert.deepEqual(
    runTarget(shardTarget, { readFile: shardRead, readDir: () => ['a.go.json', 'b.go.json'] }),
    [],
  );

  const crossShardDup = runTarget(shardTarget, {
    readFile: (p) => (p === 'cat/b.go.json' ? JSON.stringify({ entries: [{ site: 'a.go:f#1' }] }) : shardRead(p)),
    readDir: () => ['a.go.json', 'b.go.json'],
  });
  assert.equal(crossShardDup.length, 1);
  assert.match(crossShardDup[0], /appears in both/);

  const noShards = runTarget(shardTarget, { readFile: shardRead, readDir: () => [] });
  assert.equal(noShards.length, 1);
  assert.match(noShards[0], /holds no \.json shard/);

  // 'record-shards': one record per file, nested a level deep. Clean first,
  // then the three failure modes this shape has — a record filed under a name
  // that is not its own (the invariant a flat array could not state at all), a
  // shard whose field set differs from the rest of the tree, and an empty tree.
  const recordShards = {
    'base/promql/rate.json': JSON.stringify({ fixture: 'promql/rate', scan_rows: 1 }),
    'base/traceql/spans.json': JSON.stringify({ fixture: 'traceql/spans', scan_rows: 2 }),
  };
  const recordRead = (p) => {
    if (!(p in recordShards)) throw new Error(`unexpected read: ${p}`);
    return recordShards[p];
  };
  const recordRels = () => ['promql/rate.json', 'traceql/spans.json'];
  const recordTarget = { path: 'base', kind: 'record-shards', key: 'fixture', uniformShape: true };
  assert.deepEqual(
    runTarget(recordTarget, { readFile: recordRead, readDirRecursive: recordRels }),
    [],
  );

  const misfiled = runTarget(recordTarget, {
    readFile: (p) =>
      p === 'base/traceql/spans.json'
        ? JSON.stringify({ fixture: 'promql/rate', scan_rows: 2 })
        : recordRead(p),
    readDirRecursive: recordRels,
  });
  assert.equal(misfiled.length, 1);
  assert.match(misfiled[0], /but the shard is filed as/);

  const mixedShape = runTarget(recordTarget, {
    readFile: (p) =>
      p === 'base/traceql/spans.json' ? JSON.stringify({ fixture: 'traceql/spans' }) : recordRead(p),
    readDirRecursive: recordRels,
  });
  assert.equal(mixedShape.length, 1);
  assert.match(mixedShape[0], /distinct field sets across shards/);

  const noRecordShards = runTarget(recordTarget, { readFile: recordRead, readDirRecursive: () => [] });
  assert.equal(noRecordShards.length, 1);
  assert.match(noRecordShards[0], /holds no \.json shard/);

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
