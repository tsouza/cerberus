// ch-server-safety-shards.mjs — plan, run and aggregate the shards of the
// `ch-server-safety` lane (strict-scan.yml, lane `chdb.ch-server-safety`).
//
// test/chserver runs every CPU-bound cancellation build one after another in
// TestCancellation_CPUBoundEmittedShapesAcrossBuilds, behind the
// condition-cache and fleet tests. The lane splits that work across parallel
// jobs without editing the package: each build of the test's
// `cancellationBuilds` table is its own shard, selected with `go test -run`,
// and one `rest` shard runs everything else, selected as the complement with
// `go test -skip`. A test, build or shape the shard table does not name
// therefore always lands in `rest`; no selection can drop it.
//
// Subcommands:
//
//   plan
//     Writes the shard matrix (`{"include": [{id, name, kind}, ...]}`) to the
//     step output `matrix`, derived from the cancellation test's source.
//
//   images <shard> <image>...
//     Prints the images the shard boots, out of <image>..., every image the
//     package pins (the recipe passes them from just/common.just): a
//     cancellation shard's own build, or every image for `all` and `rest`.
//
//   run <shard> -- <go test command>...
//     Runs the go test command with the shard's selection appended (`-json`,
//     `-run`, `-skip`) and prints the test output. The run fails when go test
//     fails or when any test the shard owns did not report a pass, so a
//     selection that matches nothing cannot pass as an empty run.
//
//   <shard> is `all` (the whole package), `rest`, or a cancellation shard id
//   from `plan`.
//
//   aggregate
//     The `ch-server-safety` job's verdict. Env:
//       CHANGES_RESULT  needs.changes.result
//       SETUP_RESULT    needs.ch-server-safety-setup.result
//       SHARDS_RESULT   needs.ch-server-safety-shard.result
//     Fails unless all three are `success`.
//
// Exit: 0 on success, 1 otherwise.

import { spawn } from 'node:child_process';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import process from 'node:process';
import { createInterface } from 'node:readline';
import { pathToFileURL } from 'node:url';

import { error, errorStderr, log, notice, setOutput } from './lib/gh.mjs';

export const PACKAGE_DIR = 'test/chserver';
export const CANCELLATION_SOURCE = join(PACKAGE_DIR, 'cancellation_integration_test.go');
export const CANCELLATION_TEST = 'TestCancellation_CPUBoundEmittedShapesAcrossBuilds';
const CANCELLATION_TABLE = 'cancellationBuilds';

export const SHARD_ALL = 'all';
export const SHARD_REST = 'rest';
export const KIND_REST = 'rest';
export const KIND_CANCELLATION = 'cancellation';
const CANCELLATION_PREFIX = `${KIND_CANCELLATION}-`;

