package migrateverify

import (
	"net/url"
	"time"
)

// Head tokens name the three parity lanes. They are the project's canonical head
// names (the same tokens CERBERUS_ENABLED_HEADS accepts), so an operator
// configures a Loki backend and reads head="loki" in the report without a second
// vocabulary. internal/config cannot be imported here (the architecture gate
// keeps this package off the runtime layers), so the equality with config.Head*
// is pinned by a test in cmd/cerberus, which imports both.
const (
	HeadProm  = "prom"
	HeadLoki  = "loki"
	HeadTempo = "tempo"
)

// Range-query endpoint paths, appended to a backend base URL. Each is the path
// its head's upstream serves and cerberus mirrors, so the same dialect drives
// both the reference and the cerberus side of a lane.
const (
	promQueryRangePath  = "/api/v1/query_range"
	lokiQueryRangePath  = "/loki/api/v1/query_range"
	tempoQueryRangePath = "/api/metrics/query_range"
)

// scopeOrgIDHeader is the tenant header multi-tenant Loki and Tempo require when
// auth_enabled is on. Without it every reference request 401s and the whole lane
// records as VerdictError, indistinguishable from a real parity failure.
const scopeOrgIDHeader = "X-Scope-OrgID"

// Dialect is one head's query_range wire contract: where the endpoint lives, how
// a query is encoded into the query string, and how a 200 body decodes into the
// shared matrix shape. It carries DATA about a head, never transport policy —
// auth, the response-size cap, credential redaction and the non-200 rule are
// written exactly once, in HTTPBackend.QueryRange, and apply to every head.
type Dialect interface {
	// Name is the head token this dialect speaks for.
	Name() string
	// Path is the range-query endpoint appended to the backend base URL.
	Path() string
	// Encode builds the query-string parameters for expr over p.
	Encode(expr string, p Params) url.Values
	// Decode parses a 200 body into ResultType + Series. The caller stamps
	// Status; a decode error means the body was not a usable response at all.
	Decode(body []byte) (RangeResult, error)
}

// PromDialect speaks the Prometheus range API: /api/v1/query_range with
// Unix-seconds start/end and the standard matrix envelope.
func PromDialect() Dialect { return promDialect{} }

// LokiDialect speaks Loki's metric range API. Loki's `matrix` result is
// literally []prometheus/common/model.SampleStream — the same Go type Prometheus
// marshals — so it decodes with the Prometheus matrix decoder; only the endpoint
// path and the instant encoding differ.
func LokiDialect() Dialect { return lokiDialect{} }

// TempoDialect speaks Tempo's TraceQL metrics range API: /api/metrics/query_range
// with the query under `q` and a series-of-samples envelope in millisecond units.
func TempoDialect() Dialect { return tempoDialect{} }

type promDialect struct{}

func (promDialect) Name() string { return HeadProm }

func (promDialect) Path() string { return promQueryRangePath }

func (promDialect) Encode(expr string, p Params) url.Values {
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", formatTimestamp(p.Start))
	q.Set("end", formatTimestamp(p.End))
	q.Set("step", formatStep(p.Step))
	return q
}

func (promDialect) Decode(body []byte) (RangeResult, error) { return decodePromMatrix(body) }

type lokiDialect struct{}

func (lokiDialect) Name() string { return HeadLoki }

func (lokiDialect) Path() string { return lokiQueryRangePath }

// Encode sends only query/start/end/step. `limit` and `direction` are
// deliberately omitted: they steer the log-stream branch, which this lane never
// reaches (log-stream queries are routed out of scope before a request is
// issued), and both backends default them.
func (lokiDialect) Encode(expr string, p Params) url.Values {
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", formatInstantRFC3339(p.Start))
	q.Set("end", formatInstantRFC3339(p.End))
	q.Set("step", formatStep(p.Step))
	return q
}

func (lokiDialect) Decode(body []byte) (RangeResult, error) { return decodePromMatrix(body) }

// formatInstantRFC3339 renders an instant for the Loki and Tempo lanes.
//
// Both upstream Loki and upstream Tempo disambiguate an INTEGER start/end by
// digit count (<= 10 digits means seconds, otherwise nanoseconds), while
// cerberus's own heads use a magnitude heuristic with an extra millisecond
// branch. The two rules agree only on 10-digit values, and a window misread by
// six to nine orders of magnitude produces an EMPTY result on both sides — which
// Compare scores as a MATCH, i.e. a green gate that compared nothing. RFC3339Nano
// is parsed identically by all four parsers with no heuristic at all, so the
// whole failure class is removed rather than avoided.
func formatInstantRFC3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
