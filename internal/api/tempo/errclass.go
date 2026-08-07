package tempo

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/tsouza/cerberus/internal/chclient"
)

// This file owns the ONE error classification the Tempo head has. Every
// entrypoint — the HTTP handlers in this package and the gRPC
// StreamingQuerier RPCs in internal/api/tempo/grpc — routes its pipeline
// errors through ClassifyErr and then renders the resulting ErrClass with
// its own transport encoder (ErrClass.HTTPStatus here; grpcCodeFor in the
// sibling grpc package).
//
// The split exists because the two transports have different vocabularies
// for the same fact, not different facts: HTTP separates "the query text is
// malformed" (400) from "the query parsed but cannot be evaluated" (422),
// while gRPC folds both into codes.InvalidArgument. Classifying once and
// encoding twice is what keeps them from drifting — before this file there
// were six independent sentinel ladders (four entrypoints plus the
// trace-by-ID and tag-metadata helpers) and they disagreed: the same
// cancelled query was a 502 over HTTP and codes.Canceled over gRPC, and the
// metrics RPCs collapsed every budget, timeout and cancellation onto
// codes.Internal.

// Engine stage prefixes. engine.Engine wraps each pipeline stage's error
// with these markers, so a stage that has no dedicated sentinel is still
// recoverable from the error text. Parse and lower carry real sentinels
// (ErrParseStage / ErrLowerStage) and are matched before these.
const (
	engineEmitStagePrefix    = "engine: emit:"
	engineExecuteStagePrefix = "engine: execute:"
)

// StatusClientClosedRequest is nginx's non-standard 499 "Client Closed
// Request". It is the status a request gets when the caller disconnects
// before the response is written, and it is deliberately outside the 5xx
// band: the error-reason taxonomy, dashboards and alerting read 5xx as
// "cerberus or ClickHouse is unhealthy", which a client walking away is
// not. Grafana's Tempo datasource cancels in-flight searches on every
// panel re-render, query edit and tab switch, so this is a hot path.
const StatusClientClosedRequest = 499

// ErrClass is the transport-neutral classification of a query-pipeline
// error. It names WHY the request failed; the per-transport encoders name
// how to say it on the wire.
type ErrClass int

const (
	// ErrClassInternal is a cerberus-side fault: an emit-stage failure, a
	// nil error handed to the classifier, or anything with no recognised
	// sentinel. The conservative default.
	ErrClassInternal ErrClass = iota
	// ErrClassParse is a query the endpoint cannot accept as written: the
	// parser rejected the text the client sent, or the text parsed into a
	// well-formed query of the wrong KIND for the endpoint it was sent to
	// (a search expression posted to a metrics route —
	// errNotMetricsPipeline). Upstream Tempo answers both with 400, and
	// both are fixed the same way: send a different query.
	ErrClassParse
	// ErrClassLower is a well-formed TraceQL query cerberus cannot evaluate:
	// a lowering rejection, or an unsupported operator combination. The
	// query is syntactically valid, so it is not a parse error, but no
	// amount of retrying will make it work.
	ErrClassLower
	// ErrClassResourceExhausted is a per-query budget doing its job: the
	// row-count sample budget (CERBERUS_QUERY_MAX_SAMPLES), the
	// wide-projection byte budget, or ClickHouse's own memory-limit abort
	// (code 241). The query asked for too much; the server is healthy.
	ErrClassResourceExhausted
	// ErrClassUnavailable is transient downstream saturation the client
	// should back off from: the chclient circuit breaker is OPEN, or a
	// wall-clock cap fired (CERBERUS_QUERY_TIMEOUT → ClickHouse
	// max_execution_time code 159, or the request context deadline).
	// ClickHouse is healthy when it aborts an over-long query — the
	// breaker treats code 159 as a success for the same reason.
	ErrClassUnavailable
	// ErrClassCanceled is the client walking away mid-query. Not a fault on
	// either side.
	ErrClassCanceled
	// ErrClassUpstream is an execute-stage failure: ClickHouse refused or
	// dropped the query for a reason with no dedicated sentinel.
	ErrClassUpstream
)

// ClassifyErr maps a Tempo pipeline error onto its ErrClass.
//
// Order is load-bearing. Cancellation is tested first because a cancelled
// query surfaces wrapped in the engine's `execute:` marker, which would
// otherwise classify it as an upstream transport fault — the exact bug that
// made an ordinary Grafana panel re-render look like a ClickHouse outage.
// The sentinel families follow, and the engine stage-prefix string matches
// come last as the fallback for stages with no sentinel of their own.
//
// A nil error classifies as ErrClassInternal: the callers are all on an
// error path, so being asked to classify nil means the caller's own control
// flow is wrong, and the conservative 500 / codes.Internal says so.
func ClassifyErr(err error) ErrClass {
	if err == nil {
		return ErrClassInternal
	}
	switch {
	case errors.Is(err, context.Canceled):
		return ErrClassCanceled
	case errors.Is(err, chclient.ErrCircuitOpen),
		errors.Is(err, chclient.ErrQueryTimeout),
		errors.Is(err, context.DeadlineExceeded):
		return ErrClassUnavailable
	case errors.Is(err, chclient.ErrTooManySamples),
		errors.Is(err, chclient.ErrDrainBytesExceeded),
		errors.Is(err, chclient.ErrMemoryLimitExceeded):
		return ErrClassResourceExhausted
	case errors.Is(err, errParseStage),
		errors.Is(err, errNotMetricsPipeline):
		return ErrClassParse
	case errors.Is(err, errLowerStage):
		return ErrClassLower
	case strings.Contains(err.Error(), engineEmitStagePrefix):
		return ErrClassInternal
	case strings.Contains(err.Error(), engineExecuteStagePrefix):
		return ErrClassUpstream
	}
	return ErrClassInternal
}

// HTTPStatus renders an ErrClass as the status the Tempo HTTP handlers
// respond with.
func (c ErrClass) HTTPStatus() int {
	switch c {
	case ErrClassParse:
		return http.StatusBadRequest
	case ErrClassLower, ErrClassResourceExhausted:
		return http.StatusUnprocessableEntity
	case ErrClassUnavailable:
		return http.StatusServiceUnavailable
	case ErrClassCanceled:
		return StatusClientClosedRequest
	case ErrClassUpstream:
		return http.StatusBadGateway
	case ErrClassInternal:
		return http.StatusInternalServerError
	}
	return http.StatusInternalServerError
}

// httpErrStatus is the shorthand every HTTP handler in this package uses on
// its error path.
func httpErrStatus(err error) int { return ClassifyErr(err).HTTPStatus() }

// tagsErrStatus is httpErrStatus for the /search/tags and
// /search/tag/.../values metadata endpoints, which reach ClickHouse through
// h.Client directly rather than through engine.Engine. Their errors
// therefore never carry an `engine: execute:` marker, so an otherwise
// unclassified failure here IS the upstream transport fault the marker
// would have named — 502, not the 500 an engine-routed handler defaults to.
// Every classified sentinel (budgets, breaker, timeout, cancellation)
// resolves identically to the rest of the head.
func tagsErrStatus(err error) int {
	if c := ClassifyErr(err); c != ErrClassInternal {
		return c.HTTPStatus()
	}
	return http.StatusBadGateway
}
