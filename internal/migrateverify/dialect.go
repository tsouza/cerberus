package migrateverify

import (
	"net/http"
	"net/url"
	"strconv"
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
// deliberately omitted: they steer the log-stream branch, which a metric query
// never reaches, and both backends default them. The log-stream lane, which does
// reach that branch, pins both explicitly — see lokiLogStreamDialect.
func (lokiDialect) Encode(expr string, p Params) url.Values {
	q := url.Values{}
	q.Set("query", expr)
	q.Set("start", formatInstantRFC3339(p.Start))
	q.Set("end", formatInstantRFC3339(p.End))
	q.Set("step", formatStep(p.Step))
	return q
}

func (lokiDialect) Decode(body []byte) (RangeResult, error) { return decodePromMatrix(body) }

// logStreamReplayLimit is the `limit` BOTH sides receive on a log-stream replay.
// It sits AT upstream Loki's max_entries_limit_per_query default, which is also
// cerberus's own maxLogQueryLimit: above it reference Loki 400s while cerberus
// silently clamps, so the two backends would answer different questions.
//
// It is pinned rather than exposed as a flag, because the limit decides how much
// of a result is truncated — i.e. how much the gate can judge — and an operator
// knob would silently change what "parity" means between two runs.
const logStreamReplayLimit = 5000

// logStreamReplayDirection is Loki's documented default and the direction its
// log-query path assumes: the newest entries first. Both sides receive it
// explicitly so a default change on either backend cannot quietly re-point the
// truncation boundary.
const logStreamReplayDirection = "backward"

// LokiLogStreamDialect speaks Loki's log-query API: the same
// /loki/api/v1/query_range endpoint the metric lane uses, with limit and
// direction pinned, and a streams envelope decoded into a flat entry list.
func LokiLogStreamDialect() KindDialect { return lokiLogStreamDialect{} }

type lokiLogStreamDialect struct{}

func (lokiLogStreamDialect) Kind() string { return KindLogStream }

func (lokiLogStreamDialect) Head() string { return HeadLoki }

func (lokiLogStreamDialect) Path(Query) string { return lokiQueryRangePath }

// Encode sends query/start/end/limit/direction and nothing else.
//
// `step` and `interval` are deliberately absent: cerberus's log path parses no
// `interval` at all, so sending one would encode a request only the reference
// backend understands — a guaranteed difference that says nothing about parity.
// start/end are RFC3339Nano for the same reason the metric lane uses it: an
// integer instant is disambiguated by digit count upstream and by magnitude in
// cerberus, and a window misread by orders of magnitude returns empty on both
// sides, which scores as agreement while comparing nothing.
func (lokiLogStreamDialect) Encode(q Query, p Params) url.Values {
	v := url.Values{}
	v.Set("query", q.Expr)
	v.Set("start", formatInstantRFC3339(p.Start))
	v.Set("end", formatInstantRFC3339(p.End))
	v.Set("limit", strconv.Itoa(logStreamReplayLimit))
	v.Set("direction", logStreamReplayDirection)
	return v
}

// Header sends none. In particular it does NOT send
// X-Loki-Response-Encoding-Flags: categorize-labels — under that flag the two
// backends emit structurally different third elements (reference Loki omits an
// empty structuredMetadata and can carry a parsed sub-object; cerberus always
// emits the key and never emits parsed), so requesting it would manufacture a
// difference on every entry. Without it both sides emit the byte-identical
// two-element [ts, line] value.
func (lokiLogStreamDialect) Header() http.Header { return nil }

func (lokiLogStreamDialect) Decode(body []byte) (any, error) { return decodeLogStreams(body) }

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
