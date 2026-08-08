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

// The spawnSync options capture()/exec() forward. Anything outside this set is
// a caller mistake, and one that used to be SILENT: capture() builds its own
// options object rather than spreading `opts`, so an unrecognised key was
// dropped without a word. `capture(cmd, [], { shell: true })` therefore ran the
// whole command string as argv[0], which never resolves — the caller saw a
// plain non-zero status and read it as "the command said no", so the gate that
// depended on it was permanently, quietly wrong. Failing loudly turns that into
// a first-run crash instead of a lane that reports nothing for months.
const CAPTURE_OPTS = new Set(['encoding', 'maxBuffer', 'input', 'cwd', 'env', 'timeout', 'killSignal']);

// capture() — run a command, return { status, stdout, stderr }. Never
// throws on a non-zero exit; the caller decides what a failure means.
// `input` (Buffer|string) is fed to stdin when provided. `timeout` (ms), when
// set, bounds the run — spawnSync kills the child past the deadline and sets
// res.error, which we surface as a non-zero { status }.
//
// There is no `shell` option on purpose. To run something that needs a shell,
// spawn one explicitly: capture('sh', ['-c', '<script>']).
export function capture(cmd, args, opts = {}) {
  const unknown = Object.keys(opts).filter((k) => !CAPTURE_OPTS.has(k));
  if (unknown.length > 0) {
    throw new TypeError(
      `capture(${cmd}): unsupported option(s) ${unknown.join(', ')} — they are not forwarded to spawnSync. ` +
        `Supported: ${[...CAPTURE_OPTS].join(', ')}. For a shell, spawn one: capture('sh', ['-c', '…']).`,
    );
  }
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

// assertSafeArg guards a value that is ABOUT TO become a capture()/exec()
// argument and originates from outside this script's own literals (an env
// var override, in every current caller — CodeQL's js/indirect-command-
// line-injection). capture() never invokes a shell, so the risk a plain
// quoting fix would address does not apply here; what remains is ARGUMENT
// injection — a value starting with `-` being parsed by the invoked binary
// as a FLAG instead of the plain data (a package path, a git ref, an image
// reference) the caller intends it to be. Every current caller of this
// guard reads an env var no workflow in this repo actually sets (a
// developer-only manual override — see e.g. agpl-clean.mjs's
// AGPL_CLEAN_PACKAGE, brew-upgrade-path.mjs's TAP_MIGRATION_LEGACY_REV), so
// today's blast radius is "a developer's own shell env", but the guard
// stays in place regardless: it is what makes it safe for a FUTURE change
// to wire either one to real workflow input without a fresh audit.
export function assertSafeArg(value, label) {
  if (typeof value === 'string' && value.startsWith('-')) {
    throw new TypeError(
      `assertSafeArg: ${label}=${JSON.stringify(value)} starts with "-" — refusing to pass it as a ` +
        'subprocess argument, where it could be parsed as a flag instead of the plain value intended.',
    );
  }
  return value;
}

// git() — capture()-style git wrapper. Returns { status, stdout, stderr }.
export function git(args, opts = {}) {
  return capture('git', args, opts);
}

// lsFiles() — `git ls-files -z <pathspecs>` -> string[] of paths in scope.
// Honours include globs and `:!:`/`:(exclude)` exclude pathspecs exactly
// as the extracted bash did.
//
// `--cached --others --exclude-standard` rather than the bare tracked-only
// default: a file a generator just wrote but nobody `git add`-ed yet is
// invisible to a plain `git ls-files`, which reads the INDEX and nothing
// else. That is sound in CI (the runner checks out a committed tree, so
// nothing in scope is ever untracked) and sound in the pre-push hook (by
// push time everything scanned is committed), but it is a real gap for a
// scan run directly against a local working tree mid-edit — exactly the
// narrowed local reproduction CLAUDE.md's contribution rules ask for after
// a red CI check (issue #1938). `--exclude-standard` keeps `.gitignore`d
// paths out of scope exactly as before; only the untracked-but-NOT-ignored
// subset is newly included, and it is included for every caller (every
// `forbid-skip.mjs` scan routes through this one function).
export function lsFiles(pathspecs, opts = {}) {
  const res = git(['ls-files', '-z', '--cached', '--others', '--exclude-standard', '--', ...pathspecs], opts);
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
