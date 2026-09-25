// mutation-cache.mjs — drive the content-keyed verdict cache of one `mutation`
// leg (.github/workflows/mutation.yml). What the key covers, how timed-out
// mutants are handled, and when an entry is believed are stated in the header
// of ./lib/mutation-cache.mjs.
//
// MODE (env, or argv[2]):
//
//   key        Collect this checkout's key inputs for the leg and write
//              `key=<sha256>` to $GITHUB_OUTPUT.
//              Env: SCOPE, PHASE_ROW (the leg's matrix row as JSON), DIFF_REF,
//              GREMLINS_INSTALL (`<module>/cmd/gremlins@<fork ref>`, the
//              argument the workflow hands `go install`), MUTANT_TIMEOUT_MIN,
//              MUTANT_TIMEOUT_MAX.
//
//   check      After the cache restore. Validate ENTRY against KEY. A valid
//              entry writes its report to REPORT, prints the key, the run that
//              produced it and its survivors, writes a `cache` PROVENANCE
//              record, and sets `hit=true`. Anything else deletes ENTRY, says
//              why, and sets `hit=false`.
//              Env: KEY, PHASE, THRESHOLD, ENTRY, REPORT, PROVENANCE.
//
//   store      After a real gremlins run produced REPORT. Writes a `run`
//              PROVENANCE record, and writes ENTRY and sets `cacheable=true`
//              only when the verdict is timing-stable.
//              Env: KEY, PHASE, THRESHOLD, ENTRY, REPORT, PROVENANCE, RUN_URL.
//
//   aggregate  In the `mutation` aggregator. Every phase of MATRIX must have a
//              provenance record under PROVENANCE_DIR/<phase>/, every `cache`
//              record must carry an entry that validates against the record's
//              key and the phase's own threshold, and no `cache` record is
//              accepted unless MUTATION_CACHE is on.
//              Env: MATRIX, PROVENANCE_DIR, MUTATION_CACHE.
//
// MUTATION_CACHE (check, store, aggregate): `read-write` enables the cache;
// any other value, or none, makes every leg run gremlins and store nothing.
//
// Exit: 0 on success (a miss is a success of `check`); 1 on any failure.

import { existsSync, mkdirSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve, sep } from 'node:path';
import { release } from 'node:os';
import process from 'node:process';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { assertSafeArg, capture, error, log, notice, setOutput } from './lib/gh.mjs';
import {
  buildEntry,
  canonicalJson,
  legCacheKey,
  phaseRowForKey,
  sha256,
  survivors,
  timingStability,
  validateEntry,
  validateProvenance,
} from './lib/mutation-cache.mjs';

const scriptsDir = dirname(fileURLToPath(import.meta.url));

// The scripts a leg executes to reach its verdict. Their local imports are
// followed transitively by runnerScriptHashes().
export const RUNNER_SCRIPT_ENTRIES = Object.freeze([
  'mutation-run.mjs',
  'mutant-memory-guard.mjs',
  'gremlins-threshold.mjs',
  'mutation-cache.mjs',
  // The workflow declares the leg's job env, step flags and runner; the
  // setup-go action decides how the toolchain is installed.
  '../workflows/mutation.yml',
  '../actions/setup-go/action.yml',
]);

export const RUNNER_ENV_NAMES = Object.freeze(['MUTANT_TIMEOUT_MIN', 'MUTANT_TIMEOUT_MAX']);

// The Go environment that changes what a test binary is, beside `go version`.
const toolchainGoEnv = [
  'GOOS',
  'GOARCH',
  'GOAMD64',
  'CGO_ENABLED',
  'CC',
  'CXX',
  'CGO_CFLAGS',
  'CGO_CPPFLAGS',
  'CGO_CXXFLAGS',
  'CGO_LDFLAGS',
  'GOEXPERIMENT',
  'GOFLAGS',
];

// The runner the leg executes on: the hosted image and its version, the
// kernel, and the node that runs the harness.
const runnerImageEnv = ['RUNNER_OS', 'RUNNER_ARCH', 'ImageOS', 'ImageVersion'];