// cancellationImages — the images of the `cancellationBuilds` table in the
// cancellation test's source, in table order. Each row opens with the image
// literal: `{"clickhouse/clickhouse-server:<tag>", ...}`.
export function cancellationImages(source) {
  const start = source.indexOf(`var ${CANCELLATION_TABLE} = []struct {`);
  if (start < 0) throw new Error(`${CANCELLATION_SOURCE}: no \`var ${CANCELLATION_TABLE}\` table`);
  const end = source.indexOf('\n}\n', start);
  if (end < 0) throw new Error(`${CANCELLATION_SOURCE}: \`${CANCELLATION_TABLE}\` table is not closed`);
  const body = source.slice(source.indexOf('}{', start), end);
  const images = [...body.matchAll(/^\s*\{"([^"]+)"/gm)].map((m) => m[1]);
  if (images.length === 0) throw new Error(`${CANCELLATION_SOURCE}: \`${CANCELLATION_TABLE}\` lists no builds`);
  return images;
}

// topLevelTests — every `func TestX(t *testing.T)` across the package's
// sources, sorted.
export function topLevelTests(sources) {
  const names = new Set();
  for (const source of sources) {
    for (const m of source.matchAll(/^func (Test\w*)\(t \*testing\.T\)/gm)) names.add(m[1]);
  }
  return [...names].sort();
}

// imageTag — the tag of an image reference: what follows its last `:`.
export function imageTag(image) {
  return image.slice(image.lastIndexOf(':') + 1);
}

const regexMeta = /[.*+?^${}()|[\]\\]/g;

// exactNamePattern — a `go test -run`/`-skip` pattern that matches exactly the
// test named by `path`. go test splits both the pattern and the test name on
// `/`, so a subtest name that itself contains `/` (an image reference) spans
// several levels; each level is anchored on its own.
export function exactNamePattern(path) {
  return path
    .split('/')
    .map((level) => `^${level.replace(regexMeta, '\\$&')}$`)
    .join('/');
}

export function cancellationShardId(image) {
  return `${CANCELLATION_PREFIX}${imageTag(image)}`;
}

// plan — the shard matrix: one shard per cancellation build, then `rest`.
export function plan(images) {
  const include = images.map((image) => ({
    id: cancellationShardId(image),
    name: `${KIND_CANCELLATION} ${imageTag(image)}`,
    kind: KIND_CANCELLATION,
  }));
  include.push({ id: SHARD_REST, name: SHARD_REST, kind: KIND_REST });
  return { include };
}

// selection — what one shard runs: its `-run` and `-skip` patterns, the tests
// that must report a pass, and, for a cancellation shard, its one image.
export function selection(shard, { cancellation, tests }) {
  if (!tests.includes(CANCELLATION_TEST)) {
    throw new Error(`${PACKAGE_DIR} has no ${CANCELLATION_TEST}; the shard table is stale`);
  }
  const buildTest = (image) => `${CANCELLATION_TEST}/${image}`;
  if (shard === SHARD_ALL) return { run: '', skip: '', expect: tests };
  if (shard === SHARD_REST) {
    return {
      run: '',
      skip: cancellation.map((image) => exactNamePattern(buildTest(image))).join('|'),
      expect: tests,
    };
  }
  const image = cancellation.find((candidate) => cancellationShardId(candidate) === shard);
  if (image === undefined) {
    const known = [SHARD_ALL, SHARD_REST, ...cancellation.map(cancellationShardId)];
    throw new Error(`unknown shard ${JSON.stringify(shard)}; known: ${known.join(', ')}`);
  }
  return { run: exactNamePattern(buildTest(image)), skip: '', expect: [buildTest(image)], image };
}

// shardImages — the images a selection boots, out of every pinned image.
export function shardImages(sel, pinned) {
  if (sel.image === undefined) return pinned;
  if (!pinned.includes(sel.image)) {
    throw new Error(`${sel.image} is not among the pinned images; add it to just/common.just`);
  }
  return [sel.image];
}

// selectionArgs — the go test flags that apply a selection. go test reads its
// flags after the package list too, so they are appended to the command.
export function selectionArgs({ run, skip }) {
  const args = ['-json'];
  if (run !== '') args.push(`-run=${run}`);
  if (skip !== '') args.push(`-skip=${skip}`);
  return args;
}

// verdict — the problems a shard's go test events show: a test that failed,
// and a test the shard owns that never reported a pass.
export function verdict(events, expect) {
  const passed = new Set();
  const failed = new Set();
  for (const event of events) {
    if (event.Test === undefined) continue;
    if (event.Action === 'pass') passed.add(event.Test);
    if (event.Action === 'fail') failed.add(event.Test);
  }
  const problems = [...failed].sort().map((test) => `${test} failed`);
  for (const test of expect) {
    if (!passed.has(test) && !failed.has(test)) problems.push(`${test} did not run`);
  }
  return problems;
}

// aggregate — the problems in the shard jobs' results.
export function aggregate({ changes, setup, shards }) {
  const results = [
    ['changes', changes],
    ['ch-server-safety-setup', setup],
    ['ch-server-safety-shard', shards],
  ];
  return results
    .filter(([, result]) => result !== 'success')
    .map(([job, result]) => `${job} did not succeed (${result || 'no result'})`);
}

// builtUnderIntegration — whether a Go source compiles into the package under
// `-tags=integration`: it has no build constraint, or exactly that one.
export function builtUnderIntegration(source) {
  const constraint = source.match(/^\/\/go:build (.+)$/m);
  return constraint === null || constraint[1].trim() === 'integration';
}

// readPackage — the cancellation builds and the top-level tests of the
// package as `-tags=integration` compiles it.
export function readPackage(root) {
  const dir = join(root, PACKAGE_DIR);
  const sources = readdirSync(dir)
    .filter((file) => file.endsWith('_test.go'))
    .map((file) => readFileSync(join(dir, file), 'utf8'))
    .filter(builtUnderIntegration);
  return {
    cancellation: cancellationImages(readFileSync(join(root, CANCELLATION_SOURCE), 'utf8')),
    tests: topLevelTests(sources),
  };
}

function waitFor(child) {
  return new Promise((resolve) => {
    child.on('error', () => resolve(1));
    child.on('close', (code) => resolve(code ?? 1));
  });
}

async function runShard(shard, command) {
  const sel = selection(shard, readPackage(process.cwd()));
  const [cmd, ...args] = [...command, ...selectionArgs(sel)];
  log(`${cmd} ${args.join(' ')}`);
  const child = spawn(cmd, args, { stdio: ['ignore', 'pipe', 'inherit'] });
  const events = [];
  for await (const line of createInterface({ input: child.stdout })) {
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      process.stdout.write(`${line}\n`);
      continue;
    }
    events.push(event);
    if (typeof event.Output === 'string') process.stdout.write(event.Output);
  }
  const status = await waitFor(child);

  const problems = verdict(events, sel.expect);
  for (const problem of problems) error(`ch-server-safety shard ${shard}: ${problem}`);
  if (status !== 0 && problems.length === 0) error(`ch-server-safety shard ${shard}: go test exited ${status}`);
  if (status !== 0 || problems.length > 0) return 1;
  notice(`ch-server-safety shard ${shard}: ${sel.expect.length} owned test(s) passed`);
  return 0;
}

