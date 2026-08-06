// golden-shards.mjs — the shard vocabulary `just update-golden` takes, and the
// DERIVED staleness relation that decides whether the shards a contributor
// chose actually cover the change they are regenerating for.
//
// # WHY A COVERAGE CHECK EXISTS AT ALL
//
// `update-golden` used to chain all five regeneration stages unconditionally.
// That chaining was not tidiness — it was the fix for #1573 (hit again by #1571
// and #1592): a contributor regenerated one artefact, saw zero remaining churn,
// pushed, and arrived at a red lane because a DIFFERENT artefact had gone
// stale. CI can only DETECT that drift (a golden is a reviewed artifact, so the
// harness refuses to rewrite one under CI), which is precisely what made the
// omission a trap.
//
// Sharding the recipe reopens that trap unless something computes which shards
// the working tree's own diff implies are stale. This module is that
// computation. It never restates the relation as a hand-maintained table of
// fixture names — that is the drift the chaining was covering for, reintroduced
// one level up. Two derivations do the work:
//
//   1. DATA inputs. A per-fixture shard tree (`test/perf/solver-decision-
//      baseline/<name>.json`, `test/perf/cardinality-baseline/<head>/<name>.json`)
//      names, in its own file layout, the fixture each shard was derived FROM.
//      Resolving every shard id back to a `test/spec/**.txtar` and taking the
//      common directory prefix of what resolves yields that artefact's corpus
//      root — `test/spec/promql` for the solver baseline, `test/spec` for the
//      cardinality baseline — with no list of fixture names anywhere. Repoint a
//      baseline at a different corpus and the derived root follows it.
//   2. CODE inputs. `go list -deps -test` over a shard's generator packages is
//      the exact set of packages whose source can move what that shard records.
//      A change to `internal/chsql` reaches every head's SQL and so implies
//      every spec shard; a change to `internal/logql` reaches only its own.
//      The import graph draws that line, not a comment.
//
// Consumed by `.github/scripts/golden-update.mjs` (the runner behind
// `just update-golden`) and pinned by `test/regression/golden_shard_coverage_test.go`.

import { execFileSync } from 'node:child_process';
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import path from 'node:path';

/** The Go module path every first-party package import starts with. */
const MODULE_PATH = 'github.com/tsouza/cerberus';

/** The TXTAR corpus root every per-fixture shard id resolves against. */
export const SPEC_CORPUS = 'test/spec';

/** The extension a TXTAR fixture carries; a directory holding these is a corpus. */
const FIXTURE_EXT = '.txtar';

/** The extension a per-fixture baseline shard carries. */
const SHARD_EXT = '.json';

// Stage ordering. These three values encode the ONE constraint the pre-shard
// recipe documented at length, and it survives sharding unchanged:
//
//   - STAGE_PRE runs before the fixture-golden body. `solver` must, because the
//     body's default-tag lane runs TestSolverDecisionRatchet in ASSERT mode and
//     aborts on a brand-new fixture that is not yet in the routing baseline.
//     `migration` and `parity` must, so their output lands inside the closing
//     diff-stat — the drift has to appear in the diff a contributor reviews.
//   - STAGE_BODY rewrites the TXTAR `-- sql --` / `-- chplan --` /
//     `-- expected_rows --` sections.
//   - STAGE_POST runs after it. `cardinality` must, because it profiles the
//     fixture's RECORDED `-- sql --` text: run early on a brand-new fixture
//     that section is still empty, PrepareRoundTrip returns ok=false, and the
//     row the chaining exists to record is silently skipped.
export const STAGE_PRE = 0;
export const STAGE_BODY = 1;
export const STAGE_POST = 2;

/**
 * The shard table: one entry per independently-regenerable artefact group.
 *
 * Each entry declares only facts that are the shard's own definition — what it
 * writes, what regenerates it, and where its data corpus lives when that is not
 * recoverable from the artefact itself. The cross-shard staleness relation is
 * never declared here; it is derived by `impliedShards`.
 *
 *   goldens    paths the shard writes (the closing diff-stat, and the paths a
 *              change to which is pure OUTPUT rather than input).
 *   recipe     the existing `just` recipe that regenerates it, when it has one.
 *   generators `go test` invocations that regenerate it, and — for every shard,
 *              recipe-backed or not — the packages whose dep closure defines
 *              which source changes can move what it records.
 *   shardTree  a tree of one JSON shard per fixture, whose layout names the
 *              corpus it derives from. Mutually exclusive with `corpus`.
 *   corpus     data-input roots for a shard whose artefacts do not name them.
 */
