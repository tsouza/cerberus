// manual-golden-update.mjs — trusted controller for update-golden.yml.
//
// The workflow deliberately separates generation from publication. Matrix jobs
// execute a target branch with contents:read and upload one patch containing
// only their shard's generated paths. The final job executes this controller
// from the workflow ref, validates every patch again, and performs one push.
//
// Modes and environment:
//
//   node manual-golden-update.mjs dump
//     Print the complete matrix row catalogue for static check-name indexing.
//
//   MODE=plan
//     TARGET_ROOT, BRANCH, SHARDS_INPUT, DEFAULT_BRANCH, WORKFLOW_REF,
//     PUSH_TOKEN_SET, GITHUB_OUTPUT
//
//   MODE=package
//     TARGET_ROOT, SHARD, ALLOWED_SHARDS, OUTPUT_PATH
//
//   MODE=apply-push
//     TARGET_ROOT, PATCH_ROOT, BRANCH, DEFAULT_BRANCH, SHARDS_INPUT, TARGET_SHA,
//     GITHUB_OUTPUT
//
// Exits non-zero on an unsafe branch/ref, an invalid shard set, a patch that
// touches another shard, a missing/duplicate patch, or target-branch movement.

import { spawnSync } from 'node:child_process';
import {
  lstatSync,
  mkdirSync,
  readdirSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import { SHARDS, needsChdb, resolveRequested } from './lib/golden-shards.mjs';

const SCRIPT = fileURLToPath(import.meta.url);

function fail(message) {
  process.stderr.write(`::error::${message}\n`);
  throw new Error(message);
}

function required(env, name) {
  const value = String(env[name] ?? '').trim();
  if (value === '') fail(`${name} is required`);
  return value;
}

function run(command, args, { cwd, encoding = 'utf8', stdio } = {}) {
  const result = spawnSync(command, args, { cwd, encoding, stdio });
  if (result.status !== 0) {
    const detail = encoding === 'utf8' ? String(result.stderr ?? '').trim() : '';
    fail(`${command} ${args.join(' ')} failed${detail ? `: ${detail}` : ''}`);
  }
  return result;
}

function git(root, args, opts = {}) {
  return run('git', ['-C', root, ...args], opts);
}

function parseShards(raw) {
  const { shards, error } = resolveRequested(raw);
  if (error) fail(error.replace(/^error:\s*/, ''));
  return shards;
}

export function validateBranch(branch, defaultBranch) {
  if (branch === defaultBranch) fail(`refusing to push directly to protected branch ${defaultBranch}`);
  if (branch.startsWith('refs/')) fail('BRANCH must be a short branch name, not a refs/* name');
  const verdict = spawnSync('git', ['check-ref-format', '--branch', branch], { encoding: 'utf8' });
  if (verdict.status !== 0) fail(`invalid branch name ${JSON.stringify(branch)}`);
  return branch;
}

/** A matrix row gets every selected predecessor as regeneration context. */
export function buildPlan(raw) {
  const selected = parseShards(raw);
  return {
    selected,
    matrix: {
      include: selected.map((shard) => {
        const commandShards = selected.filter(
          (candidate) => SHARDS[candidate].stage < SHARDS[shard].stage,
        );
        commandShards.push(shard);
        return {
          shard,
          command_shards: commandShards.join(' '),
          allowed_shards: commandShards.join(' '),
          needs_chdb: needsChdb(commandShards),
        };
      }),
    },
  };
}

function shardPatterns(shards) {
  return [...new Set(shards.flatMap((shard) => SHARDS[shard].goldens))].sort();
}

function escapeRegex(value) {
  return value.replace(/[|\\{}()[\]^$+?.]/g, '\\$&');
}

function patternRegex(pattern) {
  const segments = pattern.split('*').map(escapeRegex);
  const body = segments.join('[^/]+');
  return new RegExp(`^${body}(?:/|$)`);
}

function expandPathspec(root, pattern) {
  if (!pattern.includes('*')) return [pattern];
  let candidates = [''];
  for (const segment of pattern.split('/')) {
    if (!segment.includes('*')) {
      candidates = candidates.map((candidate) => path.posix.join(candidate, segment));
      continue;
    }
    const matcher = new RegExp(`^${segment.split('*').map(escapeRegex).join('.*')}$`);
    const expanded = [];
    for (const candidate of candidates) {
      const directory = path.join(root, candidate);
      let entries;
      try {
        entries = readdirSync(directory, { withFileTypes: true });
      } catch {
        entries = [];
      }
      for (const entry of entries) {
        if (matcher.test(entry.name)) expanded.push(path.posix.join(candidate, entry.name));
      }
    }
    candidates = expanded;
  }
  if (candidates.length === 0) fail(`generated path pattern matched nothing: ${pattern}`);
  return candidates;
}

function shardPathspecs(root, shards) {
  return [...new Set(shardPatterns(shards).flatMap((pattern) => expandPathspec(root, pattern)))].sort();
}

export function pathOwnedBy(file, patterns) {
  return patterns.some((pattern) => patternRegex(pattern).test(file));
}

function nulLines(value) {
  return String(value ?? '').split('\0').filter(Boolean);
}

function changedPaths(root) {
  const sets = [
    git(root, ['diff', '--name-only', '-z']).stdout,
    git(root, ['diff', '--cached', '--name-only', '-z']).stdout,
    git(root, ['ls-files', '--others', '--exclude-standard', '-z']).stdout,
  ];
  return [...new Set(sets.flatMap(nulLines))].sort();
}

function assertOwnedChanges(root, shards) {
  const patterns = shardPatterns(shards);
  const unexpected = changedPaths(root).filter((file) => !pathOwnedBy(file, patterns));
  if (unexpected.length > 0) {
    fail(`regeneration changed paths outside its shard ownership: ${unexpected.join(', ')}`);
  }
}

function assertSafeGeneratedChanges(root) {
  const unsafe = [];
  for (const file of changedPaths(root)) {
    let stat;
    try {
      stat = lstatSync(path.join(root, file));
    } catch (error) {
      if (error?.code === 'ENOENT') continue; // Deleting a generated file is safe.
      throw error;
    }
    if (!stat.isFile() || (stat.mode & 0o111) !== 0) unsafe.push(file);
  }
  if (unsafe.length > 0) {
    fail(`generated changes must be regular, non-executable files: ${unsafe.join(', ')}`);
  }
}

function writeOutput(name, value, output = process.env.GITHUB_OUTPUT) {
  if (!output) return;
  writeFileSync(output, `${name}=${value}\n`, { flag: 'a' });
}

function remoteHead(root, branch) {
  const ref = `refs/heads/${branch}`;
  const stdout = git(root, ['ls-remote', '--heads', 'origin', ref]).stdout.trim();
  const rows = stdout.split('\n').filter(Boolean);
  if (rows.length !== 1) fail(`target branch ${branch} does not resolve to exactly one remote head`);
  return rows[0].split(/\s+/)[0];
}

function plan(env) {
  const targetRoot = required(env, 'TARGET_ROOT');
  validateBranch(required(env, 'BRANCH'), required(env, 'DEFAULT_BRANCH'));
  const expectedWorkflowRef = `refs/heads/${env.DEFAULT_BRANCH}`;
  if (required(env, 'WORKFLOW_REF') !== expectedWorkflowRef) {
    fail(`manual publication must be dispatched from ${expectedWorkflowRef}`);
  }
  if (required(env, 'PUSH_TOKEN_SET') !== 'true') {
    fail('RELEASE_PAT is required so the generated push triggers pull-request checks');
  }

  const result = buildPlan(required(env, 'SHARDS_INPUT'));
  const targetSha = git(targetRoot, ['rev-parse', 'HEAD']).stdout.trim();

  writeOutput('matrix', JSON.stringify(result.matrix), env.GITHUB_OUTPUT);
  writeOutput('shards', result.selected.join(' '), env.GITHUB_OUTPUT);
  writeOutput('target_sha', targetSha, env.GITHUB_OUTPUT);
}

function packagePatch(env) {
  const targetRoot = required(env, 'TARGET_ROOT');
  const shard = parseShards(required(env, 'SHARD'));
  if (shard.length !== 1 || shard[0] !== env.SHARD) fail('SHARD must name exactly one concrete shard');
  const allowed = parseShards(required(env, 'ALLOWED_SHARDS'));
  if (!allowed.includes(shard[0])) fail('ALLOWED_SHARDS must include SHARD');

  assertOwnedChanges(targetRoot, allowed);
  assertSafeGeneratedChanges(targetRoot);
  const pathspecs = shardPathspecs(targetRoot, shard);
  git(targetRoot, ['add', '-A', '--', ...pathspecs]);

  const output = path.resolve(required(env, 'OUTPUT_PATH'));
  mkdirSync(path.dirname(output), { recursive: true });
  git(targetRoot, [
    'diff',
    '--cached',
    '--binary',
    '--full-index',
    `--output=${output}`,
    '--',
    ...pathspecs,
  ]);
  if (!lstatSync(output).isFile()) fail(`patch output is not a regular file: ${output}`);
}

function walkPatchFiles(root) {
  const out = [];
  const stack = [root];
  while (stack.length > 0) {
    const dir = stack.pop();
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isSymbolicLink()) fail(`artifact contains a symbolic link: ${full}`);
      if (entry.isDirectory()) stack.push(full);
      else if (entry.isFile() && entry.name.endsWith('.patch')) out.push(full);
    }
  }
  return out.sort();
}

