// fork-tag-drift — move go.mod's tsouza/* fork `replace` targets onto the
// newest `-cerberus-*` tag each fork carries.
//
// Every `replace <module> => github.com/tsouza/<fork>[/<subdir>] vX.Y.Z-cerberus-<flavor>`
// line in every tracked go.mod is read. For each fork, the tags are listed with
// `git ls-remote --tags`, and the target is the newest version that exists for
// every submodule path the go.mod files replace (collector-contrib carries one
// tag per submodule, and all of them move together). When the target is newer
// than a pinned version:
//
//   - A fork whose tag version core equals the replaced module's `require`
//     version is release-tracked (prometheus: `v0.315.0` and
//     `v0.315.0-cerberus-parser`). Moving it also moves the require and the
//     reference image, which TestForkVersionSkew holds together, so the script
//     opens one issue naming the release move instead of a pull request.
//   - Any other fork gets one pull request from the branch
//     `deps/fork-tag/<fork>`: every go.mod's replace lines are rewritten, each
//     module is re-tidied, and the branch is force-pushed. A branch whose go.mod
//     already pins the target is left alone, so a daily run neither re-pushes
//     an open pull request nor reopens one that was closed.
//
// ENV CONTRACT
//   GITHUB_REPOSITORY  `owner/repo` the issue and pull request are opened in.
//                      Required unless DRY_RUN is set.
//   GH_TOKEN           token `gh` uses. A pull request opened with the
//                      workflow's own token runs no pull_request checks, so
//                      the workflow passes RELEASE_PAT (also the checkout's
//                      push credential). Required unless DRY_RUN is set.
//   BASE_BRANCH        branch the pull request targets. Default `main`.
//   DRY_RUN            `1` reports the plan and changes nothing.
//   FORK_HOST          base URL the fork tags are listed from. Default
//                      `https://github.com`.
//
// The pure planning functions are exported for fork-tag-drift.test.mjs.
//
// Exit: 0 when the plan was carried out (or there was nothing to do), 1 when a
// git, go or gh step failed.

import { readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { appendStepSummary, capture, error, git, log, lsFiles, notice, writeFreshFile } from './lib/gh.mjs';

const FORK_OWNER = 'tsouza';
const BRANCH_PREFIX = 'deps/fork-tag/';
const BODY_FILE = join(process.env.RUNNER_TEMP || tmpdir(), 'fork-tag-drift-body.md');
// The bot identity commits are authored under.
const COMMIT_IDENTITY = ['-c', 'user.name=github-actions[bot]', '-c', 'user.email=41898282+github-actions[bot]@users.noreply.github.com'];

// A fork replace line, inside or outside a `replace (...)` block.
const REPLACE_RE = new RegExp(
  String.raw`^\s*(?:replace\s+)?(\S+)\s+=>\s+github\.com/${FORK_OWNER}/([^/\s]+)(?:/(\S+))?\s+(v\d+\.\d+\.\d+-cerberus-[a-z0-9-]+)\s*$`,
  'gm',
);
const VERSION_RE = /^v(\d+)\.(\d+)\.(\d+)-cerberus-([a-z0-9-]+)$/;

// parseVersion splits `vX.Y.Z-cerberus-<flavor>`; null for anything else.
export function parseVersion(v) {
  const m = VERSION_RE.exec(v);
  if (!m) return null;
  return { core: [Number(m[1]), Number(m[2]), Number(m[3])], flavor: m[4] };
}

// compareVersions orders two fork tags of one flavor by their numeric core.
// Lexical order is wrong here: `v0.0.10` sorts before `v0.0.9`, and the
// prometheus fork carries both `v0.0.41-cerberus-parser` and the newer
// `v0.315.0-cerberus-parser`.
export function compareVersions(a, b) {
  const pa = parseVersion(a);
  const pb = parseVersion(b);
  if (!pa || !pb) throw new Error(`not a fork tag: ${pa ? b : a}`);
  for (let i = 0; i < pa.core.length; i++) {
    if (pa.core[i] !== pb.core[i]) return pa.core[i] - pb.core[i];
  }
  return 0;
}

// parseForkReplaces returns every tsouza/* fork replace in one go.mod text.
export function parseForkReplaces(text) {
  const out = [];
  for (const m of text.matchAll(REPLACE_RE)) {
    const version = m[4];
    out.push({ module: m[1], fork: m[2], subdir: m[3] ?? '', version, flavor: parseVersion(version).flavor });
  }
  return out;
}

// requireVersion returns the version a go.mod `require`s module at, or null.
export function requireVersion(text, module) {
  const escaped = module.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const m = new RegExp(String.raw`^\s*(?:require\s+)?${escaped}\s+(v\S+)`, 'm').exec(text);
  return m ? m[1] : null;
}

// parseTagRefs turns `git ls-remote --tags` output into tag names.
export function parseTagRefs(lsRemote) {
  const tags = new Set();
  for (const line of lsRemote.split('\n')) {
    const ref = line.split('\t')[1];
    if (!ref?.startsWith('refs/tags/')) continue;
    tags.add(ref.slice('refs/tags/'.length).replace(/\^\{\}$/, ''));
  }
  return [...tags];
}

// versionsFor returns the `-cerberus-<flavor>` versions tagged for one
// submodule path (`''` for the fork's root module).
export function versionsFor(tags, subdir, flavor) {
  const prefix = subdir ? `${subdir}/` : '';
  return tags
    .filter((t) => t.startsWith(prefix))
    .map((t) => t.slice(prefix.length))
    .filter((v) => !v.includes('/') && parseVersion(v)?.flavor === flavor);
}

// planBumps decides, per fork, whether a newer tag exists for every submodule
// the go.mod files replace. `files` is [{ path, text }]; `tagsByFork` maps a
// fork name to its tag list. Returns [{ fork, from, to, releaseTracked, modules }].
export function planBumps(files, tagsByFork) {
  const byFork = new Map();
  for (const { text } of files) {
    for (const r of parseForkReplaces(text)) {
      const entry = byFork.get(r.fork) ?? { fork: r.fork, flavor: r.flavor, replaces: [], releaseTracked: false };
      entry.replaces.push(r);
      const req = requireVersion(text, r.module);
      if (req && `${req}-cerberus-${r.flavor}` === r.version) entry.releaseTracked = true;
      byFork.set(r.fork, entry);
    }
  }
  const plan = [];
  for (const entry of byFork.values()) {
    const tags = tagsByFork.get(entry.fork) ?? [];
    const subdirs = [...new Set(entry.replaces.map((r) => r.subdir))];
    const sets = subdirs.map((s) => new Set(versionsFor(tags, s, entry.flavor)));
    const common = [...sets[0]].filter((v) => sets.every((s) => s.has(v)));
    if (common.length === 0) continue;
    const to = common.sort(compareVersions).at(-1);
    const from = entry.replaces.map((r) => r.version).sort(compareVersions)[0];
    if (compareVersions(to, from) <= 0) continue;
    plan.push({
      fork: entry.fork,
      from,
      to,
      releaseTracked: entry.releaseTracked,
      modules: [...new Set(entry.replaces.map((r) => r.module))].sort(),
    });
  }
  return plan.sort((a, b) => a.fork.localeCompare(b.fork));
}

// applyBump rewrites every replace target on `fork` in one go.mod text to `to`.
export function applyBump(text, fork, to) {
  return text.replace(REPLACE_RE, (line, _module, f, _subdir, version) =>
    f === fork ? line.replace(version, to) : line,
  );
}

export function branchFor(fork) {
  return `${BRANCH_PREFIX}${fork}`;
}

export function prTitle(bump) {
  return `chore(deps): bump ${FORK_OWNER}/${bump.fork} fork to ${bump.to}`;
}

export function issueTitle(bump) {
  return `deps: ${FORK_OWNER}/${bump.fork} fork tag ${bump.to} is not in go.mod`;
}

export function prBody(bump) {
  return [
    '## Summary',
    '',
    `The forks-monitor tagged \`${FORK_OWNER}/${bump.fork}\` at \`${bump.to}\`; go.mod pins \`${bump.from}\`.`,
    'This moves every go.mod `replace` on that fork to the new tag and re-tidies each module.',
    '',
    'Replaced modules:',
    '',
    ...bump.modules.map((m) => `- \`${m}\``),
    '',
    `Review what the tag changes: \`git log ${bump.from}..${bump.to}\` on the fork, limited to the`,
    'paths cerberus imports. A change to the DDL templates or the parser changes what cerberus',
    'emits, so dispatch `compatibility.yml` on this branch before queueing it.',
    '',
    'Opened by `.github/workflows/fork-tag-drift.yml` (see `docs/upstream-forks.md`).',
    '',
  ].join('\n');
}

export function issueBody(bump) {
  return [
    '## Problem',
    '',
    `\`${FORK_OWNER}/${bump.fork}\` carries \`${bump.to}\`; go.mod pins \`${bump.from}\`.`,
    'This fork is release-tracked: its tag names the upstream release the `require` and the',
    'compatibility reference image also name, and `TestForkVersionSkew` holds the three together,',
    'so a replace-only bump cannot pass.',
    '',
    '## Fix',
    '',
    'Move to the new release in one pull request, as `docs/upstream-forks.md`',
    '("Moving Prometheus to a new release") describes.',
    '',
    '## Acceptance',
    '',
    `- go.mod and every nested go.mod replace the fork with \`${bump.to}\`.`,
    '- `TestForkVersionSkew` passes.',
    '',
    'Opened by `.github/workflows/fork-tag-drift.yml`.',
    '',
  ].join('\n');
}

function must(res, what) {
  if (res.status !== 0) {
    throw new Error(`${what} failed (exit ${res.status}): ${res.stderr.trim() || res.stdout.trim()}`);
  }
  return res.stdout;
}

function listTags(fork, host) {
  return parseTagRefs(must(git(['ls-remote', '--tags', `${host}/${FORK_OWNER}/${fork}.git`]), `ls-remote ${fork}`));
}

function readGoMods() {
  return lsFiles(['go.mod', '**/go.mod'])
    .map((path) => ({ path, text: readFileSync(path, 'utf8') }))
    .filter((f) => parseForkReplaces(f.text).length > 0);
}

// ensureIssue opens the release-move issue unless one with the same title is open.
function ensureIssue(bump, repo) {
  const title = issueTitle(bump);
  const open = JSON.parse(
    must(
      capture('gh', ['issue', 'list', '-R', repo, '--state', 'open', '--search', `"${title}" in:title`, '--json', 'title']),
      'gh issue list',
    ),
  );
  if (open.some((i) => i.title === title)) {
    notice(`${bump.fork}: issue "${title}" is already open`);
    return;
  }
  writeFreshFile(BODY_FILE, issueBody(bump));
  must(capture('gh', ['issue', 'create', '-R', repo, '--title', title, '--body-file', BODY_FILE]), 'gh issue create');
  notice(`${bump.fork}: opened issue "${title}"`);
}

// remoteBranchPins reports whether origin's branch already pins bump.to.
function remoteBranchPins(branch, bump) {
  const heads = must(git(['ls-remote', '--heads', 'origin', branch]), 'ls-remote origin');
  if (!heads.trim()) return false;
  must(git(['fetch', '--quiet', 'origin', branch]), `fetch ${branch}`);
  const goMod = must(git(['show', 'FETCH_HEAD:go.mod']), `show ${branch}:go.mod`);
  return parseForkReplaces(goMod).some((r) => r.fork === bump.fork && r.version === bump.to);
}

function openPullRequest(bump, files, repo, base) {
  const branch = branchFor(bump.fork);
  if (remoteBranchPins(branch, bump)) {
    notice(`${bump.fork}: ${branch} already pins ${bump.to}`);
    return;
  }
  must(git(['checkout', '-B', branch, `origin/${base}`]), `checkout ${branch}`);
  for (const f of files) {
    const text = readFileSync(f.path, 'utf8');
    writeFileSync(f.path, applyBump(text, bump.fork, bump.to));
  }
  // Root module first: a nested module that replaces the root resolves
  // against the root's freshly tidied graph.
  for (const dir of files.map((f) => dirname(f.path)).sort((a, b) => a.length - b.length)) {
    must(capture('go', ['mod', 'tidy'], { cwd: dir }), `go mod tidy in ${dir}`);
  }
  must(git(['add', '--', ...files.flatMap((f) => [f.path, f.path.replace(/go\.mod$/, 'go.sum')])]), 'git add');
  must(git([...COMMIT_IDENTITY, 'commit', '--quiet', '--signoff', '-m', prTitle(bump)]), 'git commit');
  must(git(['push', '--force', 'origin', `HEAD:refs/heads/${branch}`]), `push ${branch}`);
  const existing = JSON.parse(
    must(capture('gh', ['pr', 'list', '-R', repo, '--head', branch, '--state', 'open', '--json', 'number']), 'gh pr list'),
  );
  if (existing.length > 0) {
    notice(`${bump.fork}: updated pull request #${existing[0].number}`);
    return;
  }
  writeFreshFile(BODY_FILE, prBody(bump));
  must(
    capture('gh', [
      'pr', 'create', '-R', repo, '--base', base, '--head', branch,
      '--title', prTitle(bump), '--body-file', BODY_FILE,
      '--label', 'dependencies',
    ]),
    'gh pr create',
  );
  notice(`${bump.fork}: opened a pull request moving ${bump.from} to ${bump.to}`);
}

export function main(env = process.env) {
  const dryRun = env.DRY_RUN === '1';
  const repo = env.GITHUB_REPOSITORY;
  const base = env.BASE_BRANCH || 'main';
  const host = env.FORK_HOST || 'https://github.com';
  if (!dryRun && (!repo || !env.GH_TOKEN)) {
    error('GITHUB_REPOSITORY and GH_TOKEN are required unless DRY_RUN=1');
    return 1;
  }
  const files = readGoMods();
  if (files.length === 0) {
    error('no go.mod carries a tsouza/* fork replace — the parser is broken, not go.mod');
    return 1;
  }
  const forks = [...new Set(files.flatMap((f) => parseForkReplaces(f.text).map((r) => r.fork)))];
  const tagsByFork = new Map(forks.map((f) => [f, listTags(f, host)]));
  const plan = planBumps(files, tagsByFork);

  const rows = plan.map((b) => `| \`${b.fork}\` | \`${b.from}\` | \`${b.to}\` | ${b.releaseTracked ? 'issue' : 'pull request'} |`);
  appendStepSummary(
    plan.length === 0
      ? `Every fork replace pins its fork's newest tag (${forks.join(', ')}).\n`
      : ['| Fork | Pinned | Newest | Action |', '| --- | --- | --- | --- |', ...rows, ''].join('\n'),
  );
  for (const b of plan) log(`${b.fork}: ${b.from} -> ${b.to}${b.releaseTracked ? ' (release-tracked)' : ''}`);
  if (dryRun || plan.length === 0) return 0;

  let status = 0;
  for (const bump of plan) {
    try {
      if (bump.releaseTracked) ensureIssue(bump, repo);
      else openPullRequest(bump, files, repo, base);
    } catch (e) {
      error(`${bump.fork}: ${e.message}`);
      status = 1;
    }
  }
  return status;
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) {
  process.exit(main());
}
