// Package rejectionparity owns the deliberate-rejection catalogue —
// the machine-readable inventory of every error-construction site in
// the three lowerings (internal/promql, internal/logql,
// internal/traceql) that can surface as an HTTP 422 "query is valid
// <QL> but cerberus rejects it" response.
//
// Motivation: a deliberate rejection is a CLAIM about reference
// behaviour ("the reference backend cannot answer this either") that
// no other test layer verifies differentially. The `kind != nil`
// incident proved the failure mode: cerberus rejected a query
// reference Tempo accepts, and every test layer was blind to it
// because rejections were never diffed against the reference. This
// package makes that class of wrong belief impossible to pin
// silently:
//
//  1. ScanSites enumerates every `fmt.Errorf("<head>: ...")` /
//     `errors.New("<head>: ...")` construction site in the three
//     lowering packages via go/ast — the mechanical universe of
//     rejection candidates.
//  2. catalogue.json classifies every site as either `rejection`
//     (reachable from a parseable query; carries a minimal
//     trigger query) or `internal` (defensive / invariant /
//     error-propagation wrapper; carries a rationale).
//  3. The meta-tests in catalogue_test.go pin the three-way ratchet:
//     scanned-site set == catalogue set (regenerable via
//     CERBERUS_UPDATE_INVENTORY=1), every `rejection` entry's trigger
//     query parses AND fails lowering with the site's message, and
//     the parity-corpus case set is derived 1:1 from the `rejection`
//     entries via BuildCases.
//  4. compatibility/cmd/rejection-parity consumes BuildCases inside
//     each compat harness and asserts the REFERENCE backend also
//     rejects every trigger query. Reference accepting is a
//     wrong-rejection bug to fix at the source — never an allow-list
//     entry.
//
// Adding a new rejection to a lowering therefore requires: a
// catalogue entry (the regen test fails without it), a trigger query
// (the exerciser test fails without it), and — by construction — a
// parity-corpus case the next compat run diffs against the reference
// backend.
package rejectionparity
