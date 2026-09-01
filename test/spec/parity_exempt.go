// Package spec — parity exemption declarations.
//
// This file defines the optional `parity_exempt:` section: the other half
// of the closure the `parity:` section (parity.go) leaves open. A fixture
// is either enrolled against a real reference engine (`parity:`) or it
// declares, in a reviewed and closed vocabulary, structurally WHY it
// cannot be — never neither. test/regression's
// TestPromQLParityCoverageIsComplete is what enforces the "never neither"
// half; this file is what keeps the declared reason honest rather than a
// free-form excuse.
//
// # Why a new section rather than a new value inside `parity:`
//
// parity.go's own atan2QueryPattern doc comment already rejected a new
// required `parity:` key for a narrower reason (an ULP tolerance) because
// LoadParity treats every key as required on every enrolled fixture, so
// adding one would force an edit onto every already-enrolled fixture. The
// same argument applies here with more force: an exemption fixture has no
// oracle, no endpoint and no scope to declare. Bolting `status: exempt`
// onto the `parity:` vocabulary would either make those three keys
// conditionally required — reintroducing exactly the "some keys are
// required, some aren't" ambiguity LoadParity's error path exists to
// prevent — or force meaningless placeholder values onto every exemption.
// A separate section, mutually exclusive with `parity:` (enforced by
// TestPromQLParityCoverageIsComplete), keeps both shapes simple and keeps
// LoadParity's "every key required" invariant intact for the shape it was
// built for.
//
// # Why the reason is a closed vocabulary and not free text
//
// A `reason: <anything>` line would be indistinguishable from the
// allow-list invariant 7 forbids: any excuse, once typed, would silently
// satisfy the corpus-completeness gate. Restricting `reason` to a small,
// closed set of STRUCTURAL claims — never "not gotten to yet", never
// "flaky", never a fixture-specific excuse — keeps the escape hatch
// auditable: TestParityExemptVocabulariesAreClosed pins the set, and
// widening it is a reviewed source-line change, not a per-fixture opt-out.
// `detail:` carries the free-text, fixture-specific explanation a reviewer
// needs, but it participates in no vocabulary check beyond "non-empty" —
// the STRUCTURAL claim a reason must prove is carried entirely by
// `reason:`.
package spec

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// SectionParityExempt is the TXTAR section name a fixture carries to
// declare, instead of enrolling, why it cannot be compared against a
// reference engine.
const SectionParityExempt = "parity_exempt"

// Exemption reasons. Each is a claim about the fixture's STRUCTURE, never
// about its current state of being un-gotten-to — see the package doc.
const (
	// ReasonNondeterministicSelection covers an operator whose own
	// reference implementation documents its survivor/answer set as
	// implementation-defined (PromQL's limitk tie-break, confirmed
	// against prometheus/promql/engine.go: LIMITK keeps whichever K
	// series arrive first in the input matrix's iteration order, an
	// order neither engine promises, versus cerberus's own
	// `row_number() OVER ()` with no ORDER BY). There is no "reference
	// answer" for a second implementation to agree with, however
	// faithfully it reimplements the operator.
	//
	// This is NOT the reason for limit_ratio: that sampler is
	// hash-based on the label set (HashRatioSampler in the same file),
	// and cerberus's `xxHash64` predicate reproduces the reference
	// stringlabels encoding bit-for-bit (see lowerLimitRatio's own
	// doc comment) — limit_ratio fixtures are enrollable, not exempt.
	ReasonNondeterministicSelection = "nondeterministic-selection"

	// ReasonNoComparableOracle covers a fixture whose answer shape has
	// no comparator wired into RunParity — a real Prometheus HTTP
	// concept (a label-name list, a label-value list, an exemplar
	// array) that simply is not one of the Sample-shaped,
	// `endpoint:`-dispatched comparisons RunParity performs today. The
	// upstream endpoint exists; RunParity's dispatch table does not
	// reach it.
	ReasonNoComparableOracle = "no-comparable-oracle"

	// ReasonRejectionOnly covers a fixture whose query must be REJECTED
	// rather than answered. Neither RunRoundTrip (needs `seed:` +
	// `expected_rows:`) nor RunParity (needs a round trip on top of
	// that) has a shape for an expected error; the executable assertion
	// lives in a dedicated Go test instead (see each fixture's own
	// header comment for the exact test).
	ReasonRejectionOnly = "rejection-only"

	// ReasonVacuousEmptyInput covers a fixture whose query reads a
	// SELECTOR (confirmed by RunParity's own exprReadsSeries — a
	// VectorSelector or MatrixSelector genuinely present in the parsed
	// expression) but whose seed intentionally provisions zero rows in
	// every table that selector could read. evaluatePrometheusParity
	// already refuses this case at runtime (see its own doc comment: "a
	// genuinely empty series set is only a vacuous check ... when the
	// query reads one"), so enrolling would not check cerberus against
	// the reference engine, it would check cerberus against a reference
	// engine handed no data to disagree with — every candidate answer
	// passes, so a real lowering bug in the selector path would pass
	// too. This is a fact about the SEED, not about the operator: it is
	// the reverse of ReasonNoComparableOracle (whose gap is in
	// RunParity's own dispatch table, not in the fixture's data).
	//
	// This is NOT the reason for a query that reads no selector at all
	// — `pi()`, `time()`, `vector(N)`, the zero-argument date functions,
	// and any of those same shapes used as a subquery inner — whose
	// zero-series seed is not vacuous but the query's own correct,
	// data-independent input (exprReadsSeries returns false for all of
	// them; see pi_constant.txtar, hour_default.txtar, and
	// subquery_inner_anchor_synthesised.txtar, all enrolled with a real
	// `parity:` section for exactly that reason).
	ReasonVacuousEmptyInput = "vacuous-empty-input"

	// ReasonDuplicateTimestampSeed covers a fixture whose seed
	// DELIBERATELY carries two metric samples at one (series, timestamp)
	// with DIFFERENT values, in order to pin how cerberus's own emitted
	// shape treats that pair. Like [ReasonVacuousEmptyInput] this is a
	// fact about the SEED rather than about the operator.
	//
	// Which of the two survives is implementation-defined on both sides.
	// Prometheus's TSDB appender stores at most one sample per (series,
	// timestamp): parityoracle/promql/oracle.go's appendSeries feeds a
	// real teststorage head, and the second sample is dropped at commit,
	// so the survivor is whichever arrived first — a fact about the
	// appender and about ingestion order, not about PromQL. ClickHouse
	// keeps both rows, and which one a lowering surfaces depends on the
	// emitted shape: the sorted-slab over_time path sums both, the
	// array-fold path's arraySort(groupArray((ts, value))) breaks the tie
	// by VALUE, and the rate family deduplicates by design. So the
	// reference has no answer for cerberus to agree with, however
	// faithfully it reimplements the operator.
	//
	// The contract such a fixture pins is not lost with the enrolment: it
	// is pinned by a cerberus-vs-cerberus differential over the same seed
	// (internal/promql/duplicate_timestamp_seed_chdb_test.go), a sharper
	// oracle than a reference backend for this question — it compares the
	// strategy under test against the fan-out strategy it must agree
	// with, rather than against an engine that discarded one of the
	// samples before evaluating anything.
	//
	// This is NOT the reason for a duplicate whose rows carry IDENTICAL
	// values. Prometheus still keeps one row there and ClickHouse still
	// keeps two, so a counting reducer can still diverge — but that
	// divergence has a RIGHT answer (the reference's) and cerberus can be
	// made to match it, which is an ordinary bug to fix at the source
	// rather than a structural claim.
	// increase_duplicate_timestamp_dedup.txtar seeds exactly that, stays
	// ENROLLED, and passes because the rate family deduplicates.
	ReasonDuplicateTimestampSeed = "duplicate-timestamp-seed"
)

