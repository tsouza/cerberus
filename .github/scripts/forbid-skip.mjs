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
//   CHECK  one of:
//     t-skip            Reject t.Skip / t.Skipf / t.SkipNow in *_test.go
//     not-implemented   Reject "not implemented" in internal/**/*.go (prod)
//     soft-assert       Reject soft-assertion / silent-recover patterns
//     should-skip       Reject non-empty should_skip: overlay entries
//     escape-hatch      Reject test escape-hatch primitives
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

switch (CHECK) {
  case 't-skip': {
    const { matched, output } = grepFiles({
      pathspecs: ['*_test.go', ':!:compatibility/*/upstream/**'],
      grepFlags: ['-nE'],
      regex: 't\\.Skip[fN]?\\(',
    });
    if (matched) {
      log(output);
      fail('t.Skip / t.Skipf / t.SkipNow found in test files — fix the bug, do not skip');
    }
    break;
  }

  case 'not-implemented': {
    const { matched, output } = grepFiles({
      pathspecs: ['internal/**/*.go', ':!:internal/**/*_test.go'],
      grepFlags: ['-niEH'],
      regex: 'not implemented',
    });
    if (matched) {
      log(output);
      fail(
        '"not implemented" found in production code — implement the feature or rewrite to a factual verb (rejected / unsupported / falls back)',
      );
    }
    break;
  }

  case 'soft-assert': {
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
    break;
  }

  case 'should-skip': {
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
    break;
  }

  case 'escape-hatch': {
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
    break;
  }

  default:
    error(`forbid-skip.mjs: unknown CHECK="${CHECK}" (expected one of: t-skip, not-implemented, soft-assert, should-skip, escape-hatch)`);
    process.exit(1);
}

// Reached only on a clean scan. The original bash printed nothing on pass;
// keep the log quiet but exit 0 explicitly.
process.exit(0);
