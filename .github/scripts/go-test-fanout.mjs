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
//   - one `go test <flags> -run <selector> <pkg>` process per partition.
//
// Every partition is non-empty, and every listed test lands in exactly one
// partition, so the union of the processes runs the same tests the plain
// invocation would. Each process keeps the invocation's own `-timeout`.
//
// A chDB-tagged test binary executes every query through one process-wide
// libchdb session, one query at a time, so a package whose chDB tests are
// numerous gets no parallelism from `t.Parallel` or from the runner's cores
// unless its tests are spread over several processes.
//
// Each process's output is buffered and printed whole, in a group, once every
// process has exited; a goroutine dump from one process is never interleaved
// with another's output.
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
import { parseTestInventory, partitionTests } from './lib/coverage-partition.mjs';

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
 */
export const FANOUT = {
  'github.com/tsouza/cerberus/internal/api/prom': 4,
};

/**
 * Flags that would change which tests run or how go test reports them. The
 * script owns `-run` (it is the partition) and `-list` / `-json` (it uses
 * them to enumerate); a caller passing any of these would get a partition
 * that no longer covers what their invocation selects.
 */
const OWNED_FLAGS = ['-run', '-skip', '-list', '-json'];

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
    const name = f.split('=')[0].replace(/^--/, '-');
    if (OWNED_FLAGS.includes(name)) throw new Error(`${name} is set by this script and cannot be passed in`);
  }
  return { go: argv[0], flags, packages };
}

/** The `-tags` flag (and its value) out of `flags`, for `go list`. */
export function tagArgs(flags) {
  const out = [];
  for (let i = 0; i < flags.length; i++) {
    const name = flags[i].split('=')[0].replace(/^--/, '-');
    if (name !== '-tags') continue;
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
      const { pattern } = partitionTests(inventory, index, count);
      commands.push({
        name: `${pkg} ${index}/${count}`,
        argv: [go, 'test', ...flags, '-run', pattern, pkg],
        env: {},
      });
    }
  }
  return commands;
}

function capture(argv) {
  const res = spawnSync(argv[0], argv.slice(1), { encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
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
  const results = await Promise.all(commands.map(runLegBuffered));
  const elapsedSeconds = Math.round((Date.now() - started) / 1000);
  for (const r of results) {
    group(`${r.leg.name} (exit ${r.code})`, () => log(r.out.trimEnd()));
  }
  const failed = results.filter((r) => r.code !== 0);
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
