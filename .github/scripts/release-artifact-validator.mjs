// release-artifact-validator.mjs — semantic validation for every package that
// can be published from an unpublished release candidate.
//
// The validator never extracts an archive. It parses bounded gzip/USTAR bytes
// in memory, rejects ambiguous or unsafe members, and binds wrapper metadata
// back to the exact candidate bytes. Candidate staging binds the selected
// GoReleaser outputs; sealing and every later verification re-check the closed
// package set without extracting it.

import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join, resolve } from "node:path";
import { TextDecoder } from "node:util";
import { gunzipSync } from "node:zlib";

import { caskPortabilityProblems } from "./brew-smoke.mjs";

export const RELEASE_TARGETS = Object.freeze([
  Object.freeze({ goos: "darwin", goarch: "amd64" }),
  Object.freeze({ goos: "darwin", goarch: "arm64" }),
  Object.freeze({ goos: "linux", goarch: "amd64" }),
  Object.freeze({ goos: "linux", goarch: "arm64" }),
]);

export const ARCHIVE_LIMITS = Object.freeze({
  maxCompressedBytes: 64 * 1024 * 1024,
  maxExpandedBytes: 128 * 1024 * 1024,
  maxMemberBytes: 128 * 1024 * 1024,
  maxMembers: 2048,
});
export const APP_ARCHIVE_LIMITS = Object.freeze({
  ...ARCHIVE_LIMITS,
  maxMembers: 16,
});
export const CHART_ARCHIVE_LIMITS = Object.freeze({
  maxCompressedBytes: 8 * 1024 * 1024,
  maxExpandedBytes: 32 * 1024 * 1024,
  maxMemberBytes: 16 * 1024 * 1024,
  maxMembers: 512,
});

