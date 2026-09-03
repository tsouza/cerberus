// markdownlint-run.mjs — the ONE markdownlint-cli2 invocation in the repo.
//
// WHY THIS MODULE EXISTS
//
// markdownlint was invoked from three places, and each resolved to a DIFFERENT
// engine. The divergence was silent, and it was in the dangerous direction:
//
//   `just lint-md` / `just fmt-md`  markdownlint-cli2@v0.18.1 -> markdownlint 0.38.0
//   lefthook `pre-commit`           a bare binary from $PATH   -> whatever is installed
//   `ci.yml`'s `lint` job           markdownlint-cli2-action   -> markdownlint 0.41.1
//
// markdownlint IGNORES a config key that names a rule it does not implement
// rather than rejecting it. `.markdownlint.yaml` pins `MD060: {style: aligned}`,
// and MD060 arrived in markdownlint 0.40 — so under the local pin that key
// configured nothing and the rule could not fire. `just lint-md` reported
// `0 error(s)` on a file whose table CI then failed on (tsouza/cerberus#2997,
// observed on tsouza/cerberus#2993 against a docs/test-strategy.md table).
//
// The cost is not one rule. CLAUDE.md invariant 5 requires a red CI check to be
// reproduced locally, narrowed, before the next push. For every rule newer than
// the local pin that is impossible: the local run cannot see the failure, so the
// author either pushes speculatively — the guess-and-wait loop invariant 5 exists
// to forbid — or concludes CI is flaky.
//
// THE FIX IS STRUCTURAL, NOT ASSERTED. `PINNED_CLI2_VERSION` below is the only
// markdownlint-cli2 version literal in the repository, and every caller routes
// through this module: the `lint-md` / `fmt-md` recipes, the lefthook
// `pre-commit` hook, and `ci.yml`'s `lint` job. There is no second declaration
// for a drift gate to compare against, and no glob set that can disagree with
// another glob set — `GLOBS` is likewise single. See `.github/scripts/README.md`
// for why that made a drift gate the wrong shape here.
//
// MD060 HAS NO AUTO-FIXER. `--fix` cannot repair a misaligned table; only
// `scripts/align-md-tables.py` can, which is why lefthook runs that as its own
// `md-table-align` hook BEFORE this one, and why `--fix` here prints where the
// padding pass lives when MD060 survives the fix.
//
// Modes:
//   (no args)             lint the whole tree. `just lint-md`, and ci.yml.
//   --fix                 lint and auto-fix the whole tree. `just fmt-md`.
//   --staged FILE...      auto-fix the named files. lefthook `pre-commit`.
//   --print-version       print PINNED_CLI2_VERSION. `just install-tools`.
//
// Engine selection differs between the whole-tree modes and `--staged`, and the
// reason is measured, not stylistic. `npm exec` costs 2.6-4.5 s per invocation
// even with the package already in the npx cache and even with `--offline`; the
// cost is npm's own startup, not the download. That is fine for a CI step and
// for a deliberate `just lint-md`, and it is 5x over the budget CLAUDE.md sets
// for `pre-commit` ("sub-second formatters on staged files"). So `--staged`
// prefers a $PATH binary — but ONLY when that binary's own package.json reports
// exactly PINNED_CLI2_VERSION. Anything else (absent, or any other version)
// falls back to the pinned `npm exec` and prints how to get the fast path. The
// hook is therefore slow-but-correct in the worst case and never, in any case,
// runs an engine other than the pinned one. Running the wrong engine is the
// defect; being slow is not.
//
// Env:
//   GITHUB_ACTIONS  when set, each markdownlint error is ALSO emitted as an
//                   `::error file=,line=,col=,title=::` annotation, so the
//                   finding lands on the offending line in the PR diff. The
//                   raw `file:line:col RULE …` line is printed either way —
//                   that text is what a developer greps and reproduces, so
//                   annotating must not replace it.
//   PATH            searched for `markdownlint-cli2` in `--staged` mode.
//
// Exit: the markdownlint-cli2 exit code — 0 clean, non-zero on any finding.

