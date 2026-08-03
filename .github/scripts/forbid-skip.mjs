// forbid-skip.mjs — the GA test-discipline gate, extracted from the
// `forbid-skip` job in .github/workflows/ci.yml.
//
// Each invocation runs ONE discipline scan selected by $CHECK, mirroring
// the original per-step granularity (step name + `if:` preserved in YAML).
// Every scan: `git ls-files` the relevant pathspecs, apply the SAME regex
// the bash used, print matches, emit `::error::` + exit 1 on any hit,
// exit 0 clean.
//
// The regexes are kept byte-identical to ci.yml's prior inline forms AND
// to scripts/test-forbid-skip.sh (the self-test that pins them against
// canonical match / no-match examples). When you widen or normalise a
// pattern here, update docs/forbid-skip.md AND scripts/test-forbid-skip.sh
// in the same change — the self-test is the contract.
//
// Env contract:
//   CHECK  `all` (run every scan below, in registry order), or one of:
//     t-skip            Reject t.Skip / t.Skipf / t.SkipNow in *_test.go
//     soft-assert       Reject soft-assertion / silent-recover patterns
//     should-skip       Reject non-empty should_skip: overlay entries
//     escape-hatch      Reject test escape-hatch primitives
//     feature-discipline Reject scenario-suppressing tags in .feature files
//                       and the godog skip / pending routes in harness Go
//
// Exit codes: 0 = clean, 1 = a banned pattern was found (or bad $CHECK).

import { lsFiles, error, log, capture } from './lib/gh.mjs';
import process from 'node:process';

const CHECK = process.env.CHECK || '';

// grepFiles — run `grep` with the given flag string over a pathspec set,
// preserving the exact `git ls-files -z | xargs -0 grep ... --` pipeline.
// Returns { matched, output }. We shell out to grep (not a JS regex) so
// the ERE semantics + multi-file `-H` line addressing match the original
// byte-for-byte.
function grepFiles({ pathspecs, grepFlags, regex }) {
  const files = lsFiles(pathspecs);
  if (files.length === 0) return { matched: false, output: '' };
  const res = capture('grep', [...grepFlags, '-e', regex, '--', ...files]);
  // grep exit 0 = match found, 1 = no match, >1 = real error.
  if (res.status > 1) {
    error(`grep failed (status ${res.status}): ${res.stderr.trim()}`);
    process.exit(res.status);
  }
  return { matched: res.status === 0, output: res.stdout };
}

// perlSlurp — replicate the `git ls-files -z | xargs -0 perl -0777 -ne` shape.
// Runs the perl program once per matched file (xargs would batch, but per
// $ARGV the line-number arithmetic is identical) and concatenates output.
function perlSlurp({ pathspecs, program }) {
  const files = lsFiles(pathspecs);
  let out = '';
  for (const f of files) {
    const res = capture('perl', ['-0777', '-ne', program, f]);
    if (res.status > 1) {
      error(`perl failed on ${f}: ${res.stderr.trim()}`);
      process.exit(res.status);
    }
    out += res.stdout;
  }
  return out;
}

function fail(message) {
  error(message);
  process.exit(1);
}