async function main(argv) {
  const [command, ...rest] = argv;
  if (command === 'plan') {
    const matrix = plan(readPackage(process.cwd()).cancellation);
    setOutput('matrix', JSON.stringify(matrix));
    return 0;
  }
  if (command === 'images') {
    // stdout is the image list the recipe reads, so errors go to stderr.
    const [shard, ...pinned] = rest;
    if (shard === undefined || pinned.length === 0) {
      errorStderr('usage: ch-server-safety-shards.mjs images <shard> <image>...');
      return 1;
    }
    log(shardImages(selection(shard, readPackage(process.cwd())), pinned).join(' '));
    return 0;
  }
  if (command === 'run') {
    const [shard, separator, ...goTest] = rest;
    if (shard === undefined || separator !== '--' || goTest.length === 0) {
      error('usage: ch-server-safety-shards.mjs run <shard> -- <go test command>...');
      return 1;
    }
    return runShard(shard, goTest);
  }
  if (command === 'aggregate') {
    const problems = aggregate({
      changes: process.env.CHANGES_RESULT,
      setup: process.env.SETUP_RESULT,
      shards: process.env.SHARDS_RESULT,
    });
    for (const problem of problems) error(problem);
    if (problems.length > 0) return 1;
    log('every ch-server-safety shard succeeded.');
    return 0;
  }
  error('usage: ch-server-safety-shards.mjs plan | images <shard> <image>... | run <shard> -- <command>... | aggregate');
  return 1;
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    process.exit(await main(process.argv.slice(2)));
  } catch (err) {
    errorStderr(err.message);
    process.exit(1);
  }
}