export const SHARDS = {
  promql: {
    stage: STAGE_BODY,
    goldens: ['test/spec/promql'],
    generators: [
      { pkgs: ['./internal/promql/'], run: '^TestLower$' },
      { pkgs: ['./test/spec/promql/'], tags: ['chdb'], run: '^TestRoundTripChDB$' },
    ],
  },
  logql: {
    stage: STAGE_BODY,
    goldens: ['test/spec/logql'],
    generators: [
      { pkgs: ['./internal/logql/'], run: '^TestLower$' },
      { pkgs: ['./test/spec/logql/'], tags: ['chdb'], run: '^TestRoundTripChDB$' },
    ],
  },
  traceql: {
    stage: STAGE_BODY,
    goldens: ['test/spec/traceql'],
    generators: [
      { pkgs: ['./internal/traceql/'], run: '^TestLower$' },
      { pkgs: ['./test/spec/traceql/'], tags: ['chdb'], run: '^TestRoundTripChDB$' },
    ],
  },
  chsql: {
    stage: STAGE_BODY,
    goldens: ['test/spec/chsql'],
    generators: [{ pkgs: ['./internal/chsql/'], run: '^TestEmit$' }],
  },
  optimizer: {
    stage: STAGE_BODY,
    goldens: ['test/spec/optimizer'],
    generators: [{ pkgs: ['./internal/optimizer/'], run: '^TestOptimizer$' }],
  },
  codegen: {
    stage: STAGE_BODY,
    goldens: ['test/spec/codegen'],
    generators: [
      {
        pkgs: ['./internal/chsql/'],
        run: '^(TestEmitLateMatFixtures|TestEmit_Prewhere)$',
      },
    ],
  },
  migration: {
    stage: STAGE_PRE,
    recipe: 'migration-golden',
    goldens: ['test/e2e/migration/archetypes/*/expected'],
    corpus: ['test/e2e/migration/archetypes'],
    // The Tier-0 harness does not emit the explain reports in-process: it
    // spawns the built `cerberus migrate explain` and captures its output
    // (test/e2e/migration/steps/artifacts.go's runArtifact). So the closure of
    // the harness package alone is NOT the dependency — it stops at
    // internal/migrate and never reaches the parse -> lower -> emit chain whose
    // SQL those reports record verbatim. `./cmd/cerberus/` is what actually
    // produces them, and probing it is what makes an internal/chsql change
    // imply this shard.
    generators: [
      { pkgs: ['./test/e2e/migration/tiers/tier0-offline/...'], tags: ['migration'] },
      { pkgs: ['./cmd/cerberus/'] },
    ],
  },
  parity: {
    stage: STAGE_PRE,
    recipe: 'update-parity-ledgers',
    goldens: ['test/surface-parity', 'test/rejection-parity/catalogue'],
    generators: [{ pkgs: ['./test/surface-parity/', './test/rejection-parity/'] }],
  },
  solver: {
    stage: STAGE_PRE,
    recipe: 'update-solver-decision-baseline',
    goldens: ['test/perf/solver-decision-baseline'],
    shardTree: 'test/perf/solver-decision-baseline',
    generators: [{ pkgs: ['./test/perf/'] }],
  },
  cardinality: {
    stage: STAGE_POST,
    recipe: 'update-cardinality-baseline',
    goldens: ['test/perf/cardinality-baseline'],
    shardTree: 'test/perf/cardinality-baseline',
    generators: [{ pkgs: ['./test/perf/'], tags: ['chdb'] }],
  },
};

/** Every shard name, in the order the table declares them. */
export const SHARD_NAMES = Object.keys(SHARDS);

/** Declaration index, the tie-break that keeps every ordering deterministic. */
const DECLARED_AT = new Map(SHARD_NAMES.map((n, i) => [n, i]));

/** The pseudo-shard that expands to every shard. */
export const ALL = 'all';

/** The env var carrying GOLDEN_UPDATE=1 into a golden-rewriting `go test`. */
const GOLDEN_UPDATE_ENV = 'GOLDEN_UPDATE';

