package outcome

import "fmt"

// FailureKind is the closed vocabulary of ways the TEST HARNESS itself can
// fail to produce an Outcome at all — never a verdict about what the
// system under test returned.
type FailureKind string

const (
	// FailureOracle means the independent reference evaluator (not the
	// system under test) could not produce an answer — e.g. an oracle bug
	// or an unimplementable generated shape. See
	// test/property/framework.go's "oracle error" branch, which this
	// mirrors as a distinct, typed failure rather than folding it into an
	// Outcome.
	FailureOracle FailureKind = "oracle_failure"

	// FailureTimeout means the case did not complete within its budget.
	FailureTimeout FailureKind = "timeout"

	// FailureCrash means the process under test (or the harness driving
	// it) terminated abnormally mid-case.
	FailureCrash FailureKind = "crash"

	// FailureTransport means the request never completed at all — a
	// dial/network failure distinct from a completed request that came
	// back with a non-2xx status or a non-OK gRPC code (those are
	// ClassProtocolError Outcomes, not this).
	FailureTransport FailureKind = "transport_failure"

	// FailureSubstrate means the underlying data substrate (chDB, a
	// ClickHouse container, a seeded fixture) was not in a state the case
	// could run against.
	FailureSubstrate FailureKind = "substrate_failure"

	// FailureHarnessInput means the case itself is not runnable — a
	// generator produced an invalid shape, a fixture is malformed, a
	// required field is missing. This is the harness choking on its own
	// input, never the system under test saying "not implemented"; see the
	// package doc and ClassUnsupported for why the two must never be
	// conflated.
	FailureHarnessInput FailureKind = "harness_input"
)

// ExecutionFailure is a failure of the TEST HARNESS, never a semantic
// outcome the system under test produced. It carries no Class from the
// outcome vocabulary because it is not a response at all — there was
// nothing for a head/endpoint classifier to classify. A function that
// needs to report one MUST NOT return an Outcome (of any Class, including
// ClassUnsupported) to represent it; the two are different Go types
// precisely so a harness failure cannot be silently laundered into a
// semantic verdict by a caller that only checks Outcome.Class.
type ExecutionFailure struct {
	// Kind is which way the harness failed.
	Kind FailureKind

	// Detail is a human-readable description — the underlying error text,
	// the timeout budget exceeded, the malformed field name, etc.
	Detail string
}

// Error implements the error interface so an ExecutionFailure can be
// returned and wrapped like any other Go error, while remaining a distinct
// type from Outcome at every call site that type-switches or type-asserts
// on it.
func (f ExecutionFailure) Error() string {
	return fmt.Sprintf("%s: %s", f.Kind, f.Detail)
}