import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { delimiter, dirname, join } from 'node:path';
import process from 'node:process';
import { spawnSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

import { error, log } from './lib/gh.mjs';

// The markdownlint-cli2 release the repository lints with, everywhere.
//
// It is the release `DavidAnson/markdownlint-cli2-action@v24.2.0` bundled —
// that action's package.json pins `markdownlint-cli2: 0.23.2` — because CI was
// the side enforcing the config the repo actually wrote, and the fix is for the
// local runs to see what CI sees rather than for CI to stop seeing it.
//
// Bumping it: change this literal, then run `just lint-md` and fix whatever the
// newer engine surfaces IN THE SAME CHANGE. A backlog left behind here is a
// backlog every later author inherits as a mystery red check.
export const PINNED_CLI2_VERSION = '0.23.2';

// The markdownlint release PINNED_CLI2_VERSION resolves to. Recorded so a
// reader can tell which rules are in play without resolving an npm dependency;
// nothing derives behaviour from it.
export const PINNED_MARKDOWNLINT_VERSION = '0.41.1';

// The file set, single-sourced for the same reason as the version. The three
// `compatibility/*/upstream` trees are vendored reference backends whose
// Markdown is not ours to reformat; `.markdownlintignore` names them too, and
// both are kept because the glob set is what a `--staged` run has instead of
// the ignore file's directory walk.
export const GLOBS = [
  '**/*.md',
  '!compatibility/prometheus/upstream/**',
  '!compatibility/tempo/upstream/**',
  '!compatibility/loki/upstream/**',
  '!**/node_modules/**',
];

// markdownlint-cli2 writes findings to stderr, one per line, as
//   <file>:<line>[:<col>] error <RULE>[/<alias>] <description> [<detail>]
// The `error` token and the optional column are both version-dependent shapes,
// which is exactly why the version is pinned: this parser is written against
// PINNED_CLI2_VERSION's output and nothing else.
const ERROR_LINE = /^(?<file>[^\s:][^:]*):(?<line>\d+)(?::(?<column>\d+))?\s+(?:error\s+)?(?<rule>MD\d{3}\S*)\s+(?<message>.+)$/;

// parseErrorLine — one stderr line to a finding, or null when the line is not
// one (cli2 also writes blank lines and, on a crash, a stack trace).
export function parseErrorLine(line) {
  const m = ERROR_LINE.exec(line);
  if (!m) return null;
  const { file, line: lineNumber, column, rule, message } = m.groups;
  return {
    file,
    line: Number(lineNumber),
    column: column === undefined ? undefined : Number(column),
    rule,
    message,
  };
}

// chooseEngine — which markdownlint-cli2 a `--staged` run may use.
//
// `pathVersion` is the version the $PATH binary reports, or null when there is
// no usable binary on $PATH. The pinned engine is the answer to everything that
// is not an exact match, so a wrong version can never be the one that runs.
export function chooseEngine(pathVersion) {
  if (pathVersion === PINNED_CLI2_VERSION) {
    return { kind: 'path', reason: `$PATH markdownlint-cli2 is ${pathVersion}` };
  }
  const found = pathVersion === null
    ? 'no markdownlint-cli2 on $PATH'
    : `$PATH markdownlint-cli2 is ${pathVersion}`;
  return {
    kind: 'pinned',
    reason: `${found}, not the pinned ${PINNED_CLI2_VERSION}`,
  };
}

// whichBinary — first executable named `name` on $PATH, or null.
function whichBinary(name, env = process.env) {
  for (const dir of (env.PATH || '').split(delimiter)) {
    if (!dir) continue;
    const candidate = join(dir, name);
    if (existsSync(candidate)) return candidate;
  }
  return null;
}

// pathBinaryVersion — the version of the $PATH markdownlint-cli2, or null.
//
// Read from the package's own package.json rather than by running the binary
// with `--version`: a spawn costs ~0.7 s, which is the entire `pre-commit`
// budget spent on finding out whether we may spend it.
export function pathBinaryVersion(env = process.env) {
  const bin = whichBinary('markdownlint-cli2', env);
  if (!bin) return null;
  let real;
  try {
    real = realpathSync(bin);
  } catch {
    return null;
  }
  // A global install resolves to
  // <prefix>/lib/node_modules/markdownlint-cli2/markdownlint-cli2-bin.mjs, so
  // the package root is the first ancestor carrying a package.json. Walk a
  // bounded number of levels; an unbounded walk would happily read some
  // unrelated ancestor's manifest.
  const maxAncestors = 4;
  let dir = dirname(real);
  for (let i = 0; i < maxAncestors; i += 1) {
    const manifest = join(dir, 'package.json');
    if (existsSync(manifest)) {
      try {
        const pkg = JSON.parse(readFileSync(manifest, 'utf8'));
        if (pkg.name === 'markdownlint-cli2' && typeof pkg.version === 'string') {
          return pkg.version;
        }
      } catch {
        return null;
      }
      return null;
    }
    dir = dirname(dir);
  }
  return null;
}

// report — print findings, annotating them when running under Actions.
//
// The raw line is printed unconditionally and FIRST: it is the text a developer
// greps for and re-runs against, and CLAUDE.md invariant 5 turns on being able
// to reproduce that exact string locally. The annotation is additive.
export function report(stderrText, env = process.env) {
  let findings = 0;
  for (const line of stderrText.split('\n')) {
    const trimmed = line.replace(/\r$/, '');
    if (trimmed.trim() === '') continue;
    log(trimmed);
    const finding = parseErrorLine(trimmed);
    if (!finding) continue;
    findings += 1;
    if (!env.GITHUB_ACTIONS) continue;
    error(finding.message, {
      title: finding.rule,
      file: finding.file,
      line: finding.line,
      col: finding.column,
    });
  }
  return findings;
}

// runEngine — spawn the selected markdownlint-cli2 over `args`.
function runEngine(engine, args) {
  const [command, prefix] = engine.kind === 'path'
    ? ['markdownlint-cli2', []]
    : ['npm', ['exec', '--yes', '--', `markdownlint-cli2@${PINNED_CLI2_VERSION}`]];
  const result = spawnSync(command, [...prefix, ...args], {
    encoding: 'utf8',
    stdio: ['ignore', 'inherit', 'pipe'],
  });
  if (result.error) {
    error(`could not run markdownlint-cli2: ${result.error.message}`);
    return { status: 1, stderr: '' };
  }
  return { status: result.status ?? 1, stderr: result.stderr ?? '' };
}

const MD060_HINT =
  'MD060 (table-column-style) has no auto-fixer. Realign tables with '
  + '`python3 scripts/align-md-tables.py <file>...` — lefthook runs that as its '
  + 'own `md-table-align` pre-commit hook, before the fixer.';

function main(argv, env = process.env) {
  if (argv.includes('--print-version')) {
    log(PINNED_CLI2_VERSION);
    return 0;
  }

  const staged = argv.indexOf('--staged');
  const fix = argv.includes('--fix') || staged !== -1;

  let engine;
  let targets;
  if (staged === -1) {
    engine = { kind: 'pinned', reason: 'whole-tree run' };
    targets = GLOBS;
  } else {
    targets = argv.slice(staged + 1);
    // lefthook passes {staged_files}, which is empty when nothing matched the
    // glob. Linting nothing is success, and it must not pay for an engine.
    if (targets.length === 0) return 0;
    engine = chooseEngine(pathBinaryVersion(env));
    if (engine.kind === 'pinned') {
      log(
        `markdownlint: ${engine.reason}; falling back to the pinned engine `
        + '(slower). `just install-tools` installs the matching binary.',
      );
    }
  }

  const { status, stderr } = runEngine(engine, fix ? ['--fix', ...targets] : targets);
  const findings = report(stderr, env);
  if (findings > 0 && fix && stderr.includes('MD060')) log(MD060_HINT);
  return status;
}

const invokedDirectly = process.argv[1]
  && import.meta.url === pathToFileURL(realpathSync(process.argv[1])).href;
if (invokedDirectly) {
  process.exit(main(process.argv.slice(2)));
}
