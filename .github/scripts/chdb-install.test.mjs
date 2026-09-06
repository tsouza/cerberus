// chdb-install.test.mjs — node:test guard for chdb-install.mjs's pure
// platform-to-asset mapping. This is the exact logic the replaced Justfile
// `case "$os"` / `case "$arch"` recipe body encoded inline where nothing
// could unit-test it; pinning it here is the whole point of the extraction
// (CLAUDE.md invariant 15, issue #3094).

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { assetNameFor, downloadUrlFor } from './chdb-install.mjs';

test('linux x64 maps to the linux-x86_64 asset', () => {
  assert.equal(assetNameFor('linux', 'x64'), 'linux-x86_64-libchdb.tar.gz');
});

test('linux arm64 maps to the linux-aarch64 asset', () => {
  assert.equal(assetNameFor('linux', 'arm64'), 'linux-aarch64-libchdb.tar.gz');
});

test('darwin x64 maps to the macos-x86_64 asset', () => {
  assert.equal(assetNameFor('darwin', 'x64'), 'macos-x86_64-libchdb.tar.gz');
});

test('darwin arm64 maps to the macos-arm64 asset', () => {
  assert.equal(assetNameFor('darwin', 'arm64'), 'macos-arm64-libchdb.tar.gz');
});

test('an unrecognised arch on a supported OS falls back to the x86_64 asset', () => {
  // Mirrors the replaced `case "$arch" in ...; *) asset="...x86_64..." ;;`
  // default arm — this was never an allow-list of known-good arches.
  assert.equal(assetNameFor('linux', 'riscv64'), 'linux-x86_64-libchdb.tar.gz');
  assert.equal(assetNameFor('darwin', 'riscv64'), 'macos-x86_64-libchdb.tar.gz');
});

test('an unsupported platform yields no asset', () => {
  assert.equal(assetNameFor('win32', 'x64'), null);
  assert.equal(assetNameFor('freebsd', 'arm64'), null);
});

test('downloadUrlFor builds the chdb-core release asset URL', () => {
  assert.equal(
    downloadUrlFor('v26.5.0', 'linux-x86_64-libchdb.tar.gz'),
    'https://github.com/chdb-io/chdb-core/releases/download/v26.5.0/linux-x86_64-libchdb.tar.gz',
  );
});
