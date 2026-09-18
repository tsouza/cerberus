// semantic-replay.mjs — routes one test/semantic/counterexamples/*.json
// record's contract entries (issue #3445) to the real, already-existing
// test/harness that replays the historical bug the record joins to its fix,
// and runs it (cerberus issue #3446).
//
// INDEPENDENT-ORACLE-ASSERTION BOUNDARY. This module's job ends at invoking
// the real target and relaying ITS verdict (exit code + failure text)
// verbatim. It never adds its own "does this output look correct" judgment
// on top. A PASS from replayCounterexample() means "the same recorded
// regression, with the same oracle assertions the original fix's test
// already carries, still passes" — it is never proof that some newly
// generated answer is independently correct. See docs in
// .github/scripts/README.md's "Semantic replay" section.
//
// ROUTING IS STRUCTURAL, NOT PROSE. A contract entry's free-text
// `replay_command` field is inconsistent prose across records, never a
// machine format, and is never parsed here. The real command is derived
// from the structured fields (`replay_test_path`'s extension/location,
// `replay_test_name`, `locator_path`) plus the target file's own
// `//go:build` line — read, never guessed or hardcoded from prose. Three
// routing kinds exist today, matching every contract entry currently in
// test/semantic/counterexamples/*.json:
//
//   go-test-direct   `replay_test_path` is a `_test.go` file whose content
//                     is NOT a table-driven spec.Walk/WalkShard corpus walk,
//                     and `replay_test_name` names (or starts with) a clean
//                     `^Test[A-Za-z0-9_]+$` function that file actually
//                     defines. Runs `go test [-tags <read-from-file>]
//                     -count=1 -run '^<Name>$' ./<dir>/...`.
//
//   go-test-fixture   Either (a) `replay_test_path` is a `_test.go` file
//                     whose content DOES call spec.Walk/WalkShard, and
//                     `locator_path` is a sibling `.txtar` fixture in the
//                     same directory, or (b) `replay_test_path` IS itself a
//                     `.txtar` fixture, in which case the companion
//                     `*_test.go` file in the same directory is located by
//                     convention (`roundtrip_chdb_test.go` if present, else
//                     the first sorted `*_test.go` whose content calls
//                     spec.Walk/WalkShard). Either way the fixture's
//                     basename (without `.txtar`, matching test/spec's own
//                     fixtureIDOfPath/Case.Name convention) becomes the
//                     `-run` pattern's subtest segment:
//                     `go test [-tags ...] -count=1
//                     -run '^<TestFunc>$/^<fixture>$' ./<dir>/...`.
//
//   compat-corpus     `locator_path` or `replay_test_path` names a file
//                     under `compatibility/<head>/`. Routes to
//                     `just compat-<ql>` (loki -> compat-logql, prometheus
//                     -> compat-promql, tempo -> compat-traceql). The
//                     loki-compliance-tester driver (and its prometheus/
//                     tempo siblings) has no per-case/per-file selector
//                     flag — confirmed by reading
//                     compatibility/loki/cmd/loki-compliance-tester's own
//                     flag.StringVar/BoolVar declarations — so this is
//                     always a whole-lane run, labeled as such.
//
// A contract entry can resolve to MORE THAN ONE mechanism: CTREX-1741's
// LOGQL-LABEL-MATCHER-REGEX-ANCHORING entry names both a spec fixture
// (`replay_test_path`, a .txtar) and a compat corpus file (`locator_path`,
// under compatibility/loki/) as two genuinely separate replay mechanisms
// for the same historical bug, and both are run and reported distinctly.
//
// This resolver is a small dispatch table over 3 real, already-understood
// cases, kept honestly extensible (one function per kind) — not a
// speculative framework for routing kinds that do not exist yet.
//
// SOURCE-FINGERPRINT MECHANISM. Before running any mechanism, this module
// recomputes a SHA-256 over the contract entry's own `locator_path` +
// `replay_test_path` bytes and compares it against the value checked in at
// `test/semantic/replay-fingerprints.json` (writeFingerprints() /
// computeAllFingerprints() below; refreshed by hand via
// `node .github/scripts/semantic-replay.mjs --update-fingerprints`, mirroring
// this repo's existing hand-refreshed snapshot precedent,
// .github/scripts/semantic-lane-policy-snapshot.mjs). A missing or
// mismatched fingerprint is reported as STALE, a distinct status from a
// real FAIL/ERROR — a stale reference means the record's own pointers have
// drifted from what was fingerprinted, not that the replayed test failed.
//
// FAIL-CLOSED CONDITIONS, each independently distinguishable via `status`
// + `reasonKind` (see REPLAY_STATUS below): an unknown counterexample id
// (caught by the CLI before any entry is processed, EXIT_CODES.UNKNOWN_ID);
// a missing/unresolvable selector (`ERROR` / `unresolved-selector`); zero
// selected tests actually executing (`ERROR` / `zero-selected`); a
// source-fingerprint mismatch (`STALE`).
//
// SUBSTRATE-UNAVAILABLE vs. REAL FAILURE. A chdb-tagged mechanism checks
// `libchdb.so` is installed (existsSync against CHDB_INSTALL_PATH, the same
// env var and default path just/chdb.just and .github/scripts/chdb-install.mjs
// already use) before running `go test`, rather than letting an uninstalled
// driver produce a confusing runtime panic. A compat-corpus mechanism checks
// `docker info` succeeds before attempting `just compat-<ql>`. Either check
// failing reports `SUBSTRATE-UNAVAILABLE`, never folded into `FAIL`/`ERROR`.
//
// NON-GOALS: no new runtime test-assertion framework (this only shells out
// to `go test` / `just <compat-recipe>`), no automatic golden updates, and
// this never becomes a required CI status check — it is a routing/reporting
// layer run by hand, like semantic-lane-policy-snapshot.mjs, not a merge
// gate. Only this module's own pure-routing unit tests
// (semantic-replay.test.mjs) are wired into CI, for test hygiene.
//
// Node builtins only.

