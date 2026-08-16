// release-promote.mjs — promote only the bytes in a verified release candidate.
// Every mode is a public write and therefore runs only in a workflow job that
// has independently verified the candidate and qualification attestation.
//
// Modes (MODE or argv[2]):
//   tag              create one immutable annotated tag at RELEASE_SOURCE_SHA.
//   image            copy the candidate OCI index to every destination/tag.
//   release-stage    create/reuse a draft and upload the exact candidate assets.
//   release-publish  flip the verified draft public with an explicit Latest bit.
//   cask             write the exact generated cask to the configured tap.
//   chart            push the exact candidate chart package to OCI.
//   chart-metadata   push the exact candidate Artifact Hub metadata to OCI.
//   verify-only      re-run direct authorization immediately before a writer
//                    implemented by an external action (no mutation here).
//
// Shared environment:
//   RELEASE_CANDIDATE_DIR  sealed candidate root.
//   RELEASE_SOURCE_SHA     exact 40-hex source commit.
//   RELEASE_APP_VERSION    exact application version.
//   RELEASE_APP_TAG        exact v-prefixed tag.
//   RELEASE_AUTHORIZED     literal true from the authorization barrier.
//   RELEASE_PUBLISH        literal true from the version gate.
//   RELEASE_DRY_RUN        literal false; dry-runs never invoke a writer.
//   RELEASE_ATTESTATION / RELEASE_ATTESTATION_DIGEST / RELEASE_SOURCE_TREE /
//   RELEASE_CANDIDATE_DIGEST / RELEASE_CORRELATION_NONCE /
//                          independently revalidated before every mode.
//
// Mode-specific environment:
//   RELEASE_TAG                 tag: exact candidate-bound tag to create.
//   RELEASE_TAG_KIND            tag: app or chart; selects which sealed
//                               candidate version RELEASE_TAG must equal.
//   RELEASE_TAG_MESSAGE         tag: annotation text.
//   RELEASE_TAGGER_NAME/EMAIL   tag: runtime automation identity.
//   RELEASE_IMAGE_DESTINATIONS  image: newline-separated host/repository list.
//   RELEASE_IMAGE_TAGS          image: newline-separated tags.
//   RELEASE_EXPECTED_IMAGE_DESTINATIONS
//                               image: exact destination count; omission of a
//                               configured registry fails before any copy.
//   RELEASE_IS_LATEST           image/release-publish/cask: true or false.
//   RELEASE_TAP_REPOSITORY      cask: owner/repository destination.
//   RELEASE_CHART_REPOSITORY    chart/chart-metadata: oci:// registry path
//                               without the chart name.
//   RELEASE_CHART_NAME          chart/chart-metadata: chart name (default
//                               cerberus).
//   GH_TOKEN                    release/cask authentication.
//
// Node builtins only. Publication destinations are supplied at runtime.

import { readFileSync } from 'node:fs';
import { basename, join, resolve } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { capture, error, notice, setOutput } from './lib/gh.mjs';
import { verifyCandidate } from './release-candidate.mjs';
import { verifyAttestationFromEnvironment } from './release-qualification.mjs';

const SHA_RE = /^[0-9a-f]{40}$/;
const DIGEST_RE = /^sha256:[0-9a-f]{64}$/;
const TAG_RE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
const DESTINATION_RE = /^[a-z0-9.-]+(?::[0-9]+)?\/[A-Za-z0-9._/-]+$/;
const REPOSITORY_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const OCI_REPOSITORY_RE = /^oci:\/\/[a-z0-9.-]+(?::[0-9]+)?\/[A-Za-z0-9._/-]+$/;

export class PromotionError extends Error {
  constructor(message) {
    super(message);
    this.name = 'PromotionError';
  }
}

function requiredEnv(name, pattern = null) {
  const value = String(process.env[name] ?? '').trim();
  if (value === '') throw new PromotionError(`${name} is required`);
  if (pattern && !pattern.test(value)) {
    throw new PromotionError(`${name} has invalid value ${JSON.stringify(value)}`);
  }
  return value;
}

function booleanEnv(name) {
  const value = requiredEnv(name);
  if (value !== 'true' && value !== 'false') {
    throw new PromotionError(`${name} must be true or false`);
  }
  return value === 'true';
}

function listEnv(name, pattern) {
  const values = requiredEnv(name)
    .split(/\r?\n/)
    .map((value) => value.trim())
    .filter(Boolean);
  if (values.length === 0) throw new PromotionError(`${name} must not be empty`);
  if (new Set(values).size !== values.length) throw new PromotionError(`${name} contains duplicates`);
  for (const value of values) {
    if (!pattern.test(value)) throw new PromotionError(`${name} contains invalid value ${JSON.stringify(value)}`);
  }
  return values;
}

function runChecked(command, args, label, runner = capture) {
  const result = runner(command, args);
  if (result.status !== 0) {
    throw new PromotionError(`${label} failed (exit ${result.status}):\n${result.stdout}${result.stderr}`);
  }
  return result;
}

