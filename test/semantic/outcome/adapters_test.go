package outcome

import (
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	rejectionparity "github.com/tsouza/cerberus/test/rejection-parity"
)

func TestFromHTTPStatusClassifiesByStatusClassOnly(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   Class
	}{
		{name: "200 is a value", status: http.StatusOK, want: ClassValue},
		{name: "204 is a value", status: http.StatusNoContent, want: ClassValue},
		{name: "400 is a protocol error", status: http.StatusBadRequest, want: ClassProtocolError},
		{name: "404 is a protocol error", status: http.StatusNotFound, want: ClassProtocolError},
		{name: "500 is a protocol error", status: http.StatusInternalServerError, want: ClassProtocolError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FromHTTPStatus("loki-status-parity", "logql", tc.status, "body")
			if got.Class != tc.want {
				t.Fatalf("FromHTTPStatus(%d).Class = %q, want %q", tc.status, got.Class, tc.want)
			}
			if got.HTTPStatus != tc.status {
				t.Fatalf("FromHTTPStatus(%d).HTTPStatus = %d, want %d", tc.status, got.HTTPStatus, tc.status)
			}
			if got.GRPCCode != "" {
				t.Fatalf("FromHTTPStatus must never populate GRPCCode, got %q", got.GRPCCode)
			}
			if got.Comparator != "loki-status-parity" {
				t.Fatalf("Comparator = %q, want it retained verbatim", got.Comparator)
			}
		})
	}
}

// TestFromHTTPStatusIgnoresBodyContent proves the classifier decides purely
// from the status code, never by sniffing the response body text — the
// issue's Non-goal against blanket error-string equality. A body that
// reads like a semantic rejection must not upgrade a 400 into anything but
// ClassProtocolError.
func TestFromHTTPStatusIgnoresBodyContent(t *testing.T) {
	got := FromHTTPStatus("loki-status-parity", "logql", http.StatusBadRequest, "invalid parameter to histogram_quantile: phi must be in [0,1]")
	if got.Class != ClassProtocolError {
		t.Fatalf("Class = %q, want %q regardless of body content", got.Class, ClassProtocolError)
	}
}

func TestFromGRPCCodeClassifiesByCodeOnly(t *testing.T) {
	tests := []struct {
		name string
		code codes.Code
		want Class
	}{
		{name: "OK is a value", code: codes.OK, want: ClassValue},
		{name: "InvalidArgument is a protocol error", code: codes.InvalidArgument, want: ClassProtocolError},
		{name: "Unimplemented is a protocol error", code: codes.Unimplemented, want: ClassProtocolError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FromGRPCCode("tempo-grpc-diff", "traceql", tc.code, "detail")
			if got.Class != tc.want {
				t.Fatalf("FromGRPCCode(%s).Class = %q, want %q", tc.code, got.Class, tc.want)
			}
			if got.GRPCCode != tc.code.String() {
				t.Fatalf("GRPCCode = %q, want %q", got.GRPCCode, tc.code.String())
			}
			if got.HTTPStatus != 0 {
				t.Fatalf("FromGRPCCode must never populate HTTPStatus, got %d", got.HTTPStatus)
			}
		})
	}
}

func TestFromRejectionParityEntry(t *testing.T) {
	rejection, err := FromRejectionParityEntry("rejection-parity", "promql", rejectionparity.ClassRejection)
	if err != nil {
		t.Fatalf("FromRejectionParityEntry(rejection) unexpected error: %v", err)
	}
	if rejection.Class != ClassSemanticError {
		t.Fatalf("rejection class = %q, want %q", rejection.Class, ClassSemanticError)
	}

	divergence, err := FromRejectionParityEntry("rejection-parity", "promql", rejectionparity.ClassDivergence)
	if err != nil {
		t.Fatalf("FromRejectionParityEntry(divergence) unexpected error: %v", err)
	}
	if divergence.Class != ClassUnsupported {
		t.Fatalf("divergence class = %q, want %q", divergence.Class, ClassUnsupported)
	}

	if _, err := FromRejectionParityEntry("rejection-parity", "promql", rejectionparity.ClassInternal); err == nil {
		t.Fatal("FromRejectionParityEntry(internal) must fail — an internal entry is not wire-observable")
	}

	if _, err := FromRejectionParityEntry("rejection-parity", "promql", "bogus"); err == nil {
		t.Fatal("FromRejectionParityEntry(unknown class) must fail")
	}
}

// TestHTTPStatusAndSemanticRejectionNeverConflate is the direct test for
// the acceptance criterion: HTTP status code, gRPC status code, and a
// query-semantic failure must never be conflated, even when they look
// similar (both are "a 400-shaped rejection" in a loose reading).
func TestHTTPStatusAndSemanticRejectionNeverConflate(t *testing.T) {
	protocolOutcome := FromHTTPStatus("loki-status-parity", "promql", http.StatusBadRequest, "bad request")
	semanticOutcome, err := FromRejectionParityEntry("rejection-parity", "promql", rejectionparity.ClassRejection)
	if err != nil {
		t.Fatalf("FromRejectionParityEntry unexpected error: %v", err)
	}

	if protocolOutcome.Class == semanticOutcome.Class {
		t.Fatalf("a 400 Bad Request and a genuine query-semantic rejection must not share a Class, both got %q",
			protocolOutcome.Class)
	}
	if protocolOutcome.Authority == semanticOutcome.Authority {
		t.Fatalf("a 400 Bad Request and a genuine query-semantic rejection must not share an Authority, both got %q",
			protocolOutcome.Authority)
	}
	if !strings.HasPrefix(string(protocolOutcome.Authority), "promql/http-status") {
		t.Fatalf("protocol Authority = %q, want the http-status classifier named", protocolOutcome.Authority)
	}
	if !strings.HasPrefix(string(semanticOutcome.Authority), "promql/rejection-parity") {
		t.Fatalf("semantic Authority = %q, want the rejection-parity classifier named", semanticOutcome.Authority)
	}
}

func TestFromRowEvidenceIsAlwaysValue(t *testing.T) {
	for _, comparator := range []string{"spec-fixture-roundtrip", "property-comparator", "anything"} {
		got := FromRowEvidence(comparator)
		if got.Class != ClassValue {
			t.Fatalf("FromRowEvidence(%q).Class = %q, want %q", comparator, got.Class, ClassValue)
		}
	}
}
