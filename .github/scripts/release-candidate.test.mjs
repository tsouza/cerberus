import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";
import { tmpdir } from "node:os";
import { afterEach, test } from "node:test";
import { gzipSync } from "node:zlib";

import {
  CANDIDATE_MANIFEST,
  CandidateError,
  candidateImageBuildInvocation,
  canonicalJSON,
  releaseArtifacts,
  sha256Bytes,
  sha256File,
  validateSnapshotMetadata,
  validateReleaseChecksums,
  verifyCheckout,
  verifyCandidate,
} from "./release-candidate.mjs";

const SHA = "a".repeat(40);
const TREE = "b".repeat(40);
const SOURCE_URL = "https://downloads.example.invalid/example/project";
const goreleaserConfig = new URL("../../.goreleaser.yml", import.meta.url);
const roots = [];

const tarBlockBytes = 512;

function tarOctal(header, offset, length, value) {
  header.write(value.toString(8).padStart(length - 1, "0"), offset, length - 1, "ascii");
}

function tarGz(entries) {
  const parts = [];
  for (const entry of entries) {
    const data = Buffer.isBuffer(entry.data) ? entry.data : Buffer.from(entry.data);
    const header = Buffer.alloc(tarBlockBytes);
    header.write(entry.path, 0, 100, "utf8");
    tarOctal(header, 100, 8, entry.mode ?? 0o644);
    tarOctal(header, 108, 8, 0);
    tarOctal(header, 116, 8, 0);
    tarOctal(header, 124, 12, data.length);
    tarOctal(header, 136, 12, 0);
    header.fill(0x20, 148, 156);
    header[156] = "0".charCodeAt(0);
    header.write("ustar\0", 257, 6, "binary");
    header.write("00", 263, 2, "ascii");
    const checksum = header.reduce((sum, byte) => sum + byte, 0);
    header.write(checksum.toString(8).padStart(6, "0"), 148, 6, "ascii");
    header[154] = 0;
    header[155] = 0x20;
    parts.push(header, data);
    const padding = Math.ceil(data.length / tarBlockBytes) * tarBlockBytes - data.length;
    if (padding > 0) parts.push(Buffer.alloc(padding));
  }
  parts.push(Buffer.alloc(2 * tarBlockBytes));
  return gzipSync(Buffer.concat(parts));
}

function executable(goos, goarch) {
  const binary = Buffer.alloc(64);
  if (goos === "linux") {
    Buffer.from([0x7f, 0x45, 0x4c, 0x46]).copy(binary);
    binary[4] = 2;
    binary[5] = 1;
    binary[6] = 1;
    binary.writeUInt16LE(goarch === "amd64" ? 0x3e : 0xb7, 18);
  } else {
    binary.writeUInt32LE(0xfeedfacf, 0);
    binary.writeUInt32LE(goarch === "amd64" ? 0x01000007 : 0x0100000c, 4);
  }
  binary.write(`${goos}/${goarch}`, 32, "utf8");
  return binary;
}

function applicationArchive(goos, goarch) {
  return tarGz([
    { path: "CHANGELOG.md", data: "changes" },
    { path: "LICENSE", data: "license" },
    { path: "README.md", data: "readme" },
    { path: "cerberus", data: executable(goos, goarch), mode: 0o755 },
  ]);
}

function chartArchive() {
  return tarGz([
    {
      path: "cerberus/Chart.yaml",
      data: "apiVersion: v2\nname: cerberus\ntype: application\nversion: 4.5.6\nappVersion: '1.2.3'\n",
    },
    { path: "cerberus/values.yaml", data: "replicas: 1\n" },
    { path: "cerberus/values.schema.json", data: "{}\n" },
    { path: "cerberus/templates/deployment.yaml", data: "kind: Deployment\n" },
  ]);
}

function generatedCask(archives) {
  const lines = ['cask "cerberus" do', '  version "1.2.3"', ""];
  for (const goos of ["darwin", "linux"]) {
    lines.push(`  on_${goos === "darwin" ? "macos" : "linux"} do`);
    for (const goarch of ["amd64", "arm64"]) {
      const key = `${goos}/${goarch}`;
      lines.push(
        `    on_${goarch === "amd64" ? "intel" : "arm"} do`,
        `      sha256 "${sha256Bytes(archives.get(key)).slice("sha256:".length)}"`,
        `      url "${SOURCE_URL}/releases/download/v1.2.3/cerberus_#{version}_${goos}_${goarch}.tar.gz"`,
        "    end",
      );
    }
    lines.push("  end", "");
  }
  lines.push('  binary "cerberus"', "end", "");
  return Buffer.from(lines.join("\n"));
}

