// lane-closure.mjs — which repository paths can move what a CI lane validates,
// DERIVED from the Go import graph rather than declared.
//
// # THE BLIND SPOT THIS CLOSES (#2902, from the #2895 outage)
//
// `.github/ci-lanes.json` gives every lane a `package_globs` list, and gates
// that attribute a change to a lane read it as "the paths this lane validates".
// For the reference lanes that list names the lane's OWN directories and
// nothing else. `compatibility.loki` declares
//
//     compatibility/loki/**, internal/logql/**, internal/api/loki/**
//
// and PR #2824 (`0d32cc96d`) — the change that produced #2895's 144-case LogQL
// parity regression — moved `internal/chclient`, `internal/chopt`,
// `internal/engine`, `internal/config` and `cmd/cerberus`. It matched none of
// those three globs, so the one lane that would have caught the regression did
// not consider itself affected by the change that caused it.
//
// That is not a typo in one list. cerberus is a SHARED pipeline (`chplan` →
// `optimizer` → `chsql` → `chclient`, driven by `engine`) under three thin
// heads, so a per-head glob list structurally cannot express "this lane also
// depends on the shared pipeline". Any hand-written list of that dependency is
// the drift, not the fix: the import graph moves every week and the list does
// not.
//
// So the list is not extended. Each lane's declared globs are treated as SEEDS
// — the packages the lane is about — and the paths it validates are the
// first-party dependency closure of those seeds, read out of the import graph
// `go list` reports. `lib/golden-shards.mjs` already solved the identical
// problem for the golden shards this way (`impliedShards`, derivation 2), and
// its own header says why a hand table was not acceptable there either.
//
// # WHY THIS LOADS THE GRAPH INSTEAD OF CALLING generatorPackageDirs
//
// `golden-shards.mjs`'s `generatorPackageDirs` runs `go list -deps -test` once
// per shard: ten shards, and every one of them worth its own process. The lane
// registry has 27 lanes that gate `main` while skipping pull requests, and
// several of them seed on `internal` or `test` — measured on this tree, one
// `go list -deps -test` per lane costs 82 seconds of wall clock and 58 seconds
// of CPU for the registry. Loading the module's import EDGES once per build-tag
// set (`go list -json`, 2.7s for the whole module) and walking the closure here
// costs about 20 seconds for the same answer, on a required check that also
// fires on every pull-request description edit. Same authority — `go list`
// reading the real graph — at the scale this caller works at.
//
// # NON-GO INPUTS
//
// `go list` sees Go packages, and a lane can also be moved by a compose file, a
// workflow, a reference-stack config or a fixture corpus. Those stay in
// `package_globs`, and the derived closure is UNIONED with it rather than
// replacing it. That division is stable in a way the old one was not: what is
// left declared is the lane's own artefacts, which move when the lane moves,
// while the transitive dependency — the half that drifted — is now derived.
//
// # ENV / INPUTS
//
// Pure module: no env of its own. Callers pass `repoRoot` and may inject
// `runGoList(repoRoot, args) -> string` to test without a toolchain.

