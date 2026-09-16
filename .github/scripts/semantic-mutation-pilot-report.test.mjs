// semantic-mutation-pilot-report.test.mjs — node --test guard for the
// scheduled, informational semantic-mutation pilot lane's own orchestration
// (record discovery scoped to real records, ledger-entry shape, mismatch
// counting), via an injected runMutantFn — never a real `go test -overlay`,
// mirroring semantic-mutation-corpus.test.mjs's own discipline for the
// REQUIRED corpus lane.

import assert from "node:assert/strict";
import { test } from "node:test";

import { runPilot } from "./semantic-mutation-pilot-report.mjs";
import { DEFAULT_MUTANTS_DIR } from "./lib/semantic-mutation.mjs";

const REPO_ROOT = process.cwd();

test("runPilot: only REAL (non-synthetic) records are attempted, in sorted id order", async () => {
  const attempted = [];
  const { total, mismatches, entries } = await runPilot({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    env: {},
    runMutantFn: async ({ record }) => {
      attempted.push(record.id);
      return { status: record.expected_detection, mutant_runs: [{ detector: "d", classification: record.expected_detection }] };
    },
  });

  assert.equal(total, 6, "exactly the six real, non-synthetic records");
  assert.equal(mismatches, 0);
  assert.deepEqual(attempted, [...attempted].sort(), "ids must be attempted in sorted order");
  assert.ok(attempted.every((id) => !id.startsWith("MUTANT-SYNTH-")), "no synthetic record is ever attempted");
  assert.equal(entries.length, 6);
});

test("runPilot: a mismatch is counted but never thrown — informational, never a hard failure", async () => {
  const { mismatches } = await runPilot({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    env: {},
    runMutantFn: async () => ({ status: "survived", mutant_runs: [] }),
  });
  // Every real record declares expected_detection "killed" today, so
  // forcing every observed result to "survived" must disagree on all six.
  assert.equal(mismatches, 6);
});

test("runPilot: each ledger entry carries the full schema loadMutantExecutions expects", async () => {
  const { entries } = await runPilot({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    env: {
      GITHUB_SHA: "a".repeat(40),
      GITHUB_SERVER_URL: "https://github.example",
      GITHUB_REPOSITORY: "tsouza/cerberus",
      GITHUB_RUN_ID: "12345",
    },
    runMutantFn: async ({ record }) => ({
      status: record.expected_detection,
      mutant_runs: [{ detector: "example-detector", classification: record.expected_detection }],
    }),
  });

  for (const entry of entries) {
    assert.match(entry.id, /^MUTEXEC-[A-Z][A-Z0-9-]*$/);
    assert.match(entry.mutant, /^MUTANT-/);
    assert.equal(entry.source_sha, "a".repeat(40));
    assert.equal(entry.run_ref, "https://github.example/tsouza/cerberus/actions/runs/12345");
    assert.ok(Array.isArray(entry.detectors));
    for (const d of entry.detectors) {
      assert.ok(typeof d.id === "string");
      assert.ok(typeof d.classification === "string");
    }
  }
});

test("runPilot: with no GitHub Actions env, source_sha is null and run_ref reports a readable placeholder", async () => {
  const { entries } = await runPilot({
    root: REPO_ROOT,
    dir: DEFAULT_MUTANTS_DIR,
    env: {},
    runMutantFn: async ({ record }) => ({ status: record.expected_detection, mutant_runs: [] }),
  });
  assert.equal(entries[0].source_sha, null);
  assert.match(entries[0].run_ref, /no run_ref available/);
});

test("runPilot: a load/validation failure throws rather than silently reporting zero records", async () => {
  await assert.rejects(() => runPilot({ root: REPO_ROOT, dir: "test/semantic/mutants/does-not-exist" }));
});
