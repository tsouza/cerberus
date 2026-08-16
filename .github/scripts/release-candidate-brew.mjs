// release-candidate-brew.mjs — qualify the sealed Homebrew candidate before
// any public release or tap write.
//
// Modes (MODE or argv[2]):
//   install    install the exact candidate cask from an ephemeral local tap.
//   migration  install a legacy formula, advance the tap by one commit through
//              `brew update`, and prove that Homebrew migrates and links the
//              exact candidate cask.
//
// Environment:
//   RELEASE_CANDIDATE_DIR     sealed candidate root.
//   RELEASE_CANDIDATE_DIGEST  expected sha256:<hex> candidate digest.
//   REPO_ROOT                 checkout bound to the candidate source; used by
//                             the shipped config-registry payload assertion.
//
// Candidate archives are never published for this check. The unmodified cask
// is installed with HOMEBREW_ARTIFACT_DOMAIN pointed at a loopback-only HTTP
// server. Fallback is disabled, the Homebrew cache is isolated, and a successful
// body download of the platform archive is mandatory. Imports are Node builtins
// plus repository-local validation helpers; every subprocess has a deadline.

import { spawn } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import {
  accessSync,
  chmodSync,
  constants as fsConstants,
  existsSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { delimiter, dirname, isAbsolute, join, relative, resolve, sep } from "node:path";
import process from "node:process";
import { fileURLToPath, pathToFileURL } from "node:url";

import { error, log, notice } from "./lib/gh.mjs";
import { REQUIRED_TARGETS, verifyCandidate } from "./release-candidate.mjs";

const APP_NAME = "cerberus";
const CASK_PATH = `Casks/${APP_NAME}.rb`;
const FORMULA_PATH = `Formula/${APP_NAME}.rb`;
const MIGRATIONS_PATH = "tap_migrations.json";
const CANDIDATE_DIR_DEFAULT = "build/release-candidate";
const LEGACY_VERSION = "0.0.0";
const LEGACY_REPORTED_VERSION = "legacy-candidate-fixture";
const MIGRATION_ANNOUNCEMENT = "has been migrated from a formula to a cask";
const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
const TAP_RE = /^[a-z0-9][a-z0-9-]*\/[a-z0-9][a-z0-9-]*$/;
const COMMAND_TIMEOUT_MS = 5 * 60 * 1000;
const BREW_INSTALL_TIMEOUT_MS = 20 * 60 * 1000;
const BREW_UPDATE_TIMEOUT_MS = 20 * 60 * 1000;
const CLEANUP_TIMEOUT_MS = 5 * 60 * 1000;
const SERVER_CLOSE_TIMEOUT_MS = 10 * 1000;
const COMMAND_KILL_GRACE_MS = 2 * 1000;
const MAX_COMMAND_OUTPUT_BYTES = 64 * 1024 * 1024;
const EXECUTABLE_MODE = 0o755;
const TAP_TOKEN_HEX_LENGTH = 12;
const COMMAND_NOT_STARTED_STATUS = 127;
const HTTP_OK = 200;
const HTTP_BAD_REQUEST = 400;
const HTTP_NOT_FOUND = 404;
const HTTP_METHOD_NOT_ALLOWED = 405;
const HTTP_SERVER_ERROR = 500;

export class CandidateBrewError extends Error {
  constructor(problems) {
    const list = Array.isArray(problems) ? problems : [String(problems)];
    super(`candidate Homebrew qualification failed:\n${list.map((item) => `- ${item}`).join("\n")}`);
    this.name = "CandidateBrewError";
    this.problems = list;
  }
}

function fail(message) {
  throw new CandidateBrewError(message);
}

function targetKey(target) {
  return `${target.goos}/${target.goarch}`;
}

function within(root, path) {
  const rel = relative(root, path);
  return rel === "" || (rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel));
}

function safeWorkspacePath(root, path) {
  if (typeof path !== "string" || path === "" || path.includes("\0") || isAbsolute(path)) {
    fail(`unsafe workspace-relative path ${JSON.stringify(path)}`);
  }
  const absolute = resolve(root, path);
  if (!within(resolve(root), absolute) || absolute === resolve(root)) {
    fail(`workspace path escapes its root: ${JSON.stringify(path)}`);
  }
  return absolute;
}

function sha256Hex(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}

function rubyString(value) {
  return JSON.stringify(String(value));
}

function requiredString(value, label) {
  if (typeof value !== "string" || value.trim() === "") fail(`${label} is required`);
  return value.trim();
}