function candidate() {
  return verifyCandidate(resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate'));
}

export function verifyPromotionAuthorization({
  authorized,
  publish,
  dryRun,
  verifier = verifyAttestationFromEnvironment,
}) {
  if (authorized !== 'true') {
    throw new PromotionError('release authorization barrier did not authorize public writes');
  }
  if (publish !== 'true') {
    throw new PromotionError('release version gate did not request public writes');
  }
  if (dryRun !== 'false') {
    throw new PromotionError('public writes are forbidden unless RELEASE_DRY_RUN is exactly false');
  }
  const verified = verifier();
  if (!verified?.attestation || !verified?.candidate || !verified?.digest) {
    throw new PromotionError('release attestation verifier returned incomplete authorization evidence');
  }
  return verified;
}

export function tagCommands({ tag, message, sha }) {
  if (!TAG_RE.test(tag)) throw new PromotionError(`invalid release tag ${JSON.stringify(tag)}`);
  if (!SHA_RE.test(sha)) throw new PromotionError(`invalid release source SHA ${JSON.stringify(sha)}`);
  if (typeof message !== 'string' || message.trim() === '') {
    throw new PromotionError('release tag message is required');
  }
  return Object.freeze({
    verify: ['git', ['rev-parse', '-q', '--verify', `refs/tags/${tag}`]],
    create: ['git', ['tag', '-a', tag, '-m', message, sha]],
    push: ['git', ['push', 'origin', `refs/tags/${tag}`]],
  });
}

export function candidatePromotionTag(manifest, kind) {
  if (kind === 'app') {
    const tag = manifest?.versions?.app_tag;
    if (!TAG_RE.test(tag ?? '')) throw new PromotionError('candidate application tag is invalid');
    return tag;
  }
  if (kind === 'chart') {
    const version = manifest?.versions?.chart;
    if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/.test(version ?? '')) {
      throw new PromotionError('candidate chart version is invalid');
    }
    return `chart-v${version}`;
  }
  throw new PromotionError(`release tag kind must be app or chart, got ${JSON.stringify(kind)}`);
}

export function remoteAnnotatedTagTarget(output, tag) {
  const base = `refs/tags/${tag}`;
  const peeled = `${base}^{}`;
  const refs = new Map();
  for (const line of String(output ?? '').trim().split(/\r?\n/).filter(Boolean)) {
    const match = line.match(/^([0-9a-f]{40})\s+(\S+)$/);
    if (!match) throw new PromotionError(`malformed remote tag response line ${JSON.stringify(line)}`);
    if (refs.has(match[2])) throw new PromotionError(`duplicate remote tag response for ${match[2]}`);
    refs.set(match[2], match[1]);
  }
  if (refs.size === 0) return null;
  for (const ref of refs.keys()) {
    if (ref !== base && ref !== peeled) {
      throw new PromotionError(`remote tag response contains unexpected ref ${ref}`);
    }
  }
  if (refs.has(peeled)) return { annotated: true, target: refs.get(peeled) };
  return { annotated: false, target: refs.get(base) };
}

function remoteTag(tag) {
  const base = `refs/tags/${tag}`;
  const result = capture('git', [
    'ls-remote',
    '--exit-code',
    'origin',
    base,
    `${base}^{}`,
  ]);
  if (result.status === 2) return null;
  if (result.status !== 0) {
    throw new PromotionError(
      `read remote tag ${tag} failed (exit ${result.status}):\n${result.stdout}${result.stderr}`,
    );
  }
  return remoteAnnotatedTagTarget(result.stdout, tag);
}

function tagMode() {
  const sealed = candidate();
  const tag = requiredEnv('RELEASE_TAG', TAG_RE);
  const kind = requiredEnv('RELEASE_TAG_KIND', /^(?:app|chart)$/);
  const sha = requiredEnv('RELEASE_SOURCE_SHA', SHA_RE);
  const message = requiredEnv('RELEASE_TAG_MESSAGE');
  const taggerName = requiredEnv('RELEASE_TAGGER_NAME');
  const taggerEmail = requiredEnv('RELEASE_TAGGER_EMAIL', /^[^\s@]+@[^\s@]+$/);
  const expectedTag = candidatePromotionTag(sealed.manifest, kind);
  if (tag !== expectedTag) {
    throw new PromotionError(
      `RELEASE_TAG ${tag} does not equal the sealed candidate ${kind} tag ${expectedTag}`,
    );
  }
  if (sha !== sealed.manifest.source.sha) {
    throw new PromotionError(
      `RELEASE_SOURCE_SHA ${sha} does not equal candidate source ${sealed.manifest.source.sha}`,
    );
  }
  const commands = tagCommands({ tag, message, sha });
  const published = remoteTag(tag);
  if (published) {
    if (!published.annotated) {
      throw new PromotionError(`remote tag ${tag} exists but is not annotated`);
    }
    if (published.target !== sha) {
      throw new PromotionError(
        `remote tag ${tag} targets ${published.target}, want immutable source ${sha}`,
      );
    }
    notice(`annotated tag ${tag} already targets the qualified source`);
    return;
  }
  runChecked('git', ['config', 'user.name', taggerName], 'configure release tagger name');
  runChecked('git', ['config', 'user.email', taggerEmail], 'configure release tagger email');
  const existing = capture(...commands.verify);
  if (existing.status === 0) {
    const target = runChecked('git', ['rev-list', '-n1', tag], `resolve existing tag ${tag}`).stdout.trim();
    if (target !== sha) {
      throw new PromotionError(`tag ${tag} already targets ${target}, want immutable source ${sha}`);
    }
    const type = runChecked('git', ['cat-file', '-t', `refs/tags/${tag}`], `inspect existing tag ${tag}`).stdout.trim();
    if (type !== 'tag') throw new PromotionError(`local tag ${tag} exists but is not annotated`);
  } else {
    runChecked(...commands.create, `create tag ${tag}`);
  }
  runChecked(...commands.push, `push tag ${tag}`);
  const verified = remoteTag(tag);
  if (!verified?.annotated || verified.target !== sha) {
    throw new PromotionError(`remote tag ${tag} did not resolve to the qualified annotated source after push`);
  }
  notice(`created immutable tag ${tag} at ${sha}`);
}

