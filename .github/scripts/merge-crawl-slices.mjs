// merge-crawl-slices.mjs — merge isolated crawl shard artifacts and apply the
// inventory ratchet once. Env: CRAWL_SLICE_DIR, CRAWL_STACK, SWEEP_DEPTH.

import { readdirSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import process from 'node:process';

const inventoryFilename = (stack) => `grafana-surface-inventory.${stack}.json`;

export function owner(canonical, count) {
  const base = canonical.split('#', 1)[0].split('?', 1)[0];
  let hash = 0;
  for (let i = 0; i < base.length; i++) hash = (hash * 31 + base.charCodeAt(i)) >>> 0;
  return hash % count;
}

export function mergeSlices(slices, { stack, depth, inventory, exclusions, inventoryFilename }) {
  const filename = inventoryFilename ?? `grafana-surface-inventory.${stack}.json`;
  if (!Array.isArray(inventory.surfaces)) {
    throw new Error(`loadInventory: ${filename} has no surfaces[] array`);
  }
  if (inventory.stack !== stack) {
    throw new Error(
      `loadInventory: ${filename} pins stack ${JSON.stringify(inventory.stack)} ` +
        `but the active config is ${JSON.stringify(stack)} — the config points at the wrong file`,
    );
  }
  const exclusionsFilename = `grafana-surface-exclusions.${stack}.json`;
  if (!Array.isArray(exclusions.exclusions)) {
    throw new Error(`loadExclusions: ${exclusionsFilename} has no exclusions[] array`);
  }
  if (slices.length === 0) throw new Error('crawl slice merge received no slice artifacts');
  const count = slices[0].shard?.count;
  if (!Number.isInteger(count) || count < 1) throw new Error('crawl slice has an invalid shard count');
  const indexes = new Set();
  const visited = new Set();
  const discovered = new Set();
  for (const slice of slices) {
    if (slice.stack !== stack || slice.depth !== depth || slice.shard?.count !== count) {
      throw new Error('crawl slice metadata does not match the merge invocation');
    }
    const index = slice.shard.index;
    if (!Number.isInteger(index) || index < 0 || index >= count || indexes.has(index)) {
      throw new Error(`crawl slices do not form a unique shard cover at index ${index}`);
    }
    indexes.add(index);
    for (const url of slice.visited ?? []) {
      if (owner(url, count) !== index) throw new Error(`crawl slice ${index} contains unowned state ${url}`);
      visited.add(url);
    }
    for (const url of slice.discovered ?? []) discovered.add(url);
  }
  if (indexes.size !== count) throw new Error(`crawl slices cover ${indexes.size} of ${count} shards`);

  const excluded = new Set();
  const violations = [];
  for (const exclusion of exclusions.exclusions) {
    if (!exclusion.rationale || exclusion.rationale.trim() === '') {
      violations.push(
        `exclusion ${JSON.stringify(exclusion.url)} has no rationale — every exclusion must document why the surface is genuinely uncrawlable`,
      );
    }
    if (excluded.has(exclusion.url)) {
      violations.push(`exclusion ${JSON.stringify(exclusion.url)} is listed twice`);
    }
    excluded.add(exclusion.url);
  }
  const expected = new Set(
    inventory.surfaces
      .filter((surface) => depth === 'full' || surface.lean)
      .map((surface) => surface.url),
  );
  for (const surface of inventory.surfaces) {
    if (excluded.has(surface.url)) {
      violations.push(
        `surface ${JSON.stringify(surface.url)} is in BOTH the inventory and the exclusions file — a crawled surface cannot be excluded (stale exclusion)`,
      );
    }
  }
  const ratcheted = new Set([...visited, ...discovered]);
  for (const url of [...ratcheted].sort()) {
    if (!expected.has(url)) {
      violations.push(
        `NEW surface ${JSON.stringify(url)} visited but not pinned in ${filename} — coverage grew; regenerate deliberately with CERBERUS_UPDATE_INVENTORY=1 SWEEP_DEPTH=full CRAWL_STACK=${stack}`,
      );
    }
  }
  for (const url of [...expected].sort()) {
    if (!visited.has(url)) {
      violations.push(
        `pinned surface ${JSON.stringify(url)} (${depth} set) was NOT visited — coverage shrank; fix the crawl/stack regression, never delete the row to make this pass`,
      );
    }
  }
  return { visited, discovered, violations };
}

function main() {
  const dir = process.env.CRAWL_SLICE_DIR;
  const stack = process.env.CRAWL_STACK;
  if (!dir || !stack) {
    throw new Error('CRAWL_SLICE_DIR and CRAWL_STACK are required');
  }
  const slices = readdirSync(dir)
    .filter((name) => name.endsWith('.json'))
    .map((name) => JSON.parse(readFileSync(join(dir, name), 'utf8')));
  const depth = process.env.SWEEP_DEPTH ?? slices[0]?.depth;
  if (depth !== 'lean' && depth !== 'full') {
    throw new Error('crawl slices must declare SWEEP_DEPTH=lean|full');
  }
  const crawlDir = resolve('test/e2e/playwright/crawl');
  const inventory = JSON.parse(readFileSync(join(crawlDir, inventoryFilename(stack)), 'utf8'));
  const exclusions = JSON.parse(readFileSync(join(crawlDir, `grafana-surface-exclusions.${stack}.json`), 'utf8'));
  const result = mergeSlices(slices, {
    stack,
    depth,
    inventory,
    exclusions,
    inventoryFilename: inventoryFilename(stack),
  });
  if (result.violations.length > 0) {
    throw new Error(`surface-inventory ratchet violated:\n  - ${result.violations.join('\n  - ')}`);
  }
  console.log(`crawl slice merge: ${result.visited.size} audited states from ${slices.length} shards`);
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  try {
    main();
  } catch (error) {
    console.error(error.message);
    process.exit(1);
  }
}
