// semantic-mutation-corpus.mjs — runs `just semantic-mutate <id>` for real,
// for EVERY record committed under test/semantic/mutants/, in a Go-equipped
// CI job.
//
// WHY THIS EXISTS. Before this script, the `-overlay` argv line
// (`lib/semantic-mutation.mjs`'s runGoTest()) had zero test coverage and zero
// CI execution: no `node --test` case ever called it (every runMutant()
// test injects a fake runGoTestFn precisely so it needs no Go toolchain —
// see semantic-mutation.test.mjs's own header), and the `forbid-skip` job
// that runs that self-test installs no Go toolchain at all. Deleting the
// `-overlay=...` argument entirely left every test green — an ineffective
// overlay and a genuine `survived` were indistinguishable to anything that
// actually ran. This script is what closes that gap: it is the one place a
// real `go test -overlay` invocation, against the real committed corpus, is
// exercised on every PR.
//
// Loads every record the same way the CLI does (lib/semantic-mutation.mjs's
// loadMutants()) and shells out to `just semantic-mutate <id>` for each, so
// this checks the exact command a developer runs by hand — never a
// duplicated raw invocation of semantic-mutation.mjs that could drift from
// the just recipe. Runner-scale cost: the currently-committed corpus is 13
// records (7 synthetic + 6 real, four of them chdb-tagged — issue #3449's
// counter-reset mutant and both #3451 TraceQL mutants need a live
// ClickHouse via libchdb.so, which ci.yml's "Install libchdb.so" step
// installs immediately before this script runs); each completes in low
// single-digit seconds (the slowest, MUTANT-SYNTH-TIMEOUT-LOOP, is bounded
// by its own declared timeout_seconds), well under a minute total.
//
// Env: SEMANTIC_MUTANTS_DIR (optional; default test/semantic/mutants),
// JUST_BIN (optional; default `just`).
// Exit: 0 when every record's `just semantic-mutate` run exits 0
// (observed classification matches its own declared expected_detection);
// 1 on the first one that does not, on a load/validation failure, or on a
// corpus directory holding zero records (never a vacuous "all 0 matched").

import { spawnSync } from "node:child_process";
import process from "node:process";
import { pathToFileURL } from "node:url";

import { DEFAULT_MUTANTS_DIR, loadMutants } from "./lib/semantic-mutation.mjs";
import { error, group, notice } from "./lib/gh.mjs";

// spawnFn defaults to node:child_process's real spawnSync and is injectable
// so this orchestration can be unit-tested (record discovery, sorting,
// pass/fail aggregation) without actually shelling out to `just`/`go`.
export function runCorpus({
  root = process.cwd(),
  dir = DEFAULT_MUTANTS_DIR,
  justBin = "just",
  spawnFn = spawnSync,
} = {}) {
  const records = loadMutants(dir, { root });
  if (records.size === 0) {
    // Mirrors selectDetectors' refusal to measure with nothing to detect:
    // an empty SEMANTIC_MUTANTS_DIR (a typo, an emptied directory) would
    // otherwise report "all 0 record(s) matched" and exit green.
    throw new Error(`semantic-mutation-corpus: ${dir} holds zero mutant records — refusing to report a vacuous pass`);
  }
  const failures = [];
  for (const id of [...records.keys()].sort()) {
    const result = group(`just semantic-mutate ${id}`, () =>
      spawnFn(justBin, ["semantic-mutate", id], { cwd: root, stdio: "inherit" }),
    );
    if (result.status !== 0) failures.push(id);
  }
  return { total: records.size, failures };
}

function main() {
  const root = process.cwd();
  const dir = process.env.SEMANTIC_MUTANTS_DIR || DEFAULT_MUTANTS_DIR;
  const justBin = process.env.JUST_BIN || "just";

  const { total, failures } = runCorpus({ root, dir, justBin });

  if (failures.length > 0) {
    error(
      `semantic-mutation-corpus: ${failures.length}/${total} record(s) did not match their own ` +
        `expected_detection: ${failures.join(", ")}`,
    );
    process.exit(1);
  }
  notice(`semantic-mutation-corpus: all ${total} record(s) matched their own expected_detection`);
}

if (process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main();
}