// parityExemptReasons is the single source of truth for the `reason`
// key's closed vocabulary. `detail` is deliberately absent from this
// list: it is free text, checked only for non-emptiness by
// LoadParityExempt.
var parityExemptReasons = []string{
	ReasonNondeterministicSelection,
	ReasonNoComparableOracle,
	ReasonRejectionOnly,
	ReasonVacuousEmptyInput,
	ReasonDuplicateTimestampSeed,
}

// ParityExemptReasons returns the accepted `reason` values, sorted.
// Exported for the contract test, mirroring ParityValues.
func ParityExemptReasons() []string {
	vals := append([]string(nil), parityExemptReasons...)
	sort.Strings(vals)
	return vals
}

// ParityExempt is the parsed `parity_exempt:` section.
type ParityExempt struct {
	// Reason is the closed-vocabulary structural claim.
	Reason string

	// Detail is the fixture-specific, human-authored explanation. Never
	// empty — see LoadParityExempt.
	Detail string
}

// LoadParityExempt parses a fixture's `parity_exempt:` section.
//
// The bool reports whether the fixture declared an exemption at all. A
// fixture with neither this section nor `parity:` is not caught here —
// see TestPromQLParityCoverageIsComplete, the sibling check that makes
// "neither" a failure.
//
// A fixture WITH the section but a malformed body IS an error, for the
// same reason LoadParity treats a malformed `parity:` body as an error: a
// typo must fail loudly, not quietly stop counting as either enrolled or
// exempt.
func LoadParityExempt(c *Case) (*ParityExempt, bool, error) {
	body, ok := c.Section(SectionParityExempt)
	if !ok {
		return nil, false, nil
	}

	got := map[string]string{}
	for i, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			return nil, false, fmt.Errorf(
				"%s section line %d: %q is not `key: value`", SectionParityExempt, i+1, line,
			)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		if _, dup := got[key]; dup {
			return nil, false, fmt.Errorf("%s section: key %q appears twice", SectionParityExempt, key)
		}

		switch key {
		case "reason":
			if !slices.Contains(ParityExemptReasons(), value) {
				return nil, false, fmt.Errorf(
					"%s section: key %q has value %q (accepted: %s)",
					SectionParityExempt, key, value, strings.Join(ParityExemptReasons(), ", "),
				)
			}
		case "detail":
			if value == "" {
				return nil, false, fmt.Errorf(
					"%s section: key %q must not be empty — it is the reviewed, fixture-specific "+
						"explanation a `reason:` category alone cannot carry",
					SectionParityExempt, key,
				)
			}
		default:
			return nil, false, fmt.Errorf(
				"%s section: unknown key %q (accepted: detail, reason)", SectionParityExempt, key,
			)
		}
		got[key] = value
	}

	for _, key := range []string{"detail", "reason"} {
		if _, present := got[key]; !present {
			return nil, false, fmt.Errorf("%s section: missing required key %q", SectionParityExempt, key)
		}
	}

	return &ParityExempt{Reason: got["reason"], Detail: got["detail"]}, true, nil
}