const APP_ARCHIVE_FILES = Object.freeze([
  "CHANGELOG.md",
  "LICENSE",
  "README.md",
  "cerberus",
]);
const APP_BINARY = "cerberus";
const CHART_NAME = "cerberus";
const CHART_REQUIRED_FILES = Object.freeze([
  `${CHART_NAME}/Chart.yaml`,
  `${CHART_NAME}/values.yaml`,
  `${CHART_NAME}/values.schema.json`,
]);
const CHART_REQUIRED_SCALARS = Object.freeze({
  apiVersion: "v2",
  name: CHART_NAME,
  type: "application",
});
const VERSION_RE = /^\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/;
const APP_TAG_RE = /^v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/;
const HTTPS_SOURCE_RE = /^https:\/\/[^\s?#/]+\/[^\s?#/]+\/[^\s?#/]+$/;
const SHA256_RE = /^[0-9a-f]{64}$/;
const TARGET_KEY_RE = /^(darwin|linux)\/(amd64|arm64)$/;
const CASK_ARTIFACT_RE = /^(app|artifact|audio_unit_plugin|bash_completion|binary|colorpicker|dictionary|fish_completion|font|input_method|installer|internet_plugin|keyboard_layout|manpage|mdimporter|pkg|prefpane|qlplugin|screen_saver|service|suite|vst3_plugin|vst_plugin|zsh_completion)\b/;

const TAR_BLOCK_BYTES = 512;
const TAR_END_BLOCKS = 2;
const TAR_END_BYTES = TAR_BLOCK_BYTES * TAR_END_BLOCKS;
const TAR_HEADER = Object.freeze({
  nameStart: 0,
  nameEnd: 100,
  modeStart: 100,
  modeEnd: 108,
  sizeStart: 124,
  sizeEnd: 136,
  checksumStart: 148,
  checksumEnd: 156,
  type: 156,
  linkStart: 157,
  linkEnd: 257,
  magicStart: 257,
  magicEnd: 263,
  prefixStart: 345,
  prefixEnd: 500,
});
const TAR_CHECKSUM_SPACE = 0x20;
const TAR_TYPE_REGULAR_ASCII = 0x30;
const TAR_TYPE_DIRECTORY_ASCII = 0x35;
const TAR_USTAR_MAGIC = Buffer.from([0x75, 0x73, 0x74, 0x61, 0x72, 0x00]);

const EXECUTABLE_MODE_MASK = 0o111;
const SPECIAL_MODE_MASK = 0o7000;
const ELF_MIN_HEADER_BYTES = 20;
const ELF_MAGIC = Buffer.from([0x7f, 0x45, 0x4c, 0x46]);
const ELF_CLASS_64 = 2;
const ELF_DATA_LITTLE_ENDIAN = 1;
const ELF_VERSION_CURRENT = 1;
const ELF_MACHINE_OFFSET = 18;
const ELF_MACHINE = Object.freeze({ amd64: 0x3e, arm64: 0xb7 });
const MACHO_MIN_HEADER_BYTES = 12;
const MACHO_MAGIC_64 = 0xfeedfacf;
const MACHO_CPU_OFFSET = 4;
const MACHO_CPU = Object.freeze({ amd64: 0x01000007, arm64: 0x0100000c });

const fatalUtf8 = new TextDecoder("utf-8", { fatal: true });

export class ArtifactValidationError extends Error {
  constructor(problems) {
    const list = Array.isArray(problems) ? problems : [String(problems)];
    super(`release artifacts are invalid:\n${list.map((item) => `- ${item}`).join("\n")}`);
    this.name = "ArtifactValidationError";
    this.problems = list;
  }
}

function fail(problem) {
  throw new ArtifactValidationError([problem]);
}

function asBuffer(value, label) {
  if (Buffer.isBuffer(value)) return value;
  if (value instanceof Uint8Array) {
    return Buffer.from(value.buffer, value.byteOffset, value.byteLength);
  }
  fail(`${label} must be bytes`);
}

function decodeUtf8(value, label) {
  const bytes = asBuffer(value, label);
  try {
    return fatalUtf8.decode(bytes);
  } catch (cause) {
    fail(`${label} is not valid UTF-8: ${cause.message}`);
  }
}

function requireVersion(value, label) {
  if (typeof value !== "string" || !VERSION_RE.test(value)) {
    fail(`${label} must be a bare semantic version`);
  }
  return value;
}

function targetKey(target) {
  return `${target.goos}/${target.goarch}`;
}

function normaliseLimits(overrides = {}) {
  const limits = { ...ARCHIVE_LIMITS, ...overrides };
  for (const [name, value] of Object.entries(limits)) {
    if (!Number.isSafeInteger(value) || value <= 0) {
      fail(`archive limit ${name} must be a positive safe integer`);
    }
  }
  return limits;
}

function isZeroBlock(block) {
  return block.every((byte) => byte === 0);
}

function tarString(header, start, end, label) {
  const field = header.subarray(start, end);
  const nul = field.indexOf(0);
  const body = nul < 0 ? field : field.subarray(0, nul);
  if (nul >= 0 && field.subarray(nul).some((byte) => byte !== 0)) {
    fail(`${label} has non-NUL bytes after its terminator`);
  }
  try {
    return fatalUtf8.decode(body);
  } catch (cause) {
    fail(`${label} is not valid UTF-8: ${cause.message}`);
  }
}

function tarOctal(header, start, end, label) {
  const bytes = header.subarray(start, end);
  if (bytes[0] !== undefined && (bytes[0] & 0x80) !== 0) {
    fail(`${label} uses unsupported base-256 encoding`);
  }
  for (const byte of bytes) {
    const octalDigit = byte >= 0x30 && byte <= 0x37;
    if (byte !== 0 && byte !== TAR_CHECKSUM_SPACE && !octalDigit) {
      fail(`${label} is not strict octal`);
    }
  }
  const text = bytes.toString("ascii").replace(/[\0 ]+$/g, "").replace(/^ +/g, "");
  if (!/^[0-7]+$/.test(text)) fail(`${label} is empty or malformed octal`);
  const value = Number.parseInt(text, 8);
  if (!Number.isSafeInteger(value)) fail(`${label} exceeds the safe integer range`);
  return value;
}

function validateHeaderChecksum(header, label) {
  const declared = tarOctal(
    header,
    TAR_HEADER.checksumStart,
    TAR_HEADER.checksumEnd,
    `${label} checksum`,
  );
  let actual = 0;
  for (let index = 0; index < TAR_BLOCK_BYTES; index += 1) {
    actual +=
      index >= TAR_HEADER.checksumStart && index < TAR_HEADER.checksumEnd
        ? TAR_CHECKSUM_SPACE
        : header[index];
  }
  if (declared !== actual) {
    fail(`${label} checksum is ${declared}, want ${actual}`);
  }
}

function canonicalTarPath(rawPath, type, label) {
  if (!rawPath || rawPath.includes("\\") || rawPath.startsWith("/")) {
    fail(`${label} has unsafe path ${JSON.stringify(rawPath)}`);
  }
  const directorySlash = type === "directory" && rawPath.endsWith("/");
  const path = directorySlash ? rawPath.slice(0, -1) : rawPath;
  if (!path || path.endsWith("/")) fail(`${label} has non-canonical path ${JSON.stringify(rawPath)}`);
  const parts = path.split("/");
  if (parts.some((part) => part === "" || part === "." || part === "..")) {
    fail(`${label} has unsafe path ${JSON.stringify(rawPath)}`);
  }
  return parts.join("/");
}

/** Parse a gzip-compressed USTAR archive without extracting it. */
export function parseTarGz(value, { label = "archive", limits: overrideLimits = {} } = {}) {
  const compressed = asBuffer(value, label);
  const limits = normaliseLimits(overrideLimits);
  if (compressed.length > limits.maxCompressedBytes) {
    fail(`${label} compressed size ${compressed.length} exceeds ${limits.maxCompressedBytes}`);
  }

  let expanded;
  try {
    expanded = gunzipSync(compressed, { maxOutputLength: limits.maxExpandedBytes });
  } catch (cause) {
    fail(`${label} is not a bounded valid gzip stream: ${cause.message}`);
  }
  if (expanded.length === 0 || expanded.length % TAR_BLOCK_BYTES !== 0) {
    fail(`${label} expanded tar length is not a non-zero multiple of ${TAR_BLOCK_BYTES}`);
  }

  const members = new Map();
  let offset = 0;
  while (offset < expanded.length) {
    const header = expanded.subarray(offset, offset + TAR_BLOCK_BYTES);
    if (header.length !== TAR_BLOCK_BYTES) fail(`${label} has a truncated tar header`);
    if (isZeroBlock(header)) {
      if (offset + TAR_END_BYTES > expanded.length) {
        fail(`${label} has only one tar end block`);
      }
      const second = expanded.subarray(
        offset + TAR_BLOCK_BYTES,
        offset + TAR_END_BYTES,
      );
      if (!isZeroBlock(second)) fail(`${label} has a malformed tar end marker`);
      if (!expanded.subarray(offset + TAR_END_BYTES).every((byte) => byte === 0)) {
        fail(`${label} contains non-zero data after its tar end marker`);
      }
      return members;
    }

    if (members.size >= limits.maxMembers) {
      fail(`${label} contains more than ${limits.maxMembers} members`);
    }
    const memberLabel = `${label} member ${members.size + 1}`;
    validateHeaderChecksum(header, memberLabel);
    if (!header.subarray(TAR_HEADER.magicStart, TAR_HEADER.magicEnd).equals(TAR_USTAR_MAGIC)) {
      fail(`${memberLabel} is not a USTAR header`);
    }

    const typeByte = header[TAR_HEADER.type];
    const type =
      typeByte === 0 || typeByte === TAR_TYPE_REGULAR_ASCII
        ? "file"
        : typeByte === TAR_TYPE_DIRECTORY_ASCII
          ? "directory"
          : null;
    if (!type) fail(`${memberLabel} has unsupported tar type byte ${typeByte}`);

    const name = tarString(
      header,
      TAR_HEADER.nameStart,
      TAR_HEADER.nameEnd,
      `${memberLabel} name`,
    );
    const prefix = tarString(
      header,
      TAR_HEADER.prefixStart,
      TAR_HEADER.prefixEnd,
      `${memberLabel} prefix`,
    );
    const path = canonicalTarPath(prefix ? `${prefix}/${name}` : name, type, memberLabel);
    if (members.has(path)) fail(`${label} contains duplicate canonical path ${path}`);

    const link = tarString(
      header,
      TAR_HEADER.linkStart,
      TAR_HEADER.linkEnd,
      `${memberLabel} link name`,
    );
    if (link !== "") fail(`${memberLabel} unexpectedly names link target ${JSON.stringify(link)}`);
    const mode = tarOctal(
      header,
      TAR_HEADER.modeStart,
      TAR_HEADER.modeEnd,
      `${memberLabel} mode`,
    );
    const size = tarOctal(
      header,
      TAR_HEADER.sizeStart,
      TAR_HEADER.sizeEnd,
      `${memberLabel} size`,
    );
    if (type === "directory" && size !== 0) fail(`${memberLabel} directory has non-zero size`);
    if (size > limits.maxMemberBytes) {
      fail(`${memberLabel} size ${size} exceeds ${limits.maxMemberBytes}`);
    }

    const bodyStart = offset + TAR_BLOCK_BYTES;
    const paddedSize = Math.ceil(size / TAR_BLOCK_BYTES) * TAR_BLOCK_BYTES;
    const bodyEnd = bodyStart + size;
    const nextOffset = bodyStart + paddedSize;
    if (!Number.isSafeInteger(nextOffset) || nextOffset > expanded.length) {
      fail(`${memberLabel} body exceeds the expanded tar bounds`);
    }
    if (!expanded.subarray(bodyEnd, nextOffset).every((byte) => byte === 0)) {
      fail(`${memberLabel} has non-zero tar padding`);
    }
    members.set(
      path,
      Object.freeze({
        path,
        type,
        mode,
        size,
        data: Buffer.from(expanded.subarray(bodyStart, bodyEnd)),
      }),
    );
    offset = nextOffset;
  }
  fail(`${label} has no two-block tar end marker`);
}

function validateBinaryHeader(binary, target, label) {
  if (target.goos === "linux") {
    if (binary.length < ELF_MIN_HEADER_BYTES || !binary.subarray(0, ELF_MAGIC.length).equals(ELF_MAGIC)) {
      fail(`${label} is not an ELF binary`);
    }
    if (
      binary[4] !== ELF_CLASS_64 ||
      binary[5] !== ELF_DATA_LITTLE_ENDIAN ||
      binary[6] !== ELF_VERSION_CURRENT
    ) {
      fail(`${label} is not a current little-endian ELF64 binary`);
    }
    const machine = binary.readUInt16LE(ELF_MACHINE_OFFSET);
    if (machine !== ELF_MACHINE[target.goarch]) {
      fail(`${label} ELF machine ${machine} does not match ${target.goarch}`);
    }
    return;
  }

  if (binary.length < MACHO_MIN_HEADER_BYTES || binary.readUInt32LE(0) !== MACHO_MAGIC_64) {
    fail(`${label} is not a little-endian 64-bit Mach-O binary`);
  }
  const cpu = binary.readUInt32LE(MACHO_CPU_OFFSET);
  if (cpu !== MACHO_CPU[target.goarch]) {
    fail(`${label} Mach-O CPU ${cpu} does not match ${target.goarch}`);
  }
}

function validateAppArchiveContents({ archive, expectedBinary, target }) {
  const key = targetKey(target ?? {});
  if (!TARGET_KEY_RE.test(key)) fail(`application target ${key} is unsupported`);
  const members = parseTarGz(archive, {
    label: `${key} application archive`,
    limits: APP_ARCHIVE_LIMITS,
  });
  const actualPaths = [...members.keys()].sort();
  if (JSON.stringify(actualPaths) !== JSON.stringify(APP_ARCHIVE_FILES)) {
    fail(`${key} archive members are ${JSON.stringify(actualPaths)}, want exactly ${JSON.stringify(APP_ARCHIVE_FILES)}`);
  }
  for (const member of members.values()) {
    if (member.type !== "file") fail(`${key} archive member ${member.path} is not a regular file`);
  }
  const embedded = members.get(APP_BINARY);
  if ((embedded.mode & EXECUTABLE_MODE_MASK) === 0) {
    fail(`${key} embedded binary is not executable`);
  }
  if ((embedded.mode & SPECIAL_MODE_MASK) !== 0) {
    fail(`${key} embedded binary has set-id or sticky mode bits`);
  }
  if (expectedBinary && !embedded.data.equals(expectedBinary)) {
    fail(`${key} embedded binary differs from the selected GoReleaser binary`);
  }
  validateBinaryHeader(embedded.data, target, `${key} embedded binary`);
  return Object.freeze({
    target: key,
    members: Object.freeze(actualPaths),
    binaryBytes: embedded.size,
    externallyBound: Boolean(expectedBinary),
  });
}

export function validateAppArchive({ archive, binary, target }) {
  const key = targetKey(target ?? {});
  const expectedBinary = asBuffer(binary, `${key} GoReleaser binary`);
  return validateAppArchiveContents({ archive, expectedBinary, target });
}

/** Revalidate an already-sealed app archive when its standalone binary is absent. */
export function validatePackagedAppArchive({ archive, target }) {
  return validateAppArchiveContents({ archive, expectedBinary: null, target });
}

function exactTargetMap(value, label) {
  if (!(value instanceof Map)) fail(`${label} must be a Map keyed by operating-system/architecture`);
  const expected = RELEASE_TARGETS.map(targetKey).sort();
  const actual = [...value.keys()].sort();
  if (JSON.stringify(actual) !== JSON.stringify(expected)) {
    fail(`${label} targets are ${JSON.stringify(actual)}, want exactly ${JSON.stringify(expected)}`);
  }
  return value;
}

function stripRubyComment(line) {
  let quote = "";
  let escaped = false;
  for (let index = 0; index < line.length; index += 1) {
    const character = line[index];
    if (escaped) {
      escaped = false;
      continue;
    }
    if (quote && character === "\\") {
      escaped = true;
      continue;
    }
    if (character === '"' || character === "'") {
      if (!quote) quote = character;
      else if (quote === character) quote = "";
      continue;
    }
    if (character === "#" && !quote) return line.slice(0, index);
  }
  return line;
}

function currentCaskTarget(stack) {
  if (
    stack.length !== 3 ||
    stack[0].kind !== "cask" ||
    stack[1].kind !== "os" ||
    stack[2].kind !== "arch"
  ) {
    return null;
  }
  const operatingSystems = stack.filter((item) => item.kind === "os").map((item) => item.value);
  const architectures = stack.filter((item) => item.kind === "arch").map((item) => item.value);
  if (operatingSystems.length !== 1 || architectures.length !== 1) return null;
  return `${operatingSystems[0]}/${architectures[0]}`;
}

function parseCask(source) {
  const targets = new Map(
    RELEASE_TARGETS.map((target) => [targetKey(target), { sha256: [], url: [], order: [] }]),
  );
  const stack = [];
  const versions = [];
  const binaries = [];
  const installArtifacts = [];
  const casks = [];
  const problems = [];

  for (const [lineIndex, rawLine] of source.split(/\r?\n/).entries()) {
    const code = stripRubyComment(rawLine).trim();
    if (!code) continue;
    const line = lineIndex + 1;

    if (code === "end") {
      if (stack.length === 0) problems.push(`cask line ${line} closes no block`);
      else stack.pop();
      continue;
    }

    const cask = code.match(/^cask\s+"([^"]+)"\s+do$/);
    if (code.startsWith("cask ")) {
      if (!cask) problems.push(`cask line ${line} has an unsupported declaration`);
      else {
        casks.push(cask[1]);
        stack.push({ kind: "cask", value: cask[1] });
      }
      continue;
    }

    const os = code.match(/^on_(macos|linux)\s+do$/);
    if (os) {
      stack.push({ kind: "os", value: os[1] === "macos" ? "darwin" : "linux" });
      continue;
    }
    const arch = code.match(/^on_(intel|arm)\s+do$/);
    if (arch) {
      stack.push({ kind: "arch", value: arch[1] === "intel" ? "amd64" : "arm64" });
      continue;
    }

    if (code.startsWith("version ")) {
      const match = code.match(/^version\s+"([^"]+)"$/);
      if (!match) problems.push(`cask line ${line} has an unsupported version declaration`);
      else versions.push(match[1]);
      continue;
    }
    if (code.startsWith("binary")) {
      const match = code.match(/^binary\s+"([^"]+)"$/);
      if (!match) problems.push(`cask line ${line} has an unsupported binary stanza`);
      else {
        binaries.push(match[1]);
        installArtifacts.push("binary");
      }
      continue;
    }
    const installArtifact = code.match(CASK_ARTIFACT_RE);
    if (installArtifact) installArtifacts.push(installArtifact[1]);

    for (const [field, pattern] of [
      ["sha256", /^sha256\s+"([0-9a-f]+)"$/],
      ["url", /^url\s+"([^"]+)"$/],
    ]) {
      if (!code.startsWith(`${field} `)) continue;
      const match = code.match(pattern);
      const key = currentCaskTarget(stack);
      if (!match) problems.push(`cask line ${line} has a malformed ${field} stanza`);
      else if (!key || !targets.has(key)) problems.push(`cask line ${line} places ${field} outside one target block`);
      else {
        targets.get(key)[field].push(match[1]);
        targets.get(key).order.push(field);
      }
    }

    if (/\bdo(?:\s+\|[^|]*\|)?$/.test(code)) {
      stack.push({ kind: "other", value: code });
    } else if (/^(?:if|unless|case|begin|class|module|def)\b/.test(code)) {
      stack.push({ kind: "other", value: code });
    }
  }
  if (stack.length !== 0) problems.push(`cask has ${stack.length} unclosed Ruby block(s)`);
  return { targets, versions, binaries, casks, installArtifacts, problems };
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

