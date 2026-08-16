// release-artifact-validator.test.mjs — real gzip/USTAR fixtures and negative
// controls for every package that can cross the publication boundary.

import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { gzipSync, gunzipSync } from "node:zlib";

import {
  ArtifactValidationError,
  RELEASE_TARGETS,
  parseTarGz,
  validateAppArchive,
  validateCask,
  validateHelmChart,
  validateReleaseArtifactSet,
  validateSealedCandidateArtifacts,
} from "./release-artifact-validator.mjs";

const VERSION = "1.2.3";
const APP_TAG = `v${VERSION}`;
const CHART_VERSION = "0.7.0";
const SOURCE_URL = "https://downloads.example.invalid/example/project";
const TAR_BLOCK_BYTES = 512;
const TAR_END_BLOCKS = 2;
const TAR_HEADER_BYTES = TAR_BLOCK_BYTES;
const TAR_CHECKSUM_START = 148;
const TAR_CHECKSUM_END = 156;
const TAR_TYPE_OFFSET = 156;
const TAR_LINK_START = 157;
const TAR_LINK_BYTES = 100;
const TAR_MAGIC_OFFSET = 257;
const TAR_VERSION_OFFSET = 263;
const TAR_PREFIX_OFFSET = 345;
const TAR_PREFIX_BYTES = 155;
const TAR_NAME_BYTES = 100;
const TAR_MODE_OFFSET = 100;
const TAR_UID_OFFSET = 108;
const TAR_GID_OFFSET = 116;
const TAR_SIZE_OFFSET = 124;
const TAR_MTIME_OFFSET = 136;
const TAR_MODE_BYTES = 8;
const TAR_ID_BYTES = 8;
const TAR_SIZE_BYTES = 12;
const TAR_MTIME_BYTES = 12;
const TAR_CHECKSUM_DIGITS = 6;
const TAR_CHECKSUM_SPACE = 0x20;

const targetKey = ({ goos, goarch }) => `${goos}/${goarch}`;
const bytes = (value) => (Buffer.isBuffer(value) ? value : Buffer.from(String(value)));
const digest = (value) => createHash("sha256").update(value).digest("hex");
const roundBlock = (size) => Math.ceil(size / TAR_BLOCK_BYTES) * TAR_BLOCK_BYTES;

function writeField(header, offset, length, value) {
  const encoded = Buffer.from(value, "utf8");
  assert.ok(encoded.length <= length, `${value} exceeds its USTAR field`);
  encoded.copy(header, offset);
}

function writeOctal(header, offset, length, value) {
  const encoded = value.toString(8).padStart(length - 1, "0");
  assert.ok(encoded.length <= length - 1, `${value} exceeds its USTAR octal field`);
  header.write(encoded, offset, length - 1, "ascii");
  header[offset + length - 1] = 0;
}

function splitUstarPath(path) {
  if (Buffer.byteLength(path) <= TAR_NAME_BYTES) return { name: path, prefix: "" };
  for (let index = path.lastIndexOf("/"); index > 0; index = path.lastIndexOf("/", index - 1)) {
    const prefix = path.slice(0, index);
    const name = path.slice(index + 1);
    if (
      Buffer.byteLength(prefix) <= TAR_PREFIX_BYTES &&
      Buffer.byteLength(name) <= TAR_NAME_BYTES
    ) {
      return { name, prefix };
    }
  }
  throw new Error(`fixture path cannot be represented by USTAR: ${path}`);
}