afterEach(() => {
  while (roots.length > 0) rmSync(roots.pop(), { recursive: true, force: true });
});

function tempRoot() {
  const root = mkdtempSync(join(tmpdir(), "release-candidate-test-"));
  roots.push(root);
  return root;
}

function write(root, path, body) {
  const absolute = join(root, path);
  mkdirSync(join(absolute, ".."), { recursive: true });
  writeFileSync(absolute, body);
  return absolute;
}

function git(root, args) {
  const result = spawnSync("git", args, { cwd: root, encoding: "utf8" });
  assert.equal(result.status, 0, `git ${args.join(" ")} failed: ${result.stderr}`);
  return result.stdout.trim();
}

function jsonBody(value) {
  return `${canonicalJSON(value)}\n`;
}

function candidateImageLayout(options = {}) {
  const blobs = new Map();
  const addBlob = (body, mediaType, extra = {}) => {
    const descriptor = {
      mediaType,
      digest: sha256Bytes(body),
      size: Buffer.byteLength(body),
      ...extra,
    };
    blobs.set(`app/image/blobs/sha256/${descriptor.digest.slice("sha256:".length)}`, body);
    return descriptor;
  };
  const imageLayer = "candidate-layer";
  const imageLayerDescriptor = addBlob(
    imageLayer,
    "application/vnd.oci.image.layer.v1.tar",
  );
  const imageDescriptors = (options.architectures ?? ["amd64", "arm64"]).map(
    (architecture) => {
      const config = {
        architecture,
        os: "linux",
        config: {
          Entrypoint: ["/usr/local/bin/cerberus"],
          Labels: {
            "org.opencontainers.image.title": "cerberus",
            "org.opencontainers.image.description":
              "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse",
            "org.opencontainers.image.url": SOURCE_URL,
            "org.opencontainers.image.source": SOURCE_URL,
            "org.opencontainers.image.licenses": "Apache-2.0",
            "org.opencontainers.image.version": "1.2.3",
            "org.opencontainers.image.revision": SHA,
          },
        },
        rootfs: { type: "layers", diff_ids: [sha256Bytes(imageLayer)] },
      };
      options.configMutation?.(config, architecture);
      const configDescriptor = addBlob(
        jsonBody(config),
        "application/vnd.oci.image.config.v1+json",
      );
      const manifest = {
        schemaVersion: 2,
        mediaType: "application/vnd.oci.image.manifest.v1+json",
        config: configDescriptor,
        layers: [imageLayerDescriptor],
      };
      options.imageManifestMutation?.(manifest, architecture);
      const descriptor = addBlob(
        jsonBody(manifest),
        "application/vnd.oci.image.manifest.v1+json",
        { platform: { os: "linux", architecture } },
      );
      options.imageDescriptorMutation?.(descriptor, architecture);
      return descriptor;
    },
  );

  let attestationConfig;
  const attestationDescriptors = [];
  for (const imageDescriptor of imageDescriptors) {
    const architecture = imageDescriptor.platform.architecture;
    if (options.includeAttestation?.(architecture) === false) continue;
    attestationConfig ??= addBlob(
      jsonBody({
        architecture: "unknown",
        os: "unknown",
        config: {},
        rootfs: { type: "layers", diff_ids: [] },
      }),
      "application/vnd.oci.image.config.v1+json",
    );
    const subject = [
      {
        name: "_",
        digest: { sha256: imageDescriptor.digest.slice("sha256:".length) },
      },
    ];
    const spdx = {
      _type: "https://in-toto.io/Statement/v0.1",
      subject: structuredClone(subject),
      predicateType: "https://spdx.dev/Document",
      predicate: {
        spdxVersion: "SPDX-2.3",
        dataLicense: "CC0-1.0",
        SPDXID: "SPDXRef-DOCUMENT",
        name: `cerberus-linux-${architecture}`,
        documentNamespace: `https://example.invalid/spdx/cerberus-linux-${architecture}`,
        creationInfo: {
          created: "2026-08-16T00:00:00Z",
          creators: ["Tool: buildkit-syft-scanner"],
        },
        packages: [{ name: "cerberus", SPDXID: "SPDXRef-Package-cerberus" }],
      },
    };
    const provenance = {
      _type: "https://in-toto.io/Statement/v0.1",
      subject: structuredClone(subject),
      predicateType: "https://slsa.dev/provenance/v0.2",
      predicate: {
        builder: { id: "https://github.com/docker/buildx@test" },
        buildType: "https://mobyproject.org/buildkit@v1",
        invocation: {
          parameters: {
            frontend: "dockerfile.v0",
            args: {
              "build-arg:RELEASE_VERSION": "1.2.3",
              "build-arg:SOURCE_SHA": SHA,
              "build-arg:SOURCE_URL": SOURCE_URL,
            },
          },
          environment: { platform: `linux/${architecture}` },
        },
        materials: [
          {
            uri: "pkg:docker/gcr.io/distroless/static-debian12@nonroot",
            digest: { sha256: "c".repeat(64) },
          },
        ],
        metadata: {
          completeness: { parameters: true, environment: true, materials: true },
        },
      },
    };
    options.spdxMutation?.(spdx, architecture, imageDescriptor);
    options.provenanceMutation?.(provenance, architecture, imageDescriptor);
    const statements = [
      { predicateType: "https://spdx.dev/Document", statement: spdx },
      { predicateType: "https://slsa.dev/provenance/v0.2", statement: provenance },
    ];
    options.statementsMutation?.(statements, architecture, imageDescriptor);
    const layers = statements.map(({ predicateType, statement }) =>
      addBlob(jsonBody(statement), "application/vnd.in-toto+json", {
        annotations: { "in-toto.io/predicate-type": predicateType },
      }),
    );
    const manifest = {
      schemaVersion: 2,
      mediaType: "application/vnd.oci.image.manifest.v1+json",
      config: attestationConfig,
      layers,
    };
    options.attestationManifestMutation?.(manifest, architecture, imageDescriptor);
    const descriptor = addBlob(
      jsonBody(manifest),
      "application/vnd.oci.image.manifest.v1+json",
      {
        platform: { os: "unknown", architecture: "unknown" },
        annotations: {
          "vnd.docker.reference.digest": imageDescriptor.digest,
          "vnd.docker.reference.type": "attestation-manifest",
        },
      },
    );
    options.attestationDescriptorMutation?.(descriptor, architecture, imageDescriptor);
    attestationDescriptors.push(descriptor);
  }
  const innerDescriptors = [...imageDescriptors, ...attestationDescriptors];
  options.innerDescriptorsMutation?.(innerDescriptors, {
    images: imageDescriptors,
    attestations: attestationDescriptors,
  });
  const imageIndexDocument = {
    schemaVersion: 2,
    mediaType: "application/vnd.oci.image.index.v1+json",
    manifests: innerDescriptors,
  };
  options.imageIndexMutation?.(imageIndexDocument);
  const imageIndex = jsonBody(imageIndexDocument);
  const imageDescriptor = addBlob(
    imageIndex,
    "application/vnd.oci.image.index.v1+json",
    {
      annotations: {
        "io.containerd.image.name": "docker.io/library/cerberus:1.2.3",
        "org.opencontainers.image.ref.name": "1.2.3",
      },
    },
  );
  options.rootDescriptorMutation?.(imageDescriptor);
  const rootIndexDocument = {
    schemaVersion: 2,
    mediaType: "application/vnd.oci.image.index.v1+json",
    manifests: [imageDescriptor],
  };
  options.rootIndexMutation?.(rootIndexDocument);
  return {
    imageDigest: imageDescriptor.digest,
    payloads: new Map([
      ["app/image/index.json", jsonBody(rootIndexDocument)],
      ["app/image/oci-layout", '{"imageLayoutVersion":"1.0.0"}\n'],
      ...blobs,
    ]),
  };
}