/** Human-readable vocabulary line, printed whenever a shard set is rejected. */
export function vocabulary() {
  return [...SHARD_NAMES, ALL].join(' ');
}

/**
 * Parse the raw shard argument into a validated, stage-ordered shard list.
 * Returns `{ shards, error }`; `error` is a printable multi-line string.
 */
export function resolveRequested(raw) {
  const words = String(raw ?? '')
    .split(/[\s,]+/)
    .filter(Boolean);

  if (words.length === 0) {
    return {
      shards: [],
      error: [
        'error: `just update-golden` requires at least one shard — there is no default',
        `       shards: ${vocabulary()}`,
        '       e.g.:   just update-golden promql cardinality',
        '               just update-golden all',
      ].join('\n'),
    };
  }

  const unknown = words.filter((w) => w !== ALL && !(w in SHARDS));
  if (unknown.length > 0) {
    return {
      shards: [],
      error: [
        `error: unknown shard${unknown.length > 1 ? 's' : ''}: ${unknown.join(' ')}`,
        `       shards: ${vocabulary()}`,
      ].join('\n'),
    };
  }

  const chosen = words.includes(ALL) ? new Set(SHARD_NAMES) : new Set(words);
  return { shards: orderShards(chosen), error: null };
}

/** Order an iterable of shard names by stage, then by declaration. */
export function orderShards(names) {
  return [...new Set(names)].sort(
    (a, b) => SHARDS[a].stage - SHARDS[b].stage || DECLARED_AT.get(a) - DECLARED_AT.get(b),
  );
}

/** True when regenerating any of these shards needs libchdb.so. */
export function needsChdb(names) {
  return [...names].some((n) =>
    (SHARDS[n].generators ?? []).some((g) => (g.tags ?? []).includes('chdb')),
  );
}

/**
 * The concrete commands that regenerate one shard, in order. A recipe-backed
 * shard delegates to its existing `just` recipe so the command stays defined in
 * exactly one place; everything else is a scoped `go test`.
 *
 * The scoping is the point of #1898's item 3: the pre-shard body ran
 * `GOLDEN_UPDATE=1 go test ./...`, compiling and running 74 packages to
 * regenerate goldens in five of them — and letting any unrelated failing test
 * abort the regeneration halfway through.
 */
export function commandsFor(name, { justExecutable = 'just' } = {}) {
  const shard = SHARDS[name];
  if (shard.recipe) {
    return [{ argv: [justExecutable, shard.recipe], env: {} }];
  }
  return shard.generators.map((g) => {
    const argv = ['go', 'test', '-count=1'];
    if ((g.tags ?? []).length > 0) argv.push('-tags', g.tags.join(','));
    if (g.run) argv.push('-run', g.run);
    argv.push(...g.pkgs);
    return { argv, env: { [GOLDEN_UPDATE_ENV]: '1' } };
  });
}

// ---------------------------------------------------------------------------
// Derivation 1: DATA inputs — where a shard's fixtures live, read off the
// artefacts themselves.
// ---------------------------------------------------------------------------

function toSlash(p) {
  return p.split(path.sep).join('/');
}

function isUnder(file, dir) {
  return file === dir || file.startsWith(`${dir}/`);
}

function walkFiles(root, keep) {
  const out = [];
  const stack = [root];
  while (stack.length > 0) {
    const dir = stack.pop();
    let entries;
    try {
      entries = readdirSync(dir, { withFileTypes: true });
    } catch {
      continue;
    }
    for (const e of entries) {
      const full = path.join(dir, e.name);
      if (e.isDirectory()) stack.push(full);
      else if (keep(full)) out.push(full);
    }
  }
  return out.sort();
}

/**
 * Derive the corpus root a per-fixture shard tree was generated from, by
 * resolving every shard id back to the fixture it names.
 *
 * `test/perf/cardinality-baseline/promql/foo.json` carries the id `promql/foo`,
 * which resolves under `test/spec`; `test/perf/solver-decision-baseline/foo.json`
 * carries the bare id `foo`, which resolves one directory deeper. So the root is
 * whichever candidate directory resolves the MOST ids — the cardinality baseline
 * spans three heads and lands on `test/spec`, the solver baseline reads only the
 * PromQL corpus and lands on `test/spec/promql`.
 *
 * Ties break to the DEEPEST candidate, because a shallower directory that
 * resolves the same ids resolves them only by way of the deeper one, and the
 * deeper answer is the one that discriminates: it is what keeps a LogQL fixture
 * from implying the solver baseline, which reads PromQL alone.
 *
 * Picking by count rather than by first hit matters because fixture names are
 * not unique across heads — a bare id that happens to name a fixture in two
 * corpora would otherwise drag the derived root up to their common parent and
 * make the shard fire on everything. An id that resolves nowhere contributes
 * nothing: it is an orphan shard of a fixture the corpus no longer holds, a
 * condition the ratchet tests own, and it must not move the derived root.
 */
