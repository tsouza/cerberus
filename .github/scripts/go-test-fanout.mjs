// go-test-fanout.mjs — runs one `go test` invocation, with every package named
// in FANOUT split across that many concurrent test processes.
//
// Usage (the arguments after the script ARE the `go test` command it runs):
//
//   node .github/scripts/go-test-fanout.mjs go test -timeout 10m -tags chdb -count=1 ./internal/api/... ./internal/chsql/...
//
// The script resolves the package patterns with `go list` under the same build
// tags, enumerates each FANOUT package's top-level tests with
// `go test -json -list`, and hash-partitions them into FANOUT[pkg] `-run`
// selectors (lib/coverage-partition.mjs). It then runs, concurrently:
//
//   - one `go test <flags> <every other package>` process; and
//   - one `go test <flags> -json -run <selector> <pkg>` process per partition.
//
// Every partition is non-empty, and every listed test lands in exactly one
// partition, so the union of the processes runs the same tests the plain
// invocation would. Each partition runs with `-json`, and it fails unless
// every test its selector names passed and no other test ran
// (assertExecuted). Each process keeps the invocation's own `-timeout`.
//
// A chDB-tagged test binary executes every query through one process-wide
// libchdb session, one query at a time, so a package whose chDB tests are
// numerous gets no parallelism from `t.Parallel` or from the runner's cores
// unless its tests are spread over several processes.
//
// Each process's output is buffered and printed whole, in a group, the moment
// that process exits, so a goroutine dump from one process is never
// interleaved with another's output and a finished process's diagnostics are
// already in the log if the step is cancelled later. A partition's `-json`
// events are printed as their plain `go test` output text.
//
// Env: none.
//
// Exit: 0 when every process passed; 1 on a failed process, a failed listing,
// or an argv this script cannot run faithfully.
//
// node: builtins only — no npm deps, no setup-node needed.

import process from 'node:process';
import { spawnSync } from 'node:child_process';
import { error, group, log, notice } from './lib/gh.mjs';
import { runLegBuffered } from './lib/spawn-tagged.mjs';
import { assertExecuted, parseTestInventory, partitionTests, testKey } from './lib/coverage-partition.mjs';

/**
 * Import path → process count. Every entry is a package whose own chDB suite
 * runs serially inside one libchdb session.
 *
 * internal/api/prom: the whole package took 343-443s of go test's 600s
 * per-binary alarm on ubuntu-latest and ran past the alarm on the self-hosted
 * runners (#3674). Measured under a 3-CPU cap, its 415 top-level tests summed
 * to 1172s; the name-hash partition splits that 291/354/331/197s four ways,
 * where three ways put 727s on one process (a few fold-family tests of
 * 80-213s each share a partition).
 *
 * internal/promql: 1110 top-level tests. chdb-roundtrip.mjs's own header
 * measured this package's hand-written suite alone (a distinct concern
 * from the fixture-driven TXTAR walk that script already shards
 * separately) at past 23 minutes unpartitioned. Enrolled here by #3699,
 * whose three TestMixedSetOpOr_Nested* regressions had run on no CI lane
 * at all — `roundtrip-promql-shard` covers this package too, but only on
 * a `run_heavy` push/release run, never an ordinary PR.
 *
 * A chDB test process serializes every query through one embedded
 * ClickHouse session, so wall time drops only as far as concurrent
 * processes actually get concurrent CPU — past the runner's core count,
 * more shards means more processes contending for the same cores, not more
 * throughput. A first 6-way split hit the recipe's 20m per-process timeout
 * (1117 tests, ~186/shard); widening to 15-way made it WORSE, not better —
 * several shards still timed out at the same 20m ceiling, evidence of
 * contention rather than raw per-shard test count. Matching
 * internal/api/prom's proven 4-way split then hit a different failure: the
 * test binary was killed mid-run (no Go panic or assertion, an abrupt
 * process exit) — each of promql's own chDB sessions carries far larger
 * plan trees than api/prom's, so four of them concurrently, plus the
 * fanout's own fifth "every other package" process, exceeds the
 * GitHub-hosted runner's memory rather than its CPU. 2-way trades some of
 * the wall-clock win for enough headroom to actually finish. Still not a
 * measurement; retune once a real CI run reports its per-process times.
 */
