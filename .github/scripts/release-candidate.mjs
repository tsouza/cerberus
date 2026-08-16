// release-candidate.mjs — stage, seal, and verify one unpublished release
// candidate before any tag, registry, package, or release write is permitted.
//
// Modes (MODE or argv[2]):
//   stage   validate GoReleaser's snapshot output, copy the exact archives,
//           checksum, and generated cask into the candidate, and materialise
//           the two-platform Docker build context from those same binaries.
//   image   build one unpublished multi-platform OCI layout from that context.
//   seal    bind every candidate byte to the exact source SHA/tree and write a
//           canonical manifest. Its sha256 is the immutable candidate digest.
//   verify  recompute the manifest digest, exact file roster, and every file
//           digest. Expected SHA/tree/version/digest mismatches fail closed.
//
// Environment:
//   RELEASE_CANDIDATE_DIR   candidate root (default build/release-candidate).
//   GORELEASER_DIST         snapshot output (default dist; stage only).
//   RELEASE_IMAGE_CONTEXT  staged image context
//                          (default build/release-image-context; stage only).
//   RELEASE_APP_VERSION    exact application version (required).
//   RELEASE_APP_TAG        exact v-prefixed application tag (required).
//   RELEASE_CHART_VERSION  exact chart version (required for seal/verify).
//   RELEASE_CHART_PACKAGE  packaged chart path (required for stage).
//   RELEASE_CHART_METADATA Artifact Hub repository metadata (required for
//                          stage and promoted only from the sealed candidate).
//   RELEASE_SOURCE_SHA     exact 40-hex commit (required).
//   RELEASE_SOURCE_TREE    exact 40-hex Git tree (required).
//   RELEASE_SOURCE_URL     runtime HTTPS source URL (required for every mode;
//                          sealed into source identity and cask URLs).
//   RELEASE_RUN_ID         qualification workflow run id (required for seal).
//   RELEASE_RUN_ATTEMPT    positive run attempt (required for seal).
//   RELEASE_CANDIDATE_DIGEST
//                          expected sha256:<hex> (required for verify).
//   GITHUB_OUTPUT          receives candidate/context/digest outputs.
//
// Node builtins only. A candidate directory is a closed set: an unlisted file
// is as invalid as a missing or changed file, so promotion cannot smuggle bytes
// that the qualification attestation did not bind.

import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import {
  copyFileSync,
  existsSync,
  mkdirSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

import { capture, error, notice, setOutput } from "./lib/gh.mjs";
import {
  validateReleaseArtifactSet,
  validateSealedCandidateArtifacts,
} from "./release-artifact-validator.mjs";

export const CANDIDATE_SCHEMA_VERSION = 1;
export const CANDIDATE_MANIFEST = "candidate-manifest.json";
export const REQUIRED_TARGETS = Object.freeze([
  Object.freeze({ goos: "darwin", goarch: "amd64" }),
  Object.freeze({ goos: "darwin", goarch: "arm64" }),
  Object.freeze({ goos: "linux", goarch: "amd64" }),
  Object.freeze({ goos: "linux", goarch: "arm64" }),
]);

const SHA_RE = /^[0-9a-f]{40}$/;
const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
const VERSION_RE = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/;
const SOURCE_URL_RE = /^https:\/\/[A-Za-z0-9.-]+\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const OCI_SCHEMA_VERSION = 2;
const OCI_INDEX_MEDIA_TYPE = "application/vnd.oci.image.index.v1+json";
const OCI_MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json";
const OCI_CONFIG_MEDIA_TYPE = "application/vnd.oci.image.config.v1+json";
const IN_TOTO_MEDIA_TYPE = "application/vnd.in-toto+json";
const IN_TOTO_STATEMENT_TYPE = "https://in-toto.io/Statement/v0.1";
const SPDX_PREDICATE_TYPE = "https://spdx.dev/Document";
const PROVENANCE_PREDICATE_TYPE = "https://slsa.dev/provenance/v0.2";
const BUILDKIT_BUILD_TYPE = "https://mobyproject.org/buildkit@v1";
const ATTESTATION_REFERENCE_TYPE = "attestation-manifest";
const REQUIRED_IMAGE_PLATFORMS = Object.freeze(["linux/amd64", "linux/arm64"]);
const MANIFEST_KEYS = new Set([
  "schema_version",
  "source",
  "versions",
  "run",
  "files",
  "image",
]);
const SOURCE_KEYS = new Set(["sha", "tree", "url"]);
const VERSION_KEYS = new Set(["app", "app_tag", "chart"]);
const RUN_KEYS = new Set(["id", "attempt"]);
const FILE_KEYS = new Set(["path", "size", "sha256"]);
const IMAGE_KEYS = new Set(["layout", "digest", "platforms"]);
const SOURCE_DOCUMENT_KEYS = new Set([
  "app_tag",
  "app_version",
  "sha",
  "source_url",
  "tree",
]);

export class CandidateError extends Error {
  constructor(problems) {
    super(`release candidate is invalid:\n${problems.map((p) => `- ${p}`).join("\n")}`);
    this.name = "CandidateError";
    this.problems = problems;
  }
}

function exactKeys(value, allowed, label, problems) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    problems.push(`${label} must be an object`);
    return false;
  }
  const extras = Object.keys(value).filter((key) => !allowed.has(key));
  const missing = [...allowed].filter((key) => !(key in value));
  if (extras.length > 0) problems.push(`${label} has unknown keys: ${extras.join(", ")}`);
  if (missing.length > 0) problems.push(`${label} is missing keys: ${missing.join(", ")}`);
  return extras.length === 0 && missing.length === 0;
}

function requiredString(value, label, problems, pattern = null) {
  if (typeof value !== "string" || value === "") {
    problems.push(`${label} must be a non-empty string`);
    return;
  }
  if (pattern && !pattern.test(value)) problems.push(`${label} has invalid value ${JSON.stringify(value)}`);
}

function canonicalPath(root, path, label) {
  const absolute = resolve(root, path);
  const rel = relative(root, absolute);
  if (rel === "" || rel === ".." || rel.startsWith(`..${sep}`) || rel.startsWith(sep)) {
    throw new CandidateError([`${label} escapes ${root}: ${path}`]);
  }
  return { absolute, relative: rel.split(sep).join("/") };
}

function readJSON(path, label = path) {
  let value;
  try {
    value = JSON.parse(readFileSync(path, "utf8"));
  } catch (cause) {
    throw new CandidateError([`${label} is not valid JSON: ${cause.message}`]);
  }
  return value;
}

export function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function sha256Bytes(value) {
  return `sha256:${createHash("sha256").update(value).digest("hex")}`;
}

export function sha256File(path) {
  return sha256Bytes(readFileSync(path));
}

export function manifestDigest(manifest) {
  return sha256Bytes(`${canonicalJSON(manifest)}\n`);
}

function walkFiles(root, current = root) {
  const files = [];
  for (const entry of readdirSync(current, { withFileTypes: true })) {
    const absolute = join(current, entry.name);
    if (entry.isSymbolicLink()) {
      throw new CandidateError([`candidate contains symlink ${relative(root, absolute)}`]);
    }
    if (entry.isDirectory()) files.push(...walkFiles(root, absolute));
    else if (entry.isFile()) files.push(relative(root, absolute).split(sep).join("/"));
    else throw new CandidateError([`candidate contains unsupported entry ${relative(root, absolute)}`]);
  }
  return files.sort();
}