function sealedCandidate(imageOptions = {}) {
  const root = tempRoot();
  const { imageDigest, payloads: imagePayloads } = candidateImageLayout(imageOptions);
  const archivesByTarget = new Map(
    ["darwin", "linux"].flatMap((goos) =>
      ["amd64", "arm64"].map((goarch) => [
        `${goos}/${goarch}`,
        applicationArchive(goos, goarch),
      ]),
    ),
  );
  const archives = [...archivesByTarget].map(([target, body]) => {
    const [goos, goarch] = target.split("/");
    return [`app/assets/cerberus_1.2.3_${goos}_${goarch}.tar.gz`, body];
  });
  const checksums = `${archives
    .map(([path, body]) => `${sha256Bytes(body).slice(7)}  ${basename(path)}`)
    .join("\n")}\n`;
  const payloads = new Map([
    ["app/assets/checksums.txt", checksums],
    ...archives,
    ["app/homebrew/cerberus.rb", generatedCask(archivesByTarget)],
    ...imagePayloads,
    ["chart/cerberus-4.5.6.tgz", chartArchive()],
    ["chart/artifacthub-config.yml", ""],
    ["chart/artifacthub-repo.yml", "repositoryID: candidate\n"],
    ["source.json", `${canonicalJSON({ app_tag: "v1.2.3", app_version: "1.2.3", sha: SHA, source_url: SOURCE_URL, tree: TREE })}\n`],
  ]);
  for (const [path, body] of payloads) write(root, path, body);
  const files = [...payloads.keys()].sort().map((path) => {
    const absolute = join(root, path);
    return {
      path,
      size: Buffer.byteLength(payloads.get(path)),
      sha256: sha256File(absolute),
    };
  });
  const manifest = {
    schema_version: 1,
    source: { sha: SHA, tree: TREE, url: SOURCE_URL },
    versions: { app: "1.2.3", app_tag: "v1.2.3", chart: "4.5.6" },
    run: { id: "123", attempt: 2 },
    files,
    image: {
      layout: "app/image",
      digest: imageDigest,
      platforms: ["linux/amd64", "linux/arm64"],
    },
  };
  const manifestPath = write(root, CANDIDATE_MANIFEST, `${canonicalJSON(manifest)}\n`);
  return { root, manifest, digest: sha256File(manifestPath) };
}

