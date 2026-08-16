import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  PromotionError,
  candidatePromotionTag,
  chartCandidateFiles,
  caskWrite,
  imagePromotionPlan,
  imagePromotionPreflight,
  isPrereleaseVersion,
  releaseAssetInventory,
  releaseAssets,
  remoteAnnotatedTagTarget,
  tagCommands,
  validateDraftRelease,
  validateLatestRelease,
  validateOCIPayloadManifest,
  validatePublishedRelease,
  verifyPromotionAuthorization,
} from './release-promote.mjs';

const digest = `sha256:${'a'.repeat(64)}`;
const sha = 'b'.repeat(40);

test('every promotion mode requires direct complete authorization evidence', () => {
  const evidence = { attestation: {}, candidate: {}, digest };
  assert.equal(
    verifyPromotionAuthorization({
      authorized: 'true',
      publish: 'true',
      dryRun: 'false',
      verifier: () => evidence,
    }),
    evidence,
  );
  for (const input of [
    { authorized: 'false', publish: 'true', dryRun: 'false' },
    { authorized: 'true', publish: 'false', dryRun: 'false' },
    { authorized: 'true', publish: 'true', dryRun: 'true' },
  ]) {
    let verified = false;
    assert.throws(
      () =>
        verifyPromotionAuthorization({
          ...input,
          verifier: () => {
            verified = true;
            return evidence;
          },
        }),
      PromotionError,
    );
    assert.equal(verified, false, 'invalid writer guards must fail before attestation I/O');
  }
  assert.throws(
    () =>
      verifyPromotionAuthorization({
        authorized: 'true',
        publish: 'true',
        dryRun: 'false',
        verifier: () => ({}),
      }),
    /incomplete authorization evidence/,
  );
});

test('tag plan creates and pushes only one immutable exact-source tag', () => {
  const manifest = { versions: { app_tag: 'v1.2.3', chart: '4.5.6' } };
  assert.equal(candidatePromotionTag(manifest, 'app'), 'v1.2.3');
  assert.equal(candidatePromotionTag(manifest, 'chart'), 'chart-v4.5.6');
  assert.throws(() => candidatePromotionTag(manifest, 'other'), PromotionError);
  const plan = tagCommands({ tag: 'v1.2.3', message: 'release v1.2.3', sha });
  assert.deepEqual(plan.create, ['git', ['tag', '-a', 'v1.2.3', '-m', 'release v1.2.3', sha]]);
  assert.deepEqual(plan.push, ['git', ['push', 'origin', 'refs/tags/v1.2.3']]);
  assert.throws(() => tagCommands({ tag: '../bad', message: 'bad', sha }), PromotionError);
  assert.throws(() => tagCommands({ tag: 'v1.2.3', message: '', sha }), PromotionError);
  assert.throws(() => tagCommands({ tag: 'v1.2.3', message: 'x', sha: 'short' }), PromotionError);
  assert.deepEqual(
    remoteAnnotatedTagTarget(
      `${'c'.repeat(40)}\trefs/tags/v1.2.3\n${sha}\trefs/tags/v1.2.3^{}\n`,
      'v1.2.3',
    ),
    { annotated: true, target: sha },
  );
  assert.deepEqual(
    remoteAnnotatedTagTarget(`${sha}\trefs/tags/v1.2.3\n`, 'v1.2.3'),
    { annotated: false, target: sha },
  );
  assert.equal(remoteAnnotatedTagTarget('', 'v1.2.3'), null);
});

