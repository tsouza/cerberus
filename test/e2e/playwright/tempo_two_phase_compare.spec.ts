import { test, expect, type APIRequestContext } from '@playwright/test';

// Structural two-phase A/B. The Tempo structural search two-phase split (rank
// top-N trace IDs, then hydrate wide restricted to them) MUST be result-identical
// to the traditional single wide query. This spec proves that on real
// telemetrygen-seeded traces by running the SAME structural search against two
// live cerberus heads — split ON (:8080) and split OFF (:8081,
// CERBERUS_TEMPO_STRUCTURAL_TWO_PHASE=false) — and asserting the returned trace
// set is identical.
//
// Requires `docker compose --profile twophase up` (telemetrygen-traces +
// cerberus-nosplit). Skipped when CERBERUS_NOSPLIT_URL is unset, so it no-ops in
// the normal compose-smoke shards.

const ON_URL = process.env.CERBERUS_URL || 'http://localhost:8080';
const OFF_URL = process.env.CERBERUS_NOSPLIT_URL || '';

// Both operands scope to the telemetrygen service so self-telemetry traces (whose
// volume differs over time) never enter the comparison. Children share the root's
// service.name, so a descendant match exists within each trace.
const QUERY =
  '{ resource.service.name = "twophase-gen" } >> { resource.service.name = "twophase-gen" }';
// Below the seeded trace count so phase-A top-N ranking actually selects a subset
// — the case where a divergent two-phase ranking would show up.
const LIMIT = 5;

test.describe('tempo structural two-phase A/B', () => {
  test.skip(
    !OFF_URL,
    'set CERBERUS_NOSPLIT_URL (docker compose --profile twophase up) to run the A/B',
  );

  test('split ON and OFF return an identical trace set for a structural search', async ({
    request,
  }) => {
    test.setTimeout(180_000);

    const search = async (base: string, startS: number, endS: number): Promise<string[]> => {
      const path = `/api/search?q=${encodeURIComponent(QUERY)}&start=${startS}&end=${endS}&limit=${LIMIT}`;
      const r = await request.get(base + path);
      expect(r.status(), `${base} /api/search status`).toBe(200);
      const body = await r.json();
      return ((body.traces || []) as Array<{ traceID: string }>).map((t) => t.traceID).sort();
    };

    // 1. Wait until telemetrygen is flowing: a rolling recent window on the ON
    //    head shows at least LIMIT traces.
    const deadline = Date.now() + 150_000;
    for (;;) {
      const nowS = Math.floor(Date.now() / 1000);
      const onIDs = await search(ON_URL, nowS - 300, nowS);
      if (onIDs.length >= LIMIT) break;
      if (Date.now() > deadline) {
        throw new Error(`telemetrygen never produced >= ${LIMIT} twophase-gen traces in the window`);
      }
      await new Promise((r) => setTimeout(r, 3000));
    }

    // 2. Freeze ONE window ending in the past (no new spans enter it) and query
    //    both heads back-to-back. Reading the same CH state with the same window
    //    means any divergence is the two-phase split's fault, not timing.
    const nowS = Math.floor(Date.now() / 1000);
    const startS = nowS - 300;
    const endS = nowS - 15;
    const onIDs = await search(ON_URL, startS, endS);
    const offIDs = await search(OFF_URL, startS, endS);

    expect(onIDs.length, 'ON head returned the top-N traces').toBeGreaterThanOrEqual(1);
    expect(offIDs, 'two-phase OFF trace set must equal two-phase ON (A≡B)').toEqual(onIDs);
  });
});
