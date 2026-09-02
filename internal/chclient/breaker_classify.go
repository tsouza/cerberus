package chclient

import (
	"errors"

	"github.com/ClickHouse/ch-go"
	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
)

// breaker_classify.go — WHOSE condition does a failed ClickHouse call report?
//
// The circuit breaker exists for exactly one purpose: stop hammering a backend
// that is failing to serve. Every error it counts must therefore be evidence
// about the SERVER. An error that is evidence about the STATEMENT — this one
// query asked for something the server declined — carries no such evidence, and
// counting it converts a handful of bad queries into a shed-everything outage.
//
// That is not hypothetical. During #2895 roughly 21 queries carried an
// unsatisfiable result-cache setting combination and came back as ClickHouse
// code 704 (QUERY_CACHE_USED_WITH_NONDETERMINISTIC_FUNCTIONS). The server was
// entirely healthy and answered every other query at the same instant, but the
// breaker counted the 704s, tripped, and 503'd everything behind it: 144 LogQL
// and 871 PromQL compat divergences from ~21 real failures, and a compat score
// that read like a total engine regression for 32 hours.
//
// # Why this file exists rather than a fourth hand-written arm
//
// Before #2900 breaker.record neutralised exceptions by hand-enumerated code,
// one incident at a time: 241 MEMORY_LIMIT_EXCEEDED, then 159 TIMEOUT_EXCEEDED,
// then 395 FUNCTION_THROW_IF_VALUE_IS_NON_ZERO. Each arm carried its own
// paragraph, and every paragraph made the SAME argument — "the server answered
// with a typed exception, which is positive proof it is alive and healthy".
// Code 704 is the fourth member of that class and it was missed only because
// nobody had had that incident yet. Adding a 704 arm would have set up the
// fifth. So the shared argument is promoted here into the rule itself, and the
// codes stop being a list of incidents.
//
// # The rule, and its source of truth
//
// The load-bearing observation is transport-level, not semantic: a decoded
// server exception can only exist if ClickHouse accepted the connection,
// received the statement, parsed and analysed it, and wrote a typed error
// packet back. A backend that does that is serving. Whatever else is wrong,
// it is not "down", and shedding unrelated traffic protects nothing — the
// rejection cost the server a parse, not a scan.
//
// So:
//
//   - No decoded server exception (dial refused, EOF, broken pipe, TLS
//     failure, read timeout, an unrecognised driver error) => the backend did
//     not answer => SERVER scope, counted. These are the shapes a genuine
//     outage actually wears.
//   - A decoded server exception => STATEMENT scope by default, neutral.
//   - Except for a curated minority of codes that, although decoded and
//     answered, describe a condition of the SERVER or its cluster that
//     outlives the statement — saturation, coordination loss, allocator
//     failure. Those are named in breakerServerHealthCodes below, each with
//     the reason it is there.
//
// ClickHouse publishes no machine-readable server/statement taxonomy: its
// ErrorCodes.cpp is a flat `M(code, NAME)` list with no category field, and
// neither driver exposes one. A curated set is therefore unavoidable, and
// pretending otherwise would be dishonest. What is avoidable is an unreasoned
// list of bare numbers, so the set below is keyed by ch-go's own named
// constants (chproto.Err*, generated from ErrorCodes.cpp) and every entry
// carries the sentence that justifies it. The set is the EXCEPTION to a
// principled default, not a replacement for one — it is the small side of the
// split, and a code that is not in it is still classified, not unhandled.
//
// # The default for an unknown code, and why it is the safe direction
//
// A code nobody has enumerated is treated as STATEMENT-scoped: neutral.
//
// The alternative — unknown means server-health, count it — preserves the
// breaker's protective reflex, but it is the rule that produced #2895 and it
// re-arms itself for every code ClickHouse adds. The two failure modes are not
// symmetric:
//
//   - Getting an unknown code wrong in the "count it" direction costs a total
//     outage from a handful of bad statements, with no protective benefit,
//     because the server demonstrably had capacity to answer. That is the
//     realised, observed cost.
//   - Getting an unknown code wrong in the "neutral" direction costs a delayed
//     trip. It cannot cost a MISSED trip, because a backend sick enough to
//     stop serving stops producing decoded exceptions: it refuses the dial,
//     drops the connection, or times out the read, and every one of those
//     paths is counted under the default above. The residual exposure is the
//     narrow band where a server is simultaneously healthy enough to parse SQL
//     and answer with a typed code, yet unhealthy enough that cerberus should
//     shed load — and that band is precisely what breakerServerHealthCodes
//     enumerates, extendable on evidence.
//
// New ClickHouse releases add statement-semantics codes constantly and
// server-condition codes rarely; the default is aimed at the common case, and
// the rare case has a named home. TestBreakerClassify_UnknownCodeIsStatementScoped
// pins the default so it cannot be flipped silently.