export function validateCask({ source, version, appTag, sourceUrl, archives }) {
  requireVersion(version, "cask version");
  if (typeof appTag !== "string" || !APP_TAG_RE.test(appTag) || appTag !== `v${version}`) {
    fail(`cask application tag must exactly equal v plus the application version`);
  }
  const archiveMap = exactTargetMap(archives, "cask archives");
  const sourceBase = String(sourceUrl ?? "").replace(/\/+$/, "");
  if (!HTTPS_SOURCE_RE.test(sourceBase)) {
    fail(`cask source URL must be an HTTPS repository URL without query or fragment`);
  }
  const text = decodeUtf8(source, "generated cask");
  if (text.includes("\0")) fail(`generated cask contains a NUL byte`);
  const parsed = parseCask(text);
  const problems = [...parsed.problems];

  if (parsed.casks.length !== 1 || parsed.casks[0] !== CHART_NAME) {
    problems.push(`cask declaration is ${JSON.stringify(parsed.casks)}, want exactly ${JSON.stringify(CHART_NAME)}`);
  }
  if (parsed.versions.length !== 1 || parsed.versions[0] !== version) {
    problems.push(`cask version declarations are ${JSON.stringify(parsed.versions)}, want exactly ${JSON.stringify(version)}`);
  }
  if (parsed.binaries.length !== 1 || parsed.binaries[0] !== APP_BINARY) {
    problems.push(`cask binary stanzas are ${JSON.stringify(parsed.binaries)}, want exactly ${JSON.stringify(APP_BINARY)}`);
  }
  if (JSON.stringify(parsed.installArtifacts) !== JSON.stringify(["binary"])) {
    problems.push(`cask install artifacts are ${JSON.stringify(parsed.installArtifacts)}, want exactly one binary`);
  }
  if (caskPortabilityProblems(text).length > 0) {
    problems.push(`cask contains a platform-specific or manual-install construct`);
  }

  for (const target of RELEASE_TARGETS) {
    const key = targetKey(target);
    const record = parsed.targets.get(key);
    if (record.sha256.length !== 1 || !SHA256_RE.test(record.sha256[0] ?? "")) {
      problems.push(`${key} cask sha256 declarations are ${JSON.stringify(record.sha256)}, want one lowercase digest`);
    }
    if (record.url.length !== 1) {
      problems.push(`${key} cask URL declarations are ${JSON.stringify(record.url)}, want exactly one`);
    }
    if (JSON.stringify(record.order) !== JSON.stringify(["sha256", "url"])) {
      problems.push(`${key} cask target must contain one adjacent sha256 then URL pair`);
    }
    const archive = asBuffer(archiveMap.get(key), `${key} cask archive`);
    const wantedDigest = sha256(archive);
    if (record.sha256.length === 1 && record.sha256[0] !== wantedDigest) {
      problems.push(`${key} cask sha256 does not match the candidate archive`);
    }
    if (record.url.length === 1) {
      const expanded = record.url[0].replaceAll("#{version}", version);
      const expected = `${sourceBase}/releases/download/${appTag}/cerberus_${version}_${target.goos}_${target.goarch}.tar.gz`;
      if (expanded.includes("#{") || expanded !== expected) {
        problems.push(`${key} cask URL does not identify the expected tagged candidate archive`);
      }
    }
  }
  if (problems.length > 0) throw new ArtifactValidationError(problems);
  return Object.freeze({ version, targets: Object.freeze(RELEASE_TARGETS.map(targetKey)) });
}