export function corpusRootFromShardTree(repoRoot, tree) {
  const abs = path.join(repoRoot, tree);
  if (!existsSync(abs)) return null;

  const ids = walkFiles(abs, (f) => f.endsWith(SHARD_EXT)).map((f) =>
    toSlash(path.relative(abs, f)).slice(0, -SHARD_EXT.length),
  );
  if (ids.length === 0) return null;

  const candidates = [SPEC_CORPUS, ...specSubdirs(repoRoot).map((d) => `${SPEC_CORPUS}/${d}`)];
  let best = null;
  let bestCount = 0;
  for (const c of candidates) {
    const count = ids.filter((id) =>
      existsSync(path.join(repoRoot, `${c}/${id}${FIXTURE_EXT}`)),
    ).length;
    const deeper = best !== null && c.split('/').length > best.split('/').length;
    if (count > bestCount || (count === bestCount && count > 0 && deeper)) {
      best = c;
      bestCount = count;
    }
  }
  return bestCount > 0 ? best : null;
}

function specSubdirs(repoRoot) {
  const abs = path.join(repoRoot, SPEC_CORPUS);
  try {
    return readdirSync(abs, { withFileTypes: true })
      .filter((e) => e.isDirectory())
      .map((e) => e.name)
      .sort();
  } catch {
    return [];
  }
}

/**
 * The data-input roots of one shard: derived from its shard tree when it has
 * one, declared only when the artefact cannot name them (the migration corpus,
 * whose dashboards and rules sit beside the goldens they generate).
 *
 * A shard whose goldens ARE fixtures — the TXTAR heads, where `-- sql --` and
 * `-- query.promql --` live in the same file — takes that directory as its own
 * corpus, because there is no separating the two.
 */
export function corpusRootsFor(repoRoot, name) {
  const shard = SHARDS[name];
  if (shard.shardTree) {
    const root = corpusRootFromShardTree(repoRoot, shard.shardTree);
    return root ? [root] : [];
  }
  if (shard.corpus) return [...shard.corpus];
  return shard.goldens.filter((g) => holdsFixtures(repoRoot, g));
}

function holdsFixtures(repoRoot, dir) {
  const abs = path.join(repoRoot, dir);
  try {
    if (!statSync(abs).isDirectory()) return false;
  } catch {
    return false;
  }
  return walkFiles(abs, (f) => f.endsWith(FIXTURE_EXT)).length > 0;
}

/** Golden paths of a shard whose change is pure OUTPUT, never a stale input. */
function pureGoldenPrefixes(repoRoot, name) {
  const corpus = corpusRootsFor(repoRoot, name);
  return SHARDS[name].goldens
    .flatMap((g) => (g.includes('*') ? expandGlob(repoRoot, g) : [g]))
    .filter((g) => !corpus.some((c) => isUnder(g, c) || isUnder(c, g)) || g.includes('/expected'));
}

