// maintenance-changelog-sync.mjs — reconcile a published maintenance release's
// changelog section back to main through a normal pull request.
//
// Env: RELEASE_TAG, RELEASE_BRANCH, GITHUB_REPOSITORY, GH_TOKEN.
// The checkout begins at the published maintenance commit. This script captures
// that tree's release section, switches to current origin/main, inserts the
// section after [Unreleased], and opens a PR. It never pushes to main.

import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

const changelogPath = 'CHANGELOG.md';
const gitShaHexLength = 40;
const maintenanceBranch = /^release\/\d+\.\d+\.x$/;
const releaseTag = /^v\d+\.\d+\.\d+$/;

function sectionPattern(tag) {
  const escaped = tag.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return new RegExp(`^## \\[${escaped}\\](?: — [^\\n]+)?\\n[\\s\\S]*?(?=^## \\[|(?![\\s\\S]))`, 'm');
}

export function extractReleaseSection(changelog, tag) {
  if (!releaseTag.test(tag)) throw new Error(`invalid release tag ${tag}`);
  const match = changelog.match(sectionPattern(tag));
  if (!match) throw new Error(`${changelogPath} has no ${tag} section`);
  return match[0].trimEnd();
}

export function insertReleaseSection(mainChangelog, section, tag) {
  const existing = mainChangelog.match(sectionPattern(tag));
  if (existing) {
    if (existing[0].trimEnd() !== section.trimEnd()) {
      throw new Error(`${changelogPath} already has a conflicting ${tag} section`);
    }
    return mainChangelog;
  }
  const marker = '## [Unreleased]\n';
  const at = mainChangelog.indexOf(marker);
  if (at < 0) throw new Error(`${changelogPath} has no [Unreleased] section`);
  const afterUnreleased = at + marker.length;
  const nextRelease = /^## \[v\d+\.\d+\.\d+\]/m.exec(mainChangelog.slice(afterUnreleased));
  const insertion = nextRelease ? afterUnreleased + nextRelease.index : mainChangelog.length;
  return `${mainChangelog.slice(0, insertion).trimEnd()}\n\n${section.trimEnd()}\n\n${mainChangelog.slice(insertion).trimStart()}`;
}

export function syncBranchName(tag) {
  if (!releaseTag.test(tag)) throw new Error(`invalid release tag ${tag}`);
  return `chore/changelog-${tag.slice(1)}`;
}

export function forceLeaseArg(topic, remoteLine) {
  const remoteSha = remoteLine.trim().split(/\s+/)[0] || '';
  if (remoteSha && (!/^[0-9a-f]+$/.test(remoteSha) || remoteSha.length !== gitShaHexLength)) {
    throw new Error(`invalid remote SHA for ${topic}`);
  }
  return `--force-with-lease=refs/heads/${topic}:${remoteSha}`;
}

export function validateInputs({ tag, branch, repository }) {
  if (!releaseTag.test(tag)) throw new Error(`RELEASE_TAG must be vX.Y.Z, got ${tag}`);
  if (!maintenanceBranch.test(branch)) throw new Error(`RELEASE_BRANCH must be release/X.Y.x, got ${branch}`);
  if (!/^[^/]+\/[^/]+$/.test(repository)) throw new Error(`invalid GITHUB_REPOSITORY ${repository}`);
}

function run(command, args) {
  return execFileSync(command, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] }).trim();
}

export function main(env = process.env) {
  const tag = env.RELEASE_TAG || '';
  const branch = env.RELEASE_BRANCH || '';
  const repository = env.GITHUB_REPOSITORY || '';
  validateInputs({ tag, branch, repository });
  const section = extractReleaseSection(readFileSync(changelogPath, 'utf8'), tag);
  const topic = syncBranchName(tag);

  run('git', ['fetch', 'origin', 'main']);
  run('git', ['switch', '--force-create', topic, 'origin/main']);
  const before = readFileSync(changelogPath, 'utf8');
  const after = insertReleaseSection(before, section, tag);
  if (after === before) return { topic, changed: false };

  writeFileSync(changelogPath, after);
  run('git', ['add', changelogPath]);
  run('git', ['-c', 'user.name=github-actions[bot]',
    '-c', 'user.email=41898282+github-actions[bot]@users.noreply.github.com',
    'commit', '-m', `docs(changelog): record ${tag}`]);
  const remote = run('git', ['ls-remote', '--heads', 'origin', `refs/heads/${topic}`]);
  run('git', ['push', forceLeaseArg(topic, remote), '--set-upstream', 'origin', topic]);
  const existing = run('gh', ['pr', 'list', '--repo', repository, '--head', topic, '--base', 'main',
    '--state', 'open', '--json', 'number', '--jq', '.[0].number // empty']);
  if (!existing) {
    const bodyFile = `${env.RUNNER_TEMP || '/tmp'}/maintenance-changelog-sync.md`;
    writeFileSync(bodyFile, `Records the ${tag} changelog section published from \`${branch}\`.\n`);
    run('gh', ['pr', 'create', '--repo', repository, '--base', 'main', '--head', topic,
      '--title', `docs(changelog): record ${tag}`, '--body-file', bodyFile]);
  }
  return { topic, changed: true };
}

const invokedDirectly = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) main();