function tarHeader(entry) {
  const data = bytes(entry.data ?? "");
  const declaredSize = entry.declaredSize ?? data.length;
  const header = Buffer.alloc(TAR_HEADER_BYTES);
  const { name, prefix } = splitUstarPath(entry.path);
  writeField(header, 0, TAR_NAME_BYTES, name);
  writeOctal(header, TAR_MODE_OFFSET, TAR_MODE_BYTES, entry.mode ?? 0o644);
  writeOctal(header, TAR_UID_OFFSET, TAR_ID_BYTES, 0);
  writeOctal(header, TAR_GID_OFFSET, TAR_ID_BYTES, 0);
  writeOctal(header, TAR_SIZE_OFFSET, TAR_SIZE_BYTES, declaredSize);
  writeOctal(header, TAR_MTIME_OFFSET, TAR_MTIME_BYTES, 0);
  header.fill(TAR_CHECKSUM_SPACE, TAR_CHECKSUM_START, TAR_CHECKSUM_END);
  header[TAR_TYPE_OFFSET] = String(entry.type ?? "0").charCodeAt(0);
  if (entry.link) writeField(header, TAR_LINK_START, TAR_LINK_BYTES, entry.link);
  writeField(header, TAR_MAGIC_OFFSET, 6, "ustar\0");
  writeField(header, TAR_VERSION_OFFSET, 2, "00");
  if (prefix) writeField(header, TAR_PREFIX_OFFSET, TAR_PREFIX_BYTES, prefix);
  const checksum = header.reduce((sum, byte) => sum + byte, 0);
  const encodedChecksum = checksum.toString(8).padStart(TAR_CHECKSUM_DIGITS, "0");
  header.write(encodedChecksum, TAR_CHECKSUM_START, TAR_CHECKSUM_DIGITS, "ascii");
  header[TAR_CHECKSUM_START + TAR_CHECKSUM_DIGITS] = 0;
  header[TAR_CHECKSUM_END - 1] = TAR_CHECKSUM_SPACE;
  return { header, data, declaredSize };
}

function tarBytes(entries, { endBlocks = TAR_END_BLOCKS, trailing = Buffer.alloc(0) } = {}) {
  const parts = [];
  for (const entry of entries) {
    const built = tarHeader(entry);
    parts.push(built.header, built.data);
    const paddedFrom = entry.padToDeclared === false ? built.data.length : built.declaredSize;
    const padding = roundBlock(paddedFrom) - built.data.length;
    if (padding > 0) parts.push(Buffer.alloc(padding));
  }
  parts.push(Buffer.alloc(endBlocks * TAR_BLOCK_BYTES), trailing);
  return Buffer.concat(parts);
}

function tarGz(entries, options = {}) {
  return gzipSync(tarBytes(entries, options));
}

function executable(target, suffix = "") {
  const binary = Buffer.alloc(64);
  if (target.goos === "linux") {
    Buffer.from([0x7f, 0x45, 0x4c, 0x46]).copy(binary);
    binary[4] = 2;
    binary[5] = 1;
    binary[6] = 1;
    binary.writeUInt16LE(target.goarch === "amd64" ? 0x3e : 0xb7, 18);
  } else {
    binary.writeUInt32LE(0xfeedfacf, 0);
    binary.writeUInt32LE(target.goarch === "amd64" ? 0x01000007 : 0x0100000c, 4);
  }
  binary.write(suffix, 32, "utf8");
  return binary;
}

function applicationEntries(binary, { mode = 0o755, extra = [], omit = [] } = {}) {
  return [
    { path: "CHANGELOG.md", data: "changes" },
    { path: "LICENSE", data: "license" },
    { path: "README.md", data: "readme" },
    { path: "cerberus", data: binary, mode },
    ...extra,
  ].filter((entry) => !omit.includes(entry.path));
}

function releaseMaps() {
  const binaries = new Map();
  const archives = new Map();
  for (const target of RELEASE_TARGETS) {
    const key = targetKey(target);
    const binary = executable(target, key);
    binaries.set(key, binary);
    archives.set(key, tarGz(applicationEntries(binary)));
  }
  return { binaries, archives };
}

function caskSource(
  archives,
  {
    version = VERSION,
    appTag = APP_TAG,
    targets = RELEASE_TARGETS,
    urlFor = null,
    digestFor = null,
    reversePair = false,
    binary = "cerberus",
    beforeBinary = [],
    extraBinary = [],
  } = {},
) {
  const groups = new Map([
    ["darwin", []],
    ["linux", []],
  ]);
  for (const target of targets) groups.get(target.goos).push(target);
  const lines = ['cask "cerberus" do', `  version "${version}"`, ""];
  for (const goos of ["darwin", "linux"]) {
    lines.push(`  on_${goos === "darwin" ? "macos" : "linux"} do`);
    for (const target of groups.get(goos)) {
      const key = targetKey(target);
      const sha = digestFor ? digestFor(target) : digest(archives.get(key));
      const url = urlFor
        ? urlFor(target)
        : `${SOURCE_URL}/releases/download/${appTag}/cerberus_#{version}_${target.goos}_${target.goarch}.tar.gz`;
      const pair = [`      sha256 "${sha}"`, `      url "${url}"`];
      lines.push(
        `    on_${target.goarch === "amd64" ? "intel" : "arm"} do`,
        ...(reversePair ? pair.reverse() : pair),
        "    end",
      );
    }
    lines.push("  end", "");
  }
  lines.push(...beforeBinary, `  binary "${binary}"`, ...extraBinary, "end", "");
  return Buffer.from(lines.join("\n"));
}

