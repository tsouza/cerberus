// pr-body-check.mjs — fail a pull request whose description is empty or a
// stub. A real "gh pr create … --body 'cat'" once shipped a PR whose entire
// body was the word `cat` (a malformed heredoc dropped the real text); this
// gate makes that un-mergeable: the agent or human must write an actual
// description before the PR can go green.
//
// The check strips boilerplate that carries no description (the AI-generated
// footer, Co-authored-by trailers, HTML comments / template comments) and then
// requires what remains to be a genuine description: at least MIN_CHARS of
// meaningful text and not a lone placeholder token.
//
// `pr-body` is a required status check, so it has to report on every surface
// branch protection asks it about — a pull request, the merge queue's projected
// trunk, and a push to `main` or a maintenance line. Only the first carries a
// body in the event payload, so the other two resolve the description from the
// pull request associated with the head commit, sharing forbid-deferral's
// resolver rather than growing a second one that could drift from it.
//
// A commit with NO associated pull request passes. That is not a tolerance
// list: the gate's subject is a pull request description, and a commit that has
// none has no description to be a stub. Every path that can introduce one — the
// `pull_request` event, and a squash-merge whose commit resolves back to its
// PR — is inspected.
//
// Env:
//   GITHUB_EVENT_NAME  REQUIRED. `pull_request` reads PR_BODY; anything else
//                      resolves the description from the head commit.
//   PR_BODY            REQUIRED on a pull_request run — the description (pass
//                      via env, NEVER interpolated into a shell `run:` line;
//                      bodies are attacker-controlled).
//   GITHUB_SHA         the head commit, used off the pull_request path.
//   GITHUB_REPOSITORY  REQUIRED off the pull_request path. `owner/repo`.
//   GITHUB_TOKEN       REQUIRED off the pull_request path. Needs
//                      `pull-requests: read`.
//   GITHUB_API_URL     API base (runner-provided; default https://api.github.com).
//
// argv `--self-test` runs the in-process assertion suite and exits.

import process from 'node:process';
import { pathToFileURL } from 'node:url';
import { descriptionSurface } from './forbid-deferral.mjs';
import { error, notice } from './lib/gh.mjs';

export const MIN_CHARS = 20;

// Lone-token placeholders that clear length checks only by accident. Matched
// against the whole stripped body (case-insensitive), not substrings.
const PLACEHOLDER = /^(cat|dog|todo|to-?do|wip|test|tests?|placeholder|tbd|t\.?b\.?d\.?|n\/?a|none|asdf|\.+|x+|foo|bar|stub|wip+|change|changes|update|updates|fix|fixes)$/i;

// stripHtmlComments removes every `<!-- ... -->` HTML/template comment with a
// linear `indexOf` scan, repeated to a FIXPOINT. The fixpoint matters for
// security: a single pass can re-expose a `<!--` on crafted nested markers
// (e.g. `<!<!-- -->-- x -->`), which CodeQL flags as
// js/incomplete-multi-character-sanitization. Looping until the string stops
// changing guarantees the output cannot contain `<!--` followed by a `-->`.
//
// This uses indexOf rather than a regex on purpose: the lazy regex
// `/<!--[\s\S]*?-->/` is O(n^2) on an unterminated run of `<!--` (it rescans to
// end-of-string from every open marker), which is a ReDoS on the
// attacker-controlled PR body. The indexOf scan is strictly linear, and the
// fixpoint loop stays linear overall because every pass that changes the string
// removes at least one full comment, so the string strictly shrinks.
function stripHtmlComments(input) {
  let s = String(input ?? '');
  for (let prev; prev !== s; ) {
    prev = s;
    let out = '';
    let i = 0;
    for (;;) {
      const open = s.indexOf('<!--', i);
      if (open === -1) {
        out += s.slice(i);
        break;
      }
      const close = s.indexOf('-->', open + 4);
      if (close === -1) {
        // Unterminated comment: leave the remainder untouched (it carries no
        // closing marker, so it cannot reintroduce a complete comment).
        out += s.slice(i);
        break;
      }
      out += s.slice(i, open);
      i = close + 3;
    }
    s = out;
  }
  return s;
}

// meaningfulBody strips the parts of a body that are boilerplate / not a
// description: the Claude footer, Co-authored-by trailers, HTML comments, and
// markdown image-only lines, then collapses whitespace.
export function meaningfulBody(raw) {
  return stripHtmlComments(raw)
    .replace(/^.*🤖.*$/gm, '') // the AI-generated footer line
    .replace(/^\s*Generated with \[?Claude Code.*$/gim, '')
    .replace(/^\s*Co-authored-by:.*$/gim, '')
    .replace(/^\s*!\[[^\]]*\]\([^)]*\)\s*$/gm, '') // image-only lines
    .replace(/\s+/g, ' ')
    .trim();
}