export function runnerFingerprint(env = process.env) {
  return {
    ...Object.fromEntries(runnerImageEnv.map((k) => [k, String(env[k] ?? '')])),
    kernel: release(),
    node: process.version,
  };
}

const localImportPattern = /(?:\bfrom\s+|\bimport\s*\(?\s*)['"](\.{1,2}\/[^'"]+)['"]/g;
const joinCallPattern = /filepath\.Join\(([^()]*)\)/g;
const goStringLiteralPattern = /^\s*"((?:[^"\\]|\\.)*)"\s*$/;
const relativeLiteralPattern = /"(\.\.\/[^"\s]*)"/g;

function required(name) {
  const value = String(process.env[name] ?? '').trim();
  if (value === '') throw new Error(`${name} is required`);
  return value;
}

function run(cmd, args, cwd) {
  const res = capture(cmd, args, { cwd });
  if (res.status !== 0) throw new Error(`${cmd} ${args.join(' ')} failed (${res.status}): ${res.stderr.trim()}`);
  return res.stdout;
}

function hashFile(path) {
  return sha256(readFileSync(path));
}

// hashTree maps every regular file under `dir` (recursively when asked) to its
// content hash, keyed by its path relative to `root`.
function hashTree(root, dir, { recursive }) {
  const out = {};
  if (!existsSync(dir)) return out;
  const walk = (d) => {
    for (const name of readdirSync(d).sort()) {
      const p = join(d, name);
      const st = statSync(p);
      if (st.isDirectory()) {
        if (recursive) walk(p);
      } else if (st.isFile()) {
        out[relative(root, p).split(sep).join('/')] = hashFile(p);
      }
    }
  };
  if (statSync(dir).isFile()) {
    out[relative(root, dir).split(sep).join('/')] = hashFile(dir);
    return out;
  }
  walk(dir);
  return out;
}

// runnerScriptHashes hashes each entry script and every local module it
// imports, transitively.
export function runnerScriptHashes(dir = scriptsDir, entries = RUNNER_SCRIPT_ENTRIES) {
  const seen = new Map();
  const visit = (abs) => {
    const rel = relative(dir, abs).split(sep).join('/');
    if (seen.has(rel)) return;
    const text = readFileSync(abs, 'utf8');
    seen.set(rel, sha256(text));
    for (const m of text.matchAll(localImportPattern)) visit(resolve(dirname(abs), m[1]));
  };
  for (const e of entries) visit(resolve(dir, e));
  return Object.fromEntries([...seen.entries()].sort());
}

// mainModulePackageDirs lists the directory of every main-module package in
// the scope's test closure. Without -e, a package that does not load fails the
// listing rather than dropping out of the key.
const mainModuleDirTemplate = '{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}';

function mainModulePackageDirs(root, pattern) {
  const out = run('go', ['list', '-deps', '-test', '-f', mainModuleDirTemplate, pattern], root);
  return new Set(out.split('\n').map((l) => l.trim()).filter((l) => l !== ''));
}

// literalRelativePaths extracts every all-literal relative path a Go source
// writes: `"../x"` and `filepath.Join("..", "x")`.
export function literalRelativePaths(source) {
  const out = new Set();
  for (const m of source.matchAll(joinCallPattern)) {
    const parts = m[1].split(',').filter((a) => a.trim() !== '');
    const literals = parts.map((a) => a.match(goStringLiteralPattern));
    if (literals.length === 0 || literals.some((l) => l === null)) continue;
    const joined = literals.map((l) => l[1]).join('/');
    if (joined.startsWith('..')) out.add(joined);
  }
  for (const m of source.matchAll(relativeLiteralPattern)) out.add(m[1]);
  return [...out].sort();
}

