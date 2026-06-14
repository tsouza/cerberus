// gh.mjs — shared helpers for the cerberus .github/scripts/*.mjs modules.
//
// Dependency-light by design: imports only node: builtins so the scripts
// run on a bare `ubuntu-latest` runner with `run: node ...` and need no
// `setup-node` / `npm install` step. No @actions/* toolkit dependency.
//
// Provides:
//   - GitHub workflow-command emitters (::error:: / ::notice:: / ::group::)
//     with the same escaping the official toolkit uses, so multiline
//     messages and `title=` properties survive the log annotation layer.
//   - exec() / git() / capture() wrappers around node:child_process for
//     the git-ls-files + sub-shell logic the extracted bash used.
//   - lsFiles(): a `git ls-files -z` wrapper returning a string[] of paths,
//     honouring include globs + `:!:`-style pathspec excludes.
//   - appendStepSummary(): append markdown to $GITHUB_STEP_SUMMARY.
//   - setOutput(): append `name=value` to $GITHUB_OUTPUT.

import { spawnSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import process from 'node:process';

// GitHub escapes `%`, `\r`, `\n` in workflow-command *data* (the message
// body) and additionally `:` + `,` in *property* values. Mirror the
// @actions/core escaping so multiline ::error:: bodies render intact.
function escapeData(s) {
  return String(s).replace(/%/g, '%25').replace(/\r/g, '%0D').replace(/\n/g, '%0A');
}

function escapeProp(s) {
  return String(s)
    .replace(/%/g, '%25')
    .replace(/\r/g, '%0D')
    .replace(/\n/g, '%0A')
    .replace(/:/g, '%3A')
    .replace(/,/g, '%2C');
}

function renderProps(props) {
  const entries = Object.entries(props || {}).filter(([, v]) => v !== undefined && v !== null);
  if (entries.length === 0) return '';
  return ' ' + entries.map(([k, v]) => `${k}=${escapeProp(v)}`).join(',');
}

// ::error:: — annotate a failure. `props` may carry { title, file, line }.
export function error(message, props = {}) {
  process.stdout.write(`::error${renderProps(props)}::${escapeData(message)}\n`);
}

// ::notice:: — annotate an informational pass.
export function notice(message, props = {}) {
  process.stdout.write(`::notice${renderProps(props)}::${escapeData(message)}\n`);
}

// ::warning:: — annotate a non-fatal concern.
export function warning(message, props = {}) {
  process.stdout.write(`::warning${renderProps(props)}::${escapeData(message)}\n`);
}

// ::group:: / ::endgroup:: — collapsible log section. Pass a sync fn.
export function group(title, fn) {
  process.stdout.write(`::group::${escapeData(title)}\n`);
  try {
    return fn();
  } finally {
    process.stdout.write('::endgroup::\n');
  }
}

// Plain stdout line (no annotation). Kept here so scripts import one module.
export function log(line = '') {
  process.stdout.write(`${line}\n`);
}

// capture() — run a command, return { status, stdout, stderr }. Never
// throws on a non-zero exit; the caller decides what a failure means.
// `input` (Buffer|string) is fed to stdin when provided. `timeout` (ms), when
// set, bounds the run — spawnSync kills the child past the deadline and sets
// res.error, which we surface as a non-zero { status }.
export function capture(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, {
    encoding: opts.encoding === undefined ? 'utf8' : opts.encoding,
    maxBuffer: opts.maxBuffer ?? 256 * 1024 * 1024,
    input: opts.input,
    cwd: opts.cwd,
    env: opts.env ?? process.env,
    timeout: opts.timeout,
    killSignal: opts.killSignal,
  });
  if (res.error) {
    return { status: 127, stdout: res.stdout ?? '', stderr: String(res.error.message) };
  }
  return {
    status: res.status === null ? 1 : res.status,
    stdout: res.stdout ?? '',
    stderr: res.stderr ?? '',
  };
}

// exec() — like capture() but exits the process with the command's status
// when it fails, after streaming stderr. For "run this or die" steps.
export function exec(cmd, args, opts = {}) {
  const res = capture(cmd, args, opts);
  if (res.status !== 0) {
    if (res.stdout) process.stdout.write(res.stdout);
    if (res.stderr) process.stderr.write(res.stderr);
    process.exit(res.status);
  }
  return res.stdout;
}

// git() — capture()-style git wrapper. Returns { status, stdout, stderr }.
export function git(args, opts = {}) {
  return capture('git', args, opts);
}

// lsFiles() — `git ls-files -z <pathspecs>` -> string[] of tracked paths.
// Honours include globs and `:!:`/`:(exclude)` exclude pathspecs exactly
// as the extracted bash did (git ls-files already drops .gitignored paths).
export function lsFiles(pathspecs, opts = {}) {
  const res = git(['ls-files', '-z', '--', ...pathspecs], opts);
  if (res.status !== 0) {
    error(`git ls-files failed: ${res.stderr.trim()}`);
    process.exit(res.status);
  }
  return res.stdout.split('\0').filter((p) => p.length > 0);
}

// appendStepSummary() — append markdown to the job summary, when the
// runner exposes $GITHUB_STEP_SUMMARY. No-op (logged) off-runner.
export function appendStepSummary(markdown) {
  const file = process.env.GITHUB_STEP_SUMMARY;
  if (!file) {
    log('[no $GITHUB_STEP_SUMMARY in env; step-summary markdown follows]');
    log(markdown);
    return;
  }
  appendFileSync(file, markdown.endsWith('\n') ? markdown : `${markdown}\n`);
}

// setOutput() — append `name=value` to $GITHUB_OUTPUT for downstream steps.
export function setOutput(name, value) {
  const file = process.env.GITHUB_OUTPUT;
  const line = `${name}=${value}`;
  if (!file) {
    log(line);
    return;
  }
  appendFileSync(file, `${line}\n`);
}
