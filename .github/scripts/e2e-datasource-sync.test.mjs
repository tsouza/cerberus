// e2e-datasource-sync.test.mjs — node:test guard for the pure
// `rewriteDatasourceUrls` transform behind e2e-datasource-sync.mjs
// (cerberus issue #3096). This is the ONE piece of the script that is pure
// (no kubectl); the kubectl-apply/poll/rollout-restart orchestration around
// it is exercised live by `just e2e-up E2E_MODE=split` in the real `e2e` CI
// job (the split-mode dashboard shard), which is the only place a real
// per-head Grafana pod exists to sync into.
//
// The fixture below is the real shape test/e2e/k3s/grafana.yaml's
// `grafana-datasources` ConfigMap renders (three datasources, each pointed
// at the monolith URL) — not a hand-simplified stand-in.

import assert from 'node:assert/strict';
import test from 'node:test';

import { rewriteDatasourceUrls } from './e2e-datasource-sync.mjs';

const monolithDatasources = [
  'apiVersion: 1',
  'datasources:',
  '  - name: Cerberus-Prometheus',
  '    uid: cerberus-prometheus',
  '    type: prometheus',
  '    access: proxy',
  '    url: http://cerberus:8080',
  '    isDefault: true',
  '    editable: false',
  '  - name: Cerberus-Loki',
  '    uid: cerberus-loki',
  '    type: loki',
  '    access: proxy',
  '    url: http://cerberus:8080',
  '    editable: false',
  '  - name: Cerberus-Tempo',
  '    uid: cerberus-tempo',
  '    type: tempo',
  '    access: proxy',
  '    url: http://cerberus:8080',
  '    editable: false',
].join('\n');

test('rewriteDatasourceUrls points each datasource type at its own per-head Service', () => {
  const out = rewriteDatasourceUrls(monolithDatasources);
  const lines = out.split('\n');
  assert.equal(lines.filter((l) => l.includes('url: http://prometheus:8080')).length, 1);
  assert.equal(lines.filter((l) => l.includes('url: http://loki:8080')).length, 1);
  assert.equal(lines.filter((l) => l.includes('url: http://tempo:8080')).length, 1);
  // Nothing else in the document changed.
  assert.equal(lines.filter((l) => l.includes('http://cerberus:8080')).length, 0);
});

test('rewriteDatasourceUrls is idempotent — re-running on already-rewritten input is a no-op', () => {
  const once = rewriteDatasourceUrls(monolithDatasources);
  const twice = rewriteDatasourceUrls(once);
  assert.equal(once, twice);
});

test('rewriteDatasourceUrls leaves non-datasource lines byte-identical', () => {
  const out = rewriteDatasourceUrls(monolithDatasources);
  assert.ok(out.includes('    uid: cerberus-prometheus'));
  assert.ok(out.includes('    uid: cerberus-loki'));
  assert.ok(out.includes('    isDefault: true'));
});

test('rewriteDatasourceUrls only rewrites the url line, never the uid: cerberus-* lines', () => {
  // A naive whole-line "cerberus" -> type substring replace would also wreck
  // the `uid: cerberus-loki` line; this pins that the replace is scoped to
  // the exact `url: http://cerberus:8080` line only.
  const out = rewriteDatasourceUrls(monolithDatasources);
  assert.ok(out.includes('uid: cerberus-loki'));
  assert.ok(!out.includes('uid: loki-loki'));
});
