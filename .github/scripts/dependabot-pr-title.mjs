#!/usr/bin/env node
// dependabot-pr-title.mjs — shorten a Dependabot PR title whose squash subject
// would break commitlint's `header-max-length`.
//
// WHY THIS EXISTS (issue #3647): Dependabot composes a title from the
// dependency, both versions, the directory and the group name, and nothing in
// `.github/dependabot.yml` bounds its length. `pr-hygiene.yml`'s `pr-body` job
// lints the subject a squash merge writes onto `main` — the title plus the
// ` (#N)` suffix, composed by commitlint-pr-title.mjs's `landingSubject` — so a
// title such as
//   chore(deps-dev): bump @types/node from 26.5.1 to 26.6.1 in /test/e2e/playwright in the playwright-deps group
// lands at 116 characters and holds a required check red until a human edits
// it. The gate is right; the title is the thing to repair.
//
// THE REWRITE. A title whose landing subject already fits is left alone. An
// over-long one has its optional qualifiers removed one at a time, in a fixed
// order, stopping at the first form whose landing subject fits:
//   1. ` across N directories`
//   2. ` in the <group> group`
//   3. ` in /<directory>`
//   4. ` from <old-version>`  (the target version stays)
//   5. leading `/`-segments of the dependency name, replaced by `...` and
//      removed one at a time, so the longest tail that fits survives
// The Conventional prefix, the dependency and the target version are never
// removed, so the result is a pure function of (title, number, limit). A title
// no step can bring under the limit is reported as a failure and left as is:
// inventing a different subject would hide the dependency the PR bumps.
//
// THE LIMIT is read from the repository's commitlint config, the same file the
// `pr-body` gate lints with, so the two cannot drift apart.
//
// THE TOKEN. An edit made with the workflow's GITHUB_TOKEN does not start new
// workflow runs (GitHub's recursion guard), so the `edited` event that would
// re-run `pr-hygiene` never fires and `pr-body` stays red on the old title. The
// edit is therefore made with TITLE_EDIT_TOKEN (the repository's RELEASE_PAT),
// whose `edited` event re-runs `pr-hygiene` like a human edit does. The live
// title is read with GITHUB_TOKEN.
//
// Env contract (only read when invoked as the program):
//   PR_NUMBER          REQUIRED. The pull request number.
//   PR_AUTHOR          REQUIRED. The pull request author's login. Anything
//                      other than `dependabot[bot]` is skipped.
//   GITHUB_REPOSITORY  REQUIRED. owner/name.
//   GITHUB_TOKEN       REQUIRED. Reads the live pull request.
//   TITLE_EDIT_TOKEN   Required only when a rewrite is needed. Edits the title.
//   GITHUB_API_URL     Default https://api.github.com.
//   COMMITLINT_CONFIG  Default .commitlintrc.json.
//
// Exit codes: 0 = skipped, already compliant, or rewritten; 1 = the title
// cannot be made compliant, TITLE_EDIT_TOKEN is missing when a rewrite is
// needed, an input is missing, or the API call failed.

import { readFileSync } from 'node:fs';
import process from 'node:process';
import { pathToFileURL } from 'node:url';

import { landingSubject } from './commitlint-pr-title.mjs';
import { error, notice } from './lib/gh.mjs';
import { DEFAULT_API_BASE, NOT_FOUND_THROW, ghJSON } from './lib/gh-api.mjs';
import { fetchPullRequest } from './lib/pull-request.mjs';

export const DEPENDABOT_LOGIN = 'dependabot[bot]';

// The marker that stands in for the leading dependency-path segments step 5
// removes.
export const ELIDED_PATH_PREFIX = '...';

// Index of the value in a commitlint rule tuple: [level, applicability, value].
const RULE_VALUE_INDEX = 2;

// headerMaxLength — the `header-max-length` value from a parsed commitlint
// config. A config without the rule is an error rather than a default: the
// gate this script serves would then be enforcing a limit this script cannot
// see.
export function headerMaxLength(config) {
  const rule = config?.rules?.['header-max-length'];
  const value = Array.isArray(rule) ? rule[RULE_VALUE_INDEX] : undefined;
  if (!Number.isInteger(value) || value <= 0) {
    throw new Error(`commitlint config has no usable header-max-length rule (${JSON.stringify(rule)})`);
  }
  return value;
}

// The single-dependency core: `<type>(<scope>): bump <dependency> [from <old> ]to <new>`.
const SINGLE_DEP = /^(\S+: [Bb]ump )(\S+)( (?:from \S+ )?to \S+)(.*)$/;

// Qualifier removals, in the order they are applied. Each only deletes text;
// none touches the prefix, the dependency or the target version.
export const QUALIFIER_STEPS = Object.freeze([
  { name: 'directory count', apply: (t) => t.replace(/ across \d+ director(?:y|ies)\b/, '') },
  { name: 'group', apply: (t) => t.replace(/ in the \S+ group\b/, '') },
  { name: 'directory', apply: (t) => t.replace(/ in \/\S*/, '') },
  { name: 'source version', apply: (t) => t.replace(/^(\S+: [Bb]ump \S+) from \S+ (to \S+)/, '$1 $2') },
]);

