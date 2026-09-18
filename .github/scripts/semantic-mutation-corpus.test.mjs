// semantic-mutation-corpus.test.mjs — node --test guard for
// semantic-mutation-corpus.mjs's own orchestration (record discovery and
// pass/fail aggregation), via an injected spawnFn — never a real `just`/`go`
// invocation. The real execution this script exists for runs in CI's
// `check-build` job ("Execute the semantic-mutation runner's corpus"), not
// here; see this script's own header for why.

import assert from "node:assert/strict";
import { test } from "node:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

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

  assert.equal(total, 13);
  assert.equal(failures.length, 0);
  assert.equal(calls.length, 13);
  assert.deepEqual(calls, [...calls].sort(), "ids must be attempted in sorted order");
});

test("runCorpus: collects every failing id rather than stopping at the first", () => {
  const { total, failures } = runCorpus({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    justBin: "just",
    spawnFn: (bin, args) => ({ status: args[1].includes("KILLED") ? 1 : 0 }),
  });

  assert.equal(total, 13);
  assert.deepEqual(failures, ["MUTANT-SYNTH-KILLED-INVERT"]);
});

test("runCorpus: a load/validation failure throws rather than silently reporting zero records", () => {
  assert.throws(() => runCorpus({ root: REPO_ROOT, dir: "test/semantic/mutants/does-not-exist" }));
});

// A corpus with zero records must never pass: an empty SEMANTIC_MUTANTS_DIR
// (a typo, an emptied directory) would otherwise print "all 0 record(s)
// matched" and exit green, exactly the vacuous-on-empty shape selectDetectors
// refuses for a record with zero detectors.
test("runCorpus: a directory with zero records throws rather than reporting all 0 matched", () => {
  const dir = mkdtempSync(join(tmpdir(), "semantic-mutants-empty-"));
  try {
    assert.throws(
      () => runCorpus({ root: REPO_ROOT, dir, spawnFn: () => ({ status: 0 }) }),
      /zero mutant records/,
    );
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