export function imagePromotionPlan({ layout, sourceName, digest, destinations, tags }) {
  if (!DIGEST_RE.test(digest)) throw new PromotionError(`invalid candidate image digest ${digest}`);
  if (!Array.isArray(destinations) || destinations.length === 0) {
    throw new PromotionError('image promotion needs at least one destination');
  }
  if (!Array.isArray(tags) || tags.length === 0) {
    throw new PromotionError('image promotion needs at least one tag');
  }
  if (tags[0] === 'latest') {
    throw new PromotionError('image promotion primary tag must be immutable, not latest');
  }
  const rolling = tags.filter((tag) => tag === 'latest');
  if (rolling.length > 1) throw new PromotionError('image promotion contains duplicate latest tags');
  return destinations.map((destination) => ({
    destination,
    source: `${layout}:${sourceName}`,
    primary: `${destination}:${tags[0]}`,
    digestRef: `${destination}@${digest}`,
    aliases: tags.slice(1).filter((tag) => tag !== 'latest'),
    rollingAliases: rolling,
  }));
}

export function imagePromotionPreflight({ plan, digest, resolveRef }) {
  const states = [];
  for (const item of plan) {
    const refs = [
      { ref: item.primary, rolling: false },
      ...item.aliases.map((alias) => ({
        ref: `${item.destination}:${alias}`,
        rolling: false,
      })),
      ...item.rollingAliases.map((alias) => ({
        ref: `${item.destination}:${alias}`,
        rolling: true,
      })),
    ];
    for (const { ref, rolling } of refs) {
      const state = resolveRef(ref);
      if (state.exists && state.digest !== digest && !rolling) {
        throw new PromotionError(
          `${ref} already resolves to ${state.digest}, refusing to overwrite with ${digest}`,
        );
      }
      states.push({ ref, exists: state.exists, digest: state.digest, rolling });
    }
  }
  return new Map(states.map((state) => [state.ref, state]));
}

function inspectImageRef(ref) {
  const result = capture('oras', ['resolve', ref]);
  if (result.status === 0) {
    const digest = result.stdout.trim();
    if (!DIGEST_RE.test(digest)) {
      throw new PromotionError(`${ref} resolved to invalid digest ${JSON.stringify(digest)}`);
    }
    return { exists: true, digest };
  }
  if (remoteNotFound(result)) return { exists: false, digest: null };
  throw new PromotionError(
    `could not determine whether image ref ${ref} exists:\n${result.stdout}${result.stderr}`,
  );
}

