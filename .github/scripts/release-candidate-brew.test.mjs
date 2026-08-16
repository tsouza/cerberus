import assert from "node:assert/strict";
import { test } from "node:test";

import {
  archiveDeliveryProblems,
  brewUpdateExecutionProblem,
  brewCommandEnvironment,
  candidateArchiveRoutes,
  candidateTapPlan,
  commandOnPath,
  commandResultSucceeded,
  hostTarget,
  installedCandidateProblems,
  migrationCandidateProblems,
  routeCandidateArchive,
  runCommand,
  withoutArtifactDomain,
} from "./release-candidate-brew.mjs";

const VERSION = "2.4.6";
const TAG = `v${VERSION}`;
const SOURCE_URL = "https://github.com/example/project";
const ROOT = "/tmp/candidate-brew-unit";
const TARGETS = ["darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64"];
const SHORT_COMMAND_TIMEOUT_MS = 100;
const COMMAND_BOUND_ASSERTION_MS = 4_000;
const SMALL_OUTPUT_LIMIT_BYTES = 32;
const COMMAND_NOT_STARTED_STATUS = 127;

function manifest() {
  return {
    source: { url: SOURCE_URL },
    versions: { app: VERSION, app_tag: TAG },
    files: TARGETS.map((target) => {
      const [goos, goarch] = target.split("/");
      return { path: `app/assets/cerberus_${VERSION}_${goos}_${goarch}.tar.gz` };
    }),
  };
}

function healthyInstall(overrides = {}) {
  const prefix = "/opt/brew";
  return {
    version: VERSION,
    caskList: `cerberus ${VERSION}\n`,
    formulaList: "",
    allowFormula: false,
    brewPrefix: prefix,
    resolvedPath: `${prefix}/bin/cerberus`,
    pathCommand: `${prefix}/bin/cerberus`,
    realPath: `${prefix}/Caskroom/cerberus/${VERSION}/cerberus`,
    reportedVersion: VERSION,
    schemaStatus: 0,
    schemaOutput: "CREATE TABLE telemetry",
    docsStatus: 0,
    ...overrides,
  };
}

test("archive routing retains the complete candidate source and release binding", () => {
  const routes = candidateArchiveRoutes({ manifest: manifest(), candidateRoot: ROOT });
  assert.deepEqual(routes.map((route) => route.target), TARGETS);
  const linux = routes.find((route) => route.target === "linux/amd64");
  assert.equal(linux.originalUrl, `${SOURCE_URL}/releases/download/${TAG}/cerberus_${VERSION}_linux_amd64.tar.gz`);
  assert.deepEqual(linux.paths, [
    `/${linux.originalUrl}`,
    `/example/project/releases/download/${TAG}/cerberus_${VERSION}_linux_amd64.tar.gz`,
    `/github.com/example/project/releases/download/${TAG}/cerberus_${VERSION}_linux_amd64.tar.gz`,
  ]);
  assert.equal(linux.absolutePath, `${ROOT}/app/assets/cerberus_${VERSION}_linux_amd64.tar.gz`);
});

test("archive routing rejects a manifest without every sealed platform archive", () => {
  const broken = manifest();
  broken.files.pop();
  assert.throws(
    () => candidateArchiveRoutes({ manifest: broken, candidateRoot: ROOT }),
    /does not declare app\/assets\/cerberus_2\.4\.6_linux_arm64\.tar\.gz/,
  );
});

test("the pure request router accepts only exact body and metadata requests", () => {
  const routes = candidateArchiveRoutes({ manifest: manifest(), candidateRoot: ROOT });
  const route = routes[2];
  for (const path of route.paths) {
    const get = routeCandidateArchive({ method: "GET", requestUrl: path, routes });
    assert.equal(get.status, 200, path);
    assert.equal(get.archive.target, "linux/amd64");
    assert.equal(get.sendBody, true);

    const head = routeCandidateArchive({ method: "HEAD", requestUrl: path, routes });
    assert.equal(head.status, 200, path);
    assert.equal(head.sendBody, false);
  }
  assert.equal(routeCandidateArchive({ method: "POST", requestUrl: route.paths[0], routes }).status, 405);
  assert.equal(routeCandidateArchive({ method: "GET", requestUrl: `${route.paths[0]}?other=1`, routes }).status, 400);
  assert.equal(routeCandidateArchive({ method: "GET", requestUrl: "/cerberus_2.4.6_linux_amd64.tar.gz", routes }).status, 404);
  assert.equal(routeCandidateArchive({ method: "GET", requestUrl: "http://elsewhere.invalid/file", routes }).status, 400);
});

