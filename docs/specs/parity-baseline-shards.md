# Parity baseline shards

Issue: [#2235](https://github.com/tsouza/cerberus/issues/2235)

## Problem

The compatibility ratchet stores all four heads in
`compatibility/parity-baseline.json`. Any Prometheus corpus enrollment rewrites
that file, so unrelated PromQL pull requests collide even when their cases have
different owners. Splitting only by head leaves the Prometheus hotspot intact.

## Required behaviour

The baseline is a directory with one immutable manifest and a fixed roster of
deterministic buckets for every head. A case ID owns a bucket by a versioned
SHA-256 mapping, independent of insertion order and of every other case. Adding
or removing a case therefore changes only its owning bucket.

All readers use one loader. The loader reconstructs the existing consumer
shape (`heads.<head>.{passed,total,cases}`), with the same globally sorted case
ordering and full-parity counts, and rejects the baseline before a consumer can
gate on it when any of these conditions holds:

- the manifest, a head, or an expected bucket is missing;
- an undeclared head, bucket, or root entry exists;
- a manifest or bucket is not in canonical deterministic JSON form;
- a bucket declares the wrong head/name or contains a case owned by another
  bucket;
- a case is empty, malformed, duplicated within or across buckets, or locally
  out of order;
- any declared head reconstructs to an empty roster;
- the retired monolithic `compatibility/parity-baseline.json` still exists.

`compat-baseline-sync.mjs` continues to take an authoritative
`compat-cases.json` artifact and refuses failing cases. It replaces and prunes
only the selected head's fixed bucket set. Files belonging to every other head
remain byte-identical. A successful sync round-trips through the same loader
used by the ratchet and documentation gate.

The manifest fixes 64 buckets (`00.json` through `3f.json`) for each of
`loki`, `prometheus`, `tempo`, and `tempo-grpc`. This reduces the remaining
same-head collision probability to one in 64 without introducing a shared
per-case roster that every enrollment would have to edit.

## Compatibility and migration

The migration is semantic-only: the four reconstructed rosters must be deeply
equal to the retired monolith, including global case order and derived counts.
Compatibility workflow commands keep their current interface; only the
default baseline location and their explanatory comments change. Generated
artifact merge protection applies to every manifest/bucket JSON file.

## Regression contract

Node tests exercise the real loader and sync subprocesses over temporary shard
trees. They prove missing/extra buckets, duplicates, wrong placement, malformed
entries, non-canonical bytes, hollow heads, and a stale monolith all fail. A
two-branch simulation adds two chosen Prometheus IDs with different hashes and
asserts the changed-file sets are disjoint. The sync test also snapshots every
non-selected head before and after update.

## Implementation tasks

1. Add the shared manifest/bucket loader and selected-head sync primitive.
2. Migrate the current monolithic roster into the canonical bucket tree.
3. Route the ratchet, updater, doc-count gate, and structural guard through the
   loader.
4. Update generated-artifact attributes/rosters, workflow comments, source
   comments, and compatibility documentation.
5. Add failure-mode and conflict-resistance regression coverage, then run the
   focused Node and Go gates that own these contracts.
