// pr-type-label.mjs — the single source of truth for the PR-title ->
// Conventional-Commit type-label mapping shared by `pr-label.yml`'s two
// jobs:
//
//   1. `label`    — the event-driven path. On `pull_request_target`
//                   (opened / edited / reopened) it labels the one PR in
//                   the payload.
//   2. `backfill` — the self-healing path. On a cron schedule (a few
//                   times a day) + manual dispatch it walks every OPEN PR
//                   and applies any MISSING expected type label. This
//                   catches PRs whose event-driven run was queued, failed,
//                   or never fired (the #1049 / #1050 case, where the
//                   pr-label runs sat in the Actions queue and the PRs were
//                   left unlabeled with no self-heal until a future edit).
//
// Keeping the mapping in ONE pure function means the two paths can never
// drift. The github-script steps in the workflow `require()` this module
// and call `labelsForTitle()`; there is no second copy of the table.
//
// Mapping (Conventional-Commit type -> label):
//   feat -> enhancement   fix -> bug          docs -> documentation
//   ci -> ci              test -> test        refactor -> refactor
//   perf -> performance   chore -> chore      build -> build
//   revert -> revert      style -> (none; cosmetic, no label)
// Scope overrides (checked before the bare-type table):
//   *(deps)        -> dependencies      (e.g. chore(deps), build(deps))
//   chore(release) -> release (+ chore)
//
// Dependency-light by design: imports only node: builtins (in fact none),
// so a github-script step can `require()` it with no install step.
//
// argv `--self-test` runs the in-process assertion suite and exits.

import process from 'node:process';

// The bare type -> label table. `style` is intentionally absent: a purely
// cosmetic change carries no tracking label.
export const TYPE_TO_LABEL = Object.freeze({
  feat: 'enhancement',
  fix: 'bug',
  docs: 'documentation',
  ci: 'ci',
  test: 'test',
  refactor: 'refactor',
  perf: 'performance',
  chore: 'chore',
  build: 'build',
  revert: 'revert',
});

// Conventional-Commit header: `type(scope)!: subject`. The scope and the
// `!` breaking-change marker are optional. Case-insensitive on the type so
// a stray capital doesn't silently drop the label.
const HEADER = /^([a-z]+)(?:\(([^)]*)\))?!?:/i;

// labelsForTitle returns the array of labels a PR with this title should
// carry. An empty array means "no type label applies" (no CC prefix, a
// `style:` change, or an unknown type). The scope overrides are checked
// before the bare-type table so `chore(deps)` -> dependencies (not chore)
// and `chore(release)` -> release+chore (not just chore).
export function labelsForTitle(title) {
  const m = String(title ?? '').match(HEADER);
  if (!m) return [];
  const type = m[1].toLowerCase();
  const scope = (m[2] || '').toLowerCase();

  if (scope === 'deps') return ['dependencies'];
  if (type === 'chore' && scope === 'release') return ['release', 'chore'];
  const label = TYPE_TO_LABEL[type];
  return label ? [label] : [];
}

function selfTest() {
  const assert = (cond, msg) => {
    if (!cond) throw new Error('self-test: ' + msg);
  };
  const eq = (title, want, why) => {
    const got = labelsForTitle(title);
    assert(
      got.length === want.length && got.every((v, i) => v === want[i]),
      `${why}: labelsForTitle(${JSON.stringify(title)}) = [${got}] want [${want}]`,
    );
  };

  // Bare-type mapping.
  eq('feat: add holt-winters', ['enhancement'], 'feat -> enhancement');
  eq('fix: cursor overflow', ['bug'], 'fix -> bug');
  eq('docs: tidy README', ['documentation'], 'docs -> documentation');
  eq('ci: pin actionlint', ['ci'], 'ci -> ci');
  eq('test: add fixture', ['test'], 'test -> test');
  eq('refactor: extract emitter', ['refactor'], 'refactor -> refactor');
  eq('perf: prune scan', ['performance'], 'perf -> performance');
  eq('chore: tidy', ['chore'], 'chore -> chore');
  eq('build: bump goreleaser', ['build'], 'build -> build');
  eq('revert: undo #123', ['revert'], 'revert -> revert');

  // style is cosmetic-only: no label.
  eq('style: gofmt', [], 'style -> (none)');

  // Scope overrides take precedence over the bare type.
  eq('chore(deps): bump x from 1 to 2', ['dependencies'], 'chore(deps) -> dependencies');
  eq('build(deps): bump action', ['dependencies'], 'build(deps) -> dependencies');
  eq('fix(deps): pin transitive', ['dependencies'], 'any *(deps) -> dependencies');
  eq('chore(release): v1.2.3', ['release', 'chore'], 'chore(release) -> release+chore');

  // A non-release chore scope falls through to the bare type.
  eq('chore(ci): tidy', ['chore'], 'chore(<other>) -> chore');
  // A release scope on a non-chore type is NOT the release override.
  eq('feat(release): gate', ['enhancement'], 'feat(release) -> enhancement (not release)');

  // Breaking-change marker and scopes must not break parsing.
  eq('feat!: breaking', ['enhancement'], 'feat! -> enhancement');
  eq('feat(api)!: breaking scoped', ['enhancement'], 'feat(scope)! -> enhancement');
  eq('FIX: shouty', ['bug'], 'case-insensitive type');

  // No-match cases.
  eq('', [], 'empty title');
  eq('no conventional prefix here', [], 'no CC prefix');
  eq('wibble: unknown type', [], 'unknown type -> (none)');
  eq('Merge branch main', [], 'merge subject -> (none)');
  eq(null, [], 'null title');

  process.stdout.write('::notice::pr-type-label --self-test: all assertions passed\n');
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}