// breakerScope names whose condition a failed call reports. It is the verdict
// breaker.record acts on: only breakerScopeServerHealth advances the failure
// counter.
type breakerScope int

const (
	// breakerScopeServerHealth — the error is evidence that ClickHouse
	// cannot serve this replica's traffic: it never answered, or it
	// answered with a code naming a server/cluster condition that outlives
	// the statement. Counted toward the breaker.
	breakerScopeServerHealth breakerScope = iota

	// breakerScopeStatement — ClickHouse received, parsed and answered THIS
	// statement with a typed exception. Positive proof the backend is alive;
	// the next statement may well succeed. Never counted.
	breakerScopeStatement

	// breakerScopeClient — the call failed without ever becoming a question
	// ClickHouse was asked (a local pool acquire timed out). Says nothing
	// about the backend either way. Never counted.
	breakerScopeClient
)

// String renders the scope for logs and the trip metric's cause attribute.
// The vocabulary is stable: dashboards and runbooks key on these strings.
func (s breakerScope) String() string {
	switch s {
	case breakerScopeServerHealth:
		return "server-health"
	case breakerScopeStatement:
		return "statement"
	case breakerScopeClient:
		return "client"
	default:
		return "unknown"
	}
}

// chCodeNoServerAnswer is the sentinel [breakerOutcome.Code] value meaning the
// error carried no decoded ClickHouse exception at all — the server never got
// far enough to name a code. ClickHouse reserves 0 for OK, so it can never
// collide with a real exception code.
const chCodeNoServerAnswer chproto.Error = 0

// breakerOutcome is the classification of a single failed call: the scope that
// decides whether it counts, plus the ClickHouse code that produced the
// verdict so a trip can say WHY it tripped.
type breakerOutcome struct {
	// Scope is the verdict. Only breakerScopeServerHealth counts.
	Scope breakerScope
	// Code is the ClickHouse error code the server answered with, or
	// chCodeNoServerAnswer when the error carried no decoded exception.
	Code chproto.Error
}

// Trip causes: the reason a counted failure was counted. They ride the trip
// counter as the `cause` attribute and the trip log as a field, and they are
// the two questions a triager asks first. The vocabulary is stable —
// dashboards and runbooks key on these strings.
const (
	// tripCauseNoServerAnswer — the backend never answered: a refused dial, a
	// dropped connection, a read that timed out, an unrecognised driver
	// failure. The backend is down or unreachable.
	tripCauseNoServerAnswer = "no-server-answer"
	// tripCauseServerCondition — the backend answered, but with a code naming
	// a server or cluster condition (saturation, coordination loss, allocator
	// failure). The backend is up and cannot serve.
	tripCauseServerCondition = "server-condition"
)

// allTripCauses is the closed vocabulary, used to zero-initialise the trip
// counter's streams so a healthy replica renders a flat 0 rather than "No
// data" for every cause. Keep it in sync with [breakerOutcome.tripCause] —
// TestBreakerMetrics_TripCauseVocabularyIsClosed pins that it is.
var allTripCauses = []string{tripCauseNoServerAnswer, tripCauseServerCondition}

