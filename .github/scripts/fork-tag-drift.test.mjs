import assert from 'node:assert/strict';
import { test } from 'node:test';

import {
  applyBump,
  compareVersions,
  issueTitle,
  parseForkReplaces,
  parseTagRefs,
  planBumps,
  prBody,
  prTitle,
  requireVersion,
} from './fork-tag-drift.mjs';

// A go.mod shaped like the root module's: a single-line replace, a release-
// tracked fork whose tag core equals its require, and a replace block with one
// line per collector-contrib submodule.
const ROOT = `module github.com/tsouza/cerberus

require (
	github.com/grafana/tempo v1.5.1-0.20260416081131-10f9ba37b8b9
	github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter v0.152.0
	github.com/prometheus/prometheus v0.315.0
)

replace github.com/hashicorp/memberlist => github.com/grafana/memberlist v0.3.1-0.20260410131411-8c2f3bdae9db

replace github.com/prometheus/prometheus => github.com/tsouza/prometheus v0.315.0-cerberus-parser

replace github.com/grafana/tempo => github.com/tsouza/tempo v0.0.5-cerberus-accessors

replace (
	github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter => github.com/tsouza/opentelemetry-collector-contrib/exporter/clickhouseexporter v0.0.3-cerberus-ddl
	github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal => github.com/tsouza/opentelemetry-collector-contrib/internal/coreinternal v0.0.3-cerberus-ddl
)
`;

const OTELC = 'opentelemetry-collector-contrib';

function lsRemote(tags) {
  return tags.flatMap((t, i) => [`${i}abc\trefs/tags/${t}`, `${i}def\trefs/tags/${t}^{}`]).join('\n');
}

const otelcTags = (...versions) =>
  versions.flatMap((v) => [v, `exporter/clickhouseexporter/${v}`, `internal/coreinternal/${v}`]);

test('parseForkReplaces reads single-line and block replaces, and only tsouza forks', () => {
  const got = parseForkReplaces(ROOT);
  assert.deepEqual(
    got.map((r) => [r.fork, r.subdir, r.version, r.flavor]),
    [
      ['prometheus', '', 'v0.315.0-cerberus-parser', 'parser'],
      ['tempo', '', 'v0.0.5-cerberus-accessors', 'accessors'],
      [OTELC, 'exporter/clickhouseexporter', 'v0.0.3-cerberus-ddl', 'ddl'],
      [OTELC, 'internal/coreinternal', 'v0.0.3-cerberus-ddl', 'ddl'],
    ],
  );
});

test('compareVersions orders by numeric core, not lexically', () => {
  assert.ok(compareVersions('v0.0.10-cerberus-ddl', 'v0.0.9-cerberus-ddl') > 0);
  assert.ok(compareVersions('v0.315.0-cerberus-parser', 'v0.0.41-cerberus-parser') > 0);
  assert.ok(compareVersions('v0.0.3-cerberus-ddl', 'v0.0.4-cerberus-ddl') < 0);
  assert.equal(compareVersions('v1.2.3-cerberus-x', 'v1.2.3-cerberus-x'), 0);
  assert.throws(() => compareVersions('v1.2.3', 'v1.2.3-cerberus-x'), /not a fork tag/);
});

test('parseTagRefs strips refs/tags/ and folds peeled ^{} entries', () => {
  assert.deepEqual(parseTagRefs(lsRemote(['v0.0.1-cerberus-ddl', 'a/b/v0.0.1-cerberus-ddl'])), [
    'v0.0.1-cerberus-ddl',
    'a/b/v0.0.1-cerberus-ddl',
  ]);
});

test('requireVersion reads a require inside a block', () => {
  assert.equal(requireVersion(ROOT, 'github.com/prometheus/prometheus'), 'v0.315.0');
  assert.equal(requireVersion(ROOT, 'github.com/not/there'), null);
});

