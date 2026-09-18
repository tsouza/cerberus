#!/usr/bin/env node
// semantic-replay.mjs — CLI for replaying one
// test/semantic/counterexamples/*.json record's contract entries against
// their real target test/harness (cerberus issue #3446, building on #3445).
// All routing/fingerprint/execution logic lives in
// .github/scripts/lib/semantic-replay.mjs; see that file's header for the
// routing kinds, the fingerprint mechanism, and the independent-oracle-
// assertion boundary this CLI never crosses.
//
// Usage:
//   node .github/scripts/semantic-replay.mjs <counterexample-id>
//
// `just semantic-replay id:` wraps it; `just semantic-replay-repin`
// (semantic-repin.mjs) regenerates test/semantic/replay-fingerprints.json.
//
// Exit codes (lib/semantic-replay.mjs's EXIT_CODES, repeated here so this
// is discoverable from `--help`/a misuse without reading the library):
//   0  every resolved mechanism passed.
//   1  at least one mechanism genuinely FAILED, ERRORED, or was STALE
//      (a stale region-fingerprint reference) — a real, actionable problem.
//   2  the counterexample id does not resolve to a record.
//   3  nothing FAILED/ERRORED/STALE, but at least one mechanism was
//      SUBSTRATE-UNAVAILABLE (e.g. libchdb.so not installed, docker
//      unreachable) — inconclusive, never conflated with a clean pass.
//
// This tool is a routing/reporting layer run by hand, like
// semantic-lane-policy-snapshot.mjs — it never becomes a required CI status
// check. Only its own pure-routing unit tests (semantic-replay.test.mjs)
// are wired into CI.

import process from "node:process";

import {
  EXIT_CODES,
  REPLAY_STATUS,
  classifyOverallExit,
  replayCounterexample,
} from "./lib/semantic-replay.mjs";

function indent(text, prefix = "      ") {
  return text
    .split("\n")
    .map((line) => `${prefix}${line}`)
    .join("\n");
}

function printResult(result) {
  if (!result.found) {
    console.log(`::error::semantic-replay: unknown counterexample id ${JSON.stringify(result.id)}`);
    return;
  }
  console.log(`semantic-replay: ${result.id}`);
  for (const entry of result.entries) {
    console.log(`  contract ${entry.contractId} (fingerprint: ${entry.fingerprintStatus})`);
    for (const m of entry.mechanisms) {
      const commandSuffix = m.command ? ` [${m.command}]` : "";
      console.log(`    ${m.status} (${m.reasonKind ?? "n/a"})${commandSuffix}`);
      if (m.status !== REPLAY_STATUS.PASS && m.detail) {
        console.log(indent(m.detail));
      }
    }
  }
}

function main() {
  const args = process.argv.slice(2);
  const id = args.find((a) => !a.startsWith("--"));
  if (!id) {
    console.log("::error::semantic-replay: usage: node .github/scripts/semantic-replay.mjs <counterexample-id>");
    process.exit(EXIT_CODES.FAILURE);
  }

  const result = replayCounterexample(id);
  printResult(result);
  process.exit(classifyOverallExit(result));
}

main();