test('image promotion copies one candidate digest and creates aliases from it', () => {
  const plan = imagePromotionPlan({
    layout: '/candidate/app/image',
    sourceName: '1.2.3',
    digest,
    destinations: ['registry.example/one/app', 'mirror.example/two/app'],
    tags: ['1.2.3', 'v1.2.3', 'latest'],
  });
  assert.equal(plan.length, 2);
  for (const item of plan) {
    assert.equal(item.source, '/candidate/app/image:1.2.3');
    assert.equal(item.digestRef, `${item.destination}@${digest}`);
    assert.deepEqual(item.aliases, ['v1.2.3']);
    assert.deepEqual(item.rollingAliases, ['latest']);
  }
  assert.throws(
    () => imagePromotionPlan({ layout: 'x', sourceName: 'x', digest: 'bad', destinations: ['a'], tags: ['v'] }),
    PromotionError,
  );

  const states = imagePromotionPreflight({
    plan,
    digest,
    resolveRef: (ref) =>
      ref.endsWith(':1.2.3')
        ? { exists: true, digest }
        : { exists: false, digest: null },
  });
  assert.equal(states.get('registry.example/one/app:1.2.3').exists, true);
  assert.equal(states.get('registry.example/one/app:latest').exists, false);
  const advanced = imagePromotionPreflight({
    plan,
    digest,
    resolveRef: (ref) => ({
      exists: true,
      digest: ref.endsWith(':latest') ? `sha256:${'0'.repeat(64)}` : digest,
    }),
  });
  assert.equal(advanced.get('registry.example/one/app:latest').rolling, true);
  assert.equal(advanced.get('registry.example/one/app:latest').digest, `sha256:${'0'.repeat(64)}`);
  assert.throws(
    () =>
      imagePromotionPreflight({
        plan,
        digest,
        resolveRef: (ref) => ({
          exists: true,
          digest: ref.includes('mirror.example')
            ? `sha256:${'0'.repeat(64)}`
            : digest,
        }),
      }),
    /refusing to overwrite/,
  );
});

test('release assets are exactly four archives plus the bound checksums', () => {
  const files = [
    ...['darwin_amd64', 'darwin_arm64', 'linux_amd64', 'linux_arm64'].map((target) => ({
      path: `app/assets/cerberus_1.2.3_${target}.tar.gz`,
      size: 10,
      sha256: digest,
    })),
    { path: 'app/assets/checksums.txt', size: 20, sha256: digest },
    { path: 'app/image/index.json' },
  ];
  const assets = releaseAssets({ files }, '/candidate');
  assert.equal(assets.length, 5);
  assert(assets.every((path) => path.startsWith('/candidate/app/assets/')));
  const inventory = releaseAssetInventory({ files }, '/candidate');
  assert.deepEqual(
    inventory.map((asset) => asset.name),
    [
      'cerberus_1.2.3_darwin_amd64.tar.gz',
      'cerberus_1.2.3_darwin_arm64.tar.gz',
      'cerberus_1.2.3_linux_amd64.tar.gz',
      'cerberus_1.2.3_linux_arm64.tar.gz',
      'checksums.txt',
    ],
  );
  assert.throws(() => releaseAssets({ files: files.slice(1) }, '/candidate'), PromotionError);

  const document = {
    draft: true,
    tag_name: 'v1.2.3',
    target_commitish: sha,
    assets: inventory.map((asset) => ({
      name: asset.name,
      size: asset.size,
      digest: asset.digest,
    })),
  };
  assert.equal(
    validateDraftRelease(document, {
      tag: 'v1.2.3',
      sha,
      assets: inventory,
      complete: true,
    }),
    document,
  );
  assert.throws(
    () =>
      validateDraftRelease(
        {
          ...document,
          assets: [...document.assets, { name: 'stale.zip', size: 1, digest }],
        },
        { tag: 'v1.2.3', sha, assets: inventory, complete: false },
      ),
    /unexpected stale asset/,
  );
  assert.throws(
    () =>
      validateDraftRelease(
        { ...document, assets: document.assets.slice(1) },
        { tag: 'v1.2.3', sha, assets: inventory, complete: true },
      ),
    /missing candidate asset/,
  );
  assert.throws(
    () =>
      validateDraftRelease(
        { ...document, draft: false },
        { tag: 'v1.2.3', sha, assets: inventory, complete: true },
      ),
    /draft state/,
  );

  const published = {
    ...document,
    draft: false,
    immutable: true,
    prerelease: false,
    // The release API reports the repository default branch here even when
    // the already-created annotated tag targets an exact SHA. Writer modes
    // verify that tag target separately before accepting this completion.
    target_commitish: 'main',
  };
  assert.equal(
    validatePublishedRelease(published, {
      tag: 'v1.2.3',
      sha,
      assets: inventory,
      prerelease: false,
    }),
    published,
  );
  assert.throws(
    () =>
      validatePublishedRelease(
        { ...published, immutable: false },
        { tag: 'v1.2.3', sha, assets: inventory, prerelease: false },
      ),
    /not immutable/,
  );
  assert.throws(
    () =>
      validatePublishedRelease(
        { ...published, prerelease: true },
        { tag: 'v1.2.3', sha, assets: inventory, prerelease: false },
      ),
    /prerelease state/,
  );
});