function reseal(candidate) {
  candidate.manifest.files = candidate.manifest.files.map((file) => {
    const absolute = join(candidate.root, file.path);
    return { ...file, size: statSync(absolute).size, sha256: sha256File(absolute) };
  });
  writeFileSync(
    join(candidate.root, CANDIDATE_MANIFEST),
    `${canonicalJSON(candidate.manifest)}\n`,
  );
}

test("a sealed candidate verifies against the exact source and versions", () => {
  const candidate = sealedCandidate();
  const result = verifyCandidate(candidate.root, {
    sha: SHA,
    tree: TREE,
    app: "1.2.3",
    appTag: "v1.2.3",
    chart: "4.5.6",
    digest: candidate.digest,
  });
  assert.equal(result.digest, candidate.digest);
  assert.deepEqual(result.manifest, candidate.manifest);
});

test("candidate image build writes an attested two-platform OCI layout without publishing", () => {
  const root = "/checkout";
  const context = "/checkout/build/release-image-context";
  const candidateRoot = "/checkout/build/release-candidate";
  const sourceURL = "https://example.invalid/example/project";
  const invocation = candidateImageBuildInvocation({
    root,
    context,
    candidateRoot,
    version: "1.2.3",
    sha: SHA,
    sourceURL,
  });

  assert.equal(invocation.command, "docker");
  assert.equal(invocation.cwd, root);
  assert.equal(invocation.layout, `${candidateRoot}/app/image`);
  assert.deepEqual(invocation.args, [
    "buildx",
    "build",
    "--file",
    `${root}/Dockerfile`,
    "--platform",
    "linux/amd64,linux/arm64",
    "--build-arg",
    "RELEASE_VERSION=1.2.3",
    "--build-arg",
    `SOURCE_SHA=${SHA}`,
    "--build-arg",
    `SOURCE_URL=${sourceURL}`,
    "--tag",
    "docker.io/library/cerberus:1.2.3",
    "--sbom=true",
    "--provenance=mode=max",
    "--output",
    `type=oci,dest=${candidateRoot}/app/image,tar=false`,
    context,
  ]);
  for (const forbidden of ["--push", "--load", "type=registry"]) {
    assert.equal(invocation.args.includes(forbidden), false, `${forbidden} would publish or load the candidate`);
  }
});