function chartEntries(chartYaml = null, extra = []) {
  const metadata =
    chartYaml ??
    [
      "apiVersion: v2",
      "name: cerberus",
      "description: test chart",
      "type: application",
      `version: ${CHART_VERSION}`,
      `appVersion: '${VERSION}'`,
      "annotations:",
      "  example.invalid/note: value",
      "",
    ].join("\n");
  return [
    { path: "cerberus/Chart.yaml", data: metadata },
    { path: "cerberus/values.yaml", data: "replicas: 1\n" },
    { path: "cerberus/values.schema.json", data: '{}\n' },
    { path: "cerberus/templates/deployment.yaml", data: "kind: Deployment\n" },
    ...extra,
  ];
}

function chartArchive(chartYaml = null, extra = []) {
  return tarGz(chartEntries(chartYaml, extra));
}

function fullFixture() {
  const maps = releaseMaps();
  return {
    version: VERSION,
    chartVersion: CHART_VERSION,
    appTag: APP_TAG,
    sourceUrl: SOURCE_URL,
    ...maps,
    cask: caskSource(maps.archives),
    chart: chartArchive(),
  };
}

function writeCandidate(root, fixture) {
  mkdirSync(join(root, "app", "assets"), { recursive: true });
  mkdirSync(join(root, "app", "homebrew"), { recursive: true });
  mkdirSync(join(root, "chart"), { recursive: true });
  for (const target of RELEASE_TARGETS) {
    writeFileSync(
      join(
        root,
        "app",
        "assets",
        `cerberus_${fixture.version}_${target.goos}_${target.goarch}.tar.gz`,
      ),
      fixture.archives.get(targetKey(target)),
    );
  }
  writeFileSync(join(root, "app", "homebrew", "cerberus.rb"), fixture.cask);
  writeFileSync(join(root, "chart", `cerberus-${fixture.chartVersion}.tgz`), fixture.chart);
}

function assertInvalid(call, pattern = /release artifacts are invalid/) {
  assert.throws(
    call,
    (error) => error instanceof ArtifactValidationError && pattern.test(error.message),
  );
}

test("the complete unpublished package set is semantically bound", () => {
  const result = validateReleaseArtifactSet(fullFixture());
  assert.deepEqual(
    result.application.map((item) => item.target),
    RELEASE_TARGETS.map(targetKey),
  );
  assert.ok(result.application.every((item) => item.externallyBound));
  assert.equal(result.cask.version, VERSION);
  assert.equal(result.chart.version, CHART_VERSION);
  assert.equal(result.chart.appVersion, VERSION);
});

test("the tar reader accepts a real gzip with a USTAR prefix field", () => {
  const longPath = `cerberus/${"nested/".repeat(15)}payload.yaml`;
  const members = parseTarGz(tarGz([{ path: longPath, data: "payload" }]));
  assert.equal(members.get(longPath).data.toString(), "payload");
});

test("the tar reader rejects malformed gzip and tar structure", () => {
  const valid = tarGz([{ path: "file", data: "x" }]);
  const badChecksum = gunzipSync(valid);
  badChecksum[0] ^= 1;
  const badPadding = gunzipSync(valid);
  badPadding[TAR_HEADER_BYTES + 1] = 1;
  const malformed = [
    ["invalid gzip", Buffer.from("not gzip"), /valid gzip/],
    ["truncated gzip", valid.subarray(0, valid.length - 8), /valid gzip/],
    ["non-block tar", gzipSync(Buffer.from("x")), /multiple/],
    ["bad checksum", gzipSync(badChecksum), /checksum/],
    ["one end block", tarGz([{ path: "file", data: "x" }], { endBlocks: 1 }), /one tar end block/],
    [
      "trailing data",
      tarGz([{ path: "file", data: "x" }], { trailing: Buffer.alloc(TAR_BLOCK_BYTES, 1) }),
      /after its tar end marker/,
    ],
    ["non-zero padding", gzipSync(badPadding), /padding/],
    [
      "body out of bounds",
      tarGz([{ path: "file", data: "x", declaredSize: 8192, padToDeclared: false }]),
      /body exceeds/,
    ],
  ];
  for (const [name, archive, pattern] of malformed) {
    assertInvalid(() => parseTarGz(archive, { label: name }), pattern);
  }
});