export const FANOUT = {
  'github.com/tsouza/cerberus/internal/api/prom': 4,
  'github.com/tsouza/cerberus/internal/promql': 2,
};

/**
 * Flags the script refuses, by name without the leading dash or `test.`
 * prefix.
 *
 * - `run`, `list`, `json`: the script sets these — `-run` is the partition,
 *   `-list` / `-json` enumerate the inventory and verify execution.
 * - `skip`: the inventory lists every test regardless of `-skip`, so a skipped
 *   test would read as a partition test that never ran.
 * - `bench`, `fuzz`: they run benchmarks or a fuzz target the inventory does
 *   not describe.
 * - The output and profile flags: several processes would each write the same
 *   file, and the last one to exit would silently win.
 */
const REFUSED_FLAGS = [
  'run', 'list', 'json', 'skip', 'bench', 'fuzz',
  'o', 'outputdir', 'coverprofile', 'cpuprofile', 'memprofile', 'blockprofile', 'mutexprofile', 'trace',
];

/** Largest `go list` / `go test -json -list` output capture() accepts. */
const MAX_CAPTURE_BYTES = 64 * 1024 * 1024;

/** A flag's bare name: `--test.run=X` and `-run X` both give `run`. */
function flagName(arg) {
  return arg.split('=')[0].replace(/^--?/, '').replace(/^test\./, '');
}

/**
 * Splits `go test <flags...> <packages...>` into its parts. Packages are the
 * trailing `./`-relative patterns; everything between `test` and the first
 * package is a flag (or a flag's value).
 */
export function parseInvocation(argv) {
  if (argv.length < 3 || argv[1] !== 'test') {
    throw new Error(`expected \`go test [flags] <packages>\`, got ${JSON.stringify(argv)}`);
  }
  const rest = argv.slice(2);
  const first = rest.findIndex((a) => a.startsWith('./'));
  if (first < 0) throw new Error('the go test invocation names no ./-relative package');
  const flags = rest.slice(0, first);
  const packages = rest.slice(first);
  const stray = packages.find((a) => !a.startsWith('./'));
  if (stray !== undefined) throw new Error(`argument ${JSON.stringify(stray)} follows the package list`);
  for (const f of flags) {
    if (!f.startsWith('-')) continue;
    const name = flagName(f);
    if (REFUSED_FLAGS.includes(name)) throw new Error(`-${name} cannot be passed through the fan-out (${f})`);
  }
  return { go: argv[0], flags, packages };
}

/** The `-tags` flag (and its value) out of `flags`, for `go list`. */
export function tagArgs(flags) {
  const out = [];
  for (let i = 0; i < flags.length; i++) {
    if (!flags[i].startsWith('-') || flagName(flags[i]) !== 'tags') continue;
    out.push(flags[i]);
    if (!flags[i].includes('=')) out.push(flags[i + 1]);
  }
  return out;
}

/**
 * The processes one invocation fans out to. `importPaths` is the resolved
 * package list; `inventories` maps each FANOUT package present in it to its
 * parseTestInventory result.
 */
export function planCommands({ go, flags, importPaths, inventories, fanout = FANOUT }) {
  const split = importPaths.filter((p) => fanout[p] > 1);
  const whole = importPaths.filter((p) => !(fanout[p] > 1));
  const commands = [];
  if (whole.length > 0) {
    commands.push({ name: `${whole.length} package(s)`, argv: [go, 'test', ...flags, ...whole], env: {} });
  }
  for (const pkg of split) {
    const inventory = inventories.get(pkg);
    if (!inventory) throw new Error(`no test inventory for ${pkg}`);
    const count = fanout[pkg];
    for (let index = 1; index <= count; index++) {
      const plan = partitionTests(inventory, index, count);
      commands.push({
        name: `${pkg} ${index}/${count}`,
        argv: [go, 'test', ...flags, '-json', '-run', plan.pattern, pkg],
        env: {},
        plan,
      });
    }
  }
  return commands;
}