import { existsSync, readdirSync, statSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import path from 'node:path';

import { MODULE_PATH } from './golden-shards.mjs';

/** The extension that makes a directory a Go package directory. */
const GO_EXT = '.go';

/** Directories `go list` itself never treats as packages. */
const IGNORED_DIRS = new Set(['node_modules', 'testdata', '.git']);

/** The `go list -json` fields the closure needs, and nothing else. */
const GO_LIST_FIELDS = 'ImportPath,Dir,Imports,TestImports,XTestImports';

function toSlash(p) {
  return p.split(path.sep).join('/');
}

function isDirectory(absolute) {
  try {
    return statSync(absolute).isDirectory();
  } catch {
    return false;
  }
}

/** Does this directory hold Go source anywhere beneath it? */
export function holdsGoSource(repoRoot, dir) {
  const stack = [path.join(repoRoot, dir)];
  while (stack.length > 0) {
    const current = stack.pop();
    let entries;
    try {
      entries = readdirSync(current, { withFileTypes: true });
    } catch {
      continue;
    }
    for (const entry of entries) {
      if (entry.isDirectory()) {
        if (IGNORED_DIRS.has(entry.name) || entry.name.startsWith('.')) continue;
        stack.push(path.join(current, entry.name));
      } else if (entry.name.endsWith(GO_EXT)) {
        return true;
      }
    }
  }
  return false;
}

/** The literal directory prefix of a glob — everything before its first `*`. */
export function staticPrefix(glob) {
  const segments = String(glob ?? '').split('/');
  const wildcard = segments.findIndex((segment) => segment.includes('*'));
  return wildcard === -1 ? String(glob ?? '') : segments.slice(0, wildcard).join('/');
}

/**
 * The Go package directories a lane's own `package_globs` name.
 *
 * A glob anchored at a directory that holds Go source seeds that directory. A
 * glob anchored at a single `.go` FILE seeds the package that file belongs to —
 * a file is a member of its package, and its behaviour depends on everything
 * that package depends on, so naming one file is not a way to claim a narrower
 * dependency than the package has. A glob whose first segment is already a
 * wildcard seeds nothing: it matches every path in the repository, so no
 * closure could widen it.
 */
export function laneSeedDirs(lane, { repoRoot }) {
  const seeds = new Set();
  for (const glob of lane?.package_globs ?? []) {
    const prefix = staticPrefix(glob);
    if (prefix === '') continue;
    const absolute = path.join(repoRoot, prefix);
    if (!existsSync(absolute)) continue;
    if (isDirectory(absolute)) {
      if (holdsGoSource(repoRoot, prefix)) seeds.add(prefix);
      continue;
    }
    if (!prefix.endsWith(GO_EXT)) continue;
    const owner = path.posix.dirname(prefix);
    if (owner !== '' && owner !== '.') seeds.add(owner);
  }
  return [...seeds].sort();
}

function defaultGoList(repoRoot, args) {
  return execFileSync('go', args, {
    cwd: repoRoot,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
  });
}

/**
 * Split `go list -json`'s concatenated objects. The command pretty-prints one
 * object per package with its closing brace in column zero, which is the only
 * place a `}` can start a line, so that is the record separator.
 */
export function splitGoListJSON(text) {
  const records = [];
  let buffer = [];
  for (const line of String(text ?? '').split('\n')) {
    buffer.push(line);
    if (line === '}') {
      records.push(JSON.parse(buffer.join('\n')));
      buffer = [];
    }
  }
  return records;
}

/** A first-party import path reduced to its repo-relative package directory. */
function firstPartyDir(importPath) {
  if (importPath === MODULE_PATH) return '';
  if (!importPath.startsWith(`${MODULE_PATH}/`)) return null;
  return importPath.slice(MODULE_PATH.length + 1);
}

/**
 * The first-party import graph, keyed by repo-relative package directory:
 * `Map<dir, { imports, testImports }>`.
 *
 * The two edge sets are kept apart because `go list -deps -test <pattern>`
 * keeps them apart: a package NAMED by the pattern contributes its test
 * binaries' imports, while a package merely reached through the graph
 * contributes only what it imports in production. Folding them into one set
 * would make every transitive dependency drag in its own test fixtures, and a
 * head's test helpers would then reach the other heads — the
 * everything-depends-on-everything failure, which is the same blindness this
 * module exists to remove, pointed the other way.
 *
 * `-e` keeps a package that does not build (a nested module's directory, a tag
 * combination that leaves a package empty) from failing the whole load; such a
 * package simply contributes no edges.
 */
export function loadPackageGraph({ repoRoot, tags = [], runGoList = defaultGoList }) {
  const args = ['list', '-e', `-json=${GO_LIST_FIELDS}`];
  if (tags.length > 0) args.push('-tags', [...tags].join(','));
  args.push('./...');

  const graph = new Map();
  for (const pkg of splitGoListJSON(runGoList(repoRoot, args))) {
    const dir = pkg.Dir ? toSlash(path.relative(repoRoot, pkg.Dir)) : firstPartyDir(pkg.ImportPath ?? '');
    if (dir === null || dir === '' || dir.startsWith('..')) continue;
    const node = graph.get(dir) ?? { imports: new Set(), testImports: new Set() };
    for (const [list, into] of [
      [pkg.Imports, node.imports],
      [pkg.TestImports, node.testImports],
      [pkg.XTestImports, node.testImports],
    ]) {
      for (const importPath of list ?? []) {
        const target = firstPartyDir(importPath);
        if (target) into.add(target);
      }
    }
    graph.set(dir, node);
  }
  return graph;
}

function isUnder(dir, root) {
  return dir === root || dir.startsWith(`${root}/`);
}

/**
 * Every package directory reachable from `seeds` in `graph`, seeds included.
 * A seed directory stands for every package at or beneath it, which is what
 * `./<dir>/...` means to `go list`.
 */
export function closureFrom(graph, seeds) {
  const reached = new Set();
  for (const dir of graph.keys()) {
    if (seeds.some((seed) => isUnder(dir, seed))) reached.add(dir);
  }

  // Seeds first, with their test edges; everything reached from there follows
  // production imports only. See loadPackageGraph's note on why.
  const frontier = [];
  for (const seed of [...reached]) {
    const node = graph.get(seed);
    for (const next of [...node.imports, ...node.testImports]) {
      if (reached.has(next)) continue;
      reached.add(next);
      frontier.push(next);
    }
  }
  while (frontier.length > 0) {
    for (const next of graph.get(frontier.pop())?.imports ?? []) {
      if (reached.has(next)) continue;
      reached.add(next);
      frontier.push(next);
    }
  }
  return reached;
}

/**
 * The affected-path globs of one lane: its declared `package_globs` unioned
 * with `<dir>/**` for every package in its seeds' dependency closure.
 */
export function affectedGlobsFor(lane, closureDirs) {
  const globs = new Set(lane?.package_globs ?? []);
  for (const dir of closureDirs) globs.add(`${dir}/**`);
  return [...globs].sort();
}

/**
 * `Map<laneID, string[]>` of affected-path globs for the given lanes.
 *
 * The import graph is loaded once per distinct build-tag set — build tags
 * select which files, and so which imports, a package has, so a chdb-tagged
 * lane and an untagged one do not see the same graph.
 */
export function laneAffectedGlobs(lanes, { repoRoot, runGoList = defaultGoList }) {
  const graphs = new Map();
  const out = new Map();
  for (const lane of lanes) {
    const tags = [...(lane.build_tags ?? [])].sort();
    const key = tags.join(',');
    if (!graphs.has(key)) graphs.set(key, loadPackageGraph({ repoRoot, tags, runGoList }));
    const seeds = laneSeedDirs(lane, { repoRoot });
    const closure = seeds.length === 0 ? new Set() : closureFrom(graphs.get(key), seeds);
    out.set(lane.id, affectedGlobsFor(lane, closure));
  }
  return out;
}

/** `Map<laneID, string[]>` holding each lane's DECLARED globs and nothing else. */
export function declaredGlobs(lanes) {
  return new Map(lanes.map((lane) => [lane.id, [...(lane.package_globs ?? [])]]));
}