test("the tar reader rejects unsafe paths, duplicates, links, and special records", () => {
  const cases = [
    ["traversal", [{ path: "../escape", data: "x" }], /unsafe path/],
    ["absolute", [{ path: "/escape", data: "x" }], /unsafe path/],
    ["backslash", [{ path: "dir\\escape", data: "x" }], /unsafe path/],
    [
      "duplicate",
      [
        { path: "same", data: "one" },
        { path: "same", data: "two" },
      ],
      /duplicate canonical path/,
    ],
    ["symlink", [{ path: "link", type: "2", link: "target" }], /unsupported tar type/],
    ["device", [{ path: "device", type: "3" }], /unsupported tar type/],
    ["FIFO", [{ path: "fifo", type: "6" }], /unsupported tar type/],
    ["PAX", [{ path: "pax", type: "x", data: "path=value\n" }], /unsupported tar type/],
    ["GNU long name", [{ path: "long", type: "L", data: "name" }], /unsupported tar type/],
  ];
  for (const [name, entries, pattern] of cases) {
    assertInvalid(() => parseTarGz(tarGz(entries), { label: name }), pattern);
  }
});

test("the tar reader enforces compressed, expanded, member, and count ceilings", () => {
  const archive = tarGz([
    { path: "one", data: "1234" },
    { path: "two", data: "5678" },
  ]);
  for (const [limits, pattern] of [
    [{ maxCompressedBytes: 1 }, /compressed size/],
    [{ maxExpandedBytes: TAR_BLOCK_BYTES }, /bounded valid gzip/],
    [{ maxMemberBytes: 1 }, /member 1 size/],
    [{ maxMembers: 1 }, /more than 1 members/],
  ]) {
    assertInvalid(() => parseTarGz(archive, { limits }), pattern);
  }
});

test("application archives require the exact roster and executable binary identity", () => {
  const target = { goos: "linux", goarch: "amd64" };
  const binary = executable(target);
  const good = () => tarGz(applicationEntries(binary));
  assert.equal(validateAppArchive({ archive: good(), binary, target }).target, "linux/amd64");

  const faults = [
    ["missing", tarGz(applicationEntries(binary, { omit: ["README.md"] })), binary, /members/],
    [
      "extra",
      tarGz(applicationEntries(binary, { extra: [{ path: "extra", data: "x" }] })),
      binary,
      /members/,
    ],
    ["non-executable", tarGz(applicationEntries(binary, { mode: 0o644 })), binary, /not executable/],
    ["special mode", tarGz(applicationEntries(binary, { mode: 0o4755 })), binary, /set-id/],
    ["different selected bytes", good(), executable(target, "different"), /differs/],
    ["wrong header", tarGz(applicationEntries(Buffer.alloc(64))), Buffer.alloc(64), /not an ELF/],
    [
      "wrong architecture",
      tarGz(applicationEntries(executable({ goos: "linux", goarch: "arm64" }))),
      executable({ goos: "linux", goarch: "arm64" }),
      /does not match amd64/,
    ],
  ];
  for (const [name, archive, selected, pattern] of faults) {
    assertInvalid(() => validateAppArchive({ archive, binary: selected, target }), pattern);
  }
});

test("all four executable headers are accepted only for their declared tuple", () => {
  for (const target of RELEASE_TARGETS) {
    const binary = executable(target);
    assert.doesNotThrow(() =>
      validateAppArchive({ archive: tarGz(applicationEntries(binary)), binary, target }),
    );
  }
});

test("the generated cask binds one explicit version and all four archive pairs", () => {
  const { archives } = releaseMaps();
  const source = caskSource(archives);
  const result = validateCask({
    source,
    version: VERSION,
    appTag: APP_TAG,
    sourceUrl: SOURCE_URL,
    archives,
  });
  assert.deepEqual(result.targets, RELEASE_TARGETS.map(targetKey));
});