// goPackageInputs returns the `packages` and `dataRoots` key classes for the
// scope. See the header of ./lib/mutation-cache.mjs.
export function goPackageInputs(root, scope) {
  const absRoot = resolve(root);
  const pattern = scope.endsWith('/...') ? scope : `${scope.replace(/\/$/, '')}/...`;
  const dirs = mainModulePackageDirs(absRoot, pattern);
  if (dirs.size === 0) throw new Error(`go list found no main-module package under ${pattern}`);
  const packages = {};
  const dataRoots = {};
  const inside = (p) => p === absRoot || p.startsWith(absRoot + sep);
  for (const dir of [...dirs].sort()) {
    if (!inside(dir)) throw new Error(`package directory ${dir} is outside ${absRoot}`);
    const rel = relative(absRoot, dir).split(sep).join('/') || '.';
    // Recursive: testdata/, embedded subdirectories and generated files all
    // sit under the package directory. A nested package is counted in its
    // parent too, which only costs a miss.
    packages[rel] = hashTree(absRoot, dir, { recursive: true });
    for (const name of readdirSync(dir).sort()) {
      if (!name.endsWith('.go')) continue;
      for (const lit of literalRelativePaths(readFileSync(join(dir, name), 'utf8'))) {
        const target = resolve(dir, lit);
        if (!inside(target)) throw new Error(`${rel}/${name} reads ${lit}, which is outside the repository`);
        if (!existsSync(target)) continue;
        const key = relative(absRoot, target).split(sep).join('/') || '.';
        if (dataRoots[key] === undefined) dataRoots[key] = hashTree(absRoot, target, { recursive: true });
      }
    }
  }
  return { packages, dataRoots };
}

// go.mod and go.sum pin the module graph; .gremlins.yaml is the config
// gremlins reads from the module root on every run.
export const GO_MODULE_FILES = Object.freeze(['go.mod', 'go.sum', '.gremlins.yaml']);

export function goModuleInputs(root) {
  const out = {};
  for (const name of GO_MODULE_FILES) {
    const p = join(root, name);
    out[name] = existsSync(p) ? hashFile(p) : '';
  }
  return out;
}

// scopeDiff is the content of the change a changed-line leg mutates.
export function scopeDiff(root, scope, diffRef) {
  if (diffRef === '') return '';
  if (!/^[0-9a-f]{40,64}$/i.test(diffRef)) throw new Error(`DIFF_REF is invalid: ${diffRef}`);
  const dir = scope.replace(/\/\.\.\.$/, '');
  assertSafeArg(dir, 'SCOPE');
  return sha256(run('git', ['diff', '--no-ext-diff', '--no-color', '-U0', diffRef, '--', dir], root));
}

export function collectKeyInputs({ root, scope, phaseRow, diffRef, runnerEnv, toolchain, gremlins, scripts, runner }) {
  return {
    toolchain,
    runner,
    gremlins,
    phaseRow: phaseRowForKey(phaseRow),
    runnerEnv,
    scopeDiff: scopeDiff(root, scope, diffRef),
    runnerScripts: scripts,
    goModule: goModuleInputs(root),
    ...goPackageInputs(root, scope),
  };
}

function toolchainFingerprint(root) {
  const version = run('go', ['version'], root).trim();
  const env = run('go', ['env', ...toolchainGoEnv], root).trim().split('\n');
  return { version, env: Object.fromEntries(toolchainGoEnv.map((k, i) => [k, env[i] ?? ''])) };
}

function gremlinsFingerprint() {
  const install = required('GREMLINS_INSTALL');
  const at = install.lastIndexOf('@');
  if (at <= 0) throw new Error(`GREMLINS_INSTALL has no @ref: ${install}`);
  const module = install.slice(0, at);
  const ref = install.slice(at + 1);
  assertSafeArg(ref, 'GREMLINS_REF');
  const repo = module.replace(/\/cmd\/[^/]+$/, '');
  const out = run('git', ['ls-remote', `https://${repo}`, `refs/tags/${ref}`, `refs/tags/${ref}^{}`, `refs/heads/${ref}`]);
  const lines = out.trim().split('\n').filter((l) => l !== '');
  // A peeled annotated tag names the commit; otherwise the single ref does.
  const peeled = lines.find((l) => l.endsWith('^{}')) ?? lines[0];
  const commit = peeled?.split('\t')[0] ?? '';
  if (!/^[0-9a-f]{40}$/.test(commit)) throw new Error(`cannot resolve ${repo} ${ref} to a commit`);
  return { module, ref, commit };
}