function imageMode() {
  const sealed = candidate();
  const version = requiredEnv('RELEASE_APP_VERSION');
  const destinations = listEnv('RELEASE_IMAGE_DESTINATIONS', DESTINATION_RE);
  const expectedDestinations = Number(
    requiredEnv('RELEASE_EXPECTED_IMAGE_DESTINATIONS', /^[1-9][0-9]*$/),
  );
  if (destinations.length !== expectedDestinations) {
    throw new PromotionError(
      `image promotion resolved ${destinations.length} destinations, want exactly ${expectedDestinations}`,
    );
  }
  const tags = listEnv('RELEASE_IMAGE_TAGS', TAG_RE);
  const isLatest = booleanEnv('RELEASE_IS_LATEST');
  const expectedTags = [
    sealed.manifest.versions.app,
    sealed.manifest.versions.app_tag,
    ...(isLatest && !sealed.manifest.versions.app.includes('-') ? ['latest'] : []),
  ];
  if (JSON.stringify(tags) !== JSON.stringify(expectedTags)) {
    throw new PromotionError(
      `RELEASE_IMAGE_TAGS is ${JSON.stringify(tags)}, want candidate-derived ${JSON.stringify(expectedTags)}`,
    );
  }
  const plan = imagePromotionPlan({
    layout: resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate', sealed.manifest.image.layout),
    sourceName: version,
    digest: sealed.manifest.image.digest,
    destinations,
    tags,
  });
  // Resolve every destination and alias before the first copy. A wrong tag in
  // the last registry must not be discovered after an earlier registry was
  // already mutated.
  const states = imagePromotionPreflight({
    plan,
    digest: sealed.manifest.image.digest,
    resolveRef: inspectImageRef,
  });
  for (const item of plan) {
    if (!states.get(item.primary).exists) {
      runChecked('oras', ['cp', '--from-oci-layout', item.source, item.primary], `copy image to ${item.primary}`);
    }
    const resolved = runChecked('oras', ['resolve', item.primary], `resolve ${item.primary}`).stdout.trim();
    if (resolved !== sealed.manifest.image.digest) {
      throw new PromotionError(`${item.primary} resolved to ${resolved}, want ${sealed.manifest.image.digest}`);
    }
    const missingAliases = item.aliases.filter(
      (alias) => !states.get(`${item.destination}:${alias}`).exists,
    );
    if (missingAliases.length > 0) {
      runChecked('oras', ['tag', item.digestRef, ...missingAliases], `tag image aliases for ${item.destination}`);
    }
    const staleRollingAliases = item.rollingAliases.filter((alias) => {
      const state = states.get(`${item.destination}:${alias}`);
      return !state.exists || state.digest !== sealed.manifest.image.digest;
    });
    if (staleRollingAliases.length > 0) {
      runChecked(
        'oras',
        ['tag', '--force', item.digestRef, ...staleRollingAliases],
        `advance rolling image aliases for ${item.destination}`,
      );
    }
    const allAliases = [...item.aliases, ...item.rollingAliases];
    if (allAliases.length > 0) {
      for (const alias of allAliases) {
        const aliasRef = `${item.destination}:${alias}`;
        const aliasDigest = runChecked('oras', ['resolve', aliasRef], `resolve ${aliasRef}`).stdout.trim();
        if (aliasDigest !== sealed.manifest.image.digest) {
          throw new PromotionError(
            `${aliasRef} resolved to ${aliasDigest}, want ${sealed.manifest.image.digest}`,
          );
        }
      }
    }
  }
  notice(`promoted exact candidate image ${sealed.manifest.image.digest} to ${plan.length} destinations`);
}

export function releaseAssets(manifest, root) {
  const assets = manifest.files
    .filter((file) => file.path.startsWith('app/assets/'))
    .map((file) => resolve(root, file.path));
  const archives = assets.filter((path) => path.endsWith('.tar.gz'));
  const checksums = assets.filter((path) => basename(path) === 'checksums.txt');
  if (archives.length !== 4 || checksums.length !== 1 || assets.length !== 5) {
    throw new PromotionError(`candidate release assets are ${assets.length} files (${archives.length} archives, ${checksums.length} checksums)`);
  }
  return assets.sort();
}

export function releaseAssetInventory(manifest, root) {
  const paths = new Set(releaseAssets(manifest, root));
  const inventory = manifest.files
    .filter((file) => paths.has(resolve(root, file.path)))
    .map((file) => ({
      name: basename(file.path),
      path: resolve(root, file.path),
      size: file.size,
      digest: file.sha256,
    }))
    .sort((left, right) => left.name.localeCompare(right.name));
  if (new Set(inventory.map((asset) => asset.name)).size !== inventory.length) {
    throw new PromotionError('candidate release asset names are not unique');
  }
  return inventory;
}

export function validateReleaseDocument(
  document,
  { tag, sha, assets, complete, state, prerelease = undefined },
) {
  const problems = [];
  if (document === null || typeof document !== 'object' || Array.isArray(document)) {
    throw new PromotionError('release API response must be an object');
  }
  if (state !== 'draft' && state !== 'published') {
    throw new PromotionError(`release validation state must be draft or published, got ${state}`);
  }
  const expectedDraft = state === 'draft';
  if (document.draft !== expectedDraft) {
    problems.push(
      `release ${tag} draft state is ${JSON.stringify(document.draft)}, want ${expectedDraft}`,
    );
  }
  if (state === 'published' && document.immutable !== true) {
    problems.push(`published release ${tag} is not immutable`);
  }
  if (prerelease !== undefined && document.prerelease !== prerelease) {
    problems.push(
      `release ${tag} prerelease state is ${JSON.stringify(document.prerelease)}, want ${prerelease}`,
    );
  }
  if (document.tag_name !== tag) {
    problems.push(`release tag is ${JSON.stringify(document.tag_name)}, want ${tag}`);
  }
  if (!SHA_RE.test(sha ?? '')) problems.push(`qualified release source SHA is invalid: ${sha}`);
  if (typeof document.target_commitish !== 'string' || document.target_commitish === '') {
    problems.push('release target_commitish must be a non-empty string');
  }
  if (!Array.isArray(document.assets)) {
    problems.push('release assets must be an array');
  } else {
    const expected = new Map(assets.map((asset) => [asset.name, asset]));
    const actual = new Map();
    for (const asset of document.assets) {
      const name = String(asset?.name ?? '');
      if (name === '') {
        problems.push('release contains an asset with no name');
        continue;
      }
      if (actual.has(name)) problems.push(`release asset ${name} is duplicated`);
      actual.set(name, asset);
      if (!expected.has(name)) {
        problems.push(`release contains unexpected stale asset ${name}`);
        continue;
      }
      if (complete) {
        const wanted = expected.get(name);
        if (asset.size !== wanted.size) {
          problems.push(`release asset ${name} size is ${asset.size}, want ${wanted.size}`);
        }
        if (asset.digest !== wanted.digest) {
          problems.push(
            `release asset ${name} digest is ${JSON.stringify(asset.digest)}, want ${wanted.digest}`,
          );
        }
      }
    }
    if (complete) {
      for (const name of expected.keys()) {
        if (!actual.has(name)) problems.push(`release is missing candidate asset ${name}`);
      }
    }
  }
  if (problems.length > 0) {
    throw new PromotionError(`${state} release is invalid:\n${problems.map((p) => `- ${p}`).join('\n')}`);
  }
  return document;
}

