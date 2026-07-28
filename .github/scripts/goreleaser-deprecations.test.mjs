// goreleaser-deprecations.test.mjs — node:test guard for the detector that
// decides whether a `goreleaser check` transcript contains a deprecation.
//
// The gate is only as good as this predicate: a detector that stops matching
// reports "no deprecations" on a config full of them, which reads identically to
// a healthy repo. The cases below are transcripts goreleaser actually produced
// against this project's config.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import { deprecationsIn } from './goreleaser-deprecations.mjs';

// A clean run: the shape the gate must let through.
const HEALTHY = [
  '  • checking                                  path=.goreleaser.yml',
  '  • 1 configuration file(s) validated',
  '  • thanks for using GoReleaser!',
].join('\n');

test('a clean transcript yields nothing', () => {
  assert.deepEqual(deprecationsIn(HEALTHY), []);
  assert.deepEqual(deprecationsIn(''), []);
  assert.deepEqual(deprecationsIn(undefined), []);
});

test('the three wordings goreleaser uses are all caught', () => {
  // Linked deprecations page — the marker every notice carries.
  const phasedOut =
    '  • brews is being phased out in favor of homebrew_casks, check https://goreleaser.com/deprecations#brews for more info';
  // The `DEPRECATED:` prefix form.
  const flagged =
    '  • DEPRECATED: brews should not be used anymore, check https://goreleaser.com/deprecations#brews for more info';
  // Wording that carries neither the URL nor the prefix, only "phased out".
  const bare = '  • dockers and docker_manifests are being phased out';

  for (const line of [phasedOut, flagged, bare]) {
    assert.equal(deprecationsIn(line).length, 1, `not detected: ${line}`);
  }
});

test('the bullet prefix is stripped so the message reads as a sentence', () => {
  const [only] = deprecationsIn(
    '  • brews is being phased out in favor of homebrew_casks, check https://goreleaser.com/deprecations#brews for more info',
  );
  assert.ok(only.startsWith('brews is being phased out'), `got: ${only}`);
});

test('a notice repeated per config entry is reported once', () => {
  // goreleaser emits one line per offending block; two `dockers` entries produce
  // the identical line twice. Counting them separately would misreport how many
  // distinct migrations are outstanding.
  const line =
    '  • dockers and docker_manifests are being phased out and will eventually be replaced by dockers_v2, check https://goreleaser.com/deprecations#dockers for more info';
  assert.equal(deprecationsIn([line, line].join('\n')).length, 1);
});

test('distinct deprecations are reported separately', () => {
  const transcript = [
    '  • checking                                  path=.goreleaser.yml',
    '  • dockers and docker_manifests are being phased out, check https://goreleaser.com/deprecations#dockers for more info',
    '  • DEPRECATED: brews should not be used anymore, check https://goreleaser.com/deprecations#brews for more info',
    '  • 1 configuration file(s) validated',
  ].join('\n');
  assert.equal(deprecationsIn(transcript).length, 2);
});

test('a link to the deprecations page in ordinary prose is not a false negative source', () => {
  // The detector deliberately errs toward matching: a line mentioning the
  // deprecations page IS a deprecation notice in goreleaser's output, and the
  // cost of being wrong is a visible red rather than a silent pass.
  assert.equal(deprecationsIn('  • see https://goreleaser.com/deprecations for details').length, 1);
});