/**
 * Reads one partition's `go test -json` stream: the plain output text it
 * carries, and the keys of the top-level tests that passed. Lines that are
 * not JSON (a build failure's stderr) pass through as text.
 */
export function readTestEvents(stream) {
  const text = [];
  const passed = new Set();
  for (const line of stream.split('\n')) {
    if (line === '') continue;
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      text.push(line);
      continue;
    }
    if (typeof event.Output === 'string') text.push(event.Output.replace(/\n$/, ''));
    if (event.Action === 'pass' && event.Test && !event.Test.includes('/')) {
      passed.add(testKey({ package: event.Package, name: event.Test }));
    }
  }
  return { text: text.join('\n'), passed };
}

/**
 * One finished process's verdict and printable output. A partition fails
 * when go test did, and also when any test its selector names did not pass
 * or a test outside it ran — a `-run` that matched nothing exits 0.
 */
export function judge(command, code, out) {
  if (!command.plan) return { ok: code === 0, text: out };
  const { text, passed } = readTestEvents(out);
  if (code !== 0) return { ok: false, text };
  try {
    assertExecuted(command.plan, passed);
  } catch (e) {
    return { ok: false, text: `${text}\n${e.message}` };
  }
  return { ok: true, text };
}

function capture(argv) {
  const res = spawnSync(argv[0], argv.slice(1), { encoding: 'utf8', maxBuffer: MAX_CAPTURE_BYTES });
  if (res.status !== 0) {
    throw new Error(`${argv.join(' ')} failed (exit ${res.status}): ${res.stderr || res.stdout || res.error}`);
  }
  return res.stdout;
}

async function main() {
  let invocation;
  try {
    invocation = parseInvocation(process.argv.slice(2));
  } catch (e) {
    error(`go-test-fanout: ${e.message}`);
    process.exit(1);
  }
  const { go, flags, packages } = invocation;

  let commands;
  try {
    const importPaths = capture([go, 'list', ...tagArgs(flags), ...packages]).split('\n').filter(Boolean);
    const inventories = new Map();
    for (const pkg of importPaths.filter((p) => FANOUT[p] > 1)) {
      // Listing compiles and links the package's test binary, so the
      // partition processes below start from a warm build cache instead of
      // each compiling the same binary at once.
      inventories.set(pkg, parseTestInventory(capture([go, 'test', ...flags, '-json', '-list', '.*', pkg])));
    }
    commands = planCommands({ go, flags, importPaths, inventories });
  } catch (e) {
    error(`go-test-fanout: ${e.message}`);
    process.exit(1);
  }

  notice(`go-test-fanout: ${commands.length} go test process(es)`);
  const started = Date.now();
  const results = await Promise.all(
    commands.map(async (command) => {
      const { code, out } = await runLegBuffered(command);
      const verdict = judge(command, code, out);
      group(`${command.name} (exit ${code}${verdict.ok ? '' : ', FAILED'})`, () => log(verdict.text.trimEnd()));
      return verdict;
    }),
  );
  const elapsedSeconds = Math.round((Date.now() - started) / 1000);
  const failed = results.filter((r) => !r.ok);
  if (failed.length > 0) {
    error(`go-test-fanout: ${failed.length} of ${results.length} process(es) failed after ${elapsedSeconds}s`);
    process.exit(1);
  }
  notice(`go-test-fanout: ${results.length} process(es) passed in ${elapsedSeconds}s`);
}

// Import-safe: the tests import the pure functions without running anything.
if (process.argv[1] && process.argv[1].endsWith('go-test-fanout.mjs')) {
  await main();
}