test("the tap plan preserves exact cask bytes and models a real deletion", () => {
  const cask = Buffer.from("cask bytes\u0000remain exact\n");
  const plan = candidateTapPlan({
    tapName: "candidate-local/qualification",
    caskBytes: cask,
    legacyArchiveUrl: "file:///tmp/legacy-source.tar.gz",
    legacyArchiveSha256: "a".repeat(64),
  });
  assert.deepEqual(plan.legacy.removes, []);
  assert.equal(plan.legacy.writes[0].path, "Formula/cerberus.rb");
  assert.match(plan.legacy.writes[0].bytes.toString(), /version "0\.0\.0"/);
  assert.deepEqual(plan.candidate.removes, ["Formula/cerberus.rb"]);
  assert.equal(plan.candidate.writes[0].path, "Casks/cerberus.rb");
  assert.deepEqual(plan.candidate.writes[0].bytes, cask);
  assert.equal(
    plan.candidate.writes[1].bytes.toString(),
    '{"cerberus":"candidate-local/qualification"}\n',
  );
});

test("the tap plan rejects unsafe names, mutable cask text, and remote legacy payloads", () => {
  const valid = {
    tapName: "candidate-local/qualification",
    caskBytes: Buffer.from("candidate"),
    legacyArchiveUrl: "file:///tmp/legacy-source.tar.gz",
    legacyArchiveSha256: "b".repeat(64),
  };
  assert.throws(() => candidateTapPlan({ ...valid, tapName: "../escape" }), /invalid ephemeral tap name/);
  assert.throws(() => candidateTapPlan({ ...valid, caskBytes: "candidate" }), /cask bytes are required/);
  assert.throws(() => candidateTapPlan({ ...valid, legacyArchiveUrl: "https://invalid.example/file" }), /file URL/);
});

test("the brew environment makes loopback authoritative and isolates downloads", () => {
  const env = brewCommandEnvironment(
    {
      PATH: "/bin",
      HOMEBREW_ARTIFACT_DOMAIN: "https://invalid.example",
      HOMEBREW_CACHE: "/wrong",
    },
    { artifactOrigin: "http://127.0.0.1:43123", stateRoot: "/tmp/qualification-state" },
  );
  assert.equal(env.PATH, "/bin");
  assert.equal(env.HOMEBREW_ARTIFACT_DOMAIN, "http://127.0.0.1:43123");
  assert.equal(env.HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK, "1");
  assert.equal(env.HOMEBREW_CACHE, "/tmp/qualification-state/cache");
  assert.equal(env.HOMEBREW_NO_AUTO_UPDATE, "1");
  const setup = withoutArtifactDomain(env);
  assert.equal(setup.HOMEBREW_ARTIFACT_DOMAIN, undefined);
  assert.equal(setup.HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK, undefined);
  assert.equal(setup.HOMEBREW_CACHE, "/tmp/qualification-state/cache");
  assert.equal(env.HOMEBREW_ARTIFACT_DOMAIN, "http://127.0.0.1:43123");
  assert.throws(
    () => brewCommandEnvironment({}, { artifactOrigin: "http://0.0.0.0:43123", stateRoot: "/tmp/state" }),
    /loopback origin/,
  );
  assert.throws(
    () => brewCommandEnvironment({}, { artifactOrigin: "https://127.0.0.1:43123", stateRoot: "/tmp/state" }),
    /loopback origin/,
  );
});

test("host target selection is exact and closed", () => {
  assert.equal(hostTarget("linux", "x64"), "linux/amd64");
  assert.equal(hostTarget("linux", "arm64"), "linux/arm64");
  assert.equal(hostTarget("darwin", "x64"), "darwin/amd64");
  assert.equal(hostTarget("darwin", "arm64"), "darwin/arm64");
  assert.throws(() => hostTarget("win32", "x64"), /unsupported Homebrew qualification host/);
});

test("PATH resolution is ordered, executable-only, and shell-free", () => {
  assert.equal(commandOnPath("sh", "/definitely-absent:/bin"), "/bin/sh");
  assert.equal(commandOnPath("not-present-anywhere", "/bin:/usr/bin"), "");
  assert.equal(commandOnPath("../sh", "/bin"), "");
});

test("command success rejects deadline, output-cap, and signal results even with exit zero", () => {
  const healthy = { status: 0, signal: null, timedOut: false, overflow: false };
  assert.equal(commandResultSucceeded(healthy), true);
  assert.equal(commandResultSucceeded({ ...healthy, timedOut: true }), false);
  assert.equal(commandResultSucceeded({ ...healthy, overflow: true }), false);
  assert.equal(commandResultSucceeded({ ...healthy, signal: "SIGTERM" }), false);
});