/**
 * Return every exact request path used by supported artifact-domain rewrite
 * shapes. Every path retains the complete source and release binding; basename-only
 * routing would let a cask pointing at the wrong release pass.
 */
export function candidateArchiveRoutes({ manifest, candidateRoot }) {
  if (!manifest?.source?.url || !manifest?.versions?.app || !manifest?.versions?.app_tag) {
    fail("candidate manifest lacks source URL, app version, or app tag");
  }
  let source;
  try {
    source = new URL(manifest.source.url);
  } catch (cause) {
    fail(`candidate source URL is invalid: ${cause.message}`);
  }
  if (source.protocol !== "https:" || source.search || source.hash || source.pathname === "/") {
    fail("candidate source URL must be a repository-scoped HTTPS URL without query or fragment");
  }

  const root = resolve(candidateRoot);
  const declared = new Set((manifest.files ?? []).map((item) => item?.path));
  const routes = REQUIRED_TARGETS.map((target) => {
    const filename = `${APP_NAME}_${manifest.versions.app}_${target.goos}_${target.goarch}.tar.gz`;
    const candidatePath = `app/assets/${filename}`;
    if (!declared.has(candidatePath)) fail(`candidate manifest does not declare ${candidatePath}`);
    const absolutePath = safeWorkspacePath(root, candidatePath);
    const originalUrl = `${manifest.source.url}/releases/download/${manifest.versions.app_tag}/${filename}`;
    const parsed = new URL(originalUrl);
    const paths = [
      `/${originalUrl}`,
      parsed.pathname,
      `/${parsed.host}${parsed.pathname}`,
    ];
    return Object.freeze({
      target: targetKey(target),
      filename,
      candidatePath,
      absolutePath,
      originalUrl,
      paths: Object.freeze([...new Set(paths)]),
    });
  });

  const owners = new Map();
  for (const route of routes) {
    for (const path of route.paths) {
      if (owners.has(path)) fail(`archive route ${path} is ambiguous`);
      owners.set(path, route.candidatePath);
    }
  }
  return Object.freeze(routes);
}

/** Pure HTTP router used by the loopback archive server. */
export function routeCandidateArchive({ method, requestUrl, routes }) {
  if (method !== "GET" && method !== "HEAD") {
    return Object.freeze({ status: HTTP_METHOD_NOT_ALLOWED, archive: null, sendBody: false });
  }
  let parsed;
  try {
    const base = new URL("http://127.0.0.1");
    parsed = new URL(String(requestUrl ?? ""), base);
    if (parsed.origin !== base.origin || parsed.search || parsed.hash) {
      return Object.freeze({ status: HTTP_BAD_REQUEST, archive: null, sendBody: false });
    }
  } catch {
    return Object.freeze({ status: HTTP_BAD_REQUEST, archive: null, sendBody: false });
  }
  const archive = routes.find((item) => item.paths.includes(parsed.pathname)) ?? null;
  return Object.freeze({
    status: archive ? HTTP_OK : HTTP_NOT_FOUND,
    archive,
    sendBody: Boolean(archive && method === "GET"),
  });
}

/**
 * Build the two tap states without touching disk. Buffer values make the
 * byte-for-byte cask requirement explicit and directly testable.
 */
export function candidateTapPlan({
  tapName,
  caskBytes,
  legacyArchiveUrl,
  legacyArchiveSha256,
}) {
  if (!TAP_RE.test(tapName ?? "")) fail(`invalid ephemeral tap name ${JSON.stringify(tapName)}`);
  if (!Buffer.isBuffer(caskBytes) || caskBytes.length === 0) fail("candidate cask bytes are required");
  let legacyUrl;
  try {
    legacyUrl = new URL(legacyArchiveUrl);
  } catch (cause) {
    fail(`legacy fixture URL is invalid: ${cause.message}`);
  }
  if (legacyUrl.protocol !== "file:" || !/^[0-9a-f]{64}$/.test(legacyArchiveSha256 ?? "")) {
    fail("legacy fixture must use a file URL and a lowercase SHA-256");
  }

  const formula = [
    `class Cerberus < Formula`,
    `  desc "Qualification-only migration fixture"`,
    `  homepage "https://invalid.example"`,
    `  url ${rubyString(legacyArchiveUrl)}`,
    `  version ${rubyString(LEGACY_VERSION)}`,
    `  sha256 ${rubyString(legacyArchiveSha256)}`,
    "",
    "  def install",
    `    bin.install ${rubyString(APP_NAME)}`,
    "  end",
    "end",
    "",
  ].join("\n");

  return Object.freeze({
    legacy: Object.freeze({
      writes: Object.freeze([{ path: FORMULA_PATH, bytes: Buffer.from(formula) }]),
      removes: Object.freeze([]),
    }),
    candidate: Object.freeze({
      writes: Object.freeze([
        { path: CASK_PATH, bytes: Buffer.from(caskBytes) },
        {
          path: MIGRATIONS_PATH,
          bytes: Buffer.from(`${JSON.stringify({ [APP_NAME]: tapName })}\n`),
        },
      ]),
      removes: Object.freeze([FORMULA_PATH]),
    }),
  });
}