test('planBumps finds a newer collector-contrib tag and bumps it by pull request', () => {
  const tags = new Map([
    ['prometheus', ['v0.0.41-cerberus-parser', 'v0.315.0-cerberus-parser']],
    ['tempo', ['v0.0.4-cerberus-accessors', 'v0.0.5-cerberus-accessors']],
    [OTELC, otelcTags('v0.0.3-cerberus-ddl', 'v0.0.4-cerberus-ddl', 'v0.0.10-cerberus-ddl')],
  ]);
  assert.deepEqual(planBumps([{ path: 'go.mod', text: ROOT }], tags), [
    {
      fork: OTELC,
      from: 'v0.0.3-cerberus-ddl',
      to: 'v0.0.10-cerberus-ddl',
      releaseTracked: false,
      modules: [
        'github.com/open-telemetry/opentelemetry-collector-contrib/exporter/clickhouseexporter',
        'github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal',
      ],
    },
  ]);
});

test('planBumps is empty when every fork already pins its newest tag', () => {
  const tags = new Map([
    ['prometheus', ['v0.0.41-cerberus-parser', 'v0.315.0-cerberus-parser']],
    ['tempo', ['v0.0.5-cerberus-accessors']],
    [OTELC, otelcTags('v0.0.2-cerberus-ddl', 'v0.0.3-cerberus-ddl')],
  ]);
  assert.deepEqual(planBumps([{ path: 'go.mod', text: ROOT }], tags), []);
});

test('planBumps waits until every replaced submodule carries the tag', () => {
  const tags = new Map([[OTELC, [...otelcTags('v0.0.3-cerberus-ddl'), 'exporter/clickhouseexporter/v0.0.4-cerberus-ddl']]]);
  assert.deepEqual(planBumps([{ path: 'go.mod', text: ROOT }], tags), []);
});

test('planBumps ignores tags of another flavor', () => {
  const tags = new Map([['tempo', ['v0.0.9-cerberus-parser', 'v0.0.5-cerberus-accessors']]]);
  assert.deepEqual(planBumps([{ path: 'go.mod', text: ROOT }], tags), []);
});

test('planBumps marks a fork whose tag core equals its require as release-tracked', () => {
  const tags = new Map([['prometheus', ['v0.315.0-cerberus-parser', 'v0.316.0-cerberus-parser']]]);
  const [bump] = planBumps([{ path: 'go.mod', text: ROOT }], tags);
  assert.equal(bump.fork, 'prometheus');
  assert.equal(bump.to, 'v0.316.0-cerberus-parser');
  assert.equal(bump.releaseTracked, true);
  assert.match(issueTitle(bump), /tsouza\/prometheus fork tag v0\.316\.0-cerberus-parser/);
});

test('planBumps bumps from the oldest pin when nested go.mod files disagree', () => {
  const nested = ROOT.replaceAll('v0.0.3-cerberus-ddl', 'v0.0.2-cerberus-ddl');
  const tags = new Map([[OTELC, otelcTags('v0.0.2-cerberus-ddl', 'v0.0.3-cerberus-ddl')]]);
  const plan = planBumps(
    [
      { path: 'go.mod', text: ROOT },
      { path: 'test/oracle/go.mod', text: nested },
    ],
    tags,
  );
  assert.deepEqual(
    plan.map((b) => [b.fork, b.from, b.to]),
    [[OTELC, 'v0.0.2-cerberus-ddl', 'v0.0.3-cerberus-ddl']],
  );
});

test('applyBump moves every replace on the fork and nothing else', () => {
  const out = applyBump(ROOT, OTELC, 'v0.0.4-cerberus-ddl');
  assert.equal(out.match(/v0\.0\.4-cerberus-ddl/g).length, 2);
  assert.doesNotMatch(out, /v0\.0\.3-cerberus-ddl/);
  assert.match(out, /tsouza\/prometheus v0\.315\.0-cerberus-parser/);
  assert.match(out, /tsouza\/tempo v0\.0\.5-cerberus-accessors/);
  assert.match(out, /grafana\/memberlist v0\.3\.1-0\.20260410131411-8c2f3bdae9db/);
  assert.equal(applyBump(ROOT, 'tempo', 'v0.0.5-cerberus-accessors'), ROOT);
});

test('the pull request title passes the 100-character commitlint bound', () => {
  const bump = { fork: OTELC, from: 'v0.0.3-cerberus-ddl', to: 'v0.0.4-cerberus-ddl', modules: ['m'] };
  assert.match(prTitle(bump), /^chore\(deps\): /);
  assert.ok(prTitle(bump).length <= 100, prTitle(bump));
  assert.match(prBody(bump), /`v0\.0\.3-cerberus-ddl`/);
});