test("candidate image build rejects malformed source identity before invoking Docker", () => {
  const base = {
    root: "/checkout",
    context: "/checkout/build/release-image-context",
    candidateRoot: "/checkout/build/release-candidate",
    version: "1.2.3",
    sha: SHA,
    sourceURL: "https://example.invalid/example/project",
  };
  for (const invalid of [
    { sourceURL: "http://example.invalid/example/project" },
    { sourceURL: "https://example.invalid/example/project/extra" },
    { sourceURL: "https://example.invalid/example/project?ref=main" },
    { sourceURL: "" },
    { sha: "short" },
    { version: "v1.2.3" },
  ]) {
    assert.throws(
      () => candidateImageBuildInvocation({ ...base, ...invalid }),
      (error) => error instanceof CandidateError && /invalid|non-empty/.test(error.message),
    );
  }
});

test("candidate source identity rejects untracked compilable inputs", () => {
  const root = tempRoot();
  git(root, ["init", "--quiet"]);
  git(root, ["config", "user.name", "candidate-test"]);
  git(root, ["config", "user.email", "candidate-test@example.invalid"]);
  write(root, ".gitignore", "dist/\nbuild/\n");
  write(root, "main.go", "package main\n");
  git(root, ["add", ".gitignore", "main.go"]);
  git(root, ["commit", "--quiet", "-m", "test: seed exact tree"]);
  const sha = git(root, ["rev-parse", "HEAD"]);
  const tree = git(root, ["rev-parse", "HEAD^{tree}"]);

  verifyCheckout(sha, tree, root);
  write(root, "dist/generated", "ignored build output");
  verifyCheckout(sha, tree, root);

  write(root, "injected.go", "package main\nfunc injected() {}\n");
  assert.throws(
    () => verifyCheckout(sha, tree, root),
    /tracked or untracked modifications/,
  );
  rmSync(join(root, "injected.go"));

  write(root, "main.go", "package main\nfunc changed() {}\n");
  assert.throws(
    () => verifyCheckout(sha, tree, root),
    /tracked or untracked modifications/,
  );
});

test("candidate digest mismatch fails closed", () => {
  const candidate = sealedCandidate();
  assert.throws(
    () =>
      verifyCandidate(candidate.root, {
        sha: SHA,
        tree: TREE,
        digest: `sha256:${"d".repeat(64)}`,
      }),
    (error) => error instanceof CandidateError && /candidate digest/.test(error.message),
  );
});

test("candidate checksums exactly bind all four release archives", () => {
  const candidate = sealedCandidate();
  assert.equal(validateReleaseChecksums(candidate.root, "1.2.3").length, 4);
  for (const mutation of [
    (root) => writeFileSync(join(root, "app/assets/checksums.txt"), "bad\n"),
    (root) => {
      const path = join(root, "app/assets/checksums.txt");
      writeFileSync(path, `${readFileSync(path, "utf8")}0${"0".repeat(63)}  unexpected.tar.gz\n`);
    },
    (root) => writeFileSync(join(root, "app/assets/cerberus_1.2.3_linux_amd64.tar.gz"), "changed"),
  ]) {
    const changed = sealedCandidate();
    mutation(changed.root);
    reseal(changed);
    assert.throws(() => verifyCandidate(changed.root), CandidateError);
  }
});

test("source SHA and tree mismatch each fail closed", () => {
  const candidate = sealedCandidate();
  for (const expected of [
    { sha: "e".repeat(40), tree: TREE },
    { sha: SHA, tree: "f".repeat(40) },
    { sha: SHA, tree: TREE, sourceUrl: "https://downloads.example.invalid/other/project" },
  ]) {
    assert.throws(
      () => verifyCandidate(candidate.root, expected),
      (error) => error instanceof CandidateError && /manifest\.source/.test(error.message),
    );
  }
});