// CHECKS — the registry of discipline scans, keyed by $CHECK. This is the
// single source of truth for the scan set: `all` iterates it, the unknown-CHECK
// error enumerates it, and doc-counts.mjs derives both the doc-stated scan count
// and the workflow-caller assertion from these keys. A scan added or removed
// here therefore cannot leave a stale caller or a stale doc behind.
const CHECKS = {
  't-skip': () => {
    const { matched, output } = grepFiles({
      pathspecs: ['*_test.go', ':!:compatibility/*/upstream/**'],
      grepFlags: ['-nE'],
      regex: 't\\.Skip[fN]?\\(',
    });
    if (matched) {
      log(output);
      fail('t.Skip / t.Skipf / t.SkipNow found in test files — fix the bug, do not skip');
    }
  },

  'soft-assert': () => {
    let bad = false;
    const softAssert = grepFiles({
      pathspecs: ['*_test.go', ':!:compatibility/*/upstream/**'],
      grepFlags: ['-nEH'],
      regex:
        'assert\\.Contains\\(([^,]+,\\s*){0,1}[^,]+,\\s*""\\s*\\)|assert\\.ElementsMatch\\(([^,]+,\\s*){0,1}[^,]+,\\s*\\[\\][^)]*\\{\\s*\\}\\s*\\)',
    });
    if (softAssert.matched) {
      log(softAssert.output);
      bad = true;
    }
    // Multi-line silent-recover scan — identical perl program to ci.yml.
    const matches = perlSlurp({
      pathspecs: ['*_test.go', ':!:compatibility/*/upstream/**'],
      program:
        'while (/defer\\s+recover\\s*\\(\\s*\\)|defer\\s+func\\s*\\(\\s*\\)\\s*\\{[^{}]*_\\s*=\\s*recover\\s*\\(\\s*\\)/g) {\n  my $pre = substr($_, 0, $-[0]);\n  my $line = ($pre =~ tr/\\n//) + 1;\n  print "$ARGV:$line: silent-recover pattern\\n";\n}',
    });
    if (matches.length > 0) {
      process.stdout.write(matches.endsWith('\n') ? matches : `${matches}\n`);
      bad = true;
    }
    if (bad) {
      fail(
        'soft-assertion or silent-recover pattern found — replace assert.Contains(x, "") with the actual substring, replace assert.ElementsMatch(x, []T{}) with len(x) == 0, replace defer recover() / defer func(){_ = recover()}() with assert.Panics(t, func(){...}, "reason")',
      );
    }
  },

  'should-skip': () => {
    const matches = perlSlurp({
      pathspecs: [
        'compatibility/**/*.yml',
        'compatibility/**/*.yaml',
        ':!:compatibility/*/upstream/**',
      ],
      program:
        'while (/^[ \\t]*should_skip:[ \\t]*\\n(?:[ \\t]*\\#[^\\n]*\\n|[ \\t]*\\n)*[ \\t]+-/mg) {\n  my $pre = substr($_, 0, $-[0]);\n  my $line = ($pre =~ tr/\\n//) + 1;\n  print "$ARGV:$line: non-empty should_skip block\\n";\n}',
    });
    if (matches.length > 0) {
      process.stdout.write(matches.endsWith('\n') ? matches : `${matches}\n`);
      fail(
        'non-empty should_skip overlay entry found. The consumer code was removed in the structural-cleanup PR; entries here are silently ignored. Fix the underlying bug instead of skipping the case.',
      );
    }
  },

  'escape-hatch': () => {
    const { matched, output } = grepFiles({
      pathspecs: [
        '*.ts',
        '*.tsx',
        '*.go',
        ':!:compatibility/*/upstream/**',
        ':!:**/node_modules/**',
        ':!:vendor/**',
        ':!:.claude/**',
      ],
      grepFlags: ['-nEH'],
      regex:
        'EXPECTED_EMPTY|EXPECTED_TOLERATED|isKnownTolerated|tolerated404|expect\\.soft|should_tolerate|skipReason|SkipReason|APP_NOT_INSTALLED_BANNER_PATTERNS|DRILLDOWN_UPSTREAM_GRAFANA_CONSOLE_NOISE',
    });
    if (matched) {
      log(output);
      fail(
        'test escape-hatch pattern found. Every assertion must fail loud; never mask a failure with an allow-list / tolerance / soft-assert. Fix the bug at the source (cerberus code, seed, dashboard, panel).',
      );
    }
  },

  'feature-discipline': () => {
    let bad = false;
    // A Cucumber runner's default is to report an unimplemented step as
    // *pending* and carry on — a skip wearing a hat. A scenario-suppressing
    // tag is the same move at the scenario level: `@wip` on a Scenario means
    // it never runs while the lane still reports green.
    // Case-insensitive: Gherkin tags are a closed vocabulary here (the story
    // `@MIG-\d\d`, tier `@tier[0-2]`, and archetype `@archetype:...` forms are
    // all fixed-case by construction), so this vocabulary legitimately never
    // needs mixed case — `-i` closes a scan gap (`@WIP`, `@Skip`, ...) without
    // risking a false positive on unrelated text.
    const tags = grepFiles({
      pathspecs: ['*.feature', ':!:**/node_modules/**'],
      grepFlags: ['-nEHi'],
      regex: '(^|[ \\t])@(wip|skip|ignore|manual|todo|pending)([ \\t]|$)',
    });
    if (tags.matched) {
      log(tags.output);
      bad = true;
    }
    // godog's Go-side skip routes. Step definitions live in NON-test .go
    // files, so the `t\.Skip[fN]?\(` scan over `*_test.go` structurally
    // cannot see them: a step returning godog.ErrSkip / godog.ErrPending, or
    // calling Skip / Skipf / SkipNow on the TestingT godog hands it, would
    // suppress the assertion with the gate none the wiser.
    const goSkips = grepFiles({
      pathspecs: ['test/e2e/migration/**/*.go'],
      grepFlags: ['-nEH'],
      regex: 'godog\\.(ErrSkip|ErrPending)|\\.Skip(f|Now)?\\(',
    });
    if (goSkips.matched) {
      log(goSkips.output);
      bad = true;
    }
    if (bad) {
      fail(
        'a scenario-suppressing tag or a godog skip/pending route was found. A scenario runs and asserts, or it is deleted — @wip / @skip / @ignore / @manual / @todo / @pending and godog.ErrSkip / godog.ErrPending / T(ctx).Skip are t.Skip wearing a hat. Fix the scenario or remove it.',
      );
    }
  },
};

// `all` runs every registered scan in one invocation, in registry order. It is
// for callers that want the whole discipline set without enumerating it (the
// compatibility lane's cheap-first `gate`); ci.yml keeps one named step per scan
// so a failure is identifiable from the job graph alone.
const ALL = 'all';

if (CHECK === ALL) {
  for (const [name, scan] of Object.entries(CHECKS)) {
    log(`forbid-skip: ${name}`);
    scan();
  }
} else if (Object.hasOwn(CHECKS, CHECK)) {
  CHECKS[CHECK]();
} else {
  error(
    `forbid-skip.mjs: unknown CHECK="${CHECK}" (expected ${ALL}, or one of: ${Object.keys(CHECKS).join(', ')})`,
  );
  process.exit(1);
}

// Reached only on a clean scan. The original bash printed nothing on pass;
// keep the log quiet but exit 0 explicitly.
process.exit(0);
