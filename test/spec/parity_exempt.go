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
)

// parityExemptReasons is the single source of truth for the `reason`
// key's closed vocabulary. `detail` is deliberately absent from this
// list: it is free text, checked only for non-emptiness by
// LoadParityExempt.
var parityExemptReasons = []string{
	ReasonNondeterministicSelection,
	ReasonNoComparableOracle,
	ReasonRejectionOnly,
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