function yamlScalar(raw, label) {
  const text = raw.trim();
  if (!text || text === "|" || text === ">") fail(`${label} must be an inline scalar`);
  if (text.startsWith('"')) {
    try {
      const value = JSON.parse(text);
      if (typeof value !== "string") fail(`${label} must be a string scalar`);
      return value;
    } catch (cause) {
      fail(`${label} has an invalid double-quoted scalar: ${cause.message}`);
    }
  }
  if (text.startsWith("'")) {
    if (!text.endsWith("'") || text.length < 2) fail(`${label} has an invalid single-quoted scalar`);
    return text.slice(1, -1).replaceAll("''", "'");
  }
  const withoutComment = text.replace(/\s+#.*$/, "").trim();
  if (!withoutComment || /^[!&*[{]|^(?:null|~)$/i.test(withoutComment)) {
    fail(`${label} must be a plain string scalar`);
  }
  return withoutComment;
}

function chartIdentity(source) {
  const wanted = new Set(["apiVersion", "name", "type", "version", "appVersion"]);
  const found = new Map();
  for (const [index, line] of source.split(/\r?\n/).entries()) {
    if (!line || /^\s/.test(line) || /^\s*#/.test(line)) continue;
    if (line === "---" || line === "...") continue;
    // Helm serialises top-level YAML sequences in indentationless form under
    // their preceding key (for example `keywords:\n- value`). They cannot
    // declare or override an identity key, so they are safe to ignore here.
    if (/^-\s/.test(line)) continue;
    const match = line.match(/^([A-Za-z][A-Za-z0-9_-]*)[ \t]*:(?:[ \t]*(.*))?$/);
    if (!match) fail(`Chart.yaml line ${index + 1} uses unsupported top-level YAML syntax`);
    if (match[1] === "<<") fail(`Chart.yaml top-level merge keys are unsupported`);
    if (!match || !wanted.has(match[1])) continue;
    if (found.has(match[1])) fail(`Chart.yaml top-level field ${match[1]} is duplicated`);
    found.set(match[1], yamlScalar(match[2] ?? "", `Chart.yaml ${match[1]}`));
  }
  for (const field of wanted) {
    if (!found.has(field)) fail(`Chart.yaml is missing unique top-level scalar ${field}`);
  }
  return found;
}

export function validateHelmChart({ archive, chartVersion, appVersion }) {
  requireVersion(chartVersion, "chart version");
  requireVersion(appVersion, "chart application version");
  const members = parseTarGz(archive, {
    label: "Helm chart package",
    limits: CHART_ARCHIVE_LIMITS,
  });
  const roots = new Set([...members.keys()].map((path) => path.split("/", 1)[0]));
  if (roots.size !== 1 || !roots.has(CHART_NAME)) {
    fail(`Helm chart roots are ${JSON.stringify([...roots].sort())}, want exactly ${JSON.stringify(CHART_NAME)}`);
  }
  for (const required of CHART_REQUIRED_FILES) {
    if (members.get(required)?.type !== "file") fail(`Helm chart is missing regular file ${required}`);
  }
  const hasTemplate = [...members.values()].some(
    (member) => member.type === "file" && member.path.startsWith(`${CHART_NAME}/templates/`),
  );
  if (!hasTemplate) fail(`Helm chart contains no regular template payload`);
  const chart = decodeUtf8(members.get(`${CHART_NAME}/Chart.yaml`).data, "Chart.yaml");
  const identity = chartIdentity(chart);
  for (const [field, expected] of Object.entries({
    ...CHART_REQUIRED_SCALARS,
    version: chartVersion,
    appVersion,
  })) {
    if (identity.get(field) !== expected) {
      fail(`Chart.yaml ${field} is ${JSON.stringify(identity.get(field))}, want ${JSON.stringify(expected)}`);
    }
  }
  return Object.freeze({ name: CHART_NAME, version: chartVersion, appVersion, members: members.size });
}

export function validateReleaseArtifactSet({
  version,
  chartVersion,
  appTag,
  sourceUrl,
  archives,
  binaries,
  cask,
  chart,
}) {
  requireVersion(version, "application version");
  requireVersion(chartVersion, "chart version");
  const archiveMap = exactTargetMap(archives, "application archives");
  const binaryMap = exactTargetMap(binaries, "application binaries");
  const application = RELEASE_TARGETS.map((target) =>
    validateAppArchive({
      archive: archiveMap.get(targetKey(target)),
      binary: binaryMap.get(targetKey(target)),
      target,
    }),
  );
  const caskResult = validateCask({
    source: cask,
    version,
    appTag,
    sourceUrl,
    archives: archiveMap,
  });
  const chartResult = validateHelmChart({ archive: chart, chartVersion, appVersion: version });
  return Object.freeze({
    application: Object.freeze(application),
    cask: caskResult,
    chart: chartResult,
  });
}

function readCandidateFile(path, label) {
  try {
    return readFileSync(path);
  } catch (cause) {
    fail(`${label} cannot be read: ${cause.message}`);
  }
}

/**
 * Revalidate the semantic packages inside an already-sealed candidate.
 * Standalone GoReleaser binaries are intentionally absent at this phase, so
 * app archives retain roster/mode/header checks while the stage-time wrapper
 * remains responsible for byte equality to the standalone build outputs.
 */
export function validateSealedCandidateArtifacts({
  root,
  version,
  chartVersion,
  appTag,
  sourceUrl,
}) {
  if (typeof root !== "string" || root.trim() === "") {
    fail(`sealed candidate root must be a non-empty path`);
  }
  requireVersion(version, "application version");
  requireVersion(chartVersion, "chart version");
  const candidateRoot = resolve(root);
  const archives = new Map();
  const application = [];
  for (const target of RELEASE_TARGETS) {
    const key = targetKey(target);
    const archive = readCandidateFile(
      join(
        candidateRoot,
        "app",
        "assets",
        `cerberus_${version}_${target.goos}_${target.goarch}.tar.gz`,
      ),
      `${key} sealed application archive`,
    );
    archives.set(key, archive);
    application.push(validatePackagedAppArchive({ archive, target }));
  }
  const cask = validateCask({
    source: readCandidateFile(
      join(candidateRoot, "app", "homebrew", "cerberus.rb"),
      "sealed generated cask",
    ),
    version,
    appTag,
    sourceUrl,
    archives,
  });
  const chart = validateHelmChart({
    archive: readCandidateFile(
      join(candidateRoot, "chart", `cerberus-${chartVersion}.tgz`),
      "sealed Helm chart package",
    ),
    chartVersion,
    appVersion: version,
  });
  return Object.freeze({
    application: Object.freeze(application),
    cask,
    chart,
  });
}
