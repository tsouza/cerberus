// chdb-install.mjs — installs libchdb.so, the in-process ClickHouse engine
// every chdb-tagged test lane needs, for the `just chdb-install` recipe
// (just/chdb.just). Extracted from that recipe's former inline
// `case "$os"` / `case "$arch"` platform detection, URL construction,
// `curl`/`tar`/`sudo install` sequence, and idempotency short-circuit
// (CLAUDE.md invariant 15, issue #3094, epic #3091).
//
// Self-contained by design (no `lib/` import): this script does exactly one
// thing — resolve a platform to a release asset, fetch it, install it — and
// gains nothing from the shared GitHub Actions annotation/exec helpers the
// other scripts use to talk to a workflow run; its two callers are a bare
// `just chdb-install` from a contributor's shell and the identical recipe
// running inside CI, and both just want the same plain stdout narration the
// bash recipe body printed.
//
// Platform detection uses Node's own `process.platform` / `process.arch`
// rather than shelling out to `uname`, which is both fewer moving parts and
// already normalized: Node reports `arm64` for an ARM64 host on every OS it
// runs on, so unlike the replaced bash (`case "$arch" in aarch64|arm64)`)
// there is only ever one ARM64 spelling to match, not two.
//
// Pinned to chdb-core v26.5.0 (ClickHouse 26.5) by just/chdb.just's own
// `CHDB_VERSION` variable — see that file for why. The standalone
// `<platform>-libchdb.tar.gz` assets the chdb-go driver expects live in the
// `chdb-io/chdb-core` repo, whose release tags track the ClickHouse version
// directly. Mirrors update_libchdb.sh shipped inside chdb-go.
//
// Env contract:
//   CHDB_VERSION       chdb-core release tag to install, e.g. "v26.5.0"
//                       (required — just/chdb.just always passes it).
//   CHDB_INSTALL_PATH  absolute path libchdb.so is installed to, e.g.
//                       "/usr/local/lib/libchdb.so" (required, same source).
//   CHDB_PLATFORM      optional override for `process.platform` (test hook).
//   CHDB_ARCH          optional override for `process.arch` (test hook).
//
// Exit: 0 on a fresh install or the idempotent short-circuit (install path
// already exists — delete it to force a reinstall); 1 on an unsupported
// platform or any curl/tar/install failure.

import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import process from 'node:process';

// isRegularFile — true only when `path` exists and is a regular file, never
// a directory or other entry. Matches the replaced bash's `[ -f "$path" ]`
// exactly (unlike a bare existence check, which `[ -f ]` deliberately is
// not).
export function isRegularFile(path) {
  try {
    return statSync(path).isFile();
  } catch {
    return false;
  }
}

// assetNameFor — map a (platform, arch) pair to the chdb-core release asset
// filename. Unknown/x86_64 arches fall back to the x86_64 asset for that OS,
// matching the replaced Justfile `case` statement's own default arms
// (`*) asset="linux-x86_64-libchdb.tar.gz" ;;` / the Darwin equivalent) —
// this was never an allow-list of known-good arches, just a two-way arm64
// fork, and stays that shape here.
export function assetNameFor(platform, arch) {
  switch (platform) {
    case 'linux':
      return arch === 'arm64' ? 'linux-aarch64-libchdb.tar.gz' : 'linux-x86_64-libchdb.tar.gz';
    case 'darwin':
      return arch === 'arm64' ? 'macos-arm64-libchdb.tar.gz' : 'macos-x86_64-libchdb.tar.gz';
    default:
      return null;
  }
}

// downloadUrlFor — the chdb-core release asset URL for one version + asset.
export function downloadUrlFor(version, asset) {
  return `https://github.com/chdb-io/chdb-core/releases/download/${version}/${asset}`;
}

// run — spawn `cmd` with inherited stdio (curl/tar/sudo all narrate their own
// progress, and `sudo install` needs the real TTY to prompt for a password),
// exiting the process on a spawn failure or a non-zero status. Never used for
// anything whose output this script needs to inspect.
function run(cmd, args) {
  const res = spawnSync(cmd, args, { stdio: 'inherit' });
  if (res.error) {
    console.error(`chdb-install: failed to run \`${cmd}\`: ${res.error.message}`);
    process.exit(1);
  }
  if (res.status !== 0) {
    console.error(`chdb-install: \`${[cmd, ...args].join(' ')}\` exited ${res.status}`);
    process.exit(res.status ?? 1);
  }
}

function main() {
  const chdbVersion = process.env.CHDB_VERSION;
  const installPath = process.env.CHDB_INSTALL_PATH;
  if (!chdbVersion || !installPath) {
    console.error('chdb-install: CHDB_VERSION and CHDB_INSTALL_PATH must both be set (see just/chdb.just).');
    process.exit(1);
  }

  // Idempotency short-circuit: skip the download entirely once the shared
  // library is already on disk. Deleting the install path is what forces a
  // reinstall (see just/chdb.just for how to override CHDB_VERSION at the
  // same time).
  if (isRegularFile(installPath)) {
    console.log(`==> libchdb already present at ${installPath} (delete to reinstall)`);
    process.exit(0);
  }

  const platform = process.env.CHDB_PLATFORM || process.platform;
  const arch = process.env.CHDB_ARCH || process.arch;
  const asset = assetNameFor(platform, arch);
  if (!asset) {
    console.error(`chdb-install: unsupported platform: ${platform}`);
    process.exit(1);
  }

  const url = downloadUrlFor(chdbVersion, asset);
  console.log(`==> downloading ${url}`);

  const tmp = mkdtempSync(join(tmpdir(), 'chdb-install-'));
  try {
    const archivePath = join(tmp, 'libchdb.tar.gz');
    run('curl', ['-fsSL', '-o', archivePath, url]);
    run('tar', ['-C', tmp, '-xzf', archivePath]);
    console.log(`==> installing to ${installPath} (sudo may prompt)`);
    run('sudo', ['install', '-m', '0755', join(tmp, 'libchdb.so'), installPath]);
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
  console.log(`==> libchdb ${chdbVersion} installed`);
}

const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;
if (invokedDirectly) main();
