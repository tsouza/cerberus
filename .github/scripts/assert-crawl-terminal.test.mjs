// assert-crawl-terminal.test.mjs — node:test guard for the crawl terminality
// gate.
//
// The gate exists because a de-gated lane with no reader went silent for three
// nights (#1861). A gate that itself degrades to "always ok" would reproduce
// exactly that, so these tests pin BOTH directions: the benign cases pass, and
// every not-a-verdict case actually fails.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { classifyCrawlOutcome } from './assert-crawl-terminal.mjs';

const ran = (crawlResult, depth = 'full') =>
  classifyCrawlOutcome({
    scopeResult: 'success',
    setupResult: 'success',
    hasInformational: 'true',
    crawlResult,
    depth,
  });

test('a crawl that reaches a verdict passes — success and failure are both terminal', () => {
  assert.equal(ran('success').ok, true);
  // A red crawl is loud on its own child check; re-gating it here would undo
  // the deliberate de-gate that keeps a coverage flake from blocking merges.
  assert.equal(ran('failure').ok, true);
});

test('a CANCELLED crawl fails, and says the pin was enforced by nothing', () => {
  const full = ran('cancelled', 'full');
  assert.equal(full.ok, false);
  assert.match(full.message, /CANCELLED/);
  assert.match(full.message, /FULL-depth surface pin/);
  // The message must point at the cap-vs-budget ordering, the actual cause.
  assert.match(full.message, /crawl-budget\.mjs/);

  const lean = ran('cancelled', 'lean');
  assert.equal(lean.ok, false);
  assert.match(lean.message, /lean surface pin/);
});

test('a crawl matrix that never fanned out fails rather than reading as coverage', () => {
  assert.equal(ran('skipped').ok, false);
  assert.equal(ran('').ok, false);
  assert.equal(ran('neutral').ok, false);
});

test('a planner that stopped emitting a crawl matrix at all fails', () => {
  const verdict = classifyCrawlOutcome({
    scopeResult: 'success',
    setupResult: 'success',
    hasInformational: 'false',
    crawlResult: 'skipped',
    depth: 'full',
  });
  assert.equal(verdict.ok, false);
  assert.match(verdict.message, /silently stopped existing/);
});

test('an out-of-scope change short-circuits cleanly — but only on a scope job that decided', () => {
  const outOfScope = classifyCrawlOutcome({
    scopeResult: 'success',
    setupResult: 'skipped',
    hasInformational: '',
    crawlResult: 'skipped',
    depth: 'lean',
  });
  assert.equal(outOfScope.ok, true);

  // A crashed scope job also skips setup. That is a lane that failed to
  // decide, and its silence must not be read as "nothing to do".
  const undecided = classifyCrawlOutcome({
    scopeResult: 'failure',
    setupResult: 'skipped',
    hasInformational: '',
    crawlResult: 'skipped',
    depth: 'lean',
  });
  assert.equal(undecided.ok, false);
  assert.match(undecided.message, /never decided/);
});

test('a lane whose setup did not succeed fails', () => {
  for (const setupResult of ['failure', 'cancelled', '']) {
    const verdict = classifyCrawlOutcome({
      scopeResult: 'success',
      setupResult,
      hasInformational: 'true',
      crawlResult: 'skipped',
      depth: 'full',
    });
    assert.equal(verdict.ok, false, `setup=${setupResult} must not read as coverage`);
  }
});