export function validateDraftRelease(document, options) {
  return validateReleaseDocument(document, { ...options, state: 'draft' });
}

export function validatePublishedRelease(document, options) {
  return validateReleaseDocument(document, {
    ...options,
    complete: true,
    state: 'published',
  });
}

export function isPrereleaseVersion(version) {
  if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$/.test(version ?? '')) {
    throw new PromotionError(`invalid application version ${JSON.stringify(version)}`);
  }
  return version.includes('-');
}

function releaseNotFound(result) {
  return result.status !== 0 && /(?:\b404\b|not found)/i.test(`${result.stdout}\n${result.stderr}`);
}

function readRelease(tag) {
  const result = capture('gh', [
    'api',
    `repos/{owner}/{repo}/releases/tags/${tag}`,
  ]);
  if (result.status !== 0) return { result, document: null };
  try {
    return { result, document: JSON.parse(result.stdout) };
  } catch (cause) {
    throw new PromotionError(`release ${tag} returned malformed JSON: ${cause.message}`);
  }
}

function readLatestReleaseTag() {
  const result = capture('gh', [
    'api',
    'repos/{owner}/{repo}/releases/latest',
    '--jq',
    '.tag_name',
  ]);
  if (result.status === 0) {
    const tag = result.stdout.trim();
    if (!TAG_RE.test(tag)) {
      throw new PromotionError(`latest release returned invalid tag ${JSON.stringify(tag)}`);
    }
    return tag;
  }
  if (releaseNotFound(result)) return null;
  throw new PromotionError(
    `could not read the latest release pointer:\n${result.stdout}${result.stderr}`,
  );
}

function requireRemoteTagAtSource(tag, sha) {
  const published = remoteTag(tag);
  if (!published?.annotated || published.target !== sha) {
    throw new PromotionError(
      `remote tag ${tag} is not an annotated tag at qualified source ${sha}`,
    );
  }
}

export function validateLatestRelease({ tag, isLatest, latestTag }) {
  if (!TAG_RE.test(tag)) throw new PromotionError(`invalid release tag ${JSON.stringify(tag)}`);
  if (latestTag !== null && !TAG_RE.test(latestTag)) {
    throw new PromotionError(`invalid latest release tag ${JSON.stringify(latestTag)}`);
  }
  if (isLatest && latestTag !== tag) {
    throw new PromotionError(
      `latest release pointer is ${latestTag ?? '<absent>'}, want published release ${tag}`,
    );
  }
  if (!isLatest && latestTag === tag) {
    throw new PromotionError(`release ${tag} was marked Latest even though it must not own that pointer`);
  }
}

function remoteNotFound(result) {
  return result.status !== 0 && /(?:\b404\b|not found|manifest unknown)/i.test(
    `${result.stdout}\n${result.stderr}`,
  );
}

function readOCIManifest(ref) {
  const result = capture('oras', ['manifest', 'fetch', ref]);
  if (result.status !== 0) return { result, document: null };
  try {
    return { result, document: JSON.parse(result.stdout) };
  } catch (cause) {
    throw new PromotionError(`OCI manifest ${ref} returned malformed JSON: ${cause.message}`);
  }
}

