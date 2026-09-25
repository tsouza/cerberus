// registry.test.mjs — node:test guard for the pooled image acquisition in
// lib/registry.mjs (`pullImages` over `pullImageAsync`) and its CLI,
// pull-images.mjs.
//
// No daemon: the pool is driven with an injected per-image pull, the async
// driver with an injected `runDocker`, and the CLI with a stub `docker` on
// PATH. The refs name a registry the GHCR mirror never inventories
// (`registry.invalid`), so every acquisition goes straight to the upstream
// retry loop the policy owns.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import process from 'node:process';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

import { MAX_CONCURRENT_PULLS, pullImageAsync, pullImages } from './registry.mjs';

const pullImagesScript = join(dirname(fileURLToPath(import.meta.url)), '..', 'pull-images.mjs');

const refs = (n) => Array.from({ length: n }, (_, i) => `registry.invalid/image-${i}:1`);

// A pull that reports success after `ms`, and records how many pulls were in
// flight at its busiest.
function countingPull(ms) {
  const state = { inFlight: 0, peak: 0 };
  const pull = async (image) => {
    state.inFlight++;
    state.peak = Math.max(state.peak, state.inFlight);
    await sleep(ms);
    state.inFlight--;
    return { acquired: true, output: [{ op: 'log', message: `${image} done` }] };
  };
  return { state, pull };
}

test('the pool never runs more than `concurrency` pulls at once, and does reach it', async () => {
  for (const concurrency of [1, 3, MAX_CONCURRENT_PULLS]) {
    const { state, pull } = countingPull(5);
    const { failed, skipped } = await pullImages(refs(10), { concurrency, pull, emit: () => {} });
    assert.deepEqual(failed, []);
    assert.deepEqual(skipped, []);
    assert.equal(state.peak, concurrency, `peak in-flight pulls with concurrency ${concurrency}`);
  }
});

test('the pool rejects a concurrency that is not a positive integer', async () => {
  for (const concurrency of [0, -1, 1.5, Number.NaN]) {
    await assert.rejects(pullImages(refs(2), { concurrency, pull: countingPull(0).pull }), RangeError);
  }
});

test('the default pool size is the named limit', async () => {
  const { state, pull } = countingPull(5);
  await pullImages(refs(MAX_CONCURRENT_PULLS * 3), { pull, emit: () => {} });
  assert.equal(state.peak, MAX_CONCURRENT_PULLS);
});

// A `runDocker` answering from a per-ref script of results: each `docker pull
// <ref>` takes the next entry, after `delayMs`.
function scriptedDocker(scripts, delayMs = 0, calls = []) {
  const remaining = new Map(Object.entries(scripts).map(([ref, results]) => [ref, [...results]]));
  return async (args) => {
    calls.push(args.join(' '));
    await sleep(delayMs);
    const ref = args.at(-1);
    const next = remaining.get(ref)?.shift();
    return next ?? { status: 0, stdout: `pulled ${ref}`, stderr: '' };
  };
}

const transportFault = { status: 1, stdout: '', stderr: 'read tcp: connection reset by peer\n' };
const rateLimit = { status: 1, stdout: '', stderr: 'toomanyrequests: You have reached your pull rate limit.\n' };

test("one image's retry backoff does not hold up the others", async () => {
  const [slow, ...fast] = refs(4);
  // Two transport faults: the policy sleeps 1 × step, then 2 × step, before the
  // third attempt succeeds. A blocking sleep would stall every other pull for
  // the whole of it.
  const backoffStepSeconds = 0.2;
  const runDocker = scriptedDocker({ [slow]: [transportFault, transportFault] });

  const finished = [];
  const started = Date.now();
  const pull = async (image, options) => {
    const res = await pullImageAsync(image, { ...options, runDocker });
    finished.push({ image, at: Date.now() - started });
    return res;
  };
  const { failed } = await pullImages([slow, ...fast], { concurrency: 2, backoffStepSeconds, pull, emit: () => {} });

  assert.deepEqual(failed, []);
  const slowAt = finished.find((f) => f.image === slow).at;
  const backoffMs = (1 + 2) * backoffStepSeconds * 1_000;
  assert.ok(slowAt >= backoffMs, `the slow image waited out its backoff (${slowAt} ms)`);
  // Every other image went through the second slot while the first slept.
  for (const image of fast) {
    const at = finished.find((f) => f.image === image).at;
    assert.ok(at < backoffMs, `${image} finished at ${at} ms, inside the ${backoffMs} ms backoff`);
  }
  assert.equal(finished.at(-1).image, slow);
});

test('the async driver keeps the per-image policy: a rate limit fails on the first attempt', async () => {
  const calls = [];
  const [ref] = refs(1);
  const { acquired, output } = await pullImageAsync(ref, {
    backoffStepSeconds: 0,
    runDocker: scriptedDocker({ [ref]: [rateLimit] }, 0, calls),
  });
  assert.equal(acquired, false);
  assert.deepEqual(calls, [`pull ${ref}`]);
  assert.match(output.at(-1).message, /QUOTA, not an outage/);
  assert.equal(output.at(-1).op, 'error');
});

test('the async driver keeps the per-image policy: transport faults exhaust the budget', async () => {
  const calls = [];
  const [ref] = refs(1);
  const { acquired, output } = await pullImageAsync(ref, {
    backoffStepSeconds: 0,
    consequence: 'nothing starts',
    runDocker: scriptedDocker({ [ref]: Array(5).fill(transportFault) }, 0, calls),
  });
  assert.equal(acquired, false);
  assert.equal(calls.length, 5);
  assert.deepEqual(output.at(-1), {
    op: 'error',
    message: `docker pull ${ref} failed 5 times on transport faults, so nothing starts`,
  });
});

