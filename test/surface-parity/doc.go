// Package surfaceparity owns the function-surface conformance ledger —
// the machine-readable inventory of every grammar symbol the three
// upstream parsers accept, paired with cerberus's verdict (does the
// parse → fold → lower → optimize → emit pipeline accept it?) and the
// reference backend's verdict, classified into a four-way parity grid.
//
// Motivation: the sibling rejection-parity layer
// (test/rejection-parity/) is SITE-based — it diffs cerberus's KNOWN
// 422 code-sites against the reference. It structurally misses
// functions cerberus *silently* fails to lower: a symbol that was
// never wired never gets a rejection site, so it never enters that
// catalogue, so no parity case ever diffs it. This layer inverts the
// direction: it starts from the upstream parser's ACCEPTED grammar
// (parser.Functions, the loki Op* consts, the tempo intrinsic +
// metrics-op enums) and finds everything cerberus rejects that the
// reference accepts — the wrong-rejection surface that drives the P2
// burndown waves.
//
// The four classifications:
//
//   - parity-accept  — both cerberus and the reference accept. Healthy.
//   - parity-reject  — both reject (e.g. an experimental PromQL fn the
//     reference also gates off by default). Healthy.
//   - wrong-reject   — cerberus 422s, the reference accepts. A real
//     coverage gap: a symbol cerberus silently fails to lower. THIS is
//     the class the layer exists to surface.
//   - wrong-accept   — cerberus accepts, the reference rejects. A
//     correctness risk: cerberus answers a query the reference won't.
//
// Reference oracle:
//
//   - PromQL: the FLAG-ENABLED reference Prometheus HTTP verdict, pinned
//     in promql-reference-verdicts.json. A real prom/prometheus started
//     with --enable-feature=promql-experimental-functions is driven over
//     /api/v1/query for every parser.Functions symbol + the two
//     experimental aggregators (2xx => accept, 4xx => reject). This
//     REPLACES the former `if fn.Experimental { ref = reject }` stand-in,
//     which modelled a flag-OFF reference and so mislabelled every
//     flag-ON-implemented experimental fn as a parity rejection. The
//     in-process ratchet reads the pinned artifact (Docker-free); the
//     compatibility/promql-surface CI job re-probes the live reference and
//     fails on drift, so the artifact can never silently diverge from the
//     real reference. See promql_reference.go +
//     .github/scripts/promql-surface-gate.mjs.
//   - LogQL: reference Loki accepts exactly what syntax.ParseExpr
//     (parse + validate) accepts — the same gate the wire path runs.
//   - TraceQL: reference Tempo accepts exactly what traceql.Parse +
//     traceql.Validate accept in-process.
//
// The inventory.json artifact is the authoritative ledger. The
// meta-tests in inventory_test.go pin a three-leg ratchet modelled on
// test/rejection-parity/catalogue_test.go:
//
//  1. TestInventoryIsRegenerable rescans the parser symbol tables,
//     re-runs the cerberus + reference verdicts, and diffs the
//     regenerated inventory byte-for-byte against the checked-in JSON
//     (CERBERUS_UPDATE_INVENTORY=1 rewrites it).
//  2. TestWrongRejectionsAreRatcheted pins the current wrong-reject set
//     per head: a NEW wrong-reject (a symbol that regressed from
//     accept, or a newly-added grammar symbol cerberus doesn't lower)
//     fails CI red. A burndown that FIXES a wrong-reject also fails
//     until the inventory is regenerated — the ledger moves in
//     lock-step with the surface.
//  3. TestWrongAcceptsAreRatcheted does the same for wrong-accepts.
//
// Regen mode (CERBERUS_UPDATE_INVENTORY=1, same env convention as the
// rejection-parity catalogue + test/inventory) lets a P2 burndown that
// closes a gap re-pin the ledger in the same PR.
package surfaceparity
