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
// Env:
//   PR_BODY   the pull request body (pass via env, NEVER interpolated into a
//             shell `run:` line — bodies are attacker-controlled).
//
// argv `--self-test` runs the in-process assertion suite and exits.

import { error, notice } from './lib/gh.mjs';

export const MIN_CHARS = 20;

// Lone-token placeholders that clear length checks only by accident. Matched
// against the whole stripped body (case-insensitive), not substrings.
const PLACEHOLDER = /^(cat|dog|todo|to-?do|wip|test|tests?|placeholder|tbd|t\.?b\.?d\.?|n\/?a|none|asdf|\.+|x+|foo|bar|stub|wip+|change|changes|update|updates|fix|fixes)$/i;

// meaningfulBody strips the parts of a body that are boilerplate / not a
// description: the Claude footer, Co-authored-by trailers, HTML comments, and
// markdown image-only lines, then collapses whitespace.
export function meaningfulBody(raw) {
  return String(raw ?? '')
    .replace(/<!--[\s\S]*?-->/g, '') // HTML / template comments
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
  stub('Fixes it.', 'too short (< 20 meaningful chars)');
  ok('Fixes the Loki tail overflow by advancing the cursor past the last sent row.', 'real one-liner');
  ok('Adds a guardrail.\n\nDetails: rejects empty PR bodies.\n\n🤖 Generated with [Claude Code](x)', 'real body + footer');
  // footer/trailers must not rescue an otherwise-empty body
  stub('Co-authored-by: Someone <x@y.z>\n🤖 Generated with [Claude Code](x)', 'only trailers + footer');

  notice('pr-body-check --self-test: all assertions passed');
}

if (process.argv.includes('--self-test')) {
  selfTest();
  process.exit(0);
}

const { stub, reason } = classify(process.env.PR_BODY);
if (stub) {
  error(
    `pr-body-check: this PR has no real description — ${reason}. ` +
      `Write a description of WHAT changed and WHY (the gate strips the AI footer, ` +
      `Co-authored-by trailers, and comments before measuring), then the check re-runs on edit.`,
  );
  process.exit(1);
}
notice('pr-body-check: PR description is non-empty and substantive.');
process.exit(0);