/** Expand a single-`*` directory glob against the tree. */
function expandGlob(repoRoot, pattern) {
  const star = pattern.indexOf('*');
  const head = pattern.slice(0, star).replace(/\/$/, '');
  const tail = pattern.slice(star + 1).replace(/^\//, '');
  const abs = path.join(repoRoot, head);
  let entries;
  try {
    entries = readdirSync(abs, { withFileTypes: true });
  } catch {
    return [];
  }
  return entries
    .filter((e) => e.isDirectory())
    .map((e) => (tail ? `${head}/${e.name}/${tail}` : `${head}/${e.name}`))
    .sort();
}

// ---------------------------------------------------------------------------
// Derivation 2: CODE inputs — the package dependency closure of each shard's
// generators.
// ---------------------------------------------------------------------------

/** A changed file this rule applies to: Go source, whose package it belongs to. */
const GO_EXT = '.go';

/**
 * The repo-relative directories of every first-party package the given
 * generator packages depend on, test imports included. This is the honest
 * upper bound of "source whose change can move what this shard records": the
 * generator is a test binary, and a package outside its import closure cannot
 * be linked into it.
 */
export function generatorPackageDirs(repoRoot, generators, { runGoList = defaultGoList } = {}) {
  const dirs = new Set();
  for (const g of generators) {
    const args = ['list', '-deps', '-test', '-e'];
    if ((g.tags ?? []).length > 0) args.push('-tags', g.tags.join(','));
    args.push(...g.pkgs);
    for (const line of runGoList(repoRoot, args)) {
      if (!line.startsWith(`${MODULE_PATH}/`)) continue;
      // `go list -test` names synthetic test packages as `pkg [pkg.test]`.
      const importPath = line.split(' ')[0];
      dirs.add(importPath.slice(MODULE_PATH.length + 1));
    }
  }
  return dirs;
}

function defaultGoList(repoRoot, args) {
  const out = execFileSync('go', args, {
    cwd: repoRoot,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  });
  return out.split('\n').filter(Boolean);
}

// ---------------------------------------------------------------------------
// The relation itself.
// ---------------------------------------------------------------------------

/**
 * The name a failure message calls the corpus a changed file belongs to: the
 * first path segment BELOW the corpus root when there is one (so a cardinality
 * shard rooted at `test/spec` still says "promql fixtures changed" rather than
 * "spec fixtures changed"), and the root's own name when the file sits directly
 * in it.
 */
function corpusLabel(root, file) {
  const rest = file.slice(root.length + 1).split('/');
  return rest.length > 1 ? rest[0] : path.posix.basename(root);
}

/**
 * Which shards the given changed files imply are stale, and why.
 *
 * Returns `Map<shardName, reason>` where reason is a printable phrase such as
 * `promql fixtures changed` or `internal/chsql changed`. A shard is implied when
 * either derivation fires:
 *
 *   - a changed DATA file sits under one of its derived corpus roots, or
 *   - a changed Go file's package sits in its generators' dependency closure.
 *
 * The Go-closure pass is skipped entirely when no Go source changed, which is
 * the common case for a fixture-only diff and keeps the check sub-second there.
 */
export function impliedShards(changedFiles, { repoRoot, runGoList = defaultGoList } = {}) {
  const files = [...new Set(changedFiles.map(toSlash).filter(Boolean))].sort();
  const implied = new Map();

  const note = (name, reason) => {
    if (!implied.has(name)) implied.set(name, reason);
  };

  for (const name of SHARD_NAMES) {
    const roots = corpusRootsFor(repoRoot, name);
    const goldens = pureGoldenPrefixes(repoRoot, name);
    for (const f of files) {
      if (f.endsWith(GO_EXT)) continue;
      if (!roots.some((r) => isUnder(f, r))) continue;
      if (goldens.some((g) => isUnder(f, g))) continue;
      note(name, `${corpusLabel(roots.find((r) => isUnder(f, r)), f)} fixtures changed`);
      break;
    }
  }

  const goFiles = files.filter((f) => f.endsWith(GO_EXT));
  if (goFiles.length === 0) return implied;

  const goDirs = [...new Set(goFiles.map((f) => path.posix.dirname(f)))].sort();
  for (const name of SHARD_NAMES) {
    const pkgDirs = generatorPackageDirs(repoRoot, SHARDS[name].generators, { runGoList });
    const hit = goDirs.find((d) => pkgDirs.has(d));
    if (hit) note(name, `${hit} changed`);
  }

  return implied;
}

/**
 * The coverage verdict for a chosen shard set against a set of changed files.
 * Returns `{ ok, missing, lines }`, where `lines` is the printable failure the
 * runner emits — one line per uncovered shard, then the exact command to run.
 */
export function coverageViolations(chosen, changedFiles, opts) {
  const implied = impliedShards(changedFiles, opts);
  const have = new Set(chosen);
  const missing = orderShards([...implied.keys()].filter((n) => !have.has(n)));

  if (missing.length === 0) return { ok: true, missing: [], lines: [] };

  const lines = missing.map(
    (n) => `error: ${implied.get(n)} but the \`${n}\` shard was not regenerated`,
  );
  lines.push(`       run: just update-golden ${orderShards([...have, ...missing]).join(' ')}`);
  return { ok: false, missing, lines };
}
