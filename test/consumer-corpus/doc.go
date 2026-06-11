// Package consumercorpus is the consumer-contract replay layer: a
// corpus of REAL request shapes Grafana sends to cerberus's three
// datasource APIs, replayed against the in-process HTTP handlers and
// decoded EXACTLY as the consumer decodes them (strict gogo/protobuf
// unmarshal into tempopb types for the Tempo proto endpoints, bare
// logproto-shaped JSON for Loki, Prom API envelopes for Prometheus).
//
// Why this layer exists: a week of incidents (2026-06) showed that
// consumer-contract bugs — /api/v2/traces serving a bare Trace where
// Grafana 12 strict-decodes a TraceByIDResponse envelope (#764),
// detected_fields wrapped in a query envelope where Grafana reads
// top-level `fields` (#774), /api/search omitting spanSets (#770),
// Traces Drilldown breakdown queries 422ing on `<groupBy> != nil` —
// were only caught by the e2e browser stack (minutes per run, needs
// k3d/compose). Every one of them is detectable in-process at unit
// cost: replay the captured request, decode as the consumer, assert
// the contract. This package does exactly that, in two lanes:
//
//   - replay_test.go (default build tags, runs in the `check` CI
//     lane): handlers are backed by canned-row stub queriers, so the
//     lane pins routing, status codes, envelope/wire shapes, and
//     consumer decodability.
//   - replay_chdb_test.go (`chdb` build tag, runs in the chdb CI
//     lane via `just test-chdb`): handlers are backed by a seeded
//     chDB session, so the lane additionally pins data-bearing
//     predicates (non-empty results, value sanity) through the full
//     parse → lower → optimize → emit → execute pipeline.
//
// # Corpus layout and refresh flow
//
// Corpus entries live in version-keyed directories named after the
// Grafana release whose request shapes they capture:
//
//	test/consumer-corpus/grafana-12.2.9/*.json
//
// Each file is ONE entry: the request cerberus receives, the
// Grafana-side request that produced it (provenance), and the
// per-entry expectations (status, consumer decoder, wire predicates,
// chdb-only data predicates). See corpus.go for the schema.
//
// Refresh flow: the corpus is a snapshot, mined from live stacks.
// The e2e crawler (test/e2e/playwright/crawl/) and the drilldown
// specs are the corpus miners — when they observe a new request
// shape Grafana sends (typically after a Grafana version bump), the
// shape is captured into a NEW version-keyed directory
// (grafana-<next-version>/) and the old directory stays as long as
// that Grafana version is supported by the e2e stacks. The ratchet
// meta-test (ratchet_test.go) forbids shrinking the corpus: entry
// counts may only grow, and per-datasource minimums are pinned.
package consumercorpus