export function validateOCIPayloadManifest(
  document,
  { label, layers, config = null },
) {
  const problems = [];
  if (document === null || typeof document !== 'object' || Array.isArray(document)) {
    throw new PromotionError(`${label} manifest must be an object`);
  }
  if (document.schemaVersion !== 2) problems.push(`${label} schemaVersion must be 2`);
  if (!Array.isArray(document.layers)) {
    problems.push(`${label} layers must be an array`);
  } else if (document.layers.length !== layers.length) {
    problems.push(`${label} has ${document.layers.length} layers, want ${layers.length}`);
  } else {
    for (let index = 0; index < layers.length; index += 1) {
      const actual = document.layers[index];
      const wanted = layers[index];
      for (const key of ['mediaType', 'digest', 'size']) {
        if (actual?.[key] !== wanted[key]) {
          problems.push(
            `${label} layer ${index} ${key} is ${JSON.stringify(actual?.[key])}, ` +
              `want ${JSON.stringify(wanted[key])}`,
          );
        }
      }
    }
  }
  if (config) {
    for (const key of ['mediaType', 'digest', 'size']) {
      if (document.config?.[key] !== config[key]) {
        problems.push(
          `${label} config ${key} is ${JSON.stringify(document.config?.[key])}, ` +
            `want ${JSON.stringify(config[key])}`,
        );
      }
    }
  }
  if (problems.length > 0) {
    throw new PromotionError(`${label} manifest is invalid:\n${problems.map((p) => `- ${p}`).join('\n')}`);
  }
  return document;
}

export function chartCandidateFiles(manifest, root) {
  const version = manifest.versions.chart;
  const wanted = {
    package: `chart/cerberus-${version}.tgz`,
    metadata: 'chart/artifacthub-repo.yml',
    metadataConfig: 'chart/artifacthub-config.yml',
  };
  const byPath = new Map(manifest.files.map((file) => [file.path, file]));
  const result = {};
  for (const [kind, path] of Object.entries(wanted)) {
    const file = byPath.get(path);
    if (!file) throw new PromotionError(`candidate has no bound chart ${kind} file ${path}`);
    result[kind] = { ...file, path: resolve(root, path) };
  }
  return result;
}

