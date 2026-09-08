#!/usr/bin/env node
// commitlint-pr-title.mjs — lint the subject a squash merge will write onto
// `main`, using the repository's own commitlint config.
//
// WHY THIS EXISTS (issue #3187): commitlint runs over the PR's COMMIT range
// (commitlint-range.mjs). Every commit in that range is linted — and then the
// squash merge throws all of those messages away and writes ONE subject onto
// `main`. This repository's merge setting is `squash_merge_commit_title:
// COMMIT_OR_PR_TITLE`, so that subject is the PR TITLE whenever the PR has more
// than one real commit. Nothing in CI had ever read the title.
//
// The result is on `main` already: subjects with a type outside `type-enum`
// ("investigate(chclient): …"), subjects with no Conventional prefix at all,
// and 119 subjects over the 100-character `header-max-length` — every one of
// them through a green `lint` check that had inspected the commit messages the
// squash discarded.
//
// The title is not ALWAYS what lands: a PR with a single real commit squashes
// under that commit's own subject, which commitlint-range.mjs already covers.
// This gate lints the title regardless rather than trying to predict GitHub's
// choice. Predicting it would be both fragile and wrong-way-round — a PR that
// grows a second commit after the gate ran would silently change which text was
// judged, and "the subject that CAN land is Conventional" is the property worth
// holding.
//
// THE SUFFIX IS PART OF THE SUBJECT. GitHub appends ` (#<number>)` to the
// squash subject, so a title at exactly the 100-character limit lands at 108
// and breaks the very rule that passed it. This gate therefore lints the
// COMPOSED landing subject, not the raw title — which is the only reading of
// `header-max-length` that describes what ends up on `main`.
//
// The subject that lands also decides what users SEE: prepare-release.mjs maps
// only some Conventional types into a changelog section, so a mistyped subject
// does not merely read badly, it can drop a user-visible change out of the
// release notes entirely.
//
// Env contract (only read when invoked as the program):
//   PR_TITLE           REQUIRED. The pull request title. Passed via env and
//                      never interpolated into a shell line — titles are
//                      attacker-controlled text.
//   PR_NUMBER          REQUIRED. The pull request number, used to compose the
//                      ` (#N)` suffix GitHub will append.
//   COMMITLINT_CONFIG  Path to the commitlint config. Default: .commitlintrc.json
//   GIT_CWD            Working directory for the commitlint invocation.
//
// Exit codes: 0 = the landing subject passed commitlint; 1 = it failed, or
// PR_TITLE / PR_NUMBER was absent. An absent input is a FAILURE and never a
// silent pass: this gate runs only on `pull_request`, where both always exist,
// so "nothing to lint" means the wiring broke, not that the title is fine.
//
// The orchestration (`run`) and the composition (`landingSubject`) are exported
// and pinned by the `node --test` guard commitlint-pr-title.test.mjs; the lint
// itself runs only when this file is invoked as the program.

import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { defaultLintOne } from './commitlint-range.mjs';
import { error, log, notice } from './lib/gh.mjs';

// landingSubject — the subject GitHub's squash merge will actually write.
//
// GitHub appends ` (#<number>)` unless the title already ends with that exact
// reference, which is why this is a composition rather than a plain length
// budget: linting `title` alone measures a string that never reaches `main`,
// and subtracting a fixed allowance would mis-measure both the two-digit and
// the five-digit case.
export function landingSubject(title, number) {
  const suffix = ` (#${number})`;
  return title.endsWith(suffix) ? title : `${title}${suffix}`;
}

// run — lint one PR title. `lintOne` defaults to the same real-commitlint
// invocation commitlint-range.mjs uses for a commit message, so the title is
// judged by the SAME config and the SAME CLI that judges every commit. A
// second, parallel implementation of "is this Conventional?" is exactly the
// drift this gate exists to close.
export function run({ title, number, cwd, config = '.commitlintrc.json', lintOne = defaultLintOne }) {
  if (title === undefined || title === null || title.trim() === '') {
    error(
      'commitlint-pr-title: PR_TITLE is empty — the gate examined nothing. This step runs only ' +
        'on pull_request, where a title always exists, so an empty PR_TITLE means the workflow ' +
        'wiring is broken, not that the title is clean.',
    );
    return 1;
  }
  if (!/^[0-9]+$/.test(String(number ?? '').trim())) {
    error(
      `commitlint-pr-title: PR_NUMBER is not a number (${JSON.stringify(number)}). The landing ` +
        'subject cannot be composed without it, and linting the bare title instead would measure ' +
        'a string that never reaches main.',
    );
    return 1;
  }

  const subject = landingSubject(title, String(number).trim());

  // commitlint reads a whole message; a squash subject is a header with no
  // body. Passing it verbatim is what makes header-max-length and type-enum
  // apply to it exactly as they apply to a commit subject.
  const result = lintOne(subject, { cwd, config });
  log(`⧗   squash subject   ${subject}`);
  if (result.stdout) log(result.stdout.trimEnd());
  if (result.stderr) log(result.stderr.trimEnd());

  if (result.status !== 0) {
    error(
      'commitlint-pr-title: the subject this PR would squash onto main is not a valid ' +
        `Conventional Commit header. Fix the PR TITLE — it is linted as "${subject}", including ` +
        'the ` (#N)` suffix GitHub appends, because that is the text that lands. A green commit ' +
        'range does not cover it.',
    );
    return 1;
  }

  notice(`commitlint-pr-title: landing subject linted clean (${subject.length} chars).`);
  return 0;
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
  const status = run({
    title: process.env.PR_TITLE,
    number: process.env.PR_NUMBER,
    cwd: process.env.GIT_CWD,
    config: process.env.COMMITLINT_CONFIG ?? '.commitlintrc.json',
  });
  process.exit(status);
}