function modeKey() {
  const root = process.cwd();
  const scope = required('SCOPE');
  assertSafeArg(scope, 'SCOPE');
  const phaseRow = JSON.parse(required('PHASE_ROW'));
  const runnerEnv = Object.fromEntries(RUNNER_ENV_NAMES.map((n) => [n, required(n)]));
  const inputs = collectKeyInputs({
    root,
    scope,
    phaseRow,
    diffRef: String(process.env.DIFF_REF ?? '').trim(),
    runnerEnv,
    toolchain: toolchainFingerprint(root),
    gremlins: gremlinsFingerprint(),
    scripts: runnerScriptHashes(),
    runner: runnerFingerprint(),
  });
  const key = legCacheKey(inputs);
  const files = (m) => Object.values(m).reduce((n, v) => n + Object.keys(v).length, 0);
  log(`toolchain: ${inputs.toolchain.version}`);
  log(`gremlins: ${inputs.gremlins.module}@${inputs.gremlins.ref} (${inputs.gremlins.commit})`);
  log(`packages: ${Object.keys(inputs.packages).length} directories, ${files(inputs.packages)} files`);
  log(`data roots: ${Object.keys(inputs.dataRoots).join(', ') || '(none)'} (${files(inputs.dataRoots)} files)`);
  log(`runner scripts: ${Object.keys(inputs.runnerScripts).join(', ')}`);
  log(`scope diff: ${inputs.scopeDiff === '' ? '(full phase)' : inputs.scopeDiff}`);
  notice(`mutation leg cache key ${key}`);
  setOutput('key', key);
}

// MUTATION_CACHE is the switch: only `read-write` reads or writes an entry.
// The workflow sets it on pull requests alone, and a repository variable
// turns it off there too.
export const CACHE_ON = 'read-write';

function cacheEnabled() {
  return String(process.env.MUTATION_CACHE ?? '').trim() === CACHE_ON;
}

function threshold() {
  const t = Number(required('THRESHOLD'));
  if (!Number.isFinite(t)) throw new Error('THRESHOLD is not a number');
  return t;
}

function writeCreating(path, data) {
  mkdirSync(dirname(path), { recursive: true });
  writeFileSync(path, data);
}

function writeProvenance(record) {
  writeCreating(required('PROVENANCE'), `${JSON.stringify(record, null, 2)}\n`);
}

function modeCheck() {
  const key = required('KEY');
  const phase = required('PHASE');
  const entryPath = required('ENTRY');
  const report = required('REPORT');
  if (!cacheEnabled()) {
    notice(`mutation leg cache OFF for ${phase} (MUTATION_CACHE=${process.env.MUTATION_CACHE ?? ''}); running gremlins`);
    rmSync(entryPath, { force: true });
    setOutput('hit', 'false');
    return;
  }
  if (!existsSync(entryPath)) {
    notice(`mutation leg cache MISS for ${phase}: no entry under key ${key}; running gremlins`);
    setOutput('hit', 'false');
    return;
  }
  const verdict = validateEntry(readFileSync(entryPath, 'utf8'), { key, phase, threshold: threshold() });
  if (!verdict.ok) {
    notice(`mutation leg cache MISS for ${phase}: ${verdict.reason}; discarding the entry and running gremlins`);
    rmSync(entryPath, { force: true });
    setOutput('hit', 'false');
    return;
  }
  const { entry } = verdict;
  writeFileSync(report, JSON.stringify(entry.report));
  const lived = survivors(entry.report);
  notice(
    `mutation leg cache HIT for ${phase}: gremlins NOT run. key ${key}, verdict produced by ${entry.sourceRunUrl}; ` +
      `the stored report is re-gated by the efficacy threshold step`,
  );
  log(`stored survivors (${lived.length}):`);
  for (const s of lived) log(`  ${s}`);
  writeProvenance({ phase, source: 'cache', key, digest: entry.digest, sourceRunUrl: entry.sourceRunUrl });
  setOutput('hit', 'true');
}