test("candidate source identity cannot disagree with its sealed manifest", () => {
  const candidate = sealedCandidate();
  writeFileSync(
    join(candidate.root, "source.json"),
    `${canonicalJSON({ app_tag: "v1.2.3", app_version: "1.2.3", sha: "0".repeat(40), source_url: SOURCE_URL, tree: TREE })}\n`,
  );
  reseal(candidate);
  assert.throws(() => verifyCandidate(candidate.root), /candidate source identity\.sha/);
});

test("changed, missing, and unbound files each fail closed", () => {
  {
    const candidate = sealedCandidate();
    writeFileSync(join(candidate.root, "chart/cerberus-4.5.6.tgz"), "changed");
    assert.throws(() => verifyCandidate(candidate.root), /digest|size/);
  }
  {
    const candidate = sealedCandidate();
    rmSync(join(candidate.root, "app/homebrew/cerberus.rb"));
    assert.throws(() => verifyCandidate(candidate.root), /file roster/);
  }
  {
    const candidate = sealedCandidate();
    write(candidate.root, "unbound.txt", "not in manifest");
    assert.throws(() => verifyCandidate(candidate.root), /file roster/);
  }
  {
    const candidate = sealedCandidate();
    candidate.manifest.files = candidate.manifest.files.filter(
      (file) => file.path !== "chart/artifacthub-repo.yml",
    );
    rmSync(join(candidate.root, "chart/artifacthub-repo.yml"));
    writeFileSync(
      join(candidate.root, CANDIDATE_MANIFEST),
      `${canonicalJSON(candidate.manifest)}\n`,
    );
    assert.throws(() => verifyCandidate(candidate.root), /artifacthub-repo\.yml/);
  }
});

test("OCI layout rejects unreachable blobs, wrong ref identity, and broken descriptor digests", () => {
  {
    const candidate = sealedCandidate();
    const path = "app/image/blobs/sha256/" + "0".repeat(64);
    write(candidate.root, path, "unreachable");
    candidate.manifest.files.push({
      path,
      size: Buffer.byteLength("unreachable"),
      sha256: sha256File(join(candidate.root, path)),
    });
    candidate.manifest.files.sort((left, right) => left.path.localeCompare(right.path));
    reseal(candidate);
    assert.throws(() => verifyCandidate(candidate.root), /reachable/);
  }
  {
    const candidate = sealedCandidate();
    const indexPath = join(candidate.root, "app/image/index.json");
    const index = JSON.parse(readFileSync(indexPath, "utf8"));
    index.manifests[0].annotations["org.opencontainers.image.ref.name"] = "stale";
    writeFileSync(indexPath, `${canonicalJSON(index)}\n`);
    reseal(candidate);
    assert.throws(() => verifyCandidate(candidate.root), /descriptor/);
  }
  {
    const candidate = sealedCandidate();
    const blob = candidate.manifest.files.find(
      (file) => file.path.startsWith("app/image/blobs/") && file.path !== `app/image/blobs/sha256/${candidate.manifest.image.digest.slice(7)}`,
    );
    writeFileSync(join(candidate.root, blob.path), "mutated but candidate-bound");
    reseal(candidate);
    assert.throws(() => verifyCandidate(candidate.root), /digest|size/);
  }
});

test("OCI image graph contains exactly the two release platform manifests", () => {
  for (const [name, options, problem] of [
    ["missing platform", { architectures: ["amd64"] }, /OCI image platforms/],
    [
      "unsupported platform",
      { architectures: ["amd64", "arm64", "ppc64le"] },
      /unsupported platform linux\/ppc64le/,
    ],
    [
      "duplicate platform descriptor",
      {
        innerDescriptorsMutation(descriptors, { images }) {
          descriptors.push(images[0]);
        },
      },
      /duplicates platform linux\/amd64/,
    ],
    [
      "platform variant",
      {
        imageDescriptorMutation(descriptor, architecture) {
          if (architecture === "amd64") descriptor.platform.variant = "v8";
        },
      },
      /descriptor\.platform/,
    ],
    [
      "extra tagged root",
      {
        rootIndexMutation(index) {
          index.manifests.push(structuredClone(index.manifests[0]));
        },
      },
      /exactly one tagged descriptor/,
    ],
    [
      "non-OCI image manifest",
      {
        imageManifestMutation(manifest, architecture) {
          if (architecture === "amd64") manifest.mediaType = "application/json";
        },
      },
      /manifest\.mediaType/,
    ],
  ]) {
    const candidate = sealedCandidate(options);
    assert.throws(
      () => verifyCandidate(candidate.root),
      (cause) => cause instanceof CandidateError && problem.test(cause.message),
      name,
    );
  }
});