test('publication completion accepts exact retries and verifies prerelease/latest state', () => {
  assert.equal(isPrereleaseVersion('1.2.3'), false);
  assert.equal(isPrereleaseVersion('1.2.3-rc.1'), true);
  assert.throws(() => isPrereleaseVersion('v1.2.3'), PromotionError);

  assert.doesNotThrow(() =>
    validateLatestRelease({ tag: 'v1.2.3', isLatest: true, latestTag: 'v1.2.3' }),
  );
  assert.doesNotThrow(() =>
    validateLatestRelease({ tag: 'v1.2.2', isLatest: false, latestTag: 'v1.2.3' }),
  );
  assert.throws(
    () => validateLatestRelease({ tag: 'v1.2.3', isLatest: true, latestTag: null }),
    /want published release/,
  );
  assert.throws(
    () => validateLatestRelease({ tag: 'v1.2.2', isLatest: false, latestTag: 'v1.2.2' }),
    /marked Latest/,
  );
});

test('only the highest stable release gets a cask write plan', () => {
  assert.equal(caskWrite({ isLatest: false, repository: 'runtime/value', version: '1.2.3' }), null);
  assert.deepEqual(
    caskWrite({ isLatest: true, repository: 'runtime/value', version: '1.2.3' }),
    {
      endpoint: 'repos/runtime/value/contents/Casks/cerberus.rb',
      message: 'release: update cask to 1.2.3',
    },
  );
  assert.throws(() => caskWrite({ isLatest: true, repository: 'bad', version: '1.2.3' }), PromotionError);
});

test('chart and Artifact Hub promotion select only candidate-bound payloads', () => {
  const manifest = {
    versions: { chart: '4.5.6' },
    files: [
      { path: 'chart/cerberus-4.5.6.tgz', size: 10, sha256: digest },
      { path: 'chart/artifacthub-repo.yml', size: 20, sha256: digest },
      { path: 'chart/artifacthub-config.yml', size: 0, sha256: digest },
    ],
  };
  const files = chartCandidateFiles(manifest, '/candidate');
  assert.equal(files.package.path, '/candidate/chart/cerberus-4.5.6.tgz');
  assert.equal(files.metadata.path, '/candidate/chart/artifacthub-repo.yml');

  const expected = {
    mediaType: 'application/vnd.example.payload',
    digest,
    size: 10,
  };
  const document = {
    schemaVersion: 2,
    config: {
      mediaType: 'application/vnd.example.config',
      digest,
      size: 0,
    },
    layers: [expected],
  };
  assert.equal(
    validateOCIPayloadManifest(document, {
      label: 'candidate payload',
      config: document.config,
      layers: [expected],
    }),
    document,
  );
  assert.throws(
    () =>
      validateOCIPayloadManifest(
        { ...document, layers: [{ ...expected, digest: `sha256:${'0'.repeat(64)}` }] },
        { label: 'candidate payload', config: document.config, layers: [expected] },
      ),
    /digest/,
  );
  assert.throws(
    () => chartCandidateFiles({ ...manifest, files: manifest.files.slice(1) }, '/candidate'),
    /chart package/,
  );
});