function modeStore() {
  const key = required('KEY');
  const phase = required('PHASE');
  const entryPath = required('ENTRY');
  const report = JSON.parse(readFileSync(required('REPORT'), 'utf8'));
  const sourceRunUrl = required('RUN_URL');
  const t = threshold();
  writeProvenance({ phase, source: 'run', key });
  if (!cacheEnabled()) {
    notice(`mutation leg ${phase} is not cached: MUTATION_CACHE=${process.env.MUTATION_CACHE ?? ''}`);
    setOutput('cacheable', 'false');
    return;
  }
  const stability = timingStability(report, t);
  if (!stability.stable) {
    notice(`mutation leg ${phase} is not cached: ${stability.reason}`);
    setOutput('cacheable', 'false');
    return;
  }
  const entry = buildEntry({ key, phase, threshold: t, sourceRunUrl, report });
  writeCreating(entryPath, canonicalJson(entry));
  notice(`mutation leg ${phase} verdict (${stability.pass ? 'pass' : 'fail'}) cached under key ${key}: ${stability.reason}`);
  setOutput('cacheable', 'true');
}

// aggregateProblems checks every selected phase's provenance. `readRecord`
// returns { provenance, entry } (either may be undefined) for a phase.
// A `cache` record is refused outright where the cache is off: a hit can
// never stand in for a run the event requires.
export function aggregateProblems(matrix, readRecord, { cacheAllowed }) {
  const problems = [];
  const include = matrix?.include;
  if (!Array.isArray(include)) return ['MATRIX has no include array'];
  for (const row of include) {
    const { provenance, entry } = readRecord(row.phase);
    if (provenance === undefined) {
      problems.push(`${row.phase}: no provenance record — its verdict's source is unknown`);
      continue;
    }
    if (provenance?.source === 'cache' && !cacheAllowed) {
      problems.push(`${row.phase}: a cache hit where the cache is off — this event requires a gremlins run`);
      continue;
    }
    const problem = validateProvenance(provenance, { phase: row.phase, threshold: row.efficacy, entry });
    if (problem !== null) problems.push(`${row.phase}: ${problem}`);
  }
  return problems;
}

function readJsonIfPresent(path) {
  return existsSync(path) ? JSON.parse(readFileSync(path, 'utf8')) : undefined;
}

export const PROVENANCE_FILE = 'provenance.json';
export const ENTRY_FILE = 'entry.json';

function modeAggregate() {
  const matrix = JSON.parse(required('MATRIX'));
  const dir = required('PROVENANCE_DIR');
  const problems = aggregateProblems(
    matrix,
    (phase) => {
      const d = join(dir, phase);
      return { provenance: readJsonIfPresent(join(d, PROVENANCE_FILE)), entry: readJsonIfPresent(join(d, ENTRY_FILE)) };
    },
    { cacheAllowed: cacheEnabled() },
  );
  if (problems.length > 0) {
    for (const p of problems) error(`mutation provenance: ${p}`);
    process.exit(1);
  }
  const hits = matrix.include.filter((row) => {
    const p = readJsonIfPresent(join(dir, row.phase, PROVENANCE_FILE));
    return p.source === 'cache';
  });
  notice(
    `mutation provenance: ${matrix.include.length} phase(s) accounted for — ` +
      `${hits.length} from a validated cache entry, ${matrix.include.length - hits.length} from a gremlins run`,
  );
}

const modes = { key: modeKey, check: modeCheck, store: modeStore, aggregate: modeAggregate };

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const mode = String(process.env.MODE ?? process.argv[2] ?? '').trim();
  try {
    if (!Object.hasOwn(modes, mode)) throw new Error(`MODE must be one of ${Object.keys(modes).join(', ')}`);
    modes[mode]();
  } catch (cause) {
    error(`mutation-cache: ${cause instanceof Error ? cause.message : String(cause)}`);
    process.exit(1);
  }
}