test("the generated cask rejects version, target, URL, checksum, and binary drift", () => {
  const { archives } = releaseMaps();
  const valid = caskSource(archives).toString();
  const otherKey = "darwin/arm64";
  const faults = [
    ["missing version", valid.replace(`  version "${VERSION}"\n`, ""), /version declarations/],
    [
      "duplicate version",
      valid.replace(`  version "${VERSION}"`, `  version "${VERSION}"\n  version "${VERSION}"`),
      /version declarations/,
    ],
    ["wrong version", valid.replace(VERSION, "9.9.9"), /version declarations/],
    [
      "missing target",
      caskSource(archives, { targets: RELEASE_TARGETS.slice(1) }),
      /darwin\/amd64.*sha256 declarations/,
    ],
    [
      "wrong tag URL",
      valid.replace(`/releases/download/${APP_TAG}/`, "/releases/download/v9.9.9/"),
      /URL does not identify/,
    ],
    [
      "swapped checksum",
      caskSource(archives, {
        digestFor: (target) =>
          targetKey(target) === "darwin/amd64"
            ? digest(archives.get(otherKey))
            : digest(archives.get(targetKey(target))),
      }),
      /sha256 does not match/,
    ],
    ["wrong binary", caskSource(archives, { binary: "other" }), /binary stanzas/],
    [
      "extra binary",
      caskSource(archives, { extraBinary: ['  binary "other"'] }),
      /binary stanzas/,
    ],
    ["reversed pair", caskSource(archives, { reversePair: true }), /adjacent sha256 then URL/],
  ];
  for (const [name, source, pattern] of faults) {
    assertInvalid(
      () =>
        validateCask({
          source: bytes(source),
          version: VERSION,
          appTag: APP_TAG,
          sourceUrl: SOURCE_URL,
          archives,
        }),
      pattern,
    );
  }
});

test("the generated cask rejects platform-only and manual install constructs", () => {
  const { archives } = releaseMaps();
  for (const construct of [
    '  depends_on macos: ">= :ventura"',
    '  app "Project.app"',
    '  installer manual: "Project.app"',
  ]) {
    assertInvalid(
      () =>
        validateCask({
          source: caskSource(archives, { beforeBinary: [construct] }),
          version: VERSION,
          appTag: APP_TAG,
          sourceUrl: SOURCE_URL,
          archives,
        }),
      /platform-specific or manual-install/,
    );
  }
});

test("the generated cask rejects every non-binary install artifact", () => {
  const { archives } = releaseMaps();
  for (const construct of [
    '  installer script: { executable: "setup" }',
    '  artifact "payload"',
    '  manpage "project.1"',
  ]) {
    assertInvalid(
      () =>
        validateCask({
          source: caskSource(archives, { beforeBinary: [construct] }),
          version: VERSION,
          appTag: APP_TAG,
          sourceUrl: SOURCE_URL,
          archives,
        }),
      /install artifacts/,
    );
  }
});

test("the generated cask requires fatal UTF-8 and independent tag identity", () => {
  const { archives } = releaseMaps();
  assertInvalid(
    () =>
      validateCask({
        source: Buffer.from([0xff]),
        version: VERSION,
        appTag: APP_TAG,
        sourceUrl: SOURCE_URL,
        archives,
      }),
    /valid UTF-8/,
  );
  assertInvalid(
    () =>
      validateCask({
        source: caskSource(archives),
        version: VERSION,
        appTag: "v1.2.2",
        sourceUrl: SOURCE_URL,
        archives,
      }),
    /exactly equal/,
  );
});

test("the Helm package requires one canonical root and real payload", () => {
  const result = validateHelmChart({
    archive: chartArchive(),
    chartVersion: CHART_VERSION,
    appVersion: VERSION,
  });
  assert.deepEqual(
    { name: result.name, version: result.version, appVersion: result.appVersion },
    { name: "cerberus", version: CHART_VERSION, appVersion: VERSION },
  );
});