function copyBoundFile(source, root, destination) {
  if (!existsSync(source) || !statSync(source).isFile()) {
    throw new CandidateError([`required build output is missing: ${source}`]);
  }
  const target = canonicalPath(root, destination, "candidate destination").absolute;
  mkdirSync(dirname(target), { recursive: true });
  copyFileSync(source, target);
}

function targetKey(value) {
  return `${value.goos}/${value.goarch}`;
}

function selectUniqueArtifact(artifacts, predicate, label) {
  const found = artifacts.filter(predicate);
  if (found.length !== 1) {
    throw new CandidateError([`${label}: found ${found.length}, want exactly 1`]);
  }
  return found[0];
}

export function releaseArtifacts(document, { version, commit }) {
  if (!Array.isArray(document)) throw new CandidateError(["dist/artifacts.json must be an array"]);
  const problems = [];
  const binaries = new Map();
  const archives = new Map();
  for (const target of REQUIRED_TARGETS) {
    const key = targetKey(target);
    const binary = document.filter(
      (item) => item?.type === "Binary" && item.goos === target.goos && item.goarch === target.goarch,
    );
    const archive = document.filter(
      (item) => item?.type === "Archive" && item.goos === target.goos && item.goarch === target.goarch,
    );
    if (binary.length !== 1) problems.push(`${key}: found ${binary.length} binaries, want exactly 1`);
    else binaries.set(key, binary[0]);
    if (archive.length !== 1) problems.push(`${key}: found ${archive.length} archives, want exactly 1`);
    else {
      if (archive[0].name !== `cerberus_${version}_${target.goos}_${target.goarch}.tar.gz`) {
        problems.push(`${key}: archive name ${archive[0].name} does not carry version ${version}`);
      }
      archives.set(key, archive[0]);
    }
  }
  const checksum = document.filter((item) => item?.type === "Checksum");
  const cask = document.filter((item) => item?.type === "Homebrew Cask");
  const metadata = document.filter((item) => item?.type === "Metadata");
  if (checksum.length !== 1) problems.push(`found ${checksum.length} checksum artifacts, want exactly 1`);
  if (cask.length !== 1) problems.push(`found ${cask.length} cask artifacts, want exactly 1`);
  if (metadata.length !== 1) problems.push(`found ${metadata.length} metadata artifacts, want exactly 1`);
  if (problems.length > 0) throw new CandidateError(problems);
  return {
    binaries,
    archives,
    checksum: checksum[0],
    cask: cask[0],
    metadata: metadata[0],
    commit,
  };
}

// GoReleaser snapshot metadata uses `tag` for the repository tag it discovered
// while deriving release history. It does not become the unpublished future
// tag merely because snapshot.version_template supplies RELEASE_VERSION. The
// candidate identity is instead bound by RELEASE_APP_TAG == v<version>,
// source.json, the archive names, and the sealed manifest. Treating metadata's
// historical tag as candidate identity makes every legitimate snapshot fail.
export function validateSnapshotMetadata(metadata, { version, commit }) {
  const problems = [];
  if (metadata === null || typeof metadata !== "object" || Array.isArray(metadata)) {
    problems.push("GoReleaser metadata must be an object");
  } else {
    if (metadata.version !== version) {
      problems.push(`GoReleaser version ${metadata.version} does not equal ${version}`);
    }
    if (metadata.commit !== commit) {
      problems.push(`GoReleaser commit ${metadata.commit} does not equal ${commit}`);
    }
  }
  if (problems.length > 0) throw new CandidateError(problems);
  return metadata;
}

export function validateReleaseChecksums(root, version) {
  const expected = new Map(
    REQUIRED_TARGETS.map((target) => {
      const name = `cerberus_${version}_${target.goos}_${target.goarch}.tar.gz`;
      return [name, join(root, "app/assets", name)];
    }),
  );
  const checksumPath = join(root, "app/assets/checksums.txt");
  if (!existsSync(checksumPath) || !statSync(checksumPath).isFile()) {
    throw new CandidateError([`candidate checksum file is missing: ${checksumPath}`]);
  }
  const lines = readFileSync(checksumPath, "utf8")
    .split(/\r?\n/)
    .filter((line) => line !== "");
  const actual = new Map();
  const problems = [];
  for (const line of lines) {
    const match = line.match(/^([0-9a-f]{64})  ([^/\\]+)$/);
    if (!match) {
      problems.push(`malformed checksum line ${JSON.stringify(line)}`);
      continue;
    }
    const [, digest, name] = match;
    if (actual.has(name)) problems.push(`checksum entry ${name} is duplicated`);
    actual.set(name, digest);
  }
  for (const [name, path] of expected) {
    if (!existsSync(path) || !statSync(path).isFile()) {
      problems.push(`checksum target ${name} is missing`);
      continue;
    }
    const wanted = sha256File(path).slice("sha256:".length);
    if (actual.get(name) !== wanted) {
      problems.push(`checksum for ${name} is ${actual.get(name) ?? "<missing>"}, want ${wanted}`);
    }
  }
  for (const name of actual.keys()) {
    if (!expected.has(name)) problems.push(`checksums.txt contains unexpected release asset ${name}`);
  }
  if (problems.length > 0) throw new CandidateError(problems);
  return Object.freeze([...actual.keys()].sort());
}

function requiredEnv(name, pattern = null) {
  const value = process.env[name];
  const problems = [];
  requiredString(value, name, problems, pattern);
  if (problems.length > 0) throw new CandidateError(problems);
  return value;
}

export function verifyCheckout(sha, tree, root = process.cwd()) {
  const cwd = resolve(root);
  const top = capture("git", ["rev-parse", "--show-toplevel"], { cwd });
  const head = capture("git", ["rev-parse", "HEAD"], { cwd });
  const headTree = capture("git", ["rev-parse", "HEAD^{tree}"], { cwd });
  const status = capture("git", ["status", "--porcelain=v1", "--untracked-files=all"], { cwd });
  const problems = [];
  if (top.status !== 0) problems.push(`git rev-parse --show-toplevel failed: ${top.stderr.trim()}`);
  else if (resolve(top.stdout.trim()) !== cwd) {
    problems.push(`candidate verification runs at ${cwd}, not repository root ${top.stdout.trim()}`);
  }
  if (head.status !== 0) problems.push(`git rev-parse HEAD failed: ${head.stderr.trim()}`);
  else if (head.stdout.trim() !== sha) problems.push(`checkout HEAD is ${head.stdout.trim()}, want ${sha}`);
  if (headTree.status !== 0) problems.push(`git rev-parse HEAD^{tree} failed: ${headTree.stderr.trim()}`);
  else if (headTree.stdout.trim() !== tree) {
    problems.push(`checkout tree is ${headTree.stdout.trim()}, want ${tree}`);
  }
  if (status.status !== 0) problems.push(`git status failed: ${status.stderr.trim()}`);
  else if (status.stdout.trim() !== "") {
    problems.push(
      "checkout has tracked or untracked modifications; candidate source is not the exact Git tree",
    );
  }
  if (problems.length > 0) throw new CandidateError(problems);
}