export function hostTarget(platform, architecture) {
  const goos = new Map([
    ["darwin", "darwin"],
    ["linux", "linux"],
  ]).get(platform);
  const goarch = new Map([
    ["x64", "amd64"],
    ["arm64", "arm64"],
  ]).get(architecture);
  if (!goos || !goarch) fail(`unsupported Homebrew qualification host ${platform}/${architecture}`);
  return `${goos}/${goarch}`;
}

export function brewCommandEnvironment(base, { artifactOrigin, stateRoot }) {
  let origin;
  try {
    origin = new URL(artifactOrigin);
  } catch (cause) {
    fail(`artifact origin is invalid: ${cause.message}`);
  }
  if (origin.protocol !== "http:" || origin.hostname !== "127.0.0.1" || origin.pathname !== "/") {
    fail("artifact origin must be an HTTP loopback origin with no path");
  }
  const root = resolve(stateRoot);
  return {
    ...base,
    HOMEBREW_ARTIFACT_DOMAIN: origin.origin,
    HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK: "1",
    HOMEBREW_CACHE: join(root, "cache"),
    HOMEBREW_LOGS: join(root, "logs"),
    HOMEBREW_TEMP: join(root, "temp"),
    HOMEBREW_CURL_RETRIES: "0",
    HOMEBREW_NO_ANALYTICS: "1",
    HOMEBREW_NO_ASK: "1",
    HOMEBREW_NO_AUTO_UPDATE: "1",
    HOMEBREW_NO_ENV_HINTS: "1",
    HOMEBREW_NO_GITHUB_API: "1",
    HOMEBREW_NO_INSTALL_CLEANUP: "1",
    HOMEBREW_NO_INSTALL_FROM_API: "1",
  };
}

export function withoutArtifactDomain(environment) {
  const result = { ...environment };
  delete result.HOMEBREW_ARTIFACT_DOMAIN;
  delete result.HOMEBREW_ARTIFACT_DOMAIN_NO_FALLBACK;
  return result;
}

function listedNames(output) {
  return new Set(
    String(output ?? "")
      .split("\n")
      .map((line) => line.trim().split(/\s+/)[0])
      .filter(Boolean),
  );
}

function pathInside(root, path) {
  if (!root || !path) return false;
  return within(resolve(root), resolve(path)) && resolve(root) !== resolve(path);
}

export function commandOnPath(name, pathValue, cwd = process.cwd()) {
  if (typeof name !== "string" || name === "" || name.includes("/") || name.includes("\\")) return "";
  for (const directory of String(pathValue ?? "").split(delimiter)) {
    const candidate = resolve(directory || cwd, name);
    try {
      accessSync(candidate, fsConstants.X_OK);
      if (statSync(candidate).isFile()) return candidate;
    } catch {
      // This PATH component does not provide an executable with the requested name.
    }
  }
  return "";
}

export function installedCandidateProblems({
  version,
  caskList,
  formulaList,
  allowFormula,
  brewPrefix,
  resolvedPath,
  pathCommand,
  realPath,
  reportedVersion,
  schemaStatus,
  schemaOutput,
  docsStatus,
}) {
  const problems = [];
  if (!listedNames(caskList).has(APP_NAME)) problems.push("the candidate cask is not installed");
  if (!allowFormula && listedNames(formulaList).has(APP_NAME)) {
    problems.push("the fresh-install fixture resolved to a formula instead of only the candidate cask");
  }
  if (!pathInside(brewPrefix, resolvedPath)) problems.push("the command link is outside the Homebrew prefix");
  if (pathCommand !== resolvedPath) problems.push("PATH does not resolve to the candidate Homebrew link");
  const caskRoot = join(resolve(brewPrefix || "/"), "Caskroom", APP_NAME, String(version ?? ""));
  if (!pathInside(caskRoot, realPath)) problems.push("the command link does not resolve into the candidate Caskroom");
  if (reportedVersion !== version) {
    problems.push(`the installed command reports ${JSON.stringify(reportedVersion)}, want ${JSON.stringify(version)}`);
  }
  if (schemaStatus !== 0 || !String(schemaOutput ?? "").includes("CREATE")) {
    problems.push("the installed command did not render a CREATE statement");
  }
  if (docsStatus !== 0) problems.push("the installed command's config registry does not match the bound checkout");
  return problems;
}

