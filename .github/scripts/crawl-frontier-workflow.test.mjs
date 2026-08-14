import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { test } from 'node:test';

const workflow = readFileSync(resolve('.github/workflows/e2e.yml'), 'utf8');

test('dashboard release status needs and rejects the crawl-slice merge', () => {
  assert.match(
    workflow,
    /dashboard:\n    name: dashboard\n    needs: \[dashboard-setup, dashboard-shard, dashboard-crawl-merge\]/,
  );
  assert.match(
    workflow,
    /dashboard crawl slice merge did not succeed \(\$\{\{ needs\.dashboard-crawl-merge\.result \}\}\)/,
  );
});

test('release PRs run the crawl matrix and its merge', () => {
  assert.match(
    workflow,
    /RUN_SHARD: \$\{\{ matrix\.crawlStack != 'k3d' \|\| github\.event_name == 'schedule' \|\| github\.event_name == 'workflow_dispatch' \|\| startsWith\(github\.head_ref, 'release\/'\) \}\}/,
  );
  assert.match(
    workflow,
    /github\.event_name == 'schedule' \|\| startsWith\(github\.head_ref, 'release\/'\) \|\|\n      \(github\.event_name == 'workflow_dispatch'/,
  );
});

test('ordinary dispatches merge slices while k3d regeneration remains unsharded', () => {
  assert.match(
    workflow,
    /github\.event_name == 'workflow_dispatch' &&\n      inputs\.update_crawl_inventory != 'k3d' && inputs\.update_crawl_inventory != 'both'/,
  );
  assert.match(
    workflow,
    /matrix\.crawlShardCount > 1/,
  );
});