// tripCause renders the reason this counted failure is counted.
//
// A trip can never be caused by bad statements, because statement-scoped
// rejections are not counted at all — that is the whole point of this file —
// but the trip log additionally names how many were neutralised alongside, so
// a triager is not left guessing whether the two are related.
func (o breakerOutcome) tripCause() string {
	if o.Code == chCodeNoServerAnswer {
		return tripCauseNoServerAnswer
	}
	return tripCauseServerCondition
}

// codeName renders the ClickHouse code's symbolic name (UNKNOWN_TABLE,
// SERVER_OVERLOADED, …) for the trip log, using ch-go's generated
// code-to-name table. An unrecognised code stringifies to its numeric form
// rather than an empty label. Returns "" when no server exception was decoded.
func (o breakerOutcome) codeName() string {
	if o.Code == chCodeNoServerAnswer {
		return ""
	}
	return o.Code.String()
}

// breakerServerHealthCodes is the curated minority: ClickHouse error codes
// that arrive as a decoded server exception — so the server DID answer — yet
// report a condition of the server or its cluster rather than of the
// statement. A code in this set is counted toward the breaker; every other
// decoded exception is statement-scoped and neutral (see the file header for
// why that default is the safe direction).
//
// Keyed by ch-go's own generated constants so no bare number appears here and
// each entry names itself. The value is the sentence that earns the entry its
// place: a code with no defensible reason does not belong in the set.
//
// Membership criterion, applied to every entry: retrying a DIFFERENT,
// well-formed statement against this server would fail the same way, because
// the condition belongs to the server, not to the SQL. That is exactly the
// situation the breaker was built to shed load for.
var breakerServerHealthCodes = map[chproto.Error]string{
	chproto.ErrCannotAllocateMemory: "the server's own allocator failed; the host is out of memory and the next " +
		"statement will fail identically",
	chproto.ErrDNSError: "the server cannot resolve a name it needs (cluster peer, external source); a property " +
		"of the deployment, not of the SQL",
	chproto.ErrTooManySimultaneousQueries: "the server is at its concurrency ceiling and is shedding load; backing " +
		"off until it drains is precisely what a breaker is for",
	chproto.ErrNoFreeConnection: "the server exhausted its own outbound connection pool to a remote shard; a " +
		"server-side capacity limit no statement can dodge",
	chproto.ErrSocketTimeout: "a socket the SERVER owns timed out mid-answer; the backend could not complete the " +
		"exchange it started",
	chproto.ErrNetworkError: "the server failed talking to a peer replica or shard; the cluster, not the query, " +
		"is broken",
	chproto.ErrAborted: "the server aborted the work, characteristically while shutting down; it is on its way " +
		"out of service",
	chproto.ErrNoAvailableReplica: "a distributed read found no live replica to serve it; the cluster cannot " +
		"answer any query for that data",
	chproto.ErrAllConnectionTriesFailed: "the server could not reach a shard after exhausting its retries; the " +
		"cluster topology is degraded",
	chproto.ErrAllReplicasLost: "every replica for the data is gone; the condition is total and statement-" +
		"independent",
	chproto.ErrSystemError: "a syscall-level failure inside the server; the process itself is in trouble",
	chproto.ErrCannotScheduleTask: "the server's thread pool could not accept the work; it is saturated and the " +
		"next statement will be too",
	chproto.ErrAuthenticationFailed: "the backend refuses this replica's credentials; every subsequent statement " +
		"fails identically until the deployment is fixed, so failing fast is correct",
	chproto.ErrTableIsBeingRestarted: "the table is temporarily out of service server-side; no statement can read " +
		"it until the restart completes",
	chproto.ErrServerOverloaded: "the server is explicitly signalling overload and asking callers to back off; " +
		"ignoring it would be the opposite of what a breaker is for",
	chproto.ErrKeeperException: "the coordination layer (Keeper/ZooKeeper) is unavailable; replicated tables " +
		"cannot be served regardless of the statement",
}