function chartTarget() {
  const repository = requiredEnv('RELEASE_CHART_REPOSITORY', OCI_REPOSITORY_RE);
  const name = String(process.env.RELEASE_CHART_NAME || 'cerberus').trim();
  if (!TAG_RE.test(name)) throw new PromotionError(`invalid chart name ${JSON.stringify(name)}`);
  return {
    repository,
    hostPath: repository.replace(/^oci:\/\//, ''),
    name,
  };
}

function resolvedDigest(ref) {
  const digest = runChecked('oras', ['resolve', ref], `resolve ${ref}`).stdout.trim();
  if (!DIGEST_RE.test(digest)) {
    throw new PromotionError(`${ref} resolved to invalid digest ${JSON.stringify(digest)}`);
  }
  return digest;
}

function chartMode() {
  const root = resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate');
  const sealed = verifyCandidate(root);
  const files = chartCandidateFiles(sealed.manifest, root);
  const target = chartTarget();
  const version = sealed.manifest.versions.chart;
  const ref = `${target.hostPath}/${target.name}:${version}`;
  const expectedLayer = {
    mediaType: 'application/vnd.cncf.helm.chart.content.v1.tar+gzip',
    digest: files.package.sha256,
    size: files.package.size,
  };
  let remote = readOCIManifest(ref);
  let pushedDigest = null;
  if (remote.document) {
    validateOCIPayloadManifest(remote.document, {
      label: `chart ${ref}`,
      layers: [expectedLayer],
    });
  } else {
    if (!remoteNotFound(remote.result)) {
      throw new PromotionError(
        `could not determine whether chart ${ref} exists:\n${remote.result.stdout}${remote.result.stderr}`,
      );
    }
    const pushed = runChecked(
      'helm',
      ['push', files.package.path, target.repository],
      `push exact candidate chart ${ref}`,
    );
    const match = `${pushed.stdout}\n${pushed.stderr}`.match(
      /Digest:\s*(sha256:[0-9a-f]{64})/i,
    );
    if (!match) throw new PromotionError(`helm push for ${ref} returned no sha256 digest`);
    pushedDigest = match[1].toLowerCase();
    remote = readOCIManifest(ref);
    if (!remote.document) throw new PromotionError(`chart ${ref} is unreadable after push`);
    validateOCIPayloadManifest(remote.document, {
      label: `chart ${ref}`,
      layers: [expectedLayer],
    });
  }
  const digest = resolvedDigest(ref);
  if (pushedDigest && pushedDigest !== digest) {
    throw new PromotionError(`helm pushed ${pushedDigest}, but ${ref} resolves to ${digest}`);
  }
  setOutput('chart_ref', `${target.hostPath}/${target.name}@${digest}`);
  setOutput('chart_digest', digest);
  notice(`promoted exact candidate chart package ${files.package.sha256} as ${ref}@${digest}`);
}

function chartMetadataMode() {
  const root = resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate');
  const sealed = verifyCandidate(root);
  const files = chartCandidateFiles(sealed.manifest, root);
  const target = chartTarget();
  const ref = `${target.hostPath}/${target.name}:artifacthub.io`;
  const configMediaType = 'application/vnd.cncf.artifacthub.config.v1+yaml';
  const layerMediaType =
    'application/vnd.cncf.artifacthub.repository-metadata.layer.v1.yaml';
  const expected = {
    config: {
      mediaType: configMediaType,
      digest: files.metadataConfig.sha256,
      size: files.metadataConfig.size,
    },
    layers: [
      {
        mediaType: layerMediaType,
        digest: files.metadata.sha256,
        size: files.metadata.size,
      },
    ],
  };
  let remote = readOCIManifest(ref);
  let exact = false;
  if (remote.document) {
    try {
      validateOCIPayloadManifest(remote.document, {
        label: `Artifact Hub metadata ${ref}`,
        ...expected,
      });
      exact = true;
    } catch (cause) {
      if (!(cause instanceof PromotionError)) throw cause;
    }
  } else if (!remoteNotFound(remote.result)) {
    throw new PromotionError(
      `could not determine whether Artifact Hub metadata ${ref} exists:\n` +
        `${remote.result.stdout}${remote.result.stderr}`,
    );
  }
  if (!exact) {
    runChecked(
      'oras',
      [
        'push',
        ref,
        '--config',
        `${files.metadataConfig.path}:${configMediaType}`,
        `${files.metadata.path}:${layerMediaType}`,
      ],
      `push exact candidate Artifact Hub metadata ${ref}`,
    );
    remote = readOCIManifest(ref);
  }
  if (!remote.document) throw new PromotionError(`Artifact Hub metadata ${ref} is unreadable after push`);
  validateOCIPayloadManifest(remote.document, {
    label: `Artifact Hub metadata ${ref}`,
    ...expected,
  });
  const digest = resolvedDigest(ref);
  setOutput('metadata_ref', `${target.hostPath}/${target.name}@${digest}`);
  setOutput('metadata_digest', digest);
  notice(`promoted exact candidate Artifact Hub metadata ${files.metadata.sha256} as ${ref}`);
}

function releaseStageMode() {
  const root = resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate');
  const sealed = verifyCandidate(root);
  const tag = requiredEnv('RELEASE_APP_TAG', TAG_RE);
  const sha = requiredEnv('RELEASE_SOURCE_SHA', SHA_RE);
  const version = requiredEnv('RELEASE_APP_VERSION');
  const prerelease = isPrereleaseVersion(version);
  if (tag !== sealed.manifest.versions.app_tag || version !== sealed.manifest.versions.app) {
    throw new PromotionError(
      `release identity ${tag}/${version} does not equal candidate ` +
        `${sealed.manifest.versions.app_tag}/${sealed.manifest.versions.app}`,
    );
  }
  if (sha !== sealed.manifest.source.sha) {
    throw new PromotionError(
      `release source ${sha} does not equal candidate source ${sealed.manifest.source.sha}`,
    );
  }
  requireRemoteTagAtSource(tag, sha);
  const assets = releaseAssetInventory(sealed.manifest, root);
  const existing = readRelease(tag);
  if (existing.document) {
    if (existing.document.draft === false) {
      validatePublishedRelease(existing.document, {
        tag,
        sha,
        assets,
        prerelease,
      });
      notice(`published release ${tag} already contains the exact candidate assets`);
      return;
    }
    validateDraftRelease(existing.document, {
      tag,
      sha,
      assets,
      complete: false,
      prerelease,
    });
    runChecked(
      'gh',
      ['release', 'upload', tag, ...assets.map((asset) => asset.path), '--clobber'],
      `upload exact assets to ${tag}`,
    );
  } else {
    if (!releaseNotFound(existing.result)) {
      throw new PromotionError(
        `could not determine whether release ${tag} exists:\n${existing.result.stdout}${existing.result.stderr}`,
      );
    }
    const args = [
      'release',
      'create',
      tag,
      ...assets.map((asset) => asset.path),
      '--draft',
      '--verify-tag',
      '--target',
      sha,
      '--title',
      tag,
      '--generate-notes',
    ];
    if (prerelease) args.push('--prerelease');
    runChecked('gh', args, `stage draft release ${tag}`);
  }
  const staged = readRelease(tag);
  if (!staged.document) {
    throw new PromotionError(`draft release ${tag} was not readable after asset upload`);
  }
  validateDraftRelease(staged.document, {
    tag,
    sha,
    assets,
    complete: true,
    prerelease,
  });
  notice(`staged ${assets.length} exact candidate assets on draft ${tag}`);
}

function releasePublishMode() {
  const tag = requiredEnv('RELEASE_APP_TAG', TAG_RE);
  const sha = requiredEnv('RELEASE_SOURCE_SHA', SHA_RE);
  const version = requiredEnv('RELEASE_APP_VERSION');
  const prerelease = isPrereleaseVersion(version);
  const isLatest = booleanEnv('RELEASE_IS_LATEST');
  const root = resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate');
  const sealed = verifyCandidate(root);
  if (tag !== sealed.manifest.versions.app_tag || version !== sealed.manifest.versions.app) {
    throw new PromotionError(
      `release identity ${tag}/${version} does not equal candidate ` +
        `${sealed.manifest.versions.app_tag}/${sealed.manifest.versions.app}`,
    );
  }
  if (sha !== sealed.manifest.source.sha) {
    throw new PromotionError(
      `release source ${sha} does not equal candidate source ${sealed.manifest.source.sha}`,
    );
  }
  requireRemoteTagAtSource(tag, sha);
  if (prerelease && isLatest) {
    throw new PromotionError(`prerelease ${tag} cannot own the Latest pointer`);
  }
  const assets = releaseAssetInventory(sealed.manifest, root);
  const staged = readRelease(tag);
  if (!staged.document) {
    throw new PromotionError(`draft release ${tag} is missing before publication`);
  }
  if (staged.document.draft === true) {
    validateDraftRelease(staged.document, {
      tag,
      sha,
      assets,
      complete: true,
      prerelease,
    });
    runChecked(
      'gh',
      [
        'release',
        'edit',
        tag,
        '--draft=false',
        `--prerelease=${prerelease}`,
        `--latest=${isLatest}`,
      ],
      `publish release ${tag}`,
    );
  }
  const published = readRelease(tag);
  if (!published.document) {
    throw new PromotionError(`release ${tag} is unreadable after publication`);
  }
  validatePublishedRelease(published.document, {
    tag,
    sha,
    assets,
    prerelease,
  });
  validateLatestRelease({ tag, isLatest, latestTag: readLatestReleaseTag() });
  notice(`published exact candidate release ${tag} with latest=${isLatest} prerelease=${prerelease}`);
}

export function caskWrite({ isLatest, repository, version }) {
  if (!isLatest) return null;
  if (!REPOSITORY_RE.test(repository)) throw new PromotionError(`invalid tap repository ${repository}`);
  return {
    endpoint: `repos/${repository}/contents/Casks/cerberus.rb`,
    message: `release: update cask to ${version}`,
  };
}

function readRepositoryFile(endpoint) {
  const result = capture('gh', ['api', endpoint]);
  if (result.status !== 0) {
    if (releaseNotFound(result)) return null;
    throw new PromotionError(
      `could not read repository file ${endpoint}:\n${result.stdout}${result.stderr}`,
    );
  }
  let document;
  try {
    document = JSON.parse(result.stdout);
  } catch (cause) {
    throw new PromotionError(`repository file ${endpoint} returned malformed JSON: ${cause.message}`);
  }
  const sha = String(document?.sha ?? '').trim();
  const content = String(document?.content ?? '').replace(/\s/g, '');
  if (!/^[0-9a-f]{40}$/.test(sha) || !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(content)) {
    throw new PromotionError(`repository file ${endpoint} returned invalid sha/content`);
  }
  return { sha, content };
}

function caskMode() {
  const isLatest = booleanEnv('RELEASE_IS_LATEST');
  const repository = requiredEnv('RELEASE_TAP_REPOSITORY', REPOSITORY_RE);
  const version = requiredEnv('RELEASE_APP_VERSION');
  const write = caskWrite({ isLatest, repository, version });
  if (write === null) {
    notice('candidate is not the highest stable release; the shared cask remains unchanged');
    return;
  }
  const sealed = candidate();
  const caskFile = sealed.manifest.files.find((file) => file.path === 'app/homebrew/cerberus.rb');
  if (!caskFile) throw new PromotionError('candidate has no bound generated cask');
  const content = readFileSync(
    resolve(process.env.RELEASE_CANDIDATE_DIR || 'build/release-candidate', caskFile.path),
  ).toString('base64');
  const existing = readRepositoryFile(write.endpoint);
  if (existing?.content === content) {
    notice(`the shared cask already equals the exact candidate for ${version}`);
    return;
  }
  const args = [
    'api',
    write.endpoint,
    '--method',
    'PUT',
    '-f',
    `message=${write.message}`,
    '-f',
    `content=${content}`,
  ];
  if (existing) args.push('-f', `sha=${existing.sha}`);
  runChecked('gh', args, `publish cask to ${repository}`);
  const published = readRepositoryFile(write.endpoint);
  if (!published || published.content !== content) {
    throw new PromotionError('published cask bytes do not equal the qualified candidate cask');
  }
  notice(`published the exact generated cask for ${version}`);
}

function main() {
  const mode = process.env.MODE || process.argv[2];
  verifyPromotionAuthorization({
    authorized: process.env.RELEASE_AUTHORIZED,
    publish: process.env.RELEASE_PUBLISH,
    dryRun: process.env.RELEASE_DRY_RUN,
  });
  if (mode === 'tag') tagMode();
  else if (mode === 'image') imageMode();
  else if (mode === 'release-stage') releaseStageMode();
  else if (mode === 'release-publish') releasePublishMode();
  else if (mode === 'cask') caskMode();
  else if (mode === 'chart') chartMode();
  else if (mode === 'chart-metadata') chartMetadataMode();
  else if (mode === 'verify-only') notice('direct promotion authorization verified');
  else throw new PromotionError(
    `MODE must be tag, image, release-stage, release-publish, cask, chart, chart-metadata, ` +
      `or verify-only; ` +
      `got ${JSON.stringify(mode)}`,
  );
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