test("the update allowance accepts ordinary non-zero exits but rejects incomplete execution", () => {
  const ordinaryTapFailure = { status: 1, signal: null, timedOut: false, overflow: false };
  assert.equal(brewUpdateExecutionProblem(ordinaryTapFailure), null);
  assert.match(
    brewUpdateExecutionProblem({ ...ordinaryTapFailure, signal: "SIGTERM" }),
    /brew update failed/,
  );
  assert.match(brewUpdateExecutionProblem({ ...ordinaryTapFailure, timedOut: true }), /brew update failed/);
  assert.match(brewUpdateExecutionProblem({ ...ordinaryTapFailure, overflow: true }), /brew update failed/);
  assert.match(
    brewUpdateExecutionProblem({ ...ordinaryTapFailure, status: COMMAND_NOT_STARTED_STATUS }),
    /brew update failed/,
  );
});

test("the command deadline kills descendants that retain the output pipes", async () => {
  const started = Date.now();
  const result = await runCommand("sh", ["-c", "sleep 30 & wait"], { timeout: SHORT_COMMAND_TIMEOUT_MS });
  assert.equal(result.timedOut, true);
  assert.equal(commandResultSucceeded(result), false);
  assert.ok(Date.now() - started < COMMAND_BOUND_ASSERTION_MS, "descendant-held pipes exceeded the hard bound");
});

test("the output cap terminates a command that exits cleanly on SIGTERM", async () => {
  const child = "trap 'exit 0' TERM; printf '%4096s' x; while :; do sleep 30; done";
  const result = await runCommand("sh", ["-c", child], {
    timeout: COMMAND_BOUND_ASSERTION_MS,
    maxOutput: SMALL_OUTPUT_LIMIT_BYTES,
  });
  assert.equal(result.overflow, true);
  assert.equal(commandResultSucceeded(result), false);
});

test("fresh-install verdict binds artifact kind, link target, version, and payloads", () => {
  assert.deepEqual(installedCandidateProblems(healthyInstall()), []);
  const problems = installedCandidateProblems(
    healthyInstall({
      caskList: "",
      formulaList: `cerberus ${VERSION}\n`,
      resolvedPath: "/usr/local/bin/cerberus",
      pathCommand: "/tmp/other/cerberus",
      realPath: "/opt/brew/Cellar/cerberus/2.4.5/bin/cerberus",
      reportedVersion: "2.4.60",
      schemaOutput: "",
      docsStatus: 1,
    }),
  );
  assert.equal(problems.length, 8);
  assert.match(problems.join("\n"), /not installed/);
  assert.match(problems.join("\n"), /formula/);
  assert.match(problems.join("\n"), /outside/);
  assert.match(problems.join("\n"), /PATH/);
  assert.match(problems.join("\n"), /Caskroom/);
  assert.match(problems.join("\n"), /2\.4\.60/);
  assert.match(problems.join("\n"), /CREATE/);
  assert.match(problems.join("\n"), /config registry/);
});

test("migration verdict requires the update announcement and candidate link", () => {
  const healthy = healthyInstall({
    allowFormula: true,
    formulaList: "cerberus 0.0.0\n",
    updateOutput: "candidate-local/qualification/cerberus has been migrated from a formula to a cask",
  });
  assert.deepEqual(migrationCandidateProblems(healthy), []);
  const problems = migrationCandidateProblems({ ...healthy, updateOutput: "Already up-to-date", formulaList: "" });
  assert.equal(problems.length, 1);
  assert.match(problems[0], /did not announce/);
});

test("archive delivery requires a successful body fetch of the host archive", () => {
  const healthy = [
    { method: "HEAD", status: 200, target: "linux/amd64", bytes: 0 },
    { method: "GET", status: 200, target: "linux/amd64", bytes: 2048 },
  ];
  assert.deepEqual(archiveDeliveryProblems({ events: healthy, expectedTarget: "linux/amd64" }), []);
  assert.equal(
    archiveDeliveryProblems({ events: healthy.slice(0, 1), expectedTarget: "linux/amd64" }).length,
    1,
  );
  const rejected = [...healthy, { method: "GET", status: 404, target: "", bytes: 0 }];
  assert.equal(archiveDeliveryProblems({ events: rejected, expectedTarget: "linux/amd64" }).length, 1);
  assert.equal(
    archiveDeliveryProblems({ events: healthy, expectedTarget: "darwin/arm64" }).length,
    1,
  );
});
