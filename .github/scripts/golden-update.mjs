// golden-update.mjs — the runner behind `just update-golden <shard>...`.
//
// Regenerates only the artefact groups the caller named, in the dependency
// order the recipe has always encoded, and refuses to start when the chosen
// shards do not cover what the working tree's own diff implies is stale.
//
// That refusal is the whole reason sharding is safe. The pre-shard recipe
// chained all five stages unconditionally because of #1573 (hit again by #1571
// and #1592): a contributor regenerated one artefact, saw zero remaining churn,
// pushed, and got a red lane because a DIFFERENT artefact had gone stale. CI can
// only detect that drift — a golden is a reviewed artifact, so the harness
// refuses to rewrite one under CI. Paying 14 minutes on every invocation was the
// blunt fix; computing the coverage is the precise one. See lib/golden-shards.mjs
// for the two derivations that compute it.
//
// Execution is concurrent WITHIN a stage, sequential ACROSS stages. The three
// stages (STAGE_PRE / STAGE_BODY / STAGE_POST) encode a real dependency — the
// solver/parity/migration shards must finish before the TXTAR body rewrite
// starts, and the cardinality shard must start after it, because it profiles
// the body's freshly-rewritten `-- sql --` text (see golden-shards.mjs's
// per-stage comment). Shards WITHIN one stage share no such ordering: each
// declares a disjoint `goldens` output path (enforced by
// test/regression/golden_shard_coverage_test.go), so promql/logql/traceql/
// chsql/optimizer/codegen — the six STAGE_BODY shards, the ones that actually
// dominate `update-golden all`'s wall clock — can run as concurrent `go test`
// child processes with no shared mutable state: each is a separate OS process,
// and chdb-go's `chdb.NewSession()` (chdb/session.go) provisions a fresh
// `os.MkdirTemp("", "chdb_")`-backed session per process, so there is no data
// directory to collide over. CI already leans on the same fact one level up —
// chdb.yml's `roundtrip (<ql>)` matrix runs the three QL heads as parallel
// jobs today. A shard's own multiple generators (e.g. promql's `TestLower`
// writing `-- sql --`/`-- chplan --`, then `TestRoundTripChDB` reading that
// freshly-written SQL to fill `-- expected_rows --`) still run sequentially,
// in declaration order, because that ordering IS a real data dependency.
//
// Environment inputs:
//
//   GOLDEN_SHARDS                (required) space/comma separated shard names,
//                                or `all`. Empty is an error that prints the
//                                vocabulary — there is deliberately no default.
//   GOLDEN_UPDATE_BASE_REF       (optional) git ref the changed-file set is
//                                computed against. Defaults to the merge-base
//                                with origin/main, falling back to HEAD.
//   GOLDEN_UPDATE_CHANGED_FILES  (optional) newline/space separated file list
//                                that REPLACES the git-derived one. Lets the
//                                regression pin drive the coverage check over a
//                                synthetic diff without touching the tree.
//   GOLDEN_UPDATE_CHECK_ONLY     (optional) `1` runs vocabulary + coverage and
//                                exits without regenerating anything.
//   GOLDEN_UPDATE_PRINT_PLAN     (optional) `1` prints the ordered regeneration
//                                plan (`<stage> <shard> <command>` per line)
//                                followed by one `diff <pathspec>...` line
//                                naming the scope of the closing review
//                                diff-stat, then exits. The plan is a property
//                                of the shard table alone, so this runs no
//                                coverage check.
//   CHDB_INSTALL_PATH            (optional) libchdb.so path checked before a
//                                chdb-tagged shard runs.
//   JUST_EXECUTABLE              (optional) how to re-invoke `just`.
//
// Exits 1 on an empty/unknown shard set, on a coverage violation, on a missing
// libchdb.so, or on a regeneration command failing. Within a stage, every
// shard's siblings still finish (they were already running concurrently) so
// one invocation surfaces every failure in that stage rather than just the
// first; a later stage never starts once its predecessor reports a failure.