function candidateRoot() {
  return resolve(process.env.RELEASE_CANDIDATE_DIR || "build/release-candidate");
}

function validateSourceDocument(root, expected) {
  const source = readJSON(join(root, "source.json"), "candidate source identity");
  const problems = [];
  if (exactKeys(source, SOURCE_DOCUMENT_KEYS, "candidate source identity", problems)) {
    for (const [field, wanted, pattern] of [
      ["sha", expected.sha, SHA_RE],
      ["tree", expected.tree, SHA_RE],
      ["app_version", expected.app, VERSION_RE],
      ["app_tag", expected.appTag, null],
      ["source_url", expected.sourceUrl, SOURCE_URL_RE],
    ]) {
      requiredString(source[field], `candidate source identity.${field}`, problems, pattern);
      if (source[field] !== wanted) {
        problems.push(`candidate source identity.${field} is ${source[field]}, want ${wanted}`);
      }
    }
  }
  if (problems.length > 0) throw new CandidateError(problems);
  return source;
}

function stage() {
  const root = candidateRoot();
  const dist = resolve(process.env.GORELEASER_DIST || "dist");
  const context = resolve(process.env.RELEASE_IMAGE_CONTEXT || "build/release-image-context");
  const version = requiredEnv("RELEASE_APP_VERSION", VERSION_RE);
  const tag = requiredEnv("RELEASE_APP_TAG");
  const chart = requiredEnv("RELEASE_CHART_VERSION", VERSION_RE);
  const chartPackage = resolve(requiredEnv("RELEASE_CHART_PACKAGE"));
  const chartMetadata = resolve(requiredEnv("RELEASE_CHART_METADATA"));
  const sha = requiredEnv("RELEASE_SOURCE_SHA", SHA_RE);
  const tree = requiredEnv("RELEASE_SOURCE_TREE", SHA_RE);
  const sourceUrl = requiredEnv("RELEASE_SOURCE_URL", SOURCE_URL_RE);
  verifyCheckout(sha, tree);
  if (tag !== `v${version}`) throw new CandidateError([`RELEASE_APP_TAG ${tag} must equal v${version}`]);

  validateSnapshotMetadata(readJSON(join(dist, "metadata.json"), "GoReleaser metadata"), {
    version,
    commit: sha,
  });

  const selected = releaseArtifacts(readJSON(join(dist, "artifacts.json")), {
    version,
    commit: sha,
  });
  validateReleaseArtifactSet({
    version,
    chartVersion: chart,
    appTag: tag,
    sourceUrl,
    archives: new Map(
      REQUIRED_TARGETS.map((target) => {
        const key = targetKey(target);
        return [key, readFileSync(resolve(selected.archives.get(key).path))];
      }),
    ),
    binaries: new Map(
      REQUIRED_TARGETS.map((target) => {
        const key = targetKey(target);
        return [key, readFileSync(resolve(selected.binaries.get(key).path))];
      }),
    ),
    cask: readFileSync(resolve(selected.cask.path)),
    chart: readFileSync(chartPackage),
  });
  rmSync(root, { recursive: true, force: true });
  rmSync(context, { recursive: true, force: true });
  mkdirSync(root, { recursive: true });
  mkdirSync(context, { recursive: true });

  for (const target of REQUIRED_TARGETS) {
    const key = targetKey(target);
    const archive = selected.archives.get(key);
    copyBoundFile(resolve(archive.path), root, `app/assets/${archive.name}`);
    if (target.goos === "linux") {
      const binary = selected.binaries.get(key);
      copyBoundFile(resolve(binary.path), context, `${target.goos}/${target.goarch}/cerberus`);
    }
  }
  copyBoundFile(resolve(selected.checksum.path), root, "app/assets/checksums.txt");
  copyBoundFile(resolve(selected.cask.path), root, "app/homebrew/cerberus.rb");
  copyBoundFile(chartPackage, root, `chart/cerberus-${chart}.tgz`);
  copyBoundFile(chartMetadata, root, "chart/artifacthub-repo.yml");
  writeFileSync(join(root, "chart/artifacthub-config.yml"), "");
  writeFileSync(
    join(root, "source.json"),
    `${canonicalJSON({ app_tag: tag, app_version: version, sha, source_url: sourceUrl, tree })}\n`,
  );
  setOutput("candidate_dir", root);
  setOutput("image_context", context);
  notice(`release candidate staged for ${tag} at ${sha.slice(0, 12)} tree ${tree.slice(0, 12)}`);
}

export function candidateImageBuildInvocation({
  root,
  context,
  candidateRoot,
  version,
  sha,
  sourceURL,
}) {
  const problems = [];
  requiredString(version, "release image version", problems, VERSION_RE);
  requiredString(sha, "release image source SHA", problems, SHA_RE);
  requiredString(sourceURL, "release image source URL", problems, SOURCE_URL_RE);
  if (problems.length > 0) throw new CandidateError(problems);
  const layout = join(resolve(candidateRoot), "app/image");
  return Object.freeze({
    command: "docker",
    args: Object.freeze([
      "buildx",
      "build",
      "--file",
      join(resolve(root), "Dockerfile"),
      "--platform",
      "linux/amd64,linux/arm64",
      "--build-arg",
      `RELEASE_VERSION=${version}`,
      "--build-arg",
      `SOURCE_SHA=${sha}`,
      "--build-arg",
      `SOURCE_URL=${sourceURL}`,
      "--tag",
      `docker.io/library/cerberus:${version}`,
      "--sbom=true",
      "--provenance=mode=max",
      "--output",
      `type=oci,dest=${layout},tar=false`,
      resolve(context),
    ]),
    cwd: resolve(root),
    layout,
  });
}

function image() {
  const top = capture("git", ["rev-parse", "--show-toplevel"]);
  if (top.status !== 0 || top.stdout.trim() === "") {
    throw new CandidateError([
      `cannot resolve repository root: ${top.stderr.trim() || "git returned no path"}`,
    ]);
  }
  const root = resolve(top.stdout.trim());
  const candidate = candidateRoot();
  const context = resolve(process.env.RELEASE_IMAGE_CONTEXT || "build/release-image-context");
  const version = requiredEnv("RELEASE_APP_VERSION", VERSION_RE);
  const sha = requiredEnv("RELEASE_SOURCE_SHA", SHA_RE);
  const tree = requiredEnv("RELEASE_SOURCE_TREE", SHA_RE);
  const sourceURL = requiredEnv("RELEASE_SOURCE_URL", SOURCE_URL_RE);
  verifyCheckout(sha, tree);
  validateSourceDocument(candidate, {
    sha,
    tree,
    app: version,
    appTag: requiredEnv("RELEASE_APP_TAG"),
    sourceUrl: sourceURL,
  });
  const invocation = candidateImageBuildInvocation({
    root,
    context,
    candidateRoot: candidate,
    version,
    sha,
    sourceURL,
  });
  rmSync(invocation.layout, { recursive: true, force: true });
  const result = spawnSync(invocation.command, invocation.args, {
    cwd: invocation.cwd,
    env: process.env,
    stdio: "inherit",
  });
  if (result.error || result.status !== 0) {
    throw new CandidateError([
      result.error
        ? `candidate image build failed: ${result.error.message}`
        : `candidate image build exited with status ${result.status}`,
    ]);
  }
  setOutput("image_layout", invocation.layout);
  notice(`built unpublished candidate OCI layout for ${version}`);
}

