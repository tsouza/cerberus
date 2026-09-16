package outcome

import "fmt"

// RejectionCase is a DELIBERATELY ENROLLED expectation that both cerberus
// and the reference backend reject the same query the same way. It is
// hand-authored by a caller — never inferred from the mere fact that two
// Outcomes both happened to error. That distinction is the whole point:
// issue #3429's Problem section calls out that two unrelated bugs which
// each independently error are NOT "agreement", and CertifyRejectionAgreement
// below only ever certifies a case matching one of these.
type RejectionCase struct {
	// ID is a stable identifier for the enrolled expectation — a
	// rejection-parity site key, a status-parity case description, or
	// equivalent.
	ID string

	// Head is the query language this case belongs to.
	Head string

	// Query is the trigger query both backends are expected to reject.
	Query string

	// ExpectedClass is the Class BOTH sides must classify to for this case
	// to count as agreement. Never ClassValue — a rejection case that
	// expects a real answer is not a rejection case.
	ExpectedClass Class
}

// RejectionAgreement is the certified evidence that an enrolled
// RejectionCase's expectation held: both sides actually produced the
// expected error Class, each retaining its own classification authority
// and original diagnostic payload. It can only be constructed by
// CertifyRejectionAgreement, never assembled by hand from two Outcomes
// alone, so a caller cannot accidentally fabricate "agreement" evidence by
// skipping the check.
type RejectionAgreement struct {
	Case      RejectionCase
	Reference Outcome
	System    Outcome
}

// CertifyRejectionAgreement certifies that reference and system both
// classify to rc's ExpectedClass. It fails closed on every shape that is
// NOT genuine, enrolled agreement:
//
//   - rc.ExpectedClass == ClassValue: a rejection case can never expect a
//     real answer; that is a caller bug, not evidence to certify.
//   - either side is ClassValue: at least one side actually answered, so
//     there is no rejection to agree on (this is the accept/reject deficit
//     ClassifyDeficit exists to narrate instead).
//   - either side's Class does not match rc.ExpectedClass: this is exactly
//     the "two unrelated bugs that each independently error" trap — e.g. the
//     reference backend genuinely rejects the query (ClassSemanticError)
//     while cerberus merely crashed on an unrelated transport fault
//     (ClassProtocolError). Both sides "errored", but not for the enrolled
//     reason, so this is not agreement.
//
// Row-shaped evidence alone (FromRowEvidence) can never satisfy this: it
// only ever produces ClassValue, which the second rule above always
// refuses.
func CertifyRejectionAgreement(rc RejectionCase, reference, system Outcome) (RejectionAgreement, error) {
	if rc.ExpectedClass == ClassValue {
		return RejectionAgreement{}, fmt.Errorf(
			"rejection case %q cannot expect ClassValue — a rejection case is a claim that both sides refuse the query",
			rc.ID,
		)
	}
	if reference.Class != rc.ExpectedClass {
		return RejectionAgreement{}, fmt.Errorf(
			"rejection case %q: reference outcome classified %q (authority=%s), want enrolled expectation %q",
			rc.ID, reference.Class, reference.Authority, rc.ExpectedClass,
		)
	}
	if system.Class != rc.ExpectedClass {
		return RejectionAgreement{}, fmt.Errorf(
			"rejection case %q: system outcome classified %q (authority=%s), want enrolled expectation %q",
			rc.ID, system.Class, system.Authority, rc.ExpectedClass,
		)
	}
	return RejectionAgreement{Case: rc, Reference: reference, System: system}, nil
}