export function migrationCandidateProblems(input) {
  const problems = [];
  if (!String(input.updateOutput ?? "").includes(MIGRATION_ANNOUNCEMENT)) {
    problems.push("brew update did not announce the formula-to-cask migration");
  }
  return [...problems, ...installedCandidateProblems({ ...input, allowFormula: true })];
}

export function archiveDeliveryProblems({ events, expectedTarget }) {
  const problems = [];
  const failed = events.filter((event) => event.status !== HTTP_OK);
  if (failed.length > 0) problems.push(`the archive server rejected ${failed.length} request(s)`);
  const bodies = events.filter(
    (event) => event.method === "GET" && event.status === HTTP_OK && event.target === expectedTarget && event.bytes > 0,
  );
  if (bodies.length === 0) {
    problems.push(`Homebrew did not download the ${expectedTarget} candidate archive from loopback`);
  }
  return problems;
}

function describe(result) {
  return [
    `exit=${result.status}${result.signal ? ` signal=${result.signal}` : ""}`,
    result.timedOut ? "deadline exceeded" : "",
    result.overflow ? "output limit exceeded" : "",
    result.stdout,
    result.stderr,
  ]
    .filter(Boolean)
    .join("\n")
    .trim();
}

export function commandResultSucceeded(result) {
  return Boolean(
    result &&
      result.status === 0 &&
      !result.signal &&
      result.timedOut === false &&
      result.overflow === false,
  );
}

export function brewUpdateExecutionProblem(result) {
  if (
    !result ||
    result.timedOut !== false ||
    result.overflow !== false ||
    result.signal ||
    !Number.isInteger(result.status) ||
    result.status === COMMAND_NOT_STARTED_STATUS
  ) {
    return `advancing the tap through brew update failed. ${describe(result ?? {})}`;
  }
  return null;
}

export async function runCommand(command, args, {
  cwd,
  env = process.env,
  timeout = COMMAND_TIMEOUT_MS,
  maxOutput = MAX_COMMAND_OUTPUT_BYTES,
} = {}) {
  return new Promise((resolveResult) => {
    let settled = false;
    let timedOut = false;
    let overflow = false;
    let stopping = false;
    let bytes = 0;
    const stdout = [];
    const stderr = [];
    const ownsProcessGroup = process.platform !== "win32";
    const child = spawn(command, args, {
      cwd,
      env,
      detached: ownsProcessGroup,
      stdio: ["ignore", "pipe", "pipe"],
    });
    let killTimer = null;
    let forcedSettleTimer = null;

    const killTree = (signal) => {
      try {
        if (ownsProcessGroup && child.pid) process.kill(-child.pid, signal);
        else child.kill(signal);
      } catch {
        try {
          child.kill(signal);
        } catch {
          // The process tree already exited between observation and signalling.
        }
      }
    };

    const stopTree = () => {
      if (stopping) return;
      stopping = true;
      killTree("SIGTERM");
      killTimer = setTimeout(() => {
        killTree("SIGKILL");
        forcedSettleTimer = setTimeout(() => {
          child.stdout.destroy();
          child.stderr.destroy();
          finish(null, "SIGKILL", new Error("process group did not close after SIGKILL"));
        }, COMMAND_KILL_GRACE_MS);
        forcedSettleTimer.unref();
      }, COMMAND_KILL_GRACE_MS);
      killTimer.unref();
    };

    const collect = (destination, chunk) => {
      bytes += chunk.length;
      if (bytes <= maxOutput) destination.push(chunk);
      else if (!overflow) {
        overflow = true;
        stopTree();
      }
    };
    child.stdout.on("data", (chunk) => collect(stdout, chunk));
    child.stderr.on("data", (chunk) => collect(stderr, chunk));

    const timer = setTimeout(() => {
      timedOut = true;
      stopTree();
    }, timeout);
    timer.unref();

    const finish = (status, signal, spawnError = null) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (killTimer) clearTimeout(killTimer);
      if (forcedSettleTimer) clearTimeout(forcedSettleTimer);
      resolveResult({
        status: spawnError ? COMMAND_NOT_STARTED_STATUS : (status ?? 1),
        signal,
        stdout: Buffer.concat(stdout).toString("utf8"),
        stderr: spawnError ? spawnError.message : Buffer.concat(stderr).toString("utf8"),
        timedOut,
        overflow,
      });
    };
    child.on("error", (cause) => finish(COMMAND_NOT_STARTED_STATUS, null, cause));
    child.on("close", (status, signal) => finish(status, signal));
  });
}

