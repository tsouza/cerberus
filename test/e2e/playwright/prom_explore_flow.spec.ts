import { test, expect } from '@playwright/test';

/**
 * Mirrors the request sequence Grafana's Explore page issues when a
 * user clicks through the label browser. Each step is a separate
 * request to the Prom datasource proxy; this spec validates that the
 * full sequence works end-to-end through cerberus.
 *
 * Step 1: GET /api/v1/labels — list of available label names
 *         (populates the label dropdown).
 * Step 2: GET /api/v1/label/job/values — values for the `job` label
 *         (populates the matcher-value dropdown after the user picks
 *         the `job` label).
 * Step 3: GET /api/v1/query?query=up{job="<value>"} — instant query
 *         using the picked value (this is what runs when the user
 *         clicks "Run query").
 *
 * Each step asserts the response shape so a regression at any layer
 * shows up as a specific failing step.
 */

const promProxy = '/api/datasources/proxy/uid/cerberus-prometheus/api/v1';

test('explore label browser flow: labels → values → query', async ({ request }) => {
  // Step 1: list labels.
  const labelsResp = await request.get(`${promProxy}/labels`);
  expect(labelsResp.status(), 'step 1: /labels status').toBe(200);
  const labelsBody = await labelsResp.json();
  expect(labelsBody.status, 'step 1: status').toBe('success');
  expect(labelsBody.data, 'step 1: data is array').toBeInstanceOf(Array);
  expect(labelsBody.data, 'step 1: data contains `job`').toContain('job');

  // Step 2: list values for `job`.
  const valuesResp = await request.get(`${promProxy}/label/job/values`);
  expect(valuesResp.status(), 'step 2: /label/job/values status').toBe(200);
  const valuesBody = await valuesResp.json();
  expect(valuesBody.status, 'step 2: status').toBe('success');
  expect(valuesBody.data, 'step 2: data is array').toBeInstanceOf(Array);
  expect(valuesBody.data.length, 'step 2: at least one job value').toBeGreaterThan(0);
  const firstJob = valuesBody.data[0];

  // Step 3: run query with the picked label value.
  const qResp = await request.get(
    `${promProxy}/query?query=up%7Bjob%3D%22${encodeURIComponent(firstJob)}%22%7D`,
  );
  expect(qResp.status(), 'step 3: /query status').toBe(200);
  const qBody = await qResp.json();
  expect(qBody.status, 'step 3: status').toBe('success');
  expect(qBody.data.resultType, 'step 3: resultType').toBe('vector');
  // At least one series should match.
  expect(qBody.data.result.length, 'step 3: at least one series').toBeGreaterThan(0);
  for (const s of qBody.data.result) {
    expect(s.metric.job, 'step 3: each series has the picked job').toBe(firstJob);
  }
});

test('metric-picker flow: /label/__name__/values populates metric dropdown', async ({ request }) => {
  // Grafana's metric picker queries this exact path. The seed includes
  // `up` (gauge) and `http_server_request_duration_count` (sum).
  const resp = await request.get(`${promProxy}/label/__name__/values`);
  expect(resp.status()).toBe(200);
  const body = await resp.json();
  expect(body.status).toBe('success');
  expect(body.data).toContain('up');
  expect(body.data).toContain('http_server_request_duration_count');
});