import { spawn, spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import process from 'node:process';

import { error, log } from './lib/gh.mjs';
import {
  SHARDS,
  commandsFor,
  coverageViolations,
  needsChdb,
  resolveRequested,
} from './lib/golden-shards.mjs';

const repoRoot = process.cwd();

/** Split an env-provided file list on newlines or whitespace. */
function splitList(raw) {
  return String(raw ?? '')
    .split(/[\s\n]+/)
    .filter(Boolean);
}

function git(args) {
  const r = spawnSync('git', args, { cwd: repoRoot, encoding: 'utf8' });
  if (r.status !== 0) return null;
  return r.stdout.split('\n').filter(Boolean);
}

/**
 * Every file this branch changed, working tree included.
 *
 * The base is the merge-base with origin/main rather than HEAD, because the
 * #1573 trap does not require the change to be uncommitted: a contributor who
 * commits a new fixture and only then runs the recipe has a clean `git diff
 * HEAD` and would sail past a working-tree-only check.
 */
function changedFiles() {
  const override = process.env.GOLDEN_UPDATE_CHANGED_FILES;
  if (override !== undefined && override !== '') return splitList(override);

  const explicitBase = process.env.GOLDEN_UPDATE_BASE_REF;
  const base =
    explicitBase || (git(['merge-base', 'HEAD', 'origin/main']) ?? ['HEAD'])[0] || 'HEAD';

  const tracked = git(['diff', '--name-only', base]) ?? [];
  const untracked = git(['ls-files', '--others', '--exclude-standard']) ?? [];
  return [...tracked, ...untracked];
}

/** git-diff pathspecs for a shard's goldens (glob tails trimmed off). */
function diffPaths(names) {
  const out = new Set();
  for (const n of names) {
    for (const g of SHARDS[n].goldens) {
      const star = g.indexOf('*');
      out.add(star === -1 ? g : g.slice(0, star).replace(/\/$/, ''));
    }
  }
  return [...out].sort();
}

function fail(message) {
  for (const line of String(message).split('\n')) log(line);
  error(String(message).split('\n')[0].replace(/^error:\s*/, ''));
  process.exit(1);
}

/**
 * Runs one child process, streaming its stdout/stderr to this process's own
 * streams line-by-line, each line tagged `[name] `. Line-buffering (rather
 * than piping the raw stream through) is what keeps concurrent children's
 * output from interleaving mid-line into unreadable garbage; a whole line is
 * still the smallest unit two children can race to print.
 *
 * Resolves to the exit code rather than rejecting, so a caller running
 * several of these concurrently can let every child finish (and print its
 * output) before deciding whether the group as a whole failed.
 */
function runTagged(name, argv, env, cwd) {
  return new Promise((resolve) => {
    const child = spawn(argv[0], argv.slice(1), {
      cwd,
      env: { ...process.env, ...env },
    });

    const tag = (stream, sink) => {
      let carry = '';
      stream.on('data', (chunk) => {
        carry += chunk.toString();
        const lines = carry.split('\n');
        carry = lines.pop();
        for (const l of lines) sink(`[${name}] ${l}`);
      });
      stream.on('end', () => {
        if (carry) sink(`[${name}] ${carry}`);
      });
    };
    tag(child.stdout, log);
    tag(child.stderr, log);

    child.on('close', (code) => resolve(code === null ? 1 : code));
  });
}

/** Runs one shard's generators sequentially (declaration order is a real
 * data dependency — see the module comment). Resolves to `{ name, code }`;
 * `code` is the first non-zero exit, or 0 if every generator succeeded. */
async function runShard(name, justExecutable, repoRoot) {
  for (const { argv, env } of commandsFor(name, { justExecutable })) {
    log(`==> ${name}: ${Object.entries(env)
      .map(([k, v]) => `${k}=${v}`)
      .join(' ')} ${argv.join(' ')}`.replace(/\s+/g, ' '));
    const code = await runTagged(name, argv, env, repoRoot);
    if (code !== 0) return { name, code };
  }
  return { name, code: 0 };
}

/** Groups an already stage-ordered shard list into consecutive same-stage
 * runs, preserving the incoming order within and across groups. */
function groupByStage(names) {
  const groups = [];
  for (const name of names) {
    const stage = SHARDS[name].stage;
    const last = groups[groups.length - 1];
    if (last && last.stage === stage) last.names.push(name);
    else groups.push({ stage, names: [name] });
  }
  return groups;
}

async function runShards(shards, justExecutable, repoRoot) {
  for (const { names } of groupByStage(shards)) {
    const results = await Promise.all(names.map((n) => runShard(n, justExecutable, repoRoot)));
    const failed = results.find((r) => r.code !== 0);
    if (failed) {
      error(`\`${failed.name}\` shard regeneration failed`);
      process.exit(failed.code);
    }
  }
}

async function main() {
  const { shards, error: parseError } = resolveRequested(process.env.GOLDEN_SHARDS);
  if (parseError) fail(parseError);

  const justExecutable = process.env.JUST_EXECUTABLE || 'just';

  if (process.env.GOLDEN_UPDATE_PRINT_PLAN === '1') {
    for (const name of shards) {
      for (const { argv, env } of commandsFor(name, { justExecutable })) {
        const prefix = Object.entries(env).map(([k, v]) => `${k}=${v}`);
        log(`${SHARDS[name].stage} ${name} ${[...prefix, ...argv].join(' ')}`);
      }
    }
    log(`diff ${diffPaths(shards).join(' ')}`);
    return;
  }

  const verdict = coverageViolations(shards, changedFiles(), { repoRoot });
  if (!verdict.ok) fail(verdict.lines.join('\n'));

  if (process.env.GOLDEN_UPDATE_CHECK_ONLY === '1') {
    log(`shard coverage ok: ${shards.join(' ')}`);
    return;
  }

  const chdbPath = process.env.CHDB_INSTALL_PATH;
  if (needsChdb(shards) && chdbPath && !existsSync(chdbPath)) {
    fail(
      `error: ${chdbPath} not found — run 'just chdb-install' first; without it the ` +
        'chdb-tagged -- expected_rows -- sections (and the cardinality baseline) cannot ' +
        'regenerate and go stale',
    );
  }

  await runShards(shards, justExecutable, repoRoot);

  log('');
  log(`Diff of regenerated artefacts (${shards.join(' ')}):`);
  const stat = spawnSync('git', ['--no-pager', 'diff', '--stat', '--', ...diffPaths(shards)], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
  if (stat.status !== 0) log('(no diff-stat available)');
}

await main();
