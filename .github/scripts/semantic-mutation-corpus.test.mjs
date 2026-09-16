// semantic-mutation-corpus.test.mjs — node --test guard for
// semantic-mutation-corpus.mjs's own orchestration (record discovery and
// pass/fail aggregation), via an injected spawnFn — never a real `just`/`go`
// invocation. The real execution this script exists for runs in CI's
// `check-build` job ("Execute the semantic-mutation runner's synthetic
// corpus"), not here; see this script's own header for why.

import assert from "node:assert/strict";
import { test } from "node:test";

import { runCorpus } from "./semantic-mutation-corpus.mjs";
import { DEFAULT_MUTANTS_DIR } from "./lib/semantic-mutation.mjs";

const REPO_ROOT = process.cwd();

test("runCorpus: every real committed record is attempted, in sorted id order", () => {
  const calls = [];
  const { total, failures } = runCorpus({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    justBin: "just",
    spawnFn: (bin, args) => {
      calls.push(args[1]); // ["semantic-mutate", "<id>"]
      return { status: 0 };
    },
  });

  assert.equal(total, 7);
  assert.equal(failures.length, 0);
  assert.equal(calls.length, 7);
  assert.deepEqual(calls, [...calls].sort(), "ids must be attempted in sorted order");
});

test("runCorpus: collects every failing id rather than stopping at the first", () => {
  const { total, failures } = runCorpus({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    justBin: "just",
    spawnFn: (bin, args) => ({ status: args[1].includes("KILLED") ? 1 : 0 }),
  });

  assert.equal(total, 7);
  assert.deepEqual(failures, ["MUTANT-SYNTH-KILLED-INVERT"]);
});

test("runCorpus: a load/validation failure throws rather than silently reporting zero records", () => {
  assert.throws(() => runCorpus({ root: REPO_ROOT, dir: "test/semantic/mutants/does-not-exist" }));
});
