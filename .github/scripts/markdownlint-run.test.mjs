// markdownlint-run.test.mjs — guards for the single markdownlint invocation.
//
// Two things are pinned here, and they fail for different reasons.
//
// The unit tests pin the pieces that decide WHICH engine runs and how its
// output is reported. `chooseEngine` is the one place a wrong-version binary
// can be let through, so it is exercised against the exact versions the repo
// has actually had on a developer's $PATH and in the old Justfile pin.
//
// `the pinned engine enforces MD060` is the load-bearing one, and it is what
// makes this suite more than a restatement of the code. tsouza/cerberus#2997
// was not a wrong comparison — it was a pin whose engine did not implement a
// rule `.markdownlint.yaml` configures, so the config key applied to nothing
// and a clean local run meant nothing. That defect is invisible to any check
// that only compares version strings to each other. This one runs the pinned
// engine against a deliberately misaligned table and requires MD060 to fire,
// so lowering PINNED_CLI2_VERSION back below markdownlint 0.40 — or deleting
// the MD060 key — turns this suite red instead of turning `just lint-md`
// quietly green.
//
// That test spawns `npm exec`, which is the same fetch the markdownlint step
// itself performs moments later; it adds no failure mode the lint job did not
// already have.

import { strict as assert } from 'node:assert';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, copyFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';
import process from 'node:process';

import {
  GLOBS,
  PINNED_CLI2_VERSION,
  PINNED_MARKDOWNLINT_VERSION,
  chooseEngine,
  parseErrorLine,
  report,
} from './markdownlint-run.mjs';

const REPO_ROOT = fileURLToPath(new URL('../..', import.meta.url));

// A real markdownlint-cli2 0.23.2 stderr line, copied verbatim from a run.
const REAL_LINE =
  'docs/test-strategy.md:169:4812 error MD060/table-column-style Table column '
  + 'style [Table pipe does not align with header for style "aligned"]';

test('parseErrorLine reads a real cli2 finding', () => {
  const finding = parseErrorLine(REAL_LINE);
  assert.ok(finding, 'a real finding line must parse');
  assert.equal(finding.file, 'docs/test-strategy.md');
  assert.equal(finding.line, 169);
  assert.equal(finding.column, 4812);
  assert.equal(finding.rule, 'MD060/table-column-style');
  assert.match(finding.message, /^Table column style/);
});

test('parseErrorLine reads a finding with no column', () => {
  const finding = parseErrorLine('README.md:12 error MD047/single-trailing-newline Files should end with a single newline character');
  assert.ok(finding);
  assert.equal(finding.line, 12);
  assert.equal(finding.column, undefined);
});

test('parseErrorLine rejects cli2 progress output', () => {
  for (const line of [
    `markdownlint-cli2 v${PINNED_CLI2_VERSION} (markdownlint v${PINNED_MARKDOWNLINT_VERSION})`,
    'Finding: **/*.md',
    'Linting: 53 files',
    'Summary: 0 issues in 0 files',
    '',
    '    at ModuleJob.run (node:internal/modules/esm/module_job:271:25)',
  ]) {
    assert.equal(parseErrorLine(line), null, `must not parse as a finding: ${line}`);
  }
});

test('chooseEngine takes the $PATH binary only at the pinned version', () => {
  assert.equal(chooseEngine(PINNED_CLI2_VERSION).kind, 'path');
});

test('chooseEngine refuses every version that is not the pin', () => {
  // 0.18.1 is the version the Justfile pinned while CI ran a newer one;
  // 0.22.1 is what a developer's global install actually was. Both predate
  // the pin and both must be refused, or #2997 comes back on the hook path.
  for (const skewed of ['0.18.1', '0.20.0', '0.22.1', '0.24.0', 'not-a-version']) {
    const engine = chooseEngine(skewed);
    assert.equal(engine.kind, 'pinned', `${skewed} must not be allowed to run`);
    // Substring, not a constructed RegExp: hand-escaping `.` while leaving `\`
    // unescaped is CodeQL's js/incomplete-sanitization, and the assertion never
    // needed pattern semantics — it only needs the pin's version to be named.
    assert.ok(
      engine.reason.includes(PINNED_CLI2_VERSION),
      `reason must name the pinned version ${PINNED_CLI2_VERSION}, got: ${engine.reason}`,
    );
  }
});

