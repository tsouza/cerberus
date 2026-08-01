// Tests for the GHCR image mirror's inventory → mirror-ref mapping.
//
// The Go side (`test/regression/mirror_inventory_test.go`) asks the completeness
// question: does the inventory name every upstream image the tree pulls? This
// side asks the correctness one: given an inventory, does every entry map to a
// ref the mirroring job can actually create and a consumer can actually reach?
// A null or malformed mirror ref is not visible at runtime — `mirroredRef`
// returning null is the documented MISS path, so a mapping bug degrades every
// consumer to Docker Hub silently, which is the failure the mirror exists to
// prevent.

import assert from 'node:assert/strict';
import test from 'node:test';

import { mirrorRegistry, mirroredImages, mirroredRef } from './lib/mirror.mjs';
import { plan } from './mirror-images.mjs';

test('every inventoried image maps to a mirror ref under the mirror registry', () => {
  for (const upstream of mirroredImages) {
    const mirror = mirroredRef(upstream);
    assert.ok(mirror !== null, `${upstream} is inventoried but maps to no mirror ref`);
    assert.ok(
      mirror.startsWith(`${mirrorRegistry}/`),
      `${upstream} maps to ${mirror}, which is outside ${mirrorRegistry}`,
    );
  }
});

test('the mirroring plan covers the inventory exactly, with no duplicate destinations', () => {
  const rows = plan();
  assert.deepEqual(
    rows.map((r) => r.upstream),
    mirroredImages,
    'the plan and the inventory disagree',
  );

  // Two upstream refs colliding on one mirror ref means the second copy
  // overwrites the first and a consumer of the first silently gets the wrong
  // image — the one mirror bug that does not present as a miss.
  const destinations = new Set(rows.map((r) => r.mirror));
  assert.equal(destinations.size, rows.length, 'two inventoried images map to the same mirror ref');
});

test('a bare Docker Hub name mirrors under its canonical library/ path', () => {
  // `golang:1.26` and `library/golang:1.26` are the same image to Docker Hub.
  // If they mirrored to two different packages, the mirroring job and the
  // consumer could each pick a different one and never meet.
  assert.equal(mirroredRef('golang:1.26'), `${mirrorRegistry}/library/golang:1.26`);
});

test('a ref the mirror was never told about resolves to null rather than to a phantom package', () => {
  // The consumer reads null as "pull upstream". Anything else would send it at
  // a GHCR path that has never held bytes, turning a fallback into a 404.
  assert.equal(mirroredRef('redis:7'), null, 'an uninventoried Docker Hub image must not resolve');
  assert.equal(mirroredRef(''), null);
  assert.equal(mirroredRef(undefined), null);
});

test('a ref already on another registry is never mirrored', () => {
  // A first segment carrying a dot or a port is a registry HOST, not a Docker
  // Hub owner. Mirroring these would add a hop and a staleness window against
  // registries that do not meter pulls this way.
  for (const ref of ['gcr.io/distroless/static-debian12:nonroot', 'ghcr.io/open-telemetry/opentelemetry-collector-contrib/telemetrygen:v0.140.0', 'localhost:5000/scratch:dev']) {
    assert.equal(mirroredRef(ref), null, `${ref} is not a Docker Hub ref and must not be mirrored`);
  }
});