async function runChecked(command, args, label, options = {}) {
  const result = await runCommand(command, args, options);
  if (!commandResultSucceeded(result)) {
    fail(`${label} failed. ${describe(result)}`);
  }
  return result;
}

async function startArchiveServer(routes) {
  const events = [];
  const server = createServer((request, response) => {
    const decision = routeCandidateArchive({
      method: request.method,
      requestUrl: request.url,
      routes,
    });
    const event = {
      method: request.method ?? "",
      path: request.url ?? "",
      status: decision.status,
      target: decision.archive?.target ?? "",
      bytes: 0,
    };
    events.push(event);
    response.setHeader("Connection", "close");
    response.setHeader("Cache-Control", "no-store");
    if (!decision.archive) {
      response.statusCode = decision.status;
      response.end();
      return;
    }
    try {
      const stat = statSync(decision.archive.absolutePath);
      if (!stat.isFile()) throw new Error("archive is not a regular file");
      response.statusCode = HTTP_OK;
      response.setHeader("Content-Type", "application/gzip");
      response.setHeader("Content-Length", String(stat.size));
      if (!decision.sendBody) {
        response.end();
        return;
      }
      const bytes = readFileSync(decision.archive.absolutePath);
      event.bytes = bytes.length;
      response.end(bytes);
    } catch (cause) {
      event.status = HTTP_SERVER_ERROR;
      response.statusCode = HTTP_SERVER_ERROR;
      response.end(String(cause.message));
    }
  });

  await new Promise((resolveListen, rejectListen) => {
    server.once("error", rejectListen);
    server.listen({ host: "127.0.0.1", port: 0, exclusive: true }, resolveListen);
  });
  const address = server.address();
  if (!address || typeof address === "string") fail("loopback archive server has no TCP address");

  return {
    origin: `http://127.0.0.1:${address.port}`,
    events,
    async close() {
      const closed = new Promise((resolveClose) => server.close(resolveClose));
      const deadline = new Promise((resolveClose) => {
        setTimeout(() => {
          server.closeAllConnections?.();
          resolveClose();
        }, SERVER_CLOSE_TIMEOUT_MS).unref();
      });
      await Promise.race([closed, deadline]);
    },
  };
}

function applyTapState(sourceRoot, state) {
  for (const path of state.removes) rmSync(safeWorkspacePath(sourceRoot, path), { force: true });
  for (const item of state.writes) {
    const target = safeWorkspacePath(sourceRoot, item.path);
    mkdirSync(dirname(target), { recursive: true });
    writeFileSync(target, item.bytes);
  }
}

async function commitTapState(sourceRoot, state, message) {
  applyTapState(sourceRoot, state);
  await runChecked("git", ["-C", sourceRoot, "add", "--all"], "staging ephemeral tap state");
  await runChecked("git", ["-C", sourceRoot, "commit", "-m", message], "committing ephemeral tap state");
  await runChecked("git", ["-C", sourceRoot, "push", "origin", "main"], "advancing ephemeral tap origin");
}

async function initialiseTap(root) {
  const origin = join(root, "tap-origin.git");
  const source = join(root, "tap-source");
  mkdirSync(source, { recursive: true });
  await runChecked("git", ["init", "--bare", origin], "initialising ephemeral tap origin");
  await runChecked("git", ["init", "-b", "main", source], "initialising ephemeral tap source");
  await runChecked("git", ["-C", source, "config", "core.autocrlf", "false"], "disabling tap line conversion");
  await runChecked("git", ["-C", source, "config", "user.name", "Release Qualification"], "configuring tap commit name");
  await runChecked(
    "git",
    ["-C", source, "config", "user.email", "release-qualification@invalid.example"],
    "configuring tap commit email",
  );
  await runChecked("git", ["-C", source, "remote", "add", "origin", pathToFileURL(origin).href], "configuring tap origin");
  await runChecked("git", ["--git-dir", origin, "symbolic-ref", "HEAD", "refs/heads/main"], "configuring tap default branch");
  return { origin, source, remote: pathToFileURL(origin).href };
}