test('the async driver accepts a local copy when the caller does', async () => {
  const [ref] = refs(1);
  const runDocker = async (args) =>
    args[0] === 'image' ? { status: 0, stdout: '[]', stderr: '' } : { ...transportFault };
  const { acquired, output } = await pullImageAsync(ref, { acceptLocalCopy: true, backoffStepSeconds: 0, runDocker });
  assert.equal(acquired, true);
  assert.equal(output.at(-1).op, 'notice');
  assert.match(output.at(-1).message, /already in the local daemon/);
});

test("each image's output reaches the job log as one contiguous block, never interleaved", () => {
  const images = refs(6);
  // Read what actually reaches the log — the default emitter, in a child
  // process — so a driver that wrote its lines as it produced them is caught,
  // not just a pool that reorders the blocks it was handed. Every image faults
  // once, so each produces several lines spread across a backoff, and the
  // pulls overlap.
  const registryUrl = new URL('./registry.mjs', import.meta.url).href;
  const program = `
    import { setTimeout as sleep } from 'node:timers/promises';
    import { pullImageAsync, pullImages } from ${JSON.stringify(registryUrl)};
    const images = ${JSON.stringify(images)};
    const faulted = new Set();
    const runDocker = async (args) => {
      await sleep(5);
      const ref = args.at(-1);
      if (!faulted.has(ref)) {
        faulted.add(ref);
        return { status: 1, stdout: '', stderr: 'connection reset by peer\\n' };
      }
      return { status: 0, stdout: 'pulled ' + ref, stderr: '' };
    };
    const pull = (image, options) => pullImageAsync(image, { ...options, runDocker });
    await pullImages(images, { concurrency: 3, backoffStepSeconds: 0.02, pull });
  `;
  const res = spawnSync(process.execPath, ['--input-type=module', '-e', program], { encoding: 'utf8' });
  assert.equal(res.status, 0, res.stdout + res.stderr);

  // Attribute each line to the image it names; a block is a maximal run of one
  // image's lines, and each image must own exactly one.
  const blocks = [];
  for (const line of res.stdout.split('\n')) {
    const who = images.find((r) => line.includes(r));
    if (who === undefined) continue;
    if (blocks.at(-1) !== who) blocks.push(who);
  }
  assert.equal(blocks.length, images.length, `blocks in emission order: ${blocks.join(' | ')}`);
  assert.deepEqual([...blocks].sort(), [...images].sort());
});

test('failures are aggregated; stopOnFailure starts nothing new but lets in-flight pulls finish', async () => {
  const images = refs(6);
  const bad = new Set([images[0], images[1]]);
  const pull = async (image) => {
    await sleep(bad.has(image) ? 1 : 10);
    return { acquired: !bad.has(image), output: [] };
  };

  const all = await pullImages(images, { concurrency: 2, pull, emit: () => {} });
  assert.deepEqual(all, { failed: [images[0], images[1]], skipped: [] });

  const stopped = await pullImages(images, { concurrency: 2, pull, emit: () => {}, stopOnFailure: true });
  // Both first-wave pulls were already in flight, so both report; nothing after
  // them starts.
  assert.deepEqual(stopped, { failed: [images[0], images[1]], skipped: images.slice(2) });
});

// A stub `docker` for the CLI: `pull` fails with a rate limit for any ref
// containing "bad", and succeeds otherwise.
function stubDocker() {
  const dir = mkdtempSync(join(tmpdir(), 'registry-test-'));
  writeFileSync(
    join(dir, 'docker'),
    [
      '#!/bin/sh',
      'case "$*" in',
      '  *bad*) echo "toomanyrequests: You have reached your pull rate limit." >&2; exit 1 ;;',
      '  *) echo "pulled $*"; exit 0 ;;',
      'esac',
      '',
    ].join('\n'),
  );
  chmodSync(join(dir, 'docker'), 0o755);
  return dir;
}

function runCli(args) {
  const dir = stubDocker();
  try {
    return spawnSync(process.execPath, [pullImagesScript, ...args], {
      encoding: 'utf8',
      env: { ...process.env, PATH: `${dir}:${process.env.PATH}`, IMAGE_PULL_BACKOFF_SECONDS: '0' },
    });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

test('pull-images.mjs exits 0 when every image is acquired', () => {
  const res = runCli(refs(3));
  assert.equal(res.status, 0, res.stdout + res.stderr);
});

test('pull-images.mjs exits 1 with an aggregate error naming each failed image', () => {
  const images = ['registry.invalid/bad:1', ...refs(8)];
  const res = runCli(images);
  assert.equal(res.status, 1, res.stdout + res.stderr);
  const aggregate = res.stdout.split('\n').find((l) => l.includes('image(s) could not be acquired'));
  assert.ok(aggregate, `no aggregate error in:\n${res.stdout}`);
  assert.match(aggregate, /^::error::1 of 9 image\(s\) could not be acquired: registry\.invalid\/bad:1/);
  // The per-image diagnosis is still printed, whole, before the aggregate.
  assert.match(res.stdout, /docker pull registry\.invalid\/bad:1 was refused by the registry's pull rate limit/);
});