test('chooseEngine refuses an absent binary', () => {
  assert.equal(chooseEngine(null).kind, 'pinned');
});

test('GLOBS excludes every vendored upstream corpus', () => {
  for (const head of ['prometheus', 'tempo', 'loki']) {
    assert.ok(
      GLOBS.includes(`!compatibility/${head}/upstream/**`),
      `${head}'s vendored upstream Markdown must stay out of the lint set`,
    );
  }
  assert.ok(GLOBS.includes('**/*.md'));
});

// captureStdout — run `fn` with process.stdout.write collected.
function captureStdout(fn) {
  const written = [];
  const original = process.stdout.write;
  process.stdout.write = (chunk) => {
    written.push(String(chunk));
    return true;
  };
  try {
    fn();
  } finally {
    process.stdout.write = original;
  }
  return written.join('');
}

test('report prints the raw finding whether or not it annotates', () => {
  const plain = captureStdout(() => report(REAL_LINE, {}));
  assert.ok(plain.includes(REAL_LINE), 'the greppable line must survive');
  assert.ok(!plain.includes('::error'), 'no workflow command outside Actions');
});

test('report annotates the offending line under Actions', () => {
  const annotated = captureStdout(() => report(REAL_LINE, { GITHUB_ACTIONS: 'true' }));
  assert.ok(annotated.includes(REAL_LINE), 'the raw line is additive, not replaced');
  assert.match(annotated, /::error [^:]*file=docs\/test-strategy\.md/);
  assert.match(annotated, /line=169/);
  assert.match(annotated, /col=4812/);
});

test('report counts only findings', () => {
  const text = ['Linting: 2 files', REAL_LINE, '', 'Summary: 1 issue in 1 file'].join('\n');
  let count;
  captureStdout(() => {
    count = report(text, {});
  });
  assert.equal(count, 1);
});

// The anti-vacuity test. See the header: a version comparison cannot see a
// rule that is configured but not implemented, and that is the defect.
test('the pinned engine enforces MD060 as .markdownlint.yaml configures it', () => {
  const dir = mkdtempSync(join(tmpdir(), 'cerberus-md060-'));
  try {
    copyFileSync(join(REPO_ROOT, '.markdownlint.yaml'), join(dir, '.markdownlint.yaml'));

    const misaligned = ['# Probe', '', '| left | right |', '| --- | --- |', '| a | b |', ''].join('\n');
    writeFileSync(join(dir, 'probe.md'), misaligned);

    const run = () => {
      try {
        execFileSync(
          'npm',
          ['exec', '--yes', '--', `markdownlint-cli2@${PINNED_CLI2_VERSION}`, 'probe.md'],
          { cwd: dir, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] },
        );
        return '';
      } catch (err) {
        return String(err.stderr ?? '');
      }
    };

    const stderr = run();
    const findings = stderr.split('\n').map(parseErrorLine).filter(Boolean);
    assert.ok(
      findings.some((f) => f.rule.startsWith('MD060')),
      'the pinned engine must implement MD060 — a pin that does not makes '
      + '.markdownlint.yaml\'s MD060 key configure nothing, which is #2997. '
      + `Engine reported:\n${stderr}`,
    );

    const aligned = ['# Probe', '', '| left | right |', '| ---- | ----- |', '| a    | b     |', ''].join('\n');
    writeFileSync(join(dir, 'probe.md'), aligned);
    assert.equal(run().trim(), '', 'an aligned table must satisfy MD060');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
