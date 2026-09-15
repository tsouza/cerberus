package outcome

// DeficitDirection distinguishes cerberus being too STRICT (it rejects a
// query the reference backend answers) from cerberus being too LENIENT (it
// answers a query the reference backend rejects). These are two different
// bugs — a missing feature versus a missing validation — and issue #3429
// requires they never collapse into one generic "disagreement" bucket.
type DeficitDirection string

const (
	// DeficitCerberusTooStrict: the reference backend returned a real
	// answer: cerberus did not. A feature gap or an overzealous rejection.
	DeficitCerberusTooStrict DeficitDirection = "cerberus_too_strict"

	// DeficitCerberusTooLenient: the reference backend rejected the query;
	// cerberus returned a real answer anyway. A missing validation —
	// cerberus is silently answering a query it should refuse.
	DeficitCerberusTooLenient DeficitDirection = "cerberus_too_lenient"
)

// ClassifyDeficit compares a reference-backend Outcome against cerberus's
// own Outcome for the same query and reports which accept/reject deficit
// direction applies, if either does. ok is false when the two outcomes are
// not an accept/reject deficit at all — both sides agreeing (both Value, or
// both erroring) is a different situation this function deliberately does
// not narrate; a both-error case in particular belongs to
// CertifyRejectionAgreement's deliberate-enrollment check, never to this
// one, which only ever answers the accept-vs-reject question.
func ClassifyDeficit(reference, system Outcome) (direction DeficitDirection, ok bool) {
	referenceAccepted := reference.Class == ClassValue
	systemAccepted := system.Class == ClassValue
	switch {
	case referenceAccepted && !systemAccepted:
		return DeficitCerberusTooStrict, true
	case !referenceAccepted && systemAccepted:
		return DeficitCerberusTooLenient, true
	default:
		return "", false
	}
}