async function createLegacyArchive(root) {
  const payload = join(root, "legacy-payload");
  const archive = join(root, "legacy-source.tar.gz");
  mkdirSync(payload, { recursive: true });
  const executable = join(payload, APP_NAME);
  writeFileSync(executable, `#!/bin/sh\nprintf '%s\\n' '${LEGACY_REPORTED_VERSION}'\n`);
  chmodSync(executable, EXECUTABLE_MODE);
  await runChecked("tar", ["-czf", archive, "-C", payload, APP_NAME], "building legacy formula fixture");
  const bytes = readFileSync(archive);
  return { archive, url: pathToFileURL(archive).href, sha256: sha256Hex(bytes) };
}

async function brewLists(env) {
  const cask = await runChecked("brew", ["list", "--cask", "--versions"], "listing installed casks", { env });
  const formula = await runChecked("brew", ["list", "--formula", "--versions"], "listing installed formulae", { env });
  return { cask: cask.stdout, formula: formula.stdout };
}

async function assertEmptyStartingState(env) {
  const lists = await brewLists(env);
  if (listedNames(lists.cask).has(APP_NAME) || listedNames(lists.formula).has(APP_NAME)) {
    fail(`${APP_NAME} is already installed; refusing to alter a pre-existing Homebrew installation`);
  }
}

async function assertTappedCaskBytes({ env, tapName, expected }) {
  const repository = await runChecked("brew", ["--repo", tapName], "locating ephemeral tap", { env });
  const actual = readFileSync(join(repository.stdout.trim(), CASK_PATH));
  if (!actual.equals(expected)) fail("ephemeral tap cask bytes do not equal the sealed candidate cask");
}

async function installedProbe({ env, repoRoot, brewPrefix }) {
  const resolvedPath = join(brewPrefix, "bin", APP_NAME);
  const pathCommand = commandOnPath(APP_NAME, env.PATH);
  if (!existsSync(resolvedPath)) {
    return {
      resolvedPath,
      pathCommand,
      realPath: "",
      reportedVersion: "<not linked>",
      schemaStatus: 1,
      schemaOutput: "",
      docsStatus: 1,
    };
  }
  let realPath = "";
  try {
    realPath = realpathSync(resolvedPath);
  } catch {
    // The verdict below turns an unreadable link target into a named failure.
  }
  const version = await runCommand(resolvedPath, ["--version"], { env });
  const schema = await runCommand(resolvedPath, ["migrate", "schema"], { env, cwd: repoRoot });
  const docs = await runCommand(resolvedPath, ["config-docs", "-check"], { env, cwd: repoRoot });
  return {
    resolvedPath,
    pathCommand,
    realPath,
    reportedVersion: commandResultSucceeded(version) ? version.stdout.trim() : "<version command failed>",
    schemaStatus: commandResultSucceeded(schema) ? 0 : 1,
    schemaOutput: schema.stdout,
    docsStatus: commandResultSucceeded(docs) ? 0 : 1,
  };
}

async function assertCheckout(repoRoot, manifest) {
  if (!existsSync(join(repoRoot, "docs", "configuration.md"))) {
    fail("REPO_ROOT does not contain docs/configuration.md");
  }
  const top = await runChecked("git", ["-C", repoRoot, "rev-parse", "--show-toplevel"], "locating checkout root");
  if (resolve(top.stdout.trim()) !== repoRoot) fail("REPO_ROOT must be the repository root");
  const sha = await runChecked("git", ["-C", repoRoot, "rev-parse", "HEAD"], "reading checkout commit");
  const tree = await runChecked("git", ["-C", repoRoot, "rev-parse", "HEAD^{tree}"], "reading checkout tree");
  const status = await runChecked(
    "git",
    ["-C", repoRoot, "status", "--porcelain=v1", "--untracked-files=all"],
    "checking checkout cleanliness",
  );
  const problems = [];
  if (sha.stdout.trim() !== manifest.source.sha) problems.push("checkout commit does not equal candidate source commit");
  if (tree.stdout.trim() !== manifest.source.tree) problems.push("checkout tree does not equal candidate source tree");
  if (status.stdout.trim() !== "") problems.push("checkout has tracked or untracked changes outside ignored build output");
  if (problems.length > 0) throw new CandidateBrewError(problems);
}

async function cleanupBrew({ env, tapName, tapAdded, packageTouched }) {
  const problems = [];
  const attempt = async (args, label) => {
    const result = await runCommand("brew", args, { env, timeout: CLEANUP_TIMEOUT_MS });
    if (!commandResultSucceeded(result)) problems.push(`${label}: ${describe(result)}`);
  };
  if (packageTouched) {
    const lists = await Promise.all([
      runCommand("brew", ["list", "--cask", "--versions"], { env, timeout: CLEANUP_TIMEOUT_MS }),
      runCommand("brew", ["list", "--formula", "--versions"], { env, timeout: CLEANUP_TIMEOUT_MS }),
    ]);
    if (!commandResultSucceeded(lists[0])) problems.push(`listing casks during cleanup: ${describe(lists[0])}`);
    else if (listedNames(lists[0].stdout).has(APP_NAME)) {
      await attempt(["uninstall", "--cask", "--force", APP_NAME], "removing candidate cask");
    }
    if (!commandResultSucceeded(lists[1])) problems.push(`listing formulae during cleanup: ${describe(lists[1])}`);
    else if (listedNames(lists[1].stdout).has(APP_NAME)) {
      await attempt(["uninstall", "--formula", "--force", APP_NAME], "removing legacy formula");
    }
  }
  if (tapAdded) {
    const taps = await runCommand("brew", ["tap"], { env, timeout: CLEANUP_TIMEOUT_MS });
    if (!commandResultSucceeded(taps)) problems.push(`listing taps during cleanup: ${describe(taps)}`);
    else if (new Set(taps.stdout.split("\n").map((line) => line.trim()).filter(Boolean)).has(tapName)) {
      await attempt(["untap", "--force", tapName], "removing ephemeral tap");
    }
  }
  return problems;
}

