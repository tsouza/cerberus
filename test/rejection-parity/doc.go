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
//  2. catalogue.json classifies every site into one of three classes —
//     `rejection`, `internal`, or `divergence` (see below).
//  3. The meta-tests in catalogue_test.go pin the three-way ratchet:
//     scanned-site set == catalogue set (regenerable via
//     CERBERUS_UPDATE_INVENTORY=1), every `rejection`/`divergence`
//     entry's trigger query parses AND fails lowering with the site's
//     message, and the parity-corpus case set is derived 1:1 from the
//     `rejection` + `divergence` entries via BuildCases.
//  4. compatibility/cmd/rejection-parity consumes BuildCases inside
//     each compat harness and, per case, asserts the class's claim:
//     for `rejection`, that the REFERENCE backend also rejects the
//     trigger query; for `divergence`, that it ACCEPTS it (see below).
//     A `rejection` entry whose reference accepts is a wrong-rejection
//     bug to fix at the source — never an allow-list entry.
//
// # The three classes
//
//   - `rejection`  — the parity claim is "cerberus and the reference
//     backend both reject this query". Requires a trigger query (+
//     endpoint override where needed); forbids a rationale or a
//     tracking issue.
//   - `internal`   — the site is not reachable from a parseable query
//     through the HTTP query endpoints at all (parser-enforced
//     shapes, internal invariants, %w error-propagation wrappers).
//     Requires a rationale explaining why; forbids a trigger query,
//     endpoint, or tracking issue.
//   - `divergence` — the parity claim is "cerberus rejects this query
//     AND the reference backend answers it, on purpose, and that gap
//     is tracked". Requires everything `rejection` requires PLUS an
//     open GitHub issue number (TrackingIssue) tracking closure.
//
// # Why `divergence` is not the allow-list this package forbids
//
// The obvious wrong shape for a third class would be "cerberus knows
// it disagrees with the reference here, so stop checking" — exactly
// the allow-list mechanism note (4) above forbids, and exactly the
// failure mode that let the `kind != nil` incident (a real
// wrong-rejection bug) ship silently before this package existed.
// `divergence` is deliberately NOT that. It makes three POSITIVE,
// machine-checked claims, in both directions, every compat run:
//
//  1. cerberus still rejects the trigger query — the same
//     in-process reachability proof TestRejectionTriggersExerciseSites
//     already runs for `rejection` entries, reused unchanged.
//  2. the reference backend still accepts it — literally the inverse
//     of what a `rejection` entry asserts; compatibility/cmd/
//     rejection-parity's runCase branches on the case's Class and
//     checks the opposite status-class pairing.
//  3. an issue in this repository, cited by number, is open and is an
//     issue rather than a pull request — checked on every
//     pull_request / push / merge_group by a dedicated forbid-deferral
//     workflow step, mirroring the same liveness probe
//     forbid-deferral.mjs already runs for deferral markers.
//
// Each claim is a ratchet, not an exemption:
//
//   - If someone fixes the divergence (cerberus starts answering),
//     claim 1 fails — cerberus now returns 2xx for a case the
//     catalogue says it rejects — and the entry must be deleted.
//   - If upstream changes to also reject, claim 2 fails — both sides
//     now return 4xx — and the entry must be reclassified to
//     `rejection`.
//   - If the tracking issue is closed while the entry remains, claim 3
//     fails regardless of what either backend does today.
//
// A `divergence` entry can therefore never sit still: it is always
// either being actively re-verified against both backends, or it is
// failing the build. That is what distinguishes it from an allow-list
// entry, which by definition stops anyone from checking again.
//
// Adding a new rejection to a lowering therefore requires: a
// catalogue entry (the regen test fails without it), a trigger query
// (the exerciser test fails without it), and — by construction — a
// parity-corpus case the next compat run diffs against the reference
// backend. Reclassifying a `rejection` to `divergence` additionally
// requires filing (or citing) an open tracking issue first — the
// classification test fails without one.
package rejectionparity