test("OCI platform configs bind architecture, source labels, and entrypoint", () => {
  for (const [name, configMutation, problem] of [
    [
      "architecture mismatch",
      (config, architecture) => {
        if (architecture === "amd64") config.architecture = "arm64";
      },
      /config\.architecture/,
    ],
    [
      "operating system mismatch",
      (config, architecture) => {
        if (architecture === "amd64") config.os = "darwin";
      },
      /config\.os/,
    ],
    [
      "wrong entrypoint",
      (config, architecture) => {
        if (architecture === "amd64") config.config.Entrypoint = ["/bin/sh"];
      },
      /Entrypoint/,
    ],
  ]) {
    const candidate = sealedCandidate({ configMutation });
    assert.throws(
      () => verifyCandidate(candidate.root),
      (cause) => cause instanceof CandidateError && problem.test(cause.message),
      name,
    );
  }

  for (const label of [
    "org.opencontainers.image.revision",
    "org.opencontainers.image.source",
    "org.opencontainers.image.url",
    "org.opencontainers.image.version",
  ]) {
    const candidate = sealedCandidate({
      configMutation(config, architecture) {
        if (architecture === "amd64") config.config.Labels[label] = "stale";
      },
    });
    assert.throws(
      () => verifyCandidate(candidate.root),
      (cause) => cause instanceof CandidateError && cause.message.includes(`label ${label}`),
      label,
    );
  }
});

test("OCI images require one closed subject-bound BuildKit attestation each", () => {
  for (const [name, options, problem] of [
    [
      "missing platform attestation",
      { includeAttestation: (architecture) => architecture === "amd64" },
      /linux\/arm64 image is missing its BuildKit attestation/,
    ],
    [
      "unknown referenced image",
      {
        attestationDescriptorMutation(descriptor, architecture) {
          if (architecture === "amd64") {
            descriptor.annotations["vnd.docker.reference.digest"] = `sha256:${"d".repeat(64)}`;
          }
        },
      },
      /references unknown image digest/,
    ],
    [
      "wrong statement subject",
      {
        spdxMutation(statement, architecture) {
          if (architecture === "amd64") statement.subject[0].digest.sha256 = "d".repeat(64);
        },
      },
      /subject\[0\]\.digest\.sha256/,
    ],
    [
      "missing SPDX statement",
      {
        statementsMutation(statements, architecture) {
          if (architecture === "amd64") statements.splice(0, 1);
        },
      },
      /exactly SBOM and provenance/,
    ],
    [
      "duplicate attestation",
      {
        innerDescriptorsMutation(descriptors, { attestations }) {
          descriptors.push(attestations[0]);
        },
      },
      /duplicate attestation manifests/,
    ],
    [
      "non-BuildKit attestation platform",
      {
        attestationDescriptorMutation(descriptor, architecture) {
          if (architecture === "amd64") descriptor.platform = { os: "linux", architecture };
        },
      },
      /descriptor\.platform/,
    ],
  ]) {
    const candidate = sealedCandidate(options);
    assert.throws(
      () => verifyCandidate(candidate.root),
      (cause) => cause instanceof CandidateError && problem.test(cause.message),
      name,
    );
  }
});