// elisions — step 5: the single-dependency title with ever fewer leading
// segments of the dependency path, longest tail first. Empty for a title that
// is not the single-dependency shape or whose dependency has one segment.
function elisions(title) {
  const m = SINGLE_DEP.exec(title);
  if (!m) return [];
  const [, head, dependency, versions, rest] = m;
  const segments = dependency.split('/');
  const out = [];
  for (let drop = 1; drop < segments.length; drop++) {
    out.push(`${head}${ELIDED_PATH_PREFIX}/${segments.slice(drop).join('/')}${versions}${rest}`);
  }
  return out;
}

// shortenTitle — the first candidate, in rewrite order, whose landing subject
// fits within maxLength; null when none does.
export function shortenTitle(title, { number, maxLength }) {
  const fits = (t) => landingSubject(t, number).length <= maxLength;
  let current = title;
  if (fits(current)) return current;
  for (const step of QUALIFIER_STEPS) {
    current = step.apply(current);
    if (fits(current)) return current;
  }
  return elisions(current).find(fits) ?? null;
}

// planTitle — what to do with one PR's title.
//   skip       not a Dependabot PR; never touched.
//   keep       the landing subject already fits.
//   rewrite    `title` is the compliant replacement.
//   unfixable  no rewrite fits; the title is left alone.
export function planTitle({ title, number, author, maxLength }) {
  if (author !== DEPENDABOT_LOGIN) return { action: 'skip' };
  const subject = landingSubject(title, number);
  if (subject.length <= maxLength) return { action: 'keep' };
  const shortened = shortenTitle(title, { number, maxLength });
  if (shortened === null) return { action: 'unfixable', length: subject.length };
  return { action: 'rewrite', title: shortened };
}

// run — read the live title, plan, and apply the plan through the API.
export async function run({
  number,
  author,
  repository,
  apiUrl = DEFAULT_API_BASE,
  readToken,
  editToken,
  maxLength,
  fetchImpl = globalThis.fetch,
}) {
  number = String(number ?? '').trim();
  if (!/^[0-9]+$/.test(number) || !author || !repository || !readToken) {
    error('dependabot-pr-title: PR_NUMBER, PR_AUTHOR, GITHUB_REPOSITORY and GITHUB_TOKEN are all required.');
    return 1;
  }
  if (author !== DEPENDABOT_LOGIN) {
    notice(`dependabot-pr-title: PR #${number} is authored by ${author}, not ${DEPENDABOT_LOGIN}; skipped.`);
    return 0;
  }

  const { title } = await fetchPullRequest({ apiUrl, repository, number, token: readToken, fetchImpl });
  const plan = planTitle({ title, number, author, maxLength });

  if (plan.action === 'keep') {
    notice(`dependabot-pr-title: landing subject of PR #${number} fits ${maxLength} characters; unchanged.`);
    return 0;
  }
  if (plan.action === 'unfixable') {
    error(
      `dependabot-pr-title: the landing subject of PR #${number} is ${plan.length} characters and no ` +
        `rewrite that keeps the dependency and target version fits ${maxLength}. Title: "${title}"`,
    );
    return 1;
  }
  if (!editToken) {
    error(
      'dependabot-pr-title: TITLE_EDIT_TOKEN (RELEASE_PAT) is empty. An edit made with GITHUB_TOKEN would ' +
        'not re-run pr-hygiene, leaving pr-body red on the old title.',
    );
    return 1;
  }

  const updated = await ghJSON(`${apiUrl}/repos/${repository}/pulls/${number}`, {
    token: editToken,
    what: `edit title of PR #${number}`,
    notFound: NOT_FOUND_THROW,
    init: { method: 'PATCH', body: JSON.stringify({ title: plan.title }) },
    fetchImpl,
  });
  if (updated?.title !== plan.title) {
    error(`dependabot-pr-title: PR #${number} title after the edit is "${updated?.title}", not "${plan.title}".`);
    return 1;
  }
  notice(`dependabot-pr-title: PR #${number} retitled "${title}" -> "${plan.title}".`);
  return 0;
}

const isMain = process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href;
if (isMain) {
  try {
    const config = JSON.parse(readFileSync(process.env.COMMITLINT_CONFIG || '.commitlintrc.json', 'utf8'));
    const status = await run({
      number: process.env.PR_NUMBER,
      author: process.env.PR_AUTHOR,
      repository: process.env.GITHUB_REPOSITORY,
      apiUrl: process.env.GITHUB_API_URL || DEFAULT_API_BASE,
      readToken: process.env.GITHUB_TOKEN,
      editToken: process.env.TITLE_EDIT_TOKEN,
      maxLength: headerMaxLength(config),
    });
    process.exit(status);
  } catch (err) {
    error(`dependabot-pr-title: ${err.message}`);
    process.exit(1);
  }
}