function patchesByShard(root, selected) {
  const expected = new Set(selected);
  const found = new Map();
  for (const patch of walkPatchFiles(root)) {
    const shard = path.basename(patch, '.patch');
    if (!expected.has(shard)) fail(`unexpected patch artifact ${path.basename(patch)}`);
    if (found.has(shard)) fail(`duplicate patch artifact for ${shard}`);
    found.set(shard, patch);
  }
  const missing = selected.filter((shard) => !found.has(shard));
  if (missing.length > 0) fail(`missing patch artifacts: ${missing.join(', ')}`);
  return found;
}

function assertTargetUnmoved(root, branch, targetSha) {
  const current = remoteHead(root, branch);
  if (current !== targetSha) {
    fail(`origin/${branch} moved from ${targetSha} to ${current}; dispatch again on the new head`);
  }
}

function applyAndPush(env) {
  const targetRoot = required(env, 'TARGET_ROOT');
  const patchRoot = required(env, 'PATCH_ROOT');
  const branch = validateBranch(required(env, 'BRANCH'), required(env, 'DEFAULT_BRANCH'));
  const targetSha = required(env, 'TARGET_SHA');
  const selected = parseShards(required(env, 'SHARDS_INPUT'));

  const localSha = git(targetRoot, ['rev-parse', 'HEAD']).stdout.trim();
  if (localSha !== targetSha) fail(`target checkout is ${localSha}, expected ${targetSha}`);
  if (changedPaths(targetRoot).length > 0) fail('target checkout is not clean before patch application');
  assertTargetUnmoved(targetRoot, branch, targetSha);

  const patches = patchesByShard(patchRoot, selected);
  for (const shard of selected) {
    const patch = patches.get(shard);
    if (statSync(patch).size === 0) continue;
    git(targetRoot, ['apply', '--index', '--check', patch]);
    git(targetRoot, ['apply', '--index', patch]);
  }
  assertOwnedChanges(targetRoot, selected);
  assertSafeGeneratedChanges(targetRoot);

  const staged = git(targetRoot, ['diff', '--cached', '--name-only', '-z']).stdout;
  if (nulLines(staged).length === 0) {
    process.stdout.write('::notice::Golden regeneration produced no changes; nothing to push.\n');
    writeOutput('changed', 'false', env.GITHUB_OUTPUT);
    writeOutput('new_sha', targetSha, env.GITHUB_OUTPUT);
    return;
  }

  git(targetRoot, ['config', 'user.name', 'github-actions[bot]']);
  git(targetRoot, ['config', 'user.email', '41898282+github-actions[bot]@users.noreply.github.com']);
  git(targetRoot, ['commit', '-m', 'chore(goldens): regenerate selected shards'], { stdio: 'inherit' });
  assertTargetUnmoved(targetRoot, branch, targetSha);
  git(targetRoot, ['push', 'origin', `HEAD:refs/heads/${branch}`], { stdio: 'inherit' });

  writeOutput('changed', 'true', env.GITHUB_OUTPUT);
  writeOutput('new_sha', git(targetRoot, ['rev-parse', 'HEAD']).stdout.trim(), env.GITHUB_OUTPUT);
}

export function main(env = process.env) {
  const mode = required(env, 'MODE');
  if (mode === 'plan') return plan(env);
  if (mode === 'package') return packagePatch(env);
  if (mode === 'apply-push') return applyAndPush(env);
  fail(`unknown MODE ${JSON.stringify(mode)}`);
}

function dumpMatrix() {
  process.stdout.write(`${JSON.stringify(buildPlan('all').matrix.include)}\n`);
}

if (process.argv[1] && path.resolve(process.argv[1]) === path.resolve(SCRIPT)) {
  try {
    if (process.argv[2] === 'dump') dumpMatrix();
    else main();
  } catch (error) {
    if (!String(error?.message ?? '').startsWith('manual-golden-update:')) {
      process.stderr.write(`${error?.stack ?? error}\n`);
    }
    process.exit(1);
  }
}