test("OCI attestations contain a nonempty SPDX document and maximum provenance", () => {
  for (const [name, options, problem] of [
    [
      "empty SPDX",
      {
        spdxMutation(statement, architecture) {
          if (architecture === "amd64") statement.predicate.packages = [];
        },
      },
      /packages must describe at least one package/,
    ],
    [
      "minimum provenance",
      {
        provenanceMutation(statement, architecture) {
          if (architecture === "amd64") delete statement.predicate.invocation;
        },
      },
      /invocation must be present for maximum provenance/,
    ],
    [
      "missing source build argument",
      {
        provenanceMutation(statement, architecture) {
          if (architecture === "amd64") {
            delete statement.predicate.invocation.parameters.args["build-arg:SOURCE_SHA"];
          }
        },
      },
      /build-arg:SOURCE_SHA/,
    ],
    [
      "incomplete provenance parameters",
      {
        provenanceMutation(statement, architecture) {
          if (architecture === "amd64") {
            statement.predicate.metadata.completeness.parameters = false;
          }
        },
      },
      /completeness\.parameters must be true/,
    ],
    [
      "wrong provenance platform",
      {
        provenanceMutation(statement, architecture) {
          if (architecture === "amd64") {
            statement.predicate.invocation.environment.platform = "linux/arm64";
          }
        },
      },
      /environment\.platform/,
    ],
    [
      "empty provenance materials",
      {
        provenanceMutation(statement, architecture) {
          if (architecture === "amd64") statement.predicate.materials = [];
        },
      },
      /materials must contain resolved build inputs/,
    ],
  ]) {
    const candidate = sealedCandidate(options);
    assert.throws(
      () => verifyCandidate(candidate.root),
      (cause) => cause instanceof CandidateError && problem.test(cause.message),
      name,
    );
  }
});

test("malformed, stale, and zero-attempt manifests are rejected", () => {
  for (const mutate of [
    (manifest) => {
      manifest.source.sha = "stale";
    },
    (manifest) => {
      manifest.run.attempt = 0;
    },
    (manifest) => {
      manifest.files[0].sha256 = "not-a-digest";
    },
  ]) {
    const candidate = sealedCandidate();
    mutate(candidate.manifest);
    writeFileSync(
      join(candidate.root, CANDIDATE_MANIFEST),
      `${canonicalJSON(candidate.manifest)}\n`,
    );
    assert.throws(() => verifyCandidate(candidate.root), CandidateError);
  }
});

test("GoReleaser output must contain one binary and archive per target", () => {
  const artifacts = [];
  for (const goos of ["darwin", "linux"]) {
    for (const goarch of ["amd64", "arm64"]) {
      artifacts.push({ type: "Binary", goos, goarch, path: `dist/${goos}-${goarch}/cerberus` });
      artifacts.push({
        type: "Archive",
        goos,
        goarch,
        name: `cerberus_1.2.3_${goos}_${goarch}.tar.gz`,
        path: `dist/cerberus_1.2.3_${goos}_${goarch}.tar.gz`,
      });
    }
  }
  artifacts.push(
    { type: "Checksum", path: "dist/checksums.txt" },
    { type: "Homebrew Cask", path: "dist/homebrew/Casks/cerberus.rb" },
    { type: "Metadata", path: "dist/metadata.json" },
  );
  const selected = releaseArtifacts(artifacts, { version: "1.2.3", commit: SHA });
  assert.equal(selected.binaries.size, 4);
  assert.equal(selected.archives.size, 4);

  artifacts.pop();
  assert.throws(() => releaseArtifacts(artifacts, { version: "1.2.3", commit: SHA }), /metadata/);
});

test("GoReleaser candidate cask URLs bind the runtime source and app tag", () => {
  const config = readFileSync(goreleaserConfig, "utf8");
  const template =
    'template: "{{ .Env.RELEASE_SOURCE_URL }}/releases/download/{{ .Env.RELEASE_APP_TAG }}/{{ .ArtifactName }}"';
  assert.equal(
    config.split(template).length - 1,
    1,
    "the candidate cask must have exactly one runtime-bound download template",
  );
});

test("snapshot metadata binds the candidate version and commit, not the repository history tag", () => {
  const metadata = {
    version: "1.2.3",
    commit: SHA,
    tag: "v0.9.0",
  };
  assert.equal(
    validateSnapshotMetadata(metadata, { version: "1.2.3", commit: SHA }),
    metadata,
  );
});

test("a requested-looking snapshot tag cannot hide a stale version or commit", () => {
  for (const [metadata, problem] of [
    [{ version: "1.2.2", commit: SHA, tag: "v1.2.3" }, /GoReleaser version/],
    [
      { version: "1.2.3", commit: "f".repeat(40), tag: "v1.2.3" },
      /GoReleaser commit/,
    ],
  ]) {
    assert.throws(
      () => validateSnapshotMetadata(metadata, { version: "1.2.3", commit: SHA }),
      (error) => error instanceof CandidateError && problem.test(error.message),
    );
  }
});
