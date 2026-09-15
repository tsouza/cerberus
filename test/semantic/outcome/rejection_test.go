package outcome

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/property"
)

// unprocessableEntityStatus stands in for a compatibility harness's own
// non-standard use of 422 (Prometheus's /api/v1/query itself returns 422
// for certain query-execution errors); named here since net/http exposes
// no matching constant.
const unprocessableEntityStatus = 422

func validRejectionCase() RejectionCase {
	return RejectionCase{
		ID:            "internal/promql/lower.go:lowerX#deadbeef",
		Head:          "promql",
		Query:         `histogram_quantile(-1, up)`,
		ExpectedClass: ClassSemanticError,
	}
}

func TestCertifyRejectionAgreementSucceedsAndRetainsEvidence(t *testing.T) {
	rc := validRejectionCase()
	reference := Outcome{
		Class:      ClassSemanticError,
		Authority:  "promql/reference-differential",
		Comparator: "compat-promql",
		HTTPStatus: unprocessableEntityStatus,
		Payload:    "invalid parameter to histogram_quantile: phi must be in [0,1]",
	}
	system := Outcome{
		Class:      ClassSemanticError,
		Authority:  "promql/rejection-parity",
		Comparator: "rejection-parity",
		HTTPStatus: http.StatusBadRequest,
		Payload:    "promql: invalid parameter to histogram_quantile",
	}

	agreement, err := CertifyRejectionAgreement(rc, reference, system)
	if err != nil {
		t.Fatalf("CertifyRejectionAgreement() unexpected error: %v", err)
	}

	// The acceptance criterion requires BOTH the classification authority
	// AND the original diagnostic payload to survive certification.
	if agreement.Reference.Authority != reference.Authority || agreement.System.Authority != system.Authority {
		t.Fatalf("agreement lost classification authority: got reference=%s system=%s",
			agreement.Reference.Authority, agreement.System.Authority)
	}
	if agreement.Reference.Payload != reference.Payload || agreement.System.Payload != system.Payload {
		t.Fatalf("agreement lost the original diagnostic payload: got reference=%q system=%q",
			agreement.Reference.Payload, agreement.System.Payload)
	}
	if agreement.Reference.HTTPStatus != reference.HTTPStatus || agreement.System.HTTPStatus != system.HTTPStatus {
		t.Fatalf("agreement lost the original HTTP status: got reference=%d system=%d",
			agreement.Reference.HTTPStatus, agreement.System.HTTPStatus)
	}
	if agreement.Case != rc {
		t.Fatalf("agreement.Case = %+v, want %+v", agreement.Case, rc)
	}
}

func TestCertifyRejectionAgreementRejectsExpectedClassValue(t *testing.T) {
	rc := validRejectionCase()
	rc.ExpectedClass = ClassValue
	_, err := CertifyRejectionAgreement(rc, Outcome{Class: ClassValue}, Outcome{Class: ClassValue})
	if err == nil {
		t.Fatal("a rejection case expecting ClassValue must fail to certify")
	}
}

// TestCertifyRejectionAgreementRejectsRowEvidenceAlone is the direct test
// for the acceptance criterion: golden/pinned rows alone must never be
// sufficient to certify rejection or protocol equivalence. FromRowEvidence
// is the ONLY constructor a "matching rows" verifier can call, and it can
// only ever produce ClassValue — so even a perfectly matching pair of
// row-shaped results can never certify a rejection agreement.
func TestCertifyRejectionAgreementRejectsRowEvidenceAlone(t *testing.T) {
	rc := validRejectionCase()
	referenceRows := FromRowEvidence("spec-fixture-roundtrip")
	systemRows := FromRowEvidence("spec-fixture-roundtrip")

	if referenceRows.Class != ClassValue || systemRows.Class != ClassValue {
		t.Fatalf("test setup: FromRowEvidence must always produce ClassValue, got reference=%q system=%q",
			referenceRows.Class, systemRows.Class)
	}

	_, err := CertifyRejectionAgreement(rc, referenceRows, systemRows)
	if err == nil {
		t.Fatal("CertifyRejectionAgreement certified a rejection from row evidence alone")
	}
}

// TestCertifyRejectionAgreementRejectsUnrelatedBothErrorCase is the direct
// test for the Problem section's central example and the first acceptance
// criterion: both sides erroring for UNRELATED reasons must be classified
// as non-agreement, not silently certified. Here the reference backend
// genuinely, deliberately rejects the query (ClassSemanticError) while
// cerberus separately hit an unrelated transport-level fault
// (ClassProtocolError) — two different bugs that both happen to "error".
func TestCertifyRejectionAgreementRejectsUnrelatedBothErrorCase(t *testing.T) {
	rc := validRejectionCase()
	reference := Outcome{Class: ClassSemanticError, Authority: "promql/reference-differential", Payload: "genuine semantic rejection"}
	system := Outcome{Class: ClassProtocolError, Authority: "promql/http-status", HTTPStatus: http.StatusServiceUnavailable, Payload: "unrelated 503 from an overloaded backend"}

	_, err := CertifyRejectionAgreement(rc, reference, system)
	if err == nil {
		t.Fatal("CertifyRejectionAgreement certified agreement for two unrelated error classes")
	}
	if !strings.Contains(err.Error(), string(ClassProtocolError)) {
		t.Fatalf("error = %q, want it to name the mismatched class", err.Error())
	}
}

// TestPropertyFrameworkStillFailsClosedOnBothError pins that this
// package's addition does not weaken test/property/framework.go's existing
// fail-closed contract (test/property/framework_validator_test.go already
// pins it from that side; this proves the guarantee independently from the
// outcome-vocabulary package, since #3429 must never weaken it).
func TestPropertyFrameworkStillFailsClosedOnBothError(t *testing.T) {
	oracle := property.Outcome{Err: &staticErr{"oracle rejected generated shape"}}
	system := property.Outcome{Err: &staticErr{"unrelated system substrate failure"}}

	if diff := property.ValidateOutcomes(oracle, system); diff == "" {
		t.Fatal("property.ValidateOutcomes passed a both-error case — the fail-closed contract must hold")
	}
}

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }
