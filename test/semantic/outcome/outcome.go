// Package outcome models the semantic outcome vocabulary cerberus issue
// #3429 asks for: what a system under test (cerberus or a reference
// backend) actually produced for one query, kept structurally separate
// from whether the TEST HARNESS itself managed to run the case at all
// (see ExecutionFailure).
//
// # Why this package exists
//
// Three verification layers already classify a response, each in its own
// vocabulary: test/property/framework.go's Outcome carries rows plus a bare
// Err and deliberately fails any comparison where both sides errored, for
// unrelated reasons, in any two ways (see ValidateOutcomes there — this
// package never weakens that; TestValidateOutcomesStillFailsClosedOnBothError
// pins it from this side too). Loki's status_parity.go compares raw HTTP
// status ints. Tempo's grpc_diff.go compares raw gRPC codes.Code values.
// test/rejection-parity/catalogue.go curates a hand-classified "rejection /
// internal / divergence" verdict per error-construction site. None of the
// four are wrong for their own layer, and none of the four is replaced
// here (issue #3429's own Non-goals) — this package gives their outputs a
// SHARED vocabulary so a caller spanning more than one of them (a
// downstream evidence resolver, in particular) can compare outcomes without
// re-deriving what "SyntaxError" or "SemanticError" means from scratch.
//
// # Vocabulary
//
// A Class is one of five values a system under test can produce for a
// query: Value (a real answer), SyntaxError (the query itself is
// malformed), SemanticError (the query parses but is semantically invalid
// for this data/schema), Unsupported (a real, currently-unimplemented
// feature gap), or ProtocolError (a transport/wire-level failure — an HTTP
// status class or a gRPC status code — never a query-semantic one).
//
// Deciding which Class applies is deliberately NOT this package's job.
// Every constructor in adapters.go takes an already-computed verdict from
// an existing, head/endpoint-specific classifier (an HTTP status int Loki's
// own status_parity.go observed, a gRPC codes.Code Tempo's own grpc_diff.go
// observed, or a curated catalogue.Entry.Class rejection-parity's own
// per-head curation assigned) and NORMALIZES it into this vocabulary. That
// is what "head/endpoint-specific classification authority" means in the
// issue text: PromQL's grammar, LogQL's grammar and TraceQL's grammar each
// decide what counts as a syntax error on their own terms, and this
// package never second-guesses that decision with a cross-head
// string-matching rule of its own (issue #3429's Non-goals: no blanket
// error-string equality).
package outcome

// Class is the semantic outcome vocabulary: what a system under test
// actually returned for one query, as classified by that head/endpoint's
// own authority. It intentionally excludes anything about whether the test
// harness itself succeeded in running the case — see ExecutionFailure.
type Class string

const (
	// ClassValue is a real answer: rows, a scalar, a trace set — whatever
	// shape the endpoint returns on success.
	ClassValue Class = "value"

	// ClassSyntaxError means the query itself is malformed per the head's
	// own grammar — a parse-time rejection.
	ClassSyntaxError Class = "syntax_error"

	// ClassSemanticError means the query parses but is semantically
	// invalid for this data/schema — a lowering- or evaluation-time
	// rejection that is not a grammar failure.
	ClassSemanticError Class = "semantic_error"

	// ClassUnsupported means the system under test correctly reported "not
	// implemented" for a real, currently-missing feature — never a stand-in
	// for a harness that could not run the case at all (that is always an
	// ExecutionFailure, never this).
	ClassUnsupported Class = "unsupported"

	// ClassProtocolError means the failure is transport/wire-level — an
	// HTTP status class or a gRPC status code — not a verdict about the
	// query's semantics. A 400 Bad Request and a genuine query-semantic
	// rejection are never the same Class even when they read similarly in
	// a log line: see FromHTTPStatus / FromGRPCCode versus
	// FromRejectionParityEntry.
	ClassProtocolError Class = "protocol_error"
)

// Authority names the head/endpoint-specific classifier that decided an
// Outcome's Class — for example "promql/rejection-parity",
// "loki/http-status", "tempo-grpc/status-code". It exists so a reviewer
// tracing a classification back to its source never has to guess which
// verifier produced it; see Outcome.Authority.
type Authority string

// Outcome is one classified response from a system under test for one
// query. Every field the issue's acceptance criteria requires retained
// (original payload, HTTP status / gRPC code, comparator identity) is a
// named field here — never folded away once classified.
type Outcome struct {
	// Class is the normalized verdict.
	Class Class

	// Authority is which head/endpoint-specific classifier decided Class.
	// Never empty: an Outcome with no stated authority cannot be
	// distinguished from one nobody actually classified.
	Authority Authority

	// Comparator is the identity of the verifier/test that produced this
	// Outcome — e.g. "loki-status-parity", "tempo-grpc-diff",
	// "rejection-parity", "spec-fixture-roundtrip". Retained so a reviewer
	// can trace a classification back to the exact mechanism that ran,
	// independent of Authority (which names the classification RULE, not
	// the test that invoked it).
	Comparator string

	// HTTPStatus is the observed HTTP status code, when this Outcome came
	// from an HTTP-transport verifier. Zero when not applicable — never
	// populated alongside GRPCCode; a response travels exactly one wire
	// transport.
	HTTPStatus int

	// GRPCCode is the observed gRPC status code, when this Outcome came
	// from a gRPC-transport verifier. Empty string when not applicable —
	// stored as the code's name (codes.Code.String()) rather than the
	// google.golang.org/grpc/codes type itself, so this package's public
	// API carries no gRPC dependency for callers that never touch the
	// gRPC transport.
	GRPCCode string

	// Payload is the raw diagnostic the classifier observed: a response
	// body snippet, an error message, or equivalent — whatever the
	// original verifier already had in hand. Never discarded once
	// classified; a reviewer must be able to trace a Class back to the
	// actual response that produced it.
	Payload string
}

// IsError reports whether the Outcome is anything other than a real
// answer. Convenience for callers that only need "did this side error",
// without caring which of the four error Classes applies.
func (o Outcome) IsError() bool {
	return o.Class != ClassValue
}