function validateOCIDescriptor(root, layout, descriptor, reachable, problems, label) {
  if (!descriptor || typeof descriptor !== "object" || Array.isArray(descriptor)) {
    problems.push(`${label} must be an OCI descriptor object`);
    return;
  }
  if (!DIGEST_RE.test(descriptor.digest ?? "")) {
    problems.push(`${label}.digest is not sha256`);
    return;
  }
  if (!Number.isSafeInteger(descriptor.size) || descriptor.size < 0) {
    problems.push(`${label}.size is not a non-negative integer`);
    return;
  }
  const blob = `${layout}/blobs/sha256/${descriptor.digest.slice("sha256:".length)}`;
  const absolute = join(root, blob);
  if (!existsSync(absolute) || !statSync(absolute).isFile()) {
    problems.push(`${label} references missing blob ${blob}`);
    return;
  }
  const actualSize = statSync(absolute).size;
  if (actualSize !== descriptor.size) problems.push(`${blob}: size ${actualSize}, want ${descriptor.size}`);
  const actualDigest = sha256File(absolute);
  if (actualDigest !== descriptor.digest) problems.push(`${blob}: digest ${actualDigest}, want ${descriptor.digest}`);
  if (reachable.has(blob)) return;
  reachable.add(blob);

  const mediaType = String(descriptor.mediaType ?? "");
  const isIndex = new Set([
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
  ]).has(mediaType);
  const isManifest = new Set([
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.v2+json",
  ]).has(mediaType);
  if (!isIndex && !isManifest) return;
  let document;
  try {
    document = JSON.parse(readFileSync(absolute, "utf8"));
  } catch (cause) {
    problems.push(`${blob}: JSON media type has malformed content: ${cause.message}`);
    return;
  }
  for (const [field, children] of isIndex
    ? [["manifests", document.manifests]]
    : [["layers", document.layers]]) {
    if (children === undefined) continue;
    if (!Array.isArray(children)) {
      problems.push(`${blob}.${field} must be an array`);
      continue;
    }
    children.forEach((child, index) =>
      validateOCIDescriptor(root, layout, child, reachable, problems, `${blob}.${field}[${index}]`),
    );
  }
  for (const [field, child] of isManifest
    ? [["config", document.config], ["subject", document.subject]]
    : []) {
    if (child !== undefined) {
      validateOCIDescriptor(root, layout, child, reachable, problems, `${blob}.${field}`);
    }
  }
}

