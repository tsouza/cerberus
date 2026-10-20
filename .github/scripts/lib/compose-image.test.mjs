import { test } from 'node:test';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { composeImage, readComposeImage } from './compose-image.mjs';

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..');

test('composeImage returns the first image of the repository', () => {
  const text = [
    'services:',
    '  clickhouse:',
    '    image: clickhouse/clickhouse-server:26.5',
    '  prometheus:',
    '    image: prom/prometheus:v3.15.0 # reference',
    '  other:',
    '    image: "prom/prometheus:v9.9.9"',
  ].join('\n');
  assert.equal(composeImage(text, 'prom/prometheus'), 'prom/prometheus:v3.15.0');
});

test('composeImage accepts a quoted reference', () => {
  assert.equal(composeImage("    image: 'grafana/loki:3.7.0'", 'grafana/loki'), 'grafana/loki:3.7.0');
});

test('composeImage names no image of another repository', () => {
  assert.equal(composeImage('    image: prom/prometheus-exporter:v1.0.0', 'prom/prometheus'), null);
  assert.equal(composeImage('    # image: prom/prometheus:v3.0.1', 'prom/prometheus'), null);
});

test('readComposeImage throws when the file pins none', () => {
  assert.throws(
    () => readComposeImage(join(repoRoot, 'compatibility/prometheus/docker-compose.yml'), 'no/such-image'),
    /pins no no\/such-image image/,
  );
});

test('readComposeImage reads the compatibility lane reference Prometheus', () => {
  assert.match(
    readComposeImage(join(repoRoot, 'compatibility/prometheus/docker-compose.yml'), 'prom/prometheus'),
    /^prom\/prometheus:v\d+\.\d+\.\d+$/,
  );
});