test("the Helm package rejects roots, missing payload, duplicate metadata, and bad UTF-8", () => {
  const baseYaml = chartEntries()[0].data;
  const faults = [
    ["multiple roots", chartArchive(null, [{ path: "other/file", data: "x" }]), /roots/],
    ["missing Chart.yaml", tarGz(chartEntries().slice(1)), /Chart.yaml/],
    [
      "duplicate Chart.yaml",
      tarGz([...chartEntries(), { path: "cerberus/Chart.yaml", data: baseYaml }]),
      /duplicate canonical path/,
    ],
    [
      "missing values",
      tarGz(chartEntries().filter((entry) => entry.path !== "cerberus/values.yaml")),
      /values.yaml/,
    ],
    [
      "missing schema",
      tarGz(chartEntries().filter((entry) => entry.path !== "cerberus/values.schema.json")),
      /values.schema.json/,
    ],
    [
      "missing templates",
      tarGz(chartEntries().filter((entry) => !entry.path.startsWith("cerberus/templates/"))),
      /template payload/,
    ],
    [
      "duplicate identity",
      chartArchive(`${baseYaml}\nname: cerberus\n`),
      /field name is duplicated/,
    ],
    [
      "invalid UTF-8",
      tarGz(chartEntries().map((entry) =>
        entry.path === "cerberus/Chart.yaml" ? { ...entry, data: Buffer.from([0xff]) } : entry,
      )),
      /valid UTF-8/,
    ],
  ];
  for (const [name, archive, pattern] of faults) {
    assertInvalid(
      () => validateHelmChart({ archive, chartVersion: CHART_VERSION, appVersion: VERSION }),
      pattern,
    );
  }
});

test("the Helm package pins every publication identity scalar", () => {
  const base = chartEntries()[0].data;
  for (const [field, replacement] of [
    ["apiVersion", "v1"],
    ["name", "other"],
    ["type", "library"],
    ["version", "0.6.9"],
    ["appVersion", "1.2.2"],
  ]) {
    const yaml = base.replace(new RegExp(`^${field}:.*$`, "m"), `${field}: ${replacement}`);
    assertInvalid(
      () =>
        validateHelmChart({
          archive: chartArchive(yaml),
          chartVersion: CHART_VERSION,
          appVersion: VERSION,
        }),
      new RegExp(`Chart.yaml ${field}`),
    );
  }
});

test("the release wrapper requires exactly four archive and binary targets", () => {
  for (const field of ["archives", "binaries"]) {
    const fixture = fullFixture();
    fixture[field] = new Map(fixture[field]);
    fixture[field].delete("darwin/amd64");
    assertInvalid(() => validateReleaseArtifactSet(fixture), new RegExp(`${field} targets`));
  }
  const fixture = fullFixture();
  fixture.archives = new Map(fixture.archives).set("linux/riscv64", Buffer.from("extra"));
  assertInvalid(() => validateReleaseArtifactSet(fixture), /application archives targets/);
});

test("the sealed-candidate wrapper revalidates package bytes without standalone binaries", (t) => {
  const root = mkdtempSync(join(tmpdir(), "release-artifact-validator-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const fixture = fullFixture();
  writeCandidate(root, fixture);
  const result = validateSealedCandidateArtifacts({
    root,
    version: VERSION,
    chartVersion: CHART_VERSION,
    appTag: APP_TAG,
    sourceUrl: SOURCE_URL,
  });
  assert.ok(result.application.every((item) => !item.externallyBound));

  const changedTarget = RELEASE_TARGETS[0];
  const changedKey = targetKey(changedTarget);
  const changedBinary = executable(changedTarget, "changed after sealing");
  writeFileSync(
    join(
      root,
      "app",
      "assets",
      `cerberus_${VERSION}_${changedTarget.goos}_${changedTarget.goarch}.tar.gz`,
    ),
    tarGz(applicationEntries(changedBinary)),
  );
  assert.notEqual(digest(fixture.archives.get(changedKey)), digest(tarGz(applicationEntries(changedBinary))));
  assertInvalid(
    () =>
      validateSealedCandidateArtifacts({
        root,
        version: VERSION,
        chartVersion: CHART_VERSION,
        appTag: APP_TAG,
        sourceUrl: SOURCE_URL,
      }),
    /cask sha256 does not match/,
  );
});

test("the sealed-candidate wrapper fails closed on missing or malformed package files", (t) => {
  const root = mkdtempSync(join(tmpdir(), "release-artifact-validator-"));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const fixture = fullFixture();
  writeCandidate(root, fixture);
  rmSync(join(root, "chart", `cerberus-${CHART_VERSION}.tgz`));
  assertInvalid(
    () =>
      validateSealedCandidateArtifacts({
        root,
        version: VERSION,
        chartVersion: CHART_VERSION,
        appTag: APP_TAG,
        sourceUrl: SOURCE_URL,
      }),
    /cannot be read/,
  );
});