// classify returns { stub: bool, reason?: string } for a raw body.
export function classify(raw) {
  const body = meaningfulBody(raw);
  if (body.length === 0) {
    return { stub: true, reason: 'the description is empty (after stripping the AI footer / comments / trailers)' };
  }
  if (PLACEHOLDER.test(body)) {
    return { stub: true, reason: `the description is a placeholder token: "${body}"` };
  }
  if (body.length < MIN_CHARS) {
    return { stub: true, reason: `the description has only ${body.length} meaningful characters (minimum ${MIN_CHARS})` };
  }
  return { stub: false };
}

function selfTest() {
  const assert = (c, m) => {
    if (!c) throw new Error('self-test: ' + m);
  };
  const stub = (b, why) => assert(classify(b).stub, why);
  const ok = (b, why) => assert(!classify(b).stub, why);

  stub('', 'empty body');
  stub('   \n  ', 'whitespace-only');
  stub('cat', 'the literal #1012 case');
  stub('TODO', 'placeholder TODO');
  stub('wip', 'placeholder wip');
  stub('update', 'one-word filler');
  stub('🤖 Generated with [Claude Code](https://claude.com/claude-code)', 'footer-only');
  stub('<!-- describe your change -->', 'comment-only template');
  stub('<!<!-- -->-- evil -->', 'nested markers must not re-expose a comment'); // CodeQL: incomplete-multi-character-sanitization
  assert(!meaningfulBody('<!<!-- -->-- evil -->').includes('<!--'), 'stripped body must not contain a residual <!-- marker');
  stub('Fixes it.', 'too short (< 20 meaningful chars)');
  ok('Fixes the Loki tail overflow by advancing the cursor past the last sent row.', 'real one-liner');
  ok('Adds a guardrail.\n\nDetails: rejects empty PR bodies.\n\n🤖 Generated with [Claude Code](x)', 'real body + footer');
  // footer/trailers must not rescue an otherwise-empty body
  stub('Co-authored-by: Someone <x@y.z>\n🤖 Generated with [Claude Code](x)', 'only trailers + footer');

  notice('pr-body-check --self-test: all assertions passed');
}

// unassociatedOrigin is how descriptionSurface labels a head commit that no
// pull request points at. That case is a pass here, so the label is matched
// rather than the text being classified — an empty body from an EXISTING pull
// request is still a stub.
const UNASSOCIATED_ORIGIN = 'explicitly empty: no pull request';

// inspect turns a resolved description surface into a verdict. Split out from
// main so the unassociated-commit exemption is testable on its own: it is the
// one path that passes without reading the text, and a bug there would make the
// gate green on every pull request rather than on the commits it is meant to
// exempt.
export function inspect(description) {
  if (description.origin.startsWith(UNASSOCIATED_ORIGIN)) {
    return { stub: false, unassociated: true };
  }
  return classify(description.text);
}

function requireEnv(name, why) {
  const v = process.env[name];
  if (!v) throw new Error(`${name} is unset — ${why}`);
  return v;
}

async function main() {
  const eventName = requireEnv('GITHUB_EVENT_NAME', 'the description surface is resolved per event');
  const onPullRequest = eventName === 'pull_request' || eventName === 'pull_request_target';

  const description = await descriptionSurface({
    eventName,
    repo: onPullRequest ? '' : requireEnv('GITHUB_REPOSITORY', 'the head commit cannot be resolved without it'),
    head: onPullRequest ? '' : requireEnv('GITHUB_SHA', 'there is no head commit to resolve a pull request from'),
    token: onPullRequest ? '' : requireEnv('GITHUB_TOKEN', 'resolving the associated pull request needs pull-requests:read'),
    apiBase: process.env.GITHUB_API_URL || 'https://api.github.com',
  });

  const { stub, reason, unassociated } = inspect(description);
  if (unassociated) {
    notice(`pr-body-check: ${description.origin} — no pull request description to inspect.`);
    process.exit(0);
  }
  if (stub) {
    error(
      `pr-body-check: this PR has no real description — ${reason} (read from ${description.origin}). ` +
        `Write a description of WHAT changed and WHY (the gate strips the AI footer, ` +
        `Co-authored-by trailers, and comments before measuring), then the check re-runs on edit.`,
    );
    process.exit(1);
  }
  notice(`pr-body-check: PR description is non-empty and substantive (read from ${description.origin}).`);
  process.exit(0);
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((e) => {
    error(`pr-body-check: ${e.message}`);
    process.exit(1);
  });
}