import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

import { loadSemanticModel } from "./semantic-model.mjs";
import { loadCounterexamples } from "./semantic-counterexamples.mjs";

export const DEFAULT_FINGERPRINTS_PATH = "test/semantic/replay-fingerprints.json";
export const FINGERPRINT_SCHEMA_VERSION = 1;

// Separates the (up to) two concatenated file byte-streams a fingerprint
// hashes. A plain ASCII marker, not a control byte: a NUL in the leading
// block of a committed .mjs source file trips repo-hygiene's binary-content
// classifier (this file is source, never a build artefact). Not a sequence
// either file's own contents could plausibly contain verbatim at the seam,
// so it cannot be defeated by an edit to either file individually.
const FINGERPRINT_DELIMITER = "\n<<semantic-replay-fingerprint-delimiter>>\n";

const GO_TEST_FILE_RE = /_test\.go$/;
const FIXTURE_EXT = ".txtar";
const FIXTURE_FILE_RE = /\.txtar$/;
const BUILD_TAG_RE = /^\/\/go:build (.+)$/m;
const TABLE_DRIVEN_WALK_RE = /spec\.(?:WalkShard|Walk)\(t,/;
const ANY_TEST_FUNC_RE = /func (Test[A-Za-z0-9_]+)\(t \*testing\.T\)/;
const PREFERRED_FIXTURE_COMPANION = "roundtrip_chdb_test.go";

// Maps a compatibility/<head>/ directory to the just recipe that runs its
// differential harness. The three heads compatibility/ currently has.
const COMPAT_HEAD_RECIPES = Object.freeze({
  loki: "compat-logql",
  prometheus: "compat-promql",
  tempo: "compat-traceql",
});

export const CHDB_INSTALL_PATH_ENV = "CHDB_INSTALL_PATH";
// Mirrors just/chdb.just's own CHDB_INSTALL_PATH default.
export const DEFAULT_CHDB_INSTALL_PATH = "/usr/local/lib/libchdb.so";

export const REPLAY_STATUS = Object.freeze({
  PASS: "PASS",
  FAIL: "FAIL",
  ERROR: "ERROR",
  STALE: "STALE",
  SUBSTRATE_UNAVAILABLE: "SUBSTRATE-UNAVAILABLE",
});

// Exit-code convention for the CLI (semantic-replay.mjs), documented here
// since both the CLI and this module's tests depend on it:
//   OK           every resolved mechanism passed; nothing stale, nothing
//                substrate-unavailable, nothing erroring.
//   FAILURE      at least one mechanism genuinely FAILED, ERRORED, or was
//                STALE — a real, actionable problem.
//   UNKNOWN_ID   the counterexample id itself does not resolve to a record
//                (checked before any contract entry is processed).
//   INCONCLUSIVE no FAIL/ERROR/STALE anywhere, but at least one mechanism
//                is SUBSTRATE-UNAVAILABLE — nothing is known to be broken,
//                but nothing was conclusively verified either. Kept
//                distinct from both OK and FAILURE so a caller (human or
//                script) never mistakes "couldn't check" for "checked and
//                clean" or "checked and broken".
export const EXIT_CODES = Object.freeze({
  OK: 0,
  FAILURE: 1,
  UNKNOWN_ID: 2,
  INCONCLUSIVE: 3,
});

function escapeRegExp(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function basenameNoExt(path, ext) {
  const base = path.slice(path.lastIndexOf("/") + 1);
  return base.endsWith(ext) ? base.slice(0, -ext.length) : base;
}

function tail(text, maxChars = 4000) {
  if (!text) return "";
  return text.length <= maxChars ? text : `…(truncated)…\n${text.slice(-maxChars)}`;
}

function readBuildTag(absPath) {
  const content = readFileSync(absPath, "utf8");
  const m = content.match(BUILD_TAG_RE);
  return m ? m[1].trim() : null;
}

// findCompanionTestFile — locates the *_test.go under dirAbs that walks
// fixtures via spec.Walk/WalkShard, for a contract entry whose
// `replay_test_path` is itself a bare fixture file (CTREX-1741's LogQL
// entry). Prefers the literal, conventional `roundtrip_chdb_test.go`; falls
// back to the first (sorted) *_test.go in the directory whose content calls
// spec.Walk/WalkShard, so a differently-named companion is still found
// rather than silently unresolved.
function findCompanionTestFile(dirAbs, dirRel) {
  if (existsSync(join(dirAbs, PREFERRED_FIXTURE_COMPANION))) {
    return join(dirRel, PREFERRED_FIXTURE_COMPANION);
  }
  const candidates = readdirSync(dirAbs)
    .filter((f) => GO_TEST_FILE_RE.test(f))
    .sort();
  for (const f of candidates) {
    const content = readFileSync(join(dirAbs, f), "utf8");
    if (TABLE_DRIVEN_WALK_RE.test(content)) return join(dirRel, f);
  }
  return null;
}

// resolveGoTestMechanism — the go-test-direct / go-test-fixture half of the
// dispatch table. Returns null when `replay_test_path` is neither a
// `_test.go` file nor a `.txtar` fixture (not a go-test mechanism at all,
// e.g. a compat-only entry), or a mechanism descriptor with
// `kind: "unresolved"` when it looks like a go-test mechanism but a clean
// selector cannot be derived structurally.
export function resolveGoTestMechanism(entry, { root = process.cwd() } = {}) {
  const testPath = entry.replay_test_path;

  if (GO_TEST_FILE_RE.test(testPath)) {
    const abs = resolve(root, testPath);
    const content = readFileSync(abs, "utf8");
    const buildTag = readBuildTag(abs);
    const dirRel = dirname(testPath);

    if (TABLE_DRIVEN_WALK_RE.test(content)) {
      if (!FIXTURE_FILE_RE.test(entry.locator_path) || dirname(entry.locator_path) !== dirRel) {
        return {
          kind: "unresolved",
          reason:
            `${testPath} is a table-driven spec.Walk/WalkShard corpus test, but locator_path ` +
            `${entry.locator_path} is not a sibling ${FIXTURE_EXT} fixture in ${dirRel}`,
        };
      }
      const funcMatch = content.match(ANY_TEST_FUNC_RE);
      if (!funcMatch) {
        return { kind: "unresolved", reason: `${testPath}: no top-level func Test...(t *testing.T) found` };
      }
      return {
        kind: "go-test-fixture",
        dir: dirRel,
        testFunc: funcMatch[1],
        subtest: basenameNoExt(entry.locator_path, FIXTURE_EXT),
        buildTag,
        sourceFile: testPath,
      };
    }

    const nameMatch = entry.replay_test_name.match(/^(Test[A-Za-z0-9_]+)/);
    if (!nameMatch) {
      return {
        kind: "unresolved",
        reason: `replay_test_name ${JSON.stringify(entry.replay_test_name)} has no clean ^Test[A-Za-z0-9_]+$ prefix`,
      };
    }
    const testFunc = nameMatch[1];
    if (!new RegExp(`func ${escapeRegExp(testFunc)}\\(t \\*testing\\.T\\)`).test(content)) {
      return { kind: "unresolved", reason: `${testPath}: no func ${testFunc}(t *testing.T) found` };
    }
    return { kind: "go-test-direct", dir: dirRel, testFunc, buildTag, sourceFile: testPath };
  }

  if (FIXTURE_FILE_RE.test(testPath)) {
    const dirRel = dirname(testPath);
    const dirAbs = resolve(root, dirRel);
    const companion = findCompanionTestFile(dirAbs, dirRel);
    if (!companion) {
      return {
        kind: "unresolved",
        reason: `no *_test.go under ${dirRel} calls spec.Walk/WalkShard to replay fixture ${testPath}`,
      };
    }
    const companionAbs = resolve(root, companion);
    const content = readFileSync(companionAbs, "utf8");
    const funcMatch = content.match(ANY_TEST_FUNC_RE);
    if (!funcMatch) {
      return { kind: "unresolved", reason: `${companion}: no top-level func Test...(t *testing.T) found` };
    }
    return {
      kind: "go-test-fixture",
      dir: dirRel,
      testFunc: funcMatch[1],
      subtest: basenameNoExt(testPath, FIXTURE_EXT),
      buildTag: readBuildTag(companionAbs),
      sourceFile: companion,
    };
  }

  return null;
}

function underCompatibility(path) {
  return path.startsWith("compatibility/");
}

// resolveCompatCorpusMechanism — the compat-corpus half of the dispatch
// table. `path` is whichever of locator_path/replay_test_path lies under
// compatibility/.
export function resolveCompatCorpusMechanism(path) {
  const segments = path.split("/");
  const head = segments[1];
  const recipe = COMPAT_HEAD_RECIPES[head];
  if (!recipe) {
    return {
      kind: "unresolved",
      reason: `compatibility/${head}/ has no known just compat-<head> recipe mapping (known heads: ${Object.keys(COMPAT_HEAD_RECIPES).join(", ")})`,
    };
  }
  return { kind: "compat-corpus", recipe, wholeLane: true, sourceFile: path };
}

// resolveMechanisms — every replay mechanism a contract entry names. Always
// returns a non-empty array: an entry that names neither a go-test target
// nor a compatibility/ path resolves to a single `unresolved` mechanism
// rather than an empty list, so a caller never has to special-case "no
// mechanisms" separately from "one unresolved mechanism".
export function resolveMechanisms(entry, { root = process.cwd() } = {}) {
  const mechanisms = [];

  const goMech = resolveGoTestMechanism(entry, { root });
  if (goMech) mechanisms.push(goMech);

  const compatPath = underCompatibility(entry.locator_path)
    ? entry.locator_path
    : underCompatibility(entry.replay_test_path)
      ? entry.replay_test_path
      : null;
  if (compatPath) mechanisms.push(resolveCompatCorpusMechanism(compatPath));

  if (mechanisms.length === 0) {
    return [
      {
        kind: "unresolved",
        reason:
          `cannot derive any replay mechanism from replay_test_path=${entry.replay_test_path} ` +
          `locator_path=${entry.locator_path} — neither is a _test.go, a .txtar fixture, nor a compatibility/ path`,
      },
    ];
  }
  return mechanisms;
}

// --- fingerprints -----------------------------------------------------

export function fingerprintKey(counterexampleId, contractId) {
  return `${counterexampleId}#${contractId}`;
}

// computeEntryFingerprint — SHA-256 over a contract entry's own locator_path
// + replay_test_path bytes, in that fixed order, separated by
// FINGERPRINT_DELIMITER; when the two fields name the same path they are
// hashed once (no delimiter), not doubled.
export function computeEntryFingerprint(entry, { root = process.cwd() } = {}) {
  const paths =
    entry.locator_path === entry.replay_test_path
      ? [entry.locator_path]
      : [entry.locator_path, entry.replay_test_path];
  const hash = createHash("sha256");
  paths.forEach((p, i) => {
    if (i > 0) hash.update(FINGERPRINT_DELIMITER);
    hash.update(readFileSync(resolve(root, p)));
  });
  return hash.digest("hex");
}

export function loadFingerprints(path = DEFAULT_FINGERPRINTS_PATH, { root = process.cwd() } = {}) {
  const abs = resolve(root, path);
  if (!existsSync(abs)) return { schema_version: FINGERPRINT_SCHEMA_VERSION, fingerprints: {} };
  return JSON.parse(readFileSync(abs, "utf8"));
}

// computeAllFingerprints — recomputes the fingerprint for every
// counterexample record / contract entry pair currently committed under
// test/semantic/counterexamples/.
export function computeAllFingerprints({ root = process.cwd() } = {}) {
  const model = loadSemanticModel(undefined, { root });
  const records = loadCounterexamples(model, undefined, { root });
  const fingerprints = {};
  for (const [id, record] of records) {
    for (const entry of record.contracts) {
      fingerprints[fingerprintKey(id, entry.contract_id)] = computeEntryFingerprint(entry, { root });
    }
  }
  // A pure function of the tree — no timestamp — so two refreshes from the
  // same checkout are byte-identical and `--update-fingerprints` produces
  // a diff only when a fingerprint actually moved.
  return { schema_version: FINGERPRINT_SCHEMA_VERSION, fingerprints };
}

export function writeFingerprints(path = DEFAULT_FINGERPRINTS_PATH, { root = process.cwd() } = {}) {
  const snapshot = computeAllFingerprints({ root });
  writeFileSync(resolve(root, path), JSON.stringify(snapshot, null, 2) + "\n");
  return snapshot;
}

// --- substrate detection -----------------------------------------------

export function chdbAvailable({ installPath } = {}) {
  const path = installPath ?? process.env[CHDB_INSTALL_PATH_ENV] ?? DEFAULT_CHDB_INSTALL_PATH;
  return existsSync(path);
}

export function dockerAvailable() {
  const res = spawnSync("docker", ["info"], { stdio: "ignore" });
  return res.status === 0;
}

// --- execution -----------------------------------------------------------

function goTestTarget(mechanism) {
  return mechanism.kind === "go-test-fixture" ? `${mechanism.testFunc}/${mechanism.subtest}` : mechanism.testFunc;
}

export function buildGoTestInvocation(mechanism) {
  const args = ["test"];
  if (mechanism.buildTag) args.push("-tags", mechanism.buildTag);
  args.push("-count=1");
  const runPattern =
    mechanism.kind === "go-test-fixture"
      ? `^${escapeRegExp(mechanism.testFunc)}$/^${escapeRegExp(mechanism.subtest)}$`
      : `^${escapeRegExp(mechanism.testFunc)}$`;
  args.push("-run", runPattern, "-v", `./${mechanism.dir}/...`);
  return { cmd: "go", args };
}

function commandString(cmd, args) {
  return [cmd, ...args].join(" ");
}

export function isChdbTag(buildTag) {
  return typeof buildTag === "string" && buildTag.split(/&&|\|\|/).map((s) => s.trim()).includes("chdb");
}

// runMechanism — executes one resolved mechanism and relays the real
// target's verdict verbatim: exit code + failure text, never a synthesized
// correctness judgment. See this file's header for the
// independent-oracle-assertion boundary.
export function runMechanism(mechanism, { root = process.cwd() } = {}) {
  if (mechanism.kind === "unresolved") {
    return { status: REPLAY_STATUS.ERROR, reasonKind: "unresolved-selector", detail: mechanism.reason };
  }

  if (mechanism.kind === "compat-corpus") {
    const command = `just ${mechanism.recipe}`;
    if (!dockerAvailable()) {
      return {
        status: REPLAY_STATUS.SUBSTRATE_UNAVAILABLE,
        reasonKind: "docker-unavailable",
        detail: `docker is not reachable (docker info failed) — cannot bring up the ${mechanism.recipe} compose stack`,
        command,
        wholeLane: true,
      };
    }
    const res = spawnSync("just", [mechanism.recipe], { cwd: root, encoding: "utf8" });
    const output = tail(`${res.stdout ?? ""}\n${res.stderr ?? ""}`);
    if (res.status !== 0) {
      return {
        status: REPLAY_STATUS.FAIL,
        reasonKind: "harness-nonzero-exit",
        detail: `${command} exited ${res.status}\n${output}`,
        command,
        wholeLane: true,
      };
    }
    return {
      status: REPLAY_STATUS.PASS,
      reasonKind: "ok",
      detail:
        `${command} completed (exit 0). Whole-lane run: the loki-compliance-tester driver (and its ` +
        `prometheus/tempo siblings) has no per-case selector, so this exercises the full corpus, not ` +
        `just this record's case. The driver is also report-only by design (issue #68) — a 0 exit means ` +
        `the harness ran to completion, not that every case matched; inspect its own report for the ` +
        `specific case named by ${mechanism.sourceFile}.`,
      command,
      wholeLane: true,
    };
  }

  // go-test-direct / go-test-fixture
  const { cmd, args } = buildGoTestInvocation(mechanism);
  const command = commandString(cmd, args);

  if (isChdbTag(mechanism.buildTag) && !chdbAvailable()) {
    const installPath = process.env[CHDB_INSTALL_PATH_ENV] ?? DEFAULT_CHDB_INSTALL_PATH;
    return {
      status: REPLAY_STATUS.SUBSTRATE_UNAVAILABLE,
      reasonKind: "chdb-not-installed",
      detail: `libchdb.so not found at ${installPath} — run 'just chdb-install' first`,
      command,
    };
  }

  const res = spawnSync(cmd, args, { cwd: root, encoding: "utf8" });
  const output = `${res.stdout ?? ""}\n${res.stderr ?? ""}`;

  if (/\[build failed\]/.test(output) || /^# /m.test(output)) {
    return { status: REPLAY_STATUS.ERROR, reasonKind: "build-failed", detail: tail(output), command };
  }

  const target = goTestTarget(mechanism);
  const passRe = new RegExp(`--- PASS: ${escapeRegExp(target)}\\b`);
  const failRe = new RegExp(`--- FAIL: ${escapeRegExp(target)}\\b`);
  const passed = passRe.test(output);
  const failed = failRe.test(output);

  if (!passed && !failed) {
    return {
      status: REPLAY_STATUS.ERROR,
      reasonKind: "zero-selected",
      detail: `zero selected tests executed — '-run' pattern matched nothing for ${target}\n${tail(output)}`,
      command,
    };
  }
  if (failed) {
    return { status: REPLAY_STATUS.FAIL, reasonKind: "test-failed", detail: tail(output), command };
  }
  return { status: REPLAY_STATUS.PASS, reasonKind: "ok", detail: `${target} passed`, command };
}

// --- top-level orchestration ---------------------------------------------

export function loadCounterexampleRecord(id, { root = process.cwd() } = {}) {
  const model = loadSemanticModel(undefined, { root });
  const records = loadCounterexamples(model, undefined, { root });
  return records.get(id) ?? null;
}

// checkEntryFingerprint — pure fingerprint-freshness check for one contract
// entry against an already-loaded `stored` fingerprints map (as returned by
// loadFingerprints().fingerprints). Returns null when the recorded
// fingerprint matches the live one (safe to run); otherwise returns the
// STALE mechanism result to report instead of running anything, so a stale
// pointer is never conflated with a real test failure or a substrate gap.
export function checkEntryFingerprint(counterexampleId, entry, stored, { root = process.cwd() } = {}) {
  const key = fingerprintKey(counterexampleId, entry.contract_id);
  const storedHash = stored[key];
  const liveHash = computeEntryFingerprint(entry, { root });

  if (storedHash === undefined) {
    return {
      key,
      status: "missing",
      result: {
        status: REPLAY_STATUS.STALE,
        reasonKind: "fingerprint-missing",
        detail: `no recorded fingerprint for ${key} — run 'node .github/scripts/semantic-replay.mjs --update-fingerprints'`,
      },
    };
  }
  if (storedHash !== liveHash) {
    return {
      key,
      status: "mismatch",
      result: {
        status: REPLAY_STATUS.STALE,
        reasonKind: "fingerprint-mismatch",
        detail:
          `recorded fingerprint ${storedHash} does not match the live ${liveHash} for ${key} — ` +
          `locator_path/replay_test_path content changed since the fingerprint was last refreshed. ` +
          `Confirm the drift is intentional, then run --update-fingerprints.`,
      },
    };
  }
  return { key, status: "ok", result: null };
}

// replayCounterexample — the whole per-record flow: load the record,
// fingerprint-check then route+run every contract entry's mechanism(s).
// Never throws on a routing/execution problem — those are reported as
// ERROR/STALE/SUBSTRATE-UNAVAILABLE results, not exceptions; an actual
// exception means the counterexample records or semantic model themselves
// are invalid, which `just semantic-check` already guards.
export function replayCounterexample(id, { root = process.cwd() } = {}) {
  const record = loadCounterexampleRecord(id, { root });
  if (!record) return { id, found: false, entries: [] };

  const stored = loadFingerprints(undefined, { root }).fingerprints ?? {};

  const entries = record.contracts.map((entry) => {
    const fp = checkEntryFingerprint(id, entry, stored, { root });
    if (fp.result) {
      return { contractId: entry.contract_id, fingerprintKey: fp.key, fingerprintStatus: fp.status, mechanisms: [fp.result] };
    }

    const mechanisms = resolveMechanisms(entry, { root }).map((mechanism) => ({
      mechanism,
      ...runMechanism(mechanism, { root }),
    }));
    return { contractId: entry.contract_id, fingerprintKey: fp.key, fingerprintStatus: fp.status, mechanisms };
  });

  return { id, found: true, entries };
}

export function classifyOverallExit(result) {
  if (!result.found) return EXIT_CODES.UNKNOWN_ID;
  const all = result.entries.flatMap((e) => e.mechanisms);
  const hasRealFailure = all.some(
    (m) => m.status === REPLAY_STATUS.FAIL || m.status === REPLAY_STATUS.ERROR || m.status === REPLAY_STATUS.STALE,
  );
  if (hasRealFailure) return EXIT_CODES.FAILURE;
  if (all.some((m) => m.status === REPLAY_STATUS.SUBSTRATE_UNAVAILABLE)) return EXIT_CODES.INCONCLUSIVE;
  return EXIT_CODES.OK;
}