async function qualify(mode) {
  if (mode !== "install" && mode !== "migration") {
    fail(`MODE must be install or migration, got ${JSON.stringify(mode)}`);
  }
  const candidateRoot = resolve(process.env.RELEASE_CANDIDATE_DIR || CANDIDATE_DIR_DEFAULT);
  const digest = requiredString(process.env.RELEASE_CANDIDATE_DIGEST, "RELEASE_CANDIDATE_DIGEST");
  if (!DIGEST_RE.test(digest)) fail("RELEASE_CANDIDATE_DIGEST must be sha256:<lowercase hex>");
  const repoRoot = resolve(requiredString(process.env.REPO_ROOT, "REPO_ROOT"));
  const sealed = verifyCandidate(candidateRoot, { digest });
  await assertCheckout(repoRoot, sealed.manifest);

  const caskBytes = readFileSync(join(candidateRoot, "app", "homebrew", `${APP_NAME}.rb`));
  const routes = candidateArchiveRoutes({ manifest: sealed.manifest, candidateRoot });
  const expectedTarget = hostTarget(process.platform, process.arch);
  const tempRoot = mkdtempSync(join(resolve(tmpdir()), "cerberus-release-candidate-brew-"));
  const tapName = `candidate-${randomUUID().replaceAll("-", "").slice(0, TAP_TOKEN_HEX_LENGTH)}/qualification`;
  let server = null;
  let env = null;
  let setupEnv = null;
  let tapAdded = false;
  let packageTouched = false;
  let primaryFailure = null;
  let cleanupProblems = [];
  try {
    server = await startArchiveServer(routes);
    env = brewCommandEnvironment(process.env, { artifactOrigin: server.origin, stateRoot: tempRoot });
    setupEnv = withoutArtifactDomain(env);
    for (const path of [env.HOMEBREW_CACHE, env.HOMEBREW_LOGS, env.HOMEBREW_TEMP]) {
      mkdirSync(path, { recursive: true });
    }
    await runChecked("brew", ["--version"], "locating Homebrew", { env });
    const prefixResult = await runChecked("brew", ["--prefix"], "reading Homebrew prefix", { env });
    const brewPrefix = prefixResult.stdout.trim();
    await assertEmptyStartingState(env);

    const legacy = await createLegacyArchive(tempRoot);
    const plan = candidateTapPlan({
      tapName,
      caskBytes,
      legacyArchiveUrl: legacy.url,
      legacyArchiveSha256: legacy.sha256,
    });
    const tap = await initialiseTap(tempRoot);

    if (mode === "migration") {
      await commitTapState(tap.source, plan.legacy, "test: add legacy formula fixture");
    } else {
      await commitTapState(tap.source, plan.candidate, "test: add candidate cask fixture");
    }
    tapAdded = true;
    await runChecked("brew", ["tap", tapName, tap.remote], "tapping ephemeral repository", { env });

    if (mode === "migration") {
      packageTouched = true;
      await runChecked("brew", ["install", `${tapName}/${APP_NAME}`], "installing legacy formula fixture", {
        env: setupEnv,
        timeout: BREW_INSTALL_TIMEOUT_MS,
      });
      const before = await brewLists(env);
      if (!listedNames(before.formula).has(APP_NAME) || listedNames(before.cask).has(APP_NAME)) {
        fail("legacy setup did not produce exactly a formula installation");
      }
      const legacyProbe = await runChecked(join(brewPrefix, "bin", APP_NAME), ["--version"], "probing legacy formula", { env });
      if (legacyProbe.stdout.trim() !== LEGACY_REPORTED_VERSION) {
        fail("legacy formula setup did not link the distinct fixture payload");
      }
      mkdirSync(join(brewPrefix, "Caskroom"), { recursive: true });
      await commitTapState(tap.source, plan.candidate, "test: migrate formula to candidate cask");
      // A different installed tap can make the aggregate update command return
      // non-zero after this tap has already advanced. The migration outcome is
      // asserted directly below; only an unbounded or unstartable command is an
      // execution failure in its own right.
      const update = await runCommand("brew", ["update"], { env, timeout: BREW_UPDATE_TIMEOUT_MS });
      const updateExecutionProblem = brewUpdateExecutionProblem(update);
      if (updateExecutionProblem) fail(updateExecutionProblem);
      await assertTappedCaskBytes({ env, tapName, expected: caskBytes });
      const lists = await brewLists(env);
      const probe = await installedProbe({ env, repoRoot, brewPrefix });
      const problems = [
        ...migrationCandidateProblems({
          version: sealed.manifest.versions.app,
          updateOutput: `${update.stdout}\n${update.stderr}`,
          caskList: lists.cask,
          formulaList: lists.formula,
          brewPrefix,
          ...probe,
        }),
        ...archiveDeliveryProblems({ events: server.events, expectedTarget }),
      ];
      if (problems.length > 0) throw new CandidateBrewError(problems);
    } else {
      await assertTappedCaskBytes({ env, tapName, expected: caskBytes });
      packageTouched = true;
      await runChecked(
        "brew",
        ["install", "--cask", `${tapName}/${APP_NAME}`],
        "installing exact candidate cask",
        { env, timeout: BREW_INSTALL_TIMEOUT_MS },
      );
      const lists = await brewLists(env);
      const probe = await installedProbe({ env, repoRoot, brewPrefix });
      const problems = [
        ...installedCandidateProblems({
          version: sealed.manifest.versions.app,
          caskList: lists.cask,
          formulaList: lists.formula,
          allowFormula: false,
          brewPrefix,
          ...probe,
        }),
        ...archiveDeliveryProblems({ events: server.events, expectedTarget }),
      ];
      if (problems.length > 0) throw new CandidateBrewError(problems);
    }
    notice(
      `release-candidate-brew: ${mode} qualified ${sealed.manifest.versions.app} from candidate ${sealed.digest}`,
    );
  } catch (cause) {
    primaryFailure = cause;
  } finally {
    if (env) cleanupProblems = await cleanupBrew({ env, tapName, tapAdded, packageTouched });
    if (server) await server.close();
    try {
      if (within(resolve(tmpdir()), tempRoot) && tempRoot !== resolve(tmpdir())) {
        rmSync(tempRoot, { recursive: true, force: true });
      } else {
        cleanupProblems.push(`refused to remove unsafe temporary path ${tempRoot}`);
      }
    } catch (cause) {
      cleanupProblems.push(`removing temporary qualification state: ${cause.message}`);
    }
  }

  if (primaryFailure) {
    if (cleanupProblems.length > 0) {
      throw new CandidateBrewError([primaryFailure.message, ...cleanupProblems]);
    }
    throw primaryFailure;
  }
  if (cleanupProblems.length > 0) throw new CandidateBrewError(cleanupProblems);
}

const invokedDirectly = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invokedDirectly) {
  const mode = (process.env.MODE || process.argv[2] || "").trim();
  qualify(mode).catch((cause) => {
    error(cause instanceof Error ? cause.message : String(cause));
    log("release-candidate-brew: qualification failed closed");
    process.exit(1);
  });
}
