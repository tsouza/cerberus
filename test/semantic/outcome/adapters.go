package outcome

import (
	"fmt"

	"google.golang.org/grpc/codes"

	rejectionparity "github.com/tsouza/cerberus/test/rejection-parity"
)

// httpSuccessStatusClass is the first digit of every HTTP status class that
// means "a real answer came back" (200-299). Anything outside it is a
// transport/wire-level failure, never a query-semantic one — see
// FromHTTPStatus.
const httpSuccessStatusClass = 2

// FromHTTPStatus normalizes one HTTP response's status code — the same
// signal Loki's status_parity.go already observes via fetchRawStatus, and
// the HTTP axis of Tempo's compatibility driver observes the same way —
// into the outcome vocabulary. It does not reimplement either backend's
// rejection logic: status is whatever the caller's own HTTP round trip
// already produced.
//
// The result is always ClassValue (2xx) or ClassProtocolError (everything
// else). It is never ClassSyntaxError, ClassSemanticError, or
// ClassUnsupported — an HTTP status code alone carries no query-semantic
// information, only a protocol-level verdict. A 400 that happens to be
// caused by a genuine query-semantic rejection is still classified
// ClassProtocolError here; pairing it with the matching
// ClassSyntaxError/ClassSemanticError verdict from the head's own
// classifier (FromRejectionParityEntry) is the caller's job, and keeping
// the two calls separate is what stops the two domains from being
// conflated (issue #3429 acceptance criterion).
func FromHTTPStatus(comparator, head string, status int, body string) Outcome {
	authority := Authority(fmt.Sprintf("%s/http-status", head))
	if status/100 == httpSuccessStatusClass {
		return Outcome{Class: ClassValue, Authority: authority, Comparator: comparator, HTTPStatus: status, Payload: body}
	}
	return Outcome{Class: ClassProtocolError, Authority: authority, Comparator: comparator, HTTPStatus: status, Payload: body}
}

// FromGRPCCode normalizes one gRPC response's status code — the same
// signal Tempo's grpc_diff.go already observes via
// google.golang.org/grpc/status.FromError — into the outcome vocabulary.
// It does not reimplement grpc_diff.go's own code comparison; code is
// whatever the caller's own RPC already produced.
//
// Like FromHTTPStatus, the result is always ClassValue (codes.OK) or
// ClassProtocolError (any other code) — never a query-semantic Class. The
// two constructors are kept deliberately parallel and deliberately
// separate so an HTTP status and a gRPC code — different wire domains with
// no canonical mapping between them, as grpc_diff.go's own commentary
// notes — are never accidentally compared against each other or conflated
// into one generic "error" bucket.
func FromGRPCCode(comparator, head string, code codes.Code, detail string) Outcome {
	authority := Authority(fmt.Sprintf("%s/grpc-code", head))
	if code == codes.OK {
		return Outcome{Class: ClassValue, Authority: authority, Comparator: comparator, GRPCCode: code.String(), Payload: detail}
	}
	return Outcome{Class: ClassProtocolError, Authority: authority, Comparator: comparator, GRPCCode: code.String(), Payload: detail}
}

// FromRejectionParityEntry normalizes one test/rejection-parity catalogue
// entry's already-curated classification into the outcome vocabulary. It
// does not reimplement rejectionparity's reachability analysis or its
// hand-curated class assignment — those stay entirely inside
// test/rejection-parity/catalogue.go; this only maps the finished verdict
// onto the shared vocabulary:
//
//   - ClassRejection (both backends deliberately refuse the query, parity
//     verified by the compat harnesses) normalizes to ClassSemanticError:
//     these are lowering-time rejections the wire-reachable trigger query
//     proves reachable, not grammar failures.
//   - ClassDivergence (cerberus currently rejects a query the reference
//     backend answers, tracked by an open issue) normalizes to
//     ClassUnsupported: exactly the "real, currently-unimplemented feature
//     gap" the outcome vocabulary's Unsupported class describes.
//   - ClassInternal is not wire-observable at all (the entry's own
//     doc: "not reachable from a parseable query through the HTTP query
//     endpoints") and has no outcome to normalize; this returns an error
//     rather than inventing one.
func FromRejectionParityEntry(comparator, head, catalogueClass string) (Outcome, error) {
	authority := Authority(fmt.Sprintf("%s/rejection-parity", head))
	switch catalogueClass {
	case rejectionparity.ClassRejection:
		return Outcome{Class: ClassSemanticError, Authority: authority, Comparator: comparator}, nil
	case rejectionparity.ClassDivergence:
		return Outcome{Class: ClassUnsupported, Authority: authority, Comparator: comparator}, nil
	default:
		return Outcome{}, fmt.Errorf(
			"rejection-parity class %q has no wire-observable outcome to normalize (want %q or %q)",
			catalogueClass, rejectionparity.ClassRejection, rejectionparity.ClassDivergence,
		)
	}
}

// FromRowEvidence builds the Outcome for a side that returned a row-shaped
// answer with no error — a TXTAR golden roundtrip (test/spec/parity.go's
// -- expected_rows -- comparison), a property-test comparator success
// (test/property/framework.go's compareOutcomeRows), or any verifier whose
// entire signal is "rows came back, nothing errored".
//
// It can only ever produce ClassValue: row evidence has no way to express
// a rejection or a protocol failure by construction, so this constructor
// structurally cannot produce anything else. That is what makes "golden
// rows alone certify rejection/protocol agreement" impossible rather than
// merely undesirable — CertifyRejectionAgreement's ClassValue guard refuses
// every Outcome this function can ever return.
func FromRowEvidence(comparator string) Outcome {
	return Outcome{Class: ClassValue, Authority: Authority(comparator + "/rows"), Comparator: comparator}
}
