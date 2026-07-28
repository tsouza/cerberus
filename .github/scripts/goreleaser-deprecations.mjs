// goreleaser-deprecations.mjs — fails the build when .goreleaser.yml uses any
// goreleaser feature that upstream has deprecated.
//
// Why this is a gate rather than something to notice in a log: `goreleaser
// check` reports deprecations as advisory lines and still exits 0, and the only
// place goreleaser otherwise runs is release.yml — which has no `pull_request:`
// trigger. So a deprecation notice first becomes visible in the scroll-back of a
// release that already published, which is how `brews:` stayed in the config for
// several releases after upstream replaced it with `homebrew_casks:`. Reading
// the real tool's output (rather than scanning the YAML for a hardcoded list of
// dead keys) means a deprecation announced upstream tomorrow is caught the same
// day, without this file needing to know about it in advance.
//
// A red here is a real task, not a flake: release.yml pins `version: '~> v2'`,
// so whatever goreleaser deprecates inside v2 is exactly what the next release
// run will warn about, and the fix is to migrate the config.
//
// Env contract:
//   GORELEASER_CONFIG  config file to check (default `.goreleaser.yml`).
//   GORELEASER_BIN     binary to invoke (default `goreleaser`).
//
// Exit: 0 when the config validates with no deprecation notices; 1 when
// goreleaser reports one, or when `goreleaser check` itself fails or cannot run.

import { spawnSync } from 'node:child_process';

import { error as ghError, notice as ghNotice } from './lib/gh.mjs';

// Every deprecation notice goreleaser emits links its entry on the deprecations
// page, so the URL is the most reliable marker. The other two are belt-and-
// braces for a notice that is worded without the link — matching more broadly
// costs a false positive at worst, while matching too narrowly reproduces the
// exact miss this gate exists to prevent.
const DEPRECATION_MARKERS = [/goreleaser\.com\/deprecations/i, /\bDEPRECATED\b/, /\bphased out\b/i];

// goreleaser prefixes each advisory line with a bullet; strip it so the error
// reads as a sentence.
const BULLET_PREFIX = /^\s*[•*-]\s*/;

// deprecationsIn — the deprecation notices in a `goreleaser check` transcript,
// deduplicated. goreleaser repeats a notice once per offending config entry
// (two `dockers` blocks produce two identical lines), and reporting the same
// migration N times obscures how many DISTINCT things need fixing.
export function deprecationsIn(output) {
  const seen = new Set();
  for (const raw of String(output ?? '').split('\n')) {
    const line = raw.replace(BULLET_PREFIX, '').trim();
    if (!line) continue;
    if (DEPRECATION_MARKERS.some((re) => re.test(line))) seen.add(line);
  }
  return [...seen];
}

function main() {
  const config = process.env.GORELEASER_CONFIG || '.goreleaser.yml';
  const bin = process.env.GORELEASER_BIN || 'goreleaser';

  const res = spawnSync(bin, ['check', config], { encoding: 'utf8' });
  if (res.error) {
    ghError(`goreleaser-deprecations: could not run \`${bin} check\` (${res.error.message}).`);
    process.exit(1);
  }

  // stdout and stderr both carry advisory lines depending on the version, and a
  // gate that reads only one of them would go quiet the moment goreleaser moves
  // them.
  const output = `${res.stdout ?? ''}\n${res.stderr ?? ''}`;
  const found = deprecationsIn(output);

  if (res.status !== 0) {
    ghError(`goreleaser-deprecations: \`${bin} check ${config}\` failed (exit ${res.status}).\n${output.trim()}`);
    process.exit(1);
  }

  if (found.length > 0) {
    for (const d of found) ghError(`goreleaser-deprecations: ${d}`);
    ghError(
      `${config} uses ${found.length} deprecated goreleaser feature(s). Migrate the config — ` +
        `a deprecated section keeps working only until goreleaser's next major, and the release ` +
        `workflow is the only other place this would ever surface.`,
    );
    process.exit(1);
  }

  ghNotice(`goreleaser-deprecations: ${config} validates with no deprecation notices.`);
  process.exit(0);
}

const invokedDirectly = process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href;

if (invokedDirectly) main();