// classifyBreakerOutcome derives the [breakerOutcome] for a non-nil error
// observed by the breaker. err is the RAW error the driver returned — the
// breaker classifies before chclient's own wrapping runs — so both dials are
// normalised here rather than at the call sites.
//
// Ordering is deliberate:
//
//  1. A local pool acquire timeout never became a question ClickHouse was
//     asked, so it is decided before anything tries to read a server code.
//  2. Everything else is reduced to "did the server answer with a typed
//     exception, and if so which code", which decides the rest.
//
// Cancellation belongs to record rather than to this function: a caller
// walking away has its own half-open probe semantics (release the probe slot
// rather than resolve it either way). See record's cancellation arm.
func classifyBreakerOutcome(err error) breakerOutcome {
	// A pool acquire timeout means every connection in the LOCAL pool was
	// busy and the acquire blocked past DialTimeout without one freeing up.
	// That is a cerberus-side sizing signal — this replica is asking for more
	// concurrency than MaxOpenConns allows — and says nothing about whether
	// ClickHouse is alive. The solver's sharded fan-out makes it reachable
	// against a perfectly healthy backend, so counting it would let a
	// too-small pool 503 traffic CH was ready to serve. The fix for a
	// recurring acquire timeout is to raise MaxOpenConns, not to fail CH
	// health.
	if errors.Is(err, clickhouse.ErrAcquireConnTimeout) {
		return breakerOutcome{Scope: breakerScopeClient, Code: chCodeNoServerAnswer}
	}

	// The columnar sample-budget sentinel is cerberus's OWN stop signal: the
	// decoder latched a *TooManySamplesError and returned errBudgetExceeded
	// purely to unwind ch-go's pool.Do. ClickHouse was mid-answer and
	// perfectly healthy; this process chose to stop reading. Classified here
	// rather than filtered at the columnar call site so every caller of record
	// gets one verdict from one place.
	if isBudgetErr(err) {
		return breakerOutcome{Scope: breakerScopeClient, Code: chCodeNoServerAnswer}
	}

	code, answered := serverExceptionCode(err)
	if !answered {
		// The backend never named a code: a refused dial, a dropped
		// connection, a read that timed out, a driver failure. These are the
		// shapes a real outage wears, and they are the reason the breaker
		// exists.
		return breakerOutcome{Scope: breakerScopeServerHealth, Code: chCodeNoServerAnswer}
	}
	if _, serverScoped := breakerServerHealthCodes[code]; serverScoped {
		return breakerOutcome{Scope: breakerScopeServerHealth, Code: code}
	}
	// The server parsed this statement and answered it with a typed code.
	// Whatever it objected to belongs to the statement, and the backend
	// proved it is serving in the act of saying so.
	return breakerOutcome{Scope: breakerScopeStatement, Code: code}
}

// serverExceptionCode extracts the ClickHouse error code err carries, and
// reports whether the server answered with a typed exception at all.
//
// It accepts every shape the same rejection wears in this package:
//
//   - the row dial's *clickhouse.Exception, raw off clickhouse-go/v2;
//   - the columnar dial's ch-go *proto.Exception, which carries the identical
//     wire code under a different Go type (the breaker sees the RAW ch-go
//     error there — see columnarQuery — so normalising only at the row path
//     would leave the columnar dial classifying every server rejection as an
//     outage);
//   - chclient's own already-wrapped forms, *MemoryLimitError and
//     *QueryTimeoutError, whose Cause is normally the exception but is
//     permitted to be nil (the constructors accept a nil Cause), leaving the
//     sentinel as the only evidence. Those two sentinels are by construction
//     codes 241 and 159 — per-query resource caps the server enforced — so
//     they resolve to those codes rather than to "no answer".
func serverExceptionCode(err error) (chproto.Error, bool) {
	var ex *clickhouse.Exception
	if errors.As(err, &ex) {
		return chproto.Error(ex.Code), true
	}
	if chEx, ok := ch.AsException(err); ok {
		return chEx.Code, true
	}
	// Sentinel-only fallbacks. errors.Is rather than errors.As because these
	// sentinels are what survives when the wrapper was built without a Cause.
	if errors.Is(err, ErrMemoryLimitExceeded) {
		return chproto.ErrMemoryLimitExceeded, true
	}
	if errors.Is(err, ErrQueryTimeout) {
		return chproto.ErrTimeoutExceeded, true
	}
	return chCodeNoServerAnswer, false
}