function plainObject(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function validateOCIEnvelope(document, mediaType, label, problems) {
  if (!plainObject(document)) {
    problems.push(`${label} must be an object`);
    return false;
  }
  if (document.schemaVersion !== OCI_SCHEMA_VERSION) {
    problems.push(`${label}.schemaVersion must be ${OCI_SCHEMA_VERSION}`);
  }
  if (document.mediaType !== mediaType) {
    problems.push(`${label}.mediaType is ${document.mediaType}, want ${mediaType}`);
  }
  return document.schemaVersion === OCI_SCHEMA_VERSION && document.mediaType === mediaType;
}

function descriptorJSON(root, layout, descriptor, label) {
  return readJSON(
    join(root, layout, "blobs", "sha256", descriptor.digest.slice("sha256:".length)),
    label,
  );
}

function validateInTotoSubject(statement, subjectDigest, label, problems) {
  if (!Array.isArray(statement.subject) || statement.subject.length !== 1) {
    problems.push(`${label}.subject must contain exactly one image subject`);
    return;
  }
  const [subject] = statement.subject;
  if (!plainObject(subject)) {
    problems.push(`${label}.subject[0] must be an object`);
    return;
  }
  requiredString(subject.name, `${label}.subject[0].name`, problems);
  if (!plainObject(subject.digest)) {
    problems.push(`${label}.subject[0].digest must be an object`);
    return;
  }
  const wanted = subjectDigest.slice("sha256:".length);
  if (subject.digest.sha256 !== wanted) {
    problems.push(`${label}.subject[0].digest.sha256 is ${subject.digest.sha256}, want ${wanted}`);
  }
}

function validateSPDXPredicate(predicate, label, problems) {
  if (!plainObject(predicate)) {
    problems.push(`${label} must be an SPDX document object`);
    return;
  }
  requiredString(predicate.spdxVersion, `${label}.spdxVersion`, problems, /^SPDX-2\.[23]$/);
  if (predicate.dataLicense !== "CC0-1.0") {
    problems.push(`${label}.dataLicense must be CC0-1.0`);
  }
  if (predicate.SPDXID !== "SPDXRef-DOCUMENT") {
    problems.push(`${label}.SPDXID must be SPDXRef-DOCUMENT`);
  }
  requiredString(predicate.name, `${label}.name`, problems);
  requiredString(
    predicate.documentNamespace,
    `${label}.documentNamespace`,
    problems,
    /^https?:\/\//,
  );
  if (!plainObject(predicate.creationInfo)) {
    problems.push(`${label}.creationInfo must be an object`);
  } else {
    requiredString(predicate.creationInfo.created, `${label}.creationInfo.created`, problems);
    if (!Array.isArray(predicate.creationInfo.creators) || predicate.creationInfo.creators.length === 0) {
      problems.push(`${label}.creationInfo.creators must be a non-empty array`);
    }
  }
  if (!Array.isArray(predicate.packages) || predicate.packages.length === 0) {
    problems.push(`${label}.packages must describe at least one package`);
  } else {
    predicate.packages.forEach((item, index) => {
      if (!plainObject(item)) {
        problems.push(`${label}.packages[${index}] must be an object`);
        return;
      }
      requiredString(item.name, `${label}.packages[${index}].name`, problems);
      requiredString(item.SPDXID, `${label}.packages[${index}].SPDXID`, problems);
    });
  }
}

function validateMaxProvenance(predicate, expected, platform, label, problems) {
  if (!plainObject(predicate)) {
    problems.push(`${label} must be a provenance object`);
    return;
  }
  if (predicate.buildType !== BUILDKIT_BUILD_TYPE) {
    problems.push(`${label}.buildType is ${predicate.buildType}, want ${BUILDKIT_BUILD_TYPE}`);
  }
  if (!plainObject(predicate.builder)) {
    problems.push(`${label}.builder must be an object`);
  } else {
    requiredString(predicate.builder.id, `${label}.builder.id`, problems);
  }
  if (!plainObject(predicate.invocation)) {
    problems.push(`${label}.invocation must be present for maximum provenance`);
  } else {
    const parameters = predicate.invocation.parameters;
    if (!plainObject(parameters)) {
      problems.push(`${label}.invocation.parameters must be present for maximum provenance`);
    } else if (!plainObject(parameters.args)) {
      problems.push(`${label}.invocation.parameters.args must contain the release build arguments`);
    } else {
      for (const [name, wanted] of [
        ["build-arg:RELEASE_VERSION", expected.version],
        ["build-arg:SOURCE_SHA", expected.sha],
        ["build-arg:SOURCE_URL", expected.sourceUrl],
      ]) {
        if (parameters.args[name] !== wanted) {
          problems.push(`${label}.invocation.parameters.args.${name} is ${parameters.args[name]}, want ${wanted}`);
        }
      }
    }
    if (!plainObject(predicate.invocation.environment)) {
      problems.push(`${label}.invocation.environment must be present for maximum provenance`);
    } else if (predicate.invocation.environment.platform !== platform) {
      problems.push(
        `${label}.invocation.environment.platform is ${predicate.invocation.environment.platform}, want ${platform}`,
      );
    }
  }
  if (!Array.isArray(predicate.materials) || predicate.materials.length === 0) {
    problems.push(`${label}.materials must contain resolved build inputs`);
  }
  const completeness = predicate.metadata?.completeness;
  if (!plainObject(completeness)) {
    problems.push(`${label}.metadata.completeness must be present for maximum provenance`);
  } else {
    for (const field of ["parameters", "environment", "materials"]) {
      if (completeness[field] !== true) {
        problems.push(`${label}.metadata.completeness.${field} must be true`);
      }
    }
  }
}

function validateAttestationStatement(
  statement,
  predicateType,
  subjectDigest,
  expected,
  platform,
  label,
  problems,
) {
  if (!plainObject(statement)) {
    problems.push(`${label} must be an in-toto statement object`);
    return;
  }
  if (statement._type !== IN_TOTO_STATEMENT_TYPE) {
    problems.push(`${label}._type is ${statement._type}, want ${IN_TOTO_STATEMENT_TYPE}`);
  }
  if (statement.predicateType !== predicateType) {
    problems.push(`${label}.predicateType is ${statement.predicateType}, want ${predicateType}`);
  }
  validateInTotoSubject(statement, subjectDigest, label, problems);
  if (predicateType === SPDX_PREDICATE_TYPE) {
    validateSPDXPredicate(statement.predicate, `${label}.predicate`, problems);
  } else if (predicateType === PROVENANCE_PREDICATE_TYPE) {
    validateMaxProvenance(
      statement.predicate,
      expected,
      platform,
      `${label}.predicate`,
      problems,
    );
  }
}

function validatePlatformImage(root, layout, descriptor, expected, platform, problems) {
  const label = `OCI ${platform} image`;
  if (descriptor.mediaType !== OCI_MANIFEST_MEDIA_TYPE) {
    problems.push(`${label} descriptor.mediaType is ${descriptor.mediaType}, want ${OCI_MANIFEST_MEDIA_TYPE}`);
  }
  const [expectedOS, expectedArchitecture] = platform.split("/");
  const expectedPlatform = { os: expectedOS, architecture: expectedArchitecture };
  if (canonicalJSON(descriptor.platform) !== canonicalJSON(expectedPlatform)) {
    problems.push(
      `${label} descriptor.platform is ${canonicalJSON(descriptor.platform)}, want ${canonicalJSON(expectedPlatform)}`,
    );
  }
  const manifest = descriptorJSON(root, layout, descriptor, `${label} manifest`);
  validateOCIEnvelope(manifest, OCI_MANIFEST_MEDIA_TYPE, `${label} manifest`, problems);
  if (!plainObject(manifest.config)) {
    problems.push(`${label} manifest.config must be an OCI descriptor`);
    return;
  }
  if (manifest.config.mediaType !== OCI_CONFIG_MEDIA_TYPE) {
    problems.push(
      `${label} manifest.config.mediaType is ${manifest.config.mediaType}, want ${OCI_CONFIG_MEDIA_TYPE}`,
    );
  }
  if (!Array.isArray(manifest.layers) || manifest.layers.length === 0) {
    problems.push(`${label} manifest.layers must be a non-empty array`);
  }
  const config = descriptorJSON(root, layout, manifest.config, `${label} config`);
  if (config.os !== expectedOS) problems.push(`${label} config.os is ${config.os}, want ${expectedOS}`);
  if (config.architecture !== expectedArchitecture) {
    problems.push(`${label} config.architecture is ${config.architecture}, want ${expectedArchitecture}`);
  }
  if (!plainObject(config.config)) {
    problems.push(`${label} config.config must be an object`);
    return;
  }
  const requiredLabels = {
    "org.opencontainers.image.title": "cerberus",
    "org.opencontainers.image.description":
      "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse",
    "org.opencontainers.image.url": expected.sourceUrl,
    "org.opencontainers.image.source": expected.sourceUrl,
    "org.opencontainers.image.licenses": "Apache-2.0",
    "org.opencontainers.image.version": expected.version,
    "org.opencontainers.image.revision": expected.sha,
  };
  const labels = config.config.Labels;
  if (!plainObject(labels)) {
    problems.push(`${label} config.config.Labels must be an object`);
  } else {
    for (const [name, wanted] of Object.entries(requiredLabels)) {
      if (labels[name] !== wanted) {
        problems.push(`${label} label ${name} is ${labels[name]}, want ${wanted}`);
      }
    }
  }
  const entrypoint = config.config.Entrypoint;
  if (canonicalJSON(entrypoint) !== canonicalJSON(["/usr/local/bin/cerberus"])) {
    problems.push(
      `${label} Entrypoint is ${canonicalJSON(entrypoint)}, want ["/usr/local/bin/cerberus"]`,
    );
  }
}

function validatePlatformAttestation(
  root,
  layout,
  descriptor,
  imageDescriptor,
  expected,
  platform,
  problems,
) {
  const label = `OCI ${platform} attestation`;
  if (descriptor.mediaType !== OCI_MANIFEST_MEDIA_TYPE) {
    problems.push(`${label} descriptor.mediaType is ${descriptor.mediaType}, want ${OCI_MANIFEST_MEDIA_TYPE}`);
  }
  const attestationPlatform = { os: "unknown", architecture: "unknown" };
  if (canonicalJSON(descriptor.platform) !== canonicalJSON(attestationPlatform)) {
    problems.push(
      `${label} descriptor.platform is ${canonicalJSON(descriptor.platform)}, want ${canonicalJSON(attestationPlatform)}`,
    );
  }
  const referenced = descriptor.annotations?.["vnd.docker.reference.digest"];
  if (referenced !== imageDescriptor.digest) {
    problems.push(`${label} references ${referenced}, want ${imageDescriptor.digest}`);
  }
  const manifest = descriptorJSON(root, layout, descriptor, `${label} manifest`);
  validateOCIEnvelope(manifest, OCI_MANIFEST_MEDIA_TYPE, `${label} manifest`, problems);
  if (!plainObject(manifest.config) || manifest.config.mediaType !== OCI_CONFIG_MEDIA_TYPE) {
    problems.push(`${label} manifest.config must be an OCI image config descriptor`);
  } else {
    const config = descriptorJSON(root, layout, manifest.config, `${label} config`);
    if (config.os !== "unknown" || config.architecture !== "unknown") {
      problems.push(`${label} config must declare unknown/unknown`);
    }
  }
  if (!Array.isArray(manifest.layers) || manifest.layers.length !== 2) {
    problems.push(`${label} manifest.layers must contain exactly SBOM and provenance`);
    return;
  }
  const statements = new Map();
  manifest.layers.forEach((layer, index) => {
    const layerLabel = `${label} manifest.layers[${index}]`;
    if (!plainObject(layer)) {
      problems.push(`${layerLabel} must be an OCI descriptor`);
      return;
    }
    if (layer.mediaType !== IN_TOTO_MEDIA_TYPE) {
      problems.push(`${layerLabel}.mediaType is ${layer.mediaType}, want ${IN_TOTO_MEDIA_TYPE}`);
    }
    const predicateType = layer.annotations?.["in-toto.io/predicate-type"];
    if (![SPDX_PREDICATE_TYPE, PROVENANCE_PREDICATE_TYPE].includes(predicateType)) {
      problems.push(`${layerLabel} has unsupported predicate type ${predicateType}`);
      return;
    }
    if (statements.has(predicateType)) {
      problems.push(`${label} has duplicate ${predicateType} statements`);
      return;
    }
    const statement = descriptorJSON(root, layout, layer, `${layerLabel} statement`);
    statements.set(predicateType, statement);
    validateAttestationStatement(
      statement,
      predicateType,
      imageDescriptor.digest,
      expected,
      platform,
      `${layerLabel} statement`,
      problems,
    );
  });
  for (const predicateType of [SPDX_PREDICATE_TYPE, PROVENANCE_PREDICATE_TYPE]) {
    if (!statements.has(predicateType)) {
      problems.push(`${label} is missing ${predicateType}`);
    }
  }
}

export function imageDescriptor(root, expected) {
  const expectedProblems = [];
  if (!plainObject(expected)) {
    throw new CandidateError(["OCI image expectations must be an object"]);
  }
  requiredString(expected.version, "OCI image expected version", expectedProblems, VERSION_RE);
  requiredString(expected.sha, "OCI image expected source SHA", expectedProblems, SHA_RE);
  requiredString(
    expected.sourceUrl,
    "OCI image expected source URL",
    expectedProblems,
    SOURCE_URL_RE,
  );
  if (expectedProblems.length > 0) throw new CandidateError(expectedProblems);

  const layout = "app/image";
  const layoutDocument = readJSON(join(root, layout, "oci-layout"), "OCI layout marker");
  if (
    layoutDocument === null ||
    typeof layoutDocument !== "object" ||
    Array.isArray(layoutDocument) ||
    Object.keys(layoutDocument).length !== 1 ||
    layoutDocument.imageLayoutVersion !== "1.0.0"
  ) {
    throw new CandidateError(["OCI layout marker must exactly declare imageLayoutVersion 1.0.0"]);
  }
  const indexPath = join(root, layout, "index.json");
  const index = readJSON(indexPath, "OCI image index");
  const indexProblems = [];
  validateOCIEnvelope(index, OCI_INDEX_MEDIA_TYPE, "OCI image index", indexProblems);
  const descriptors = Array.isArray(index.manifests) ? index.manifests : [];
  if (!Array.isArray(index.manifests)) indexProblems.push("OCI image index.manifests must be an array");
  if (descriptors.length !== 1) {
    indexProblems.push(`OCI image index must contain exactly one tagged descriptor, found ${descriptors.length}`);
  }
  const chosen = descriptors.length === 1 ? descriptors[0] : null;
  if (chosen?.mediaType !== OCI_INDEX_MEDIA_TYPE) {
    indexProblems.push(`OCI tagged descriptor.mediaType must be ${OCI_INDEX_MEDIA_TYPE}`);
  }
  if (chosen?.annotations?.["org.opencontainers.image.ref.name"] !== expected.version) {
    indexProblems.push(`OCI tagged descriptor must have ref name ${expected.version}`);
  }
  const imageName = `docker.io/library/cerberus:${expected.version}`;
  if (chosen?.annotations?.["io.containerd.image.name"] !== imageName) {
    indexProblems.push(`OCI tagged descriptor must have image name ${imageName}`);
  }
  if (!DIGEST_RE.test(chosen?.digest ?? "")) {
    indexProblems.push("OCI tagged descriptor must have a sha256 digest");
  }
  if (indexProblems.length > 0) throw new CandidateError(indexProblems);

  const reachable = new Set();
  const integrityProblems = [];
  descriptors.forEach((descriptor, index) =>
    validateOCIDescriptor(root, layout, descriptor, reachable, integrityProblems, `OCI index.manifests[${index}]`),
  );
  const actualLayoutFiles = walkFiles(join(root, layout)).map((path) => `${layout}/${path}`);
  const expectedLayoutFiles = [`${layout}/index.json`, `${layout}/oci-layout`, ...reachable].sort();
  if (JSON.stringify(actualLayoutFiles) !== JSON.stringify(expectedLayoutFiles)) {
    integrityProblems.push(
      `OCI layout file roster is ${JSON.stringify(actualLayoutFiles)}, want exactly reachable ${JSON.stringify(expectedLayoutFiles)}`,
    );
  }
  if (integrityProblems.length > 0) throw new CandidateError(integrityProblems);
  const blobPath = join(root, layout, "blobs", "sha256", chosen.digest.slice("sha256:".length));
  const imageIndex = readJSON(blobPath, "OCI image manifest list");
  const semanticProblems = [];
  validateOCIEnvelope(
    imageIndex,
    OCI_INDEX_MEDIA_TYPE,
    "OCI image manifest list",
    semanticProblems,
  );
  const innerDescriptors = Array.isArray(imageIndex.manifests) ? imageIndex.manifests : [];
  if (!Array.isArray(imageIndex.manifests)) {
    semanticProblems.push("OCI image manifest list.manifests must be an array");
  }
  const images = new Map();
  const attestations = [];
  innerDescriptors.forEach((descriptor, index) => {
    const label = `OCI image manifest list.manifests[${index}]`;
    if (!plainObject(descriptor)) {
      semanticProblems.push(`${label} must be an OCI descriptor`);
      return;
    }
    const referenceType = descriptor.annotations?.["vnd.docker.reference.type"];
    if (referenceType === undefined) {
      const platform = `${descriptor.platform?.os}/${descriptor.platform?.architecture}`;
      if (!REQUIRED_IMAGE_PLATFORMS.includes(platform)) {
        semanticProblems.push(`${label} has unsupported platform ${platform}`);
      } else if (images.has(platform)) {
        semanticProblems.push(`${label} duplicates platform ${platform}`);
      } else {
        images.set(platform, descriptor);
      }
      if (descriptor.mediaType !== OCI_MANIFEST_MEDIA_TYPE) {
        semanticProblems.push(`${label}.mediaType is ${descriptor.mediaType}, want ${OCI_MANIFEST_MEDIA_TYPE}`);
      }
    } else if (referenceType === ATTESTATION_REFERENCE_TYPE) {
      attestations.push(descriptor);
    } else {
      semanticProblems.push(`${label} has unsupported reference type ${referenceType}`);
    }
  });
  const platforms = [...images.keys()].sort();
  if (canonicalJSON(platforms) !== canonicalJSON(REQUIRED_IMAGE_PLATFORMS)) {
    semanticProblems.push(
      `OCI image platforms are ${canonicalJSON(platforms)}, want ${canonicalJSON(REQUIRED_IMAGE_PLATFORMS)}`,
    );
  }
  for (const platform of REQUIRED_IMAGE_PLATFORMS) {
    const descriptor = images.get(platform);
    if (descriptor) validatePlatformImage(root, layout, descriptor, expected, platform, semanticProblems);
  }
  const imagesByDigest = new Map([...images].map(([platform, descriptor]) => [descriptor.digest, { platform, descriptor }]));
  const attestationsByDigest = new Map();
  attestations.forEach((descriptor, index) => {
    const referenced = descriptor.annotations?.["vnd.docker.reference.digest"];
    const image = imagesByDigest.get(referenced);
    if (!image) {
      semanticProblems.push(`OCI attestation[${index}] references unknown image digest ${referenced}`);
      return;
    }
    if (attestationsByDigest.has(referenced)) {
      semanticProblems.push(`OCI image ${referenced} has duplicate attestation manifests`);
      return;
    }
    attestationsByDigest.set(referenced, descriptor);
    validatePlatformAttestation(
      root,
      layout,
      descriptor,
      image.descriptor,
      expected,
      image.platform,
      semanticProblems,
    );
  });
  for (const [platform, descriptor] of images) {
    if (!attestationsByDigest.has(descriptor.digest)) {
      semanticProblems.push(`OCI ${platform} image is missing its BuildKit attestation manifest`);
    }
  }
  if (semanticProblems.length > 0) throw new CandidateError(semanticProblems);
  return { layout, digest: chosen.digest, platforms };
}

function seal() {
  const root = candidateRoot();
  const sha = requiredEnv("RELEASE_SOURCE_SHA", SHA_RE);
  const tree = requiredEnv("RELEASE_SOURCE_TREE", SHA_RE);
  const app = requiredEnv("RELEASE_APP_VERSION", VERSION_RE);
  const appTag = requiredEnv("RELEASE_APP_TAG");
  const chart = requiredEnv("RELEASE_CHART_VERSION", VERSION_RE);
  const sourceUrl = requiredEnv("RELEASE_SOURCE_URL", SOURCE_URL_RE);
  const runID = requiredEnv("RELEASE_RUN_ID", /^[1-9][0-9]*$/);
  const attemptText = requiredEnv("RELEASE_RUN_ATTEMPT", /^[1-9][0-9]*$/);
  verifyCheckout(sha, tree);
  if (appTag !== `v${app}`) throw new CandidateError([`RELEASE_APP_TAG ${appTag} must equal v${app}`]);
  validateSourceDocument(root, { sha, tree, app, appTag, sourceUrl });
  const existingManifest = join(root, CANDIDATE_MANIFEST);
  rmSync(existingManifest, { force: true });
  const paths = walkFiles(root);
  for (const required of [
    "source.json",
    "app/assets/checksums.txt",
    "app/homebrew/cerberus.rb",
    "app/image/index.json",
    `chart/cerberus-${chart}.tgz`,
    "chart/artifacthub-repo.yml",
    "chart/artifacthub-config.yml",
  ]) {
    if (!paths.includes(required)) throw new CandidateError([`candidate is missing ${required}`]);
  }
  const archives = paths.filter((path) => path.startsWith("app/assets/cerberus_") && path.endsWith(".tar.gz"));
  if (archives.length !== REQUIRED_TARGETS.length) {
    throw new CandidateError([`candidate has ${archives.length} release archives, want ${REQUIRED_TARGETS.length}`]);
  }
  validateReleaseChecksums(root, app);
  validateSealedCandidateArtifacts({
    root,
    version: app,
    chartVersion: chart,
    appTag,
    sourceUrl,
  });
  const files = paths.map((path) => {
    const absolute = canonicalPath(root, path, "candidate file").absolute;
    return { path, size: statSync(absolute).size, sha256: sha256File(absolute) };
  });
  const manifest = {
    schema_version: CANDIDATE_SCHEMA_VERSION,
    source: { sha, tree, url: sourceUrl },
    versions: { app, app_tag: appTag, chart },
    run: { id: runID, attempt: Number(attemptText) },
    files,
    image: imageDescriptor(root, { version: app, sha, sourceUrl }),
  };
  validateManifest(manifest);
  writeFileSync(existingManifest, `${canonicalJSON(manifest)}\n`);
  const digest = sha256File(existingManifest);
  setOutput("candidate_digest", digest);
  setOutput("image_digest", manifest.image.digest);
  setOutput("source_tree", tree);
  notice(
    `sealed ${files.length} candidate files as ${digest} (image ${manifest.image.digest}, source ${sha.slice(0, 12)})`,
  );
}

export function validateManifest(manifest, expected = {}) {
  const problems = [];
  if (!exactKeys(manifest, MANIFEST_KEYS, "manifest", problems)) throw new CandidateError(problems);
  if (manifest.schema_version !== CANDIDATE_SCHEMA_VERSION) {
    problems.push(`manifest.schema_version must be ${CANDIDATE_SCHEMA_VERSION}`);
  }
  if (exactKeys(manifest.source, SOURCE_KEYS, "manifest.source", problems)) {
    requiredString(manifest.source.sha, "manifest.source.sha", problems, SHA_RE);
    requiredString(manifest.source.tree, "manifest.source.tree", problems, SHA_RE);
    requiredString(manifest.source.url, "manifest.source.url", problems, SOURCE_URL_RE);
  }
  if (exactKeys(manifest.versions, VERSION_KEYS, "manifest.versions", problems)) {
    requiredString(manifest.versions.app, "manifest.versions.app", problems, VERSION_RE);
    requiredString(manifest.versions.chart, "manifest.versions.chart", problems, VERSION_RE);
    requiredString(manifest.versions.app_tag, "manifest.versions.app_tag", problems);
    if (manifest.versions.app_tag !== `v${manifest.versions.app}`) {
      problems.push("manifest.versions.app_tag must be v-prefixed manifest.versions.app");
    }
  }
  if (exactKeys(manifest.run, RUN_KEYS, "manifest.run", problems)) {
    requiredString(manifest.run.id, "manifest.run.id", problems, /^[1-9][0-9]*$/);
    if (!Number.isInteger(manifest.run.attempt) || manifest.run.attempt < 1) {
      problems.push("manifest.run.attempt must be a positive integer");
    }
  }
  if (!Array.isArray(manifest.files) || manifest.files.length === 0) {
    problems.push("manifest.files must be a non-empty array");
  } else {
    let previous = "";
    const seen = new Set();
    for (let index = 0; index < manifest.files.length; index += 1) {
      const item = manifest.files[index];
      const label = `manifest.files[${index}]`;
      if (!exactKeys(item, FILE_KEYS, label, problems)) continue;
      requiredString(item.path, `${label}.path`, problems);
      requiredString(item.sha256, `${label}.sha256`, problems, DIGEST_RE);
      if (!Number.isInteger(item.size) || item.size < 0) problems.push(`${label}.size must be non-negative integer`);
      if (typeof item.path === "string") {
        if (item.path === CANDIDATE_MANIFEST || item.path.startsWith("/") || item.path.includes("..")) {
          problems.push(`${label}.path is not a canonical candidate-relative path`);
        }
        if (seen.has(item.path)) problems.push(`${label}.path is duplicated`);
        if (previous !== "" && item.path.localeCompare(previous) <= 0) problems.push(`${label}.path is not sorted`);
        seen.add(item.path);
        previous = item.path;
      }
    }
    if (
      typeof manifest.versions?.app === "string" &&
      typeof manifest.versions?.chart === "string"
    ) {
      const paths = new Set(manifest.files.map((item) => item?.path));
      const requiredPaths = [
        "source.json",
        "app/assets/checksums.txt",
        "app/homebrew/cerberus.rb",
        "app/image/index.json",
        `chart/cerberus-${manifest.versions.chart}.tgz`,
        "chart/artifacthub-repo.yml",
        "chart/artifacthub-config.yml",
        ...REQUIRED_TARGETS.map(
          (target) =>
            `app/assets/cerberus_${manifest.versions.app}_${target.goos}_${target.goarch}.tar.gz`,
        ),
      ];
      for (const path of requiredPaths) {
        if (!paths.has(path)) problems.push(`manifest.files is missing required candidate file ${path}`);
      }
    }
  }
  if (exactKeys(manifest.image, IMAGE_KEYS, "manifest.image", problems)) {
    requiredString(manifest.image.layout, "manifest.image.layout", problems);
    requiredString(manifest.image.digest, "manifest.image.digest", problems, DIGEST_RE);
    if (JSON.stringify(manifest.image.platforms) !== JSON.stringify(["linux/amd64", "linux/arm64"])) {
      problems.push("manifest.image.platforms must exactly equal linux/amd64 + linux/arm64");
    }
  }
  for (const [field, actual, wanted] of [
    ["source.sha", manifest.source?.sha, expected.sha],
    ["source.tree", manifest.source?.tree, expected.tree],
    ["source.url", manifest.source?.url, expected.sourceUrl],
    ["versions.app", manifest.versions?.app, expected.app],
    ["versions.app_tag", manifest.versions?.app_tag, expected.appTag],
    ["versions.chart", manifest.versions?.chart, expected.chart],
  ]) {
    if (wanted !== undefined && actual !== wanted) problems.push(`manifest.${field} is ${actual}, want ${wanted}`);
  }
  if (problems.length > 0) throw new CandidateError(problems);
  return manifest;
}

export function verifyCandidate(root, expected = {}) {
  const manifestPath = join(root, CANDIDATE_MANIFEST);
  if (!existsSync(manifestPath)) throw new CandidateError([`candidate manifest is missing: ${manifestPath}`]);
  const manifest = validateManifest(readJSON(manifestPath, "candidate manifest"), expected);
  validateSourceDocument(root, {
    sha: manifest.source.sha,
    tree: manifest.source.tree,
    app: manifest.versions.app,
    appTag: manifest.versions.app_tag,
    sourceUrl: manifest.source.url,
  });
  const actualDigest = sha256File(manifestPath);
  if (expected.digest !== undefined && actualDigest !== expected.digest) {
    throw new CandidateError([`candidate digest is ${actualDigest}, want ${expected.digest}`]);
  }
  const actualPaths = walkFiles(root).filter((path) => path !== CANDIDATE_MANIFEST);
  const expectedPaths = manifest.files.map((item) => item.path);
  if (JSON.stringify(actualPaths) !== JSON.stringify(expectedPaths)) {
    throw new CandidateError([
      `candidate file roster is ${JSON.stringify(actualPaths)}, want ${JSON.stringify(expectedPaths)}`,
    ]);
  }
  const problems = [];
  for (const item of manifest.files) {
    const absolute = canonicalPath(root, item.path, "manifest file").absolute;
    const stat = statSync(absolute);
    if (stat.size !== item.size) problems.push(`${item.path}: size ${stat.size}, want ${item.size}`);
    const digest = sha256File(absolute);
    if (digest !== item.sha256) problems.push(`${item.path}: digest ${digest}, want ${item.sha256}`);
  }
  try {
    validateReleaseChecksums(root, manifest.versions.app);
  } catch (cause) {
    if (cause instanceof CandidateError) problems.push(...cause.problems);
    else throw cause;
  }
  try {
    validateSealedCandidateArtifacts({
      root,
      version: manifest.versions.app,
      chartVersion: manifest.versions.chart,
      appTag: manifest.versions.app_tag,
      sourceUrl: manifest.source.url,
    });
  } catch (cause) {
    if (cause instanceof Error) problems.push(cause.message);
    else throw cause;
  }
  let recomputedImage;
  try {
    recomputedImage = imageDescriptor(root, {
      version: manifest.versions.app,
      sha: manifest.source.sha,
      sourceUrl: manifest.source.url,
    });
  } catch (cause) {
    if (cause instanceof CandidateError) problems.push(...cause.problems);
    else throw cause;
  }
  if (recomputedImage && canonicalJSON(recomputedImage) !== canonicalJSON(manifest.image)) {
    problems.push("manifest.image does not match the verified OCI layout descriptor");
  }
  if (problems.length > 0) throw new CandidateError(problems);
  return { manifest, digest: actualDigest };
}

function verify() {
  const expected = {
    sha: requiredEnv("RELEASE_SOURCE_SHA", SHA_RE),
    tree: requiredEnv("RELEASE_SOURCE_TREE", SHA_RE),
    app: requiredEnv("RELEASE_APP_VERSION", VERSION_RE),
    appTag: requiredEnv("RELEASE_APP_TAG"),
    chart: requiredEnv("RELEASE_CHART_VERSION", VERSION_RE),
    sourceUrl: requiredEnv("RELEASE_SOURCE_URL", SOURCE_URL_RE),
    digest: requiredEnv("RELEASE_CANDIDATE_DIGEST", DIGEST_RE),
  };
  verifyCheckout(expected.sha, expected.tree);
  const result = verifyCandidate(candidateRoot(), expected);
  setOutput("candidate_digest", result.digest);
  setOutput("image_digest", result.manifest.image.digest);
  notice(
    `verified candidate ${result.digest} for ${result.manifest.source.sha.slice(0, 12)} ` +
      `tree ${result.manifest.source.tree.slice(0, 12)}`,
  );
}

function main() {
  const mode = process.env.MODE || process.argv[2];
  if (mode === "stage") stage();
  else if (mode === "image") image();
  else if (mode === "seal") seal();
  else if (mode === "verify") verify();
  else throw new CandidateError([`MODE must be stage, image, seal, or verify; got ${JSON.stringify(mode)}`]);
}

const invokedDirectly = process.argv[1] && fileURLToPath(import.meta.url) === resolve(process.argv[1]);
if (invokedDirectly) {
  try {
    main();
  } catch (cause) {
    error(cause instanceof Error ? cause.message : String(cause));
    process.exit(1);
  }
}
