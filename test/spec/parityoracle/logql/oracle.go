//go:build agpl_oracle

// Package logql evaluates a fixture's query on the REAL upstream Loki
// query engine, so a TXTAR fixture's answer can be checked against
// something other than cerberus's own output.
//
// It is the LogQL sibling of test/spec/parityoracle/promql and exists
// for the same reason: `expected_rows:` is regenerated FROM cerberus's
// own output, so a wrong lowering gets its wrong answer re-pinned and
// passes forever. The reference answer here is computed live on every
// run and stored nowhere, so there is no artefact for `just
// update-golden` to overwrite.
//
// # The one rule this package exists to enforce
//
// It imports NOTHING from internal/{promql,logql,traceql,chplan,chsql,
// optimizer,schema}. An oracle that shares code with the system under
// test cannot disagree with it about the thing they share, so any such
// import silently converts this package from an oracle into a mirror —
// and a mirror always agrees. test/regression's parity contract test
// enforces the rule mechanically with `go list -deps -test`, under this
// package's own build configuration, because a rule this easy to break
// by accident and this invisible when broken cannot be left to review.
//
// The same reasoning rules out reusing compatibility/loki/cmd/seed's
// row-to-stream conversion, which is otherwise exactly this translation:
// it imports internal/logql and internal/schema. Being equal by
// construction is right for the compat MIRROR, whose job is to put
// identical data into both backends, and disqualifying here.
//
// # Why this is behind the agpl_oracle build tag
//
// github.com/grafana/loki/v3/pkg/logql is AGPLv3 and cerberus ships
// under Apache-2.0 (invariant 14). What protects the binary is
// REACHABILITY from ./cmd/cerberus, which .github/scripts/agpl-clean.mjs
// measures; the `agpl_oracle` tag keeps this package out of every normal
// build, and the gate asserts the binary's dependency closure stays
// AGPL-free.
//
// # What the reference can and cannot see
//
// [logql.NewMockQuerier] is the upstream in-process Querier. Its
// processStream/processSeries helpers call the pipeline with
// `labels.EmptyLabels()` for structured metadata, so entries'
// StructuredMetadata is DISCARDED by the reference engine. That is a
// hard boundary rather than an inconvenience: a fixture whose seed
// populates LogAttributes (cerberus's structured-metadata carrier) or
// SeverityText would have the two engines answering over different data,
// and the caller must refuse such a fixture loudly rather than compare
// two different questions. [Stream] therefore models only what the
// reference can actually observe — stream labels, timestamp, line.
package logql

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/grafana/dskit/user"
	"github.com/grafana/loki/v3/pkg/logproto"
	"github.com/grafana/loki/v3/pkg/logql"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/grafana/loki/v3/pkg/util/validation"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
)

// mockQuerierShards is the shard count handed to [logql.NewMockQuerier].
// Zero means "do not shard": the fixture corpus asks unsharded queries,
// and sharding would change which streams a query observes for a reason
// that has nothing to do with the lowering under test.
const mockQuerierShards = 0

// entryLimit bounds the number of log entries the reference engine will
// return for a log query. It is deliberately far above any fixture's
// working set — the corpus seeds a handful of lines per stream — so a
// fixture can never pass or fail because of the limit rather than
// because of the query.
const entryLimit = 1_000_000

// referenceTenant is the org id injected into the evaluation context.
// Loki's engine resolves a tenant on every Exec and fails with "no org
// id" without one; the corpus is single-tenant, so the value only has to
// exist.
const referenceTenant = "cerberus-parity"

// maxSeries bounds how many series one evaluation may produce, and
// queryTimeout bounds how long it may take. Both are deliberately far
// beyond any fixture's working set, for the same reason as [entryLimit]:
// a limit that can bite is a second thing a fixture might be failing on.
const (
	maxSeries    = math.MaxInt32
	queryTimeout = time.Hour
)

// referenceLimits is the per-tenant limit set the reference engine runs
// under.
//
// Upstream ships logql.NoLimits, and this type exists because that value
// hard-disables multi-variant queries (`variants(...)`) through an
// unexported field. Inheriting that would exclude a whole LogQL surface
// from parity for an INSTANCE-CONFIGURATION reason rather than a
// semantic one — the reference engine implements variants perfectly well
// — and an exclusion nobody can lift is exactly the shape invariant 7
// forbids. Every knob here is therefore set explicitly, at a value that
// cannot decide a fixture's answer.
type referenceLimits struct{}

func (referenceLimits) MaxQuerySeries(context.Context, string) int { return maxSeries }

func (referenceLimits) MaxQueryRange(context.Context, string) time.Duration { return 0 }

func (referenceLimits) QueryTimeout(context.Context, string) time.Duration { return queryTimeout }

func (referenceLimits) BlockedQueries(context.Context, string) []*validation.BlockedQuery {
	return nil
}

func (referenceLimits) EnableMultiVariantQueries(string) bool { return true }

func (referenceLimits) MaxScanTaskParallelism(string) int { return 0 }

func (referenceLimits) DebugEngineTasks(string) bool { return false }

func (referenceLimits) DebugEngineStreams(string) bool { return false }

// Stream is one input log stream: an identifying label set plus its
// entries, exactly as they were read back out of ClickHouse.
type Stream struct {
	// Labels is the stream's full label set.
	Labels map[string]string

	// Entries are the log lines, which need not be evenly spaced.
	Entries []Entry
}

// Entry is one log line at an exact millisecond timestamp.
type Entry struct {
	// TMillis is the entry time in Unix milliseconds.
	TMillis int64

	// Line is the log line itself.
	Line string
}

// Result is one output sample from the reference engine, flattened so
// the caller can compare without depending on Loki's own types.
type Result struct {
	// Labels is the output series' label set.
	Labels map[string]string

	// TMillis is the output timestamp in Unix milliseconds.
	TMillis int64

	// Value is the output value.
	Value float64
}

// Query describes one evaluation to run.
type Query struct {
	// Expr is the LogQL expression, verbatim from the fixture.
	Expr string

	// Start is the instant for an instant query, or the range start.
	Start time.Time

	// End equals Start for an instant query.
	End time.Time

	// Step is zero for an instant query, and the range step otherwise.
	Step time.Duration
}

// IsRange reports whether this is a range query.
func (q Query) IsRange() bool { return q.Step > 0 }

// Evaluate runs q against the real Loki engine over streams, and returns
// the resulting samples sorted deterministically.
//
// It returns an error rather than failing the test so the caller can
// attribute a failure precisely: a Loki PARSE error on a fixture's query
// usually means the fixture exercises a cerberus extension that upstream
// does not accept, which is a fact about the fixture and not a parity
// failure.
func Evaluate(tb testing.TB, streams []Stream, q Query) ([]Result, error) {
	tb.Helper()

	engine := logql.NewEngine(
		logql.EngineOpts{},
		logql.NewMockQuerier(mockQuerierShards, toLokiStreams(streams)),
		referenceLimits{},
		log.NewNopLogger(),
	)

	step, interval := q.Step, time.Duration(0)
	params, err := logql.NewLiteralParams(
		q.Expr, q.Start, q.End, step, interval, logproto.FORWARD, entryLimit, nil, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("reference engine rejected the query: %w", err)
	}

	ctx := user.InjectOrgID(context.Background(), referenceTenant)
	res, err := engine.Query(params).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("reference engine evaluation failed: %w", err)
	}
	return flatten(res.Data)
}

// toLokiStreams converts the transport-free [Stream] shape into Loki's
// own push type.
//
// The label set is rendered through prometheus/model/labels because that
// is the exact form MockQuerier parses back — building the `{k="v"}`
// text by hand would introduce a second, subtly different quoting rule
// between the two halves of one round trip.
//
// StructuredMetadata is deliberately never populated: MockQuerier
// discards it (see the package doc), so writing it here would make the
// input look richer than what the engine actually evaluates.
func toLokiStreams(streams []Stream) []logproto.Stream {
	out := make([]logproto.Stream, 0, len(streams))
	for _, s := range streams {
		entries := make([]logproto.Entry, 0, len(s.Entries))
		for _, e := range s.Entries {
			entries = append(entries, logproto.Entry{
				Timestamp: time.UnixMilli(e.TMillis).UTC(),
				Line:      e.Line,
			})
		}
		out = append(out, logproto.Stream{
			Labels:  labels.FromMap(s.Labels).String(),
			Entries: entries,
		})
	}
	return out
}

// flatten converts Loki's result value into the transport-free Result
// shape, sorted by (labels, timestamp) so comparison never depends on
// the engine's internal ordering.
//
// A LOG query's answer ([logqlmodel.Streams]) is rejected rather than
// coerced. Its shape is lines, not samples, and cerberus's own answer
// for such a fixture is a `SELECT *` row set whose column layout is
// whatever the fixture's seed DDL happened to declare — there is no
// honest element-wise comparison between the two, and manufacturing one
// would manufacture a green. A fixture enrolled with a log query fails
// here, loudly, rather than being quietly compared against nothing.
func flatten(v parser.Value) ([]Result, error) {
	var out []Result

	switch val := v.(type) {
	case promql.Matrix:
		for _, s := range val {
			if len(s.Histograms) > 0 {
				return nil, histogramValuedErr(s.Metric)
			}
			for _, p := range s.Floats {
				out = append(out, Result{Labels: s.Metric.Map(), TMillis: p.T, Value: p.F})
			}
		}
	case promql.Vector:
		for _, s := range val {
			if s.H != nil {
				return nil, histogramValuedErr(s.Metric)
			}
			out = append(out, Result{Labels: s.Metric.Map(), TMillis: s.T, Value: s.F})
		}
	case promql.Scalar:
		out = append(out, Result{Labels: map[string]string{}, TMillis: val.T, Value: val.V})
	case logqlmodel.Streams:
		return nil, fmt.Errorf(
			"reference answer is %d log stream(s), not samples; a log query's answer cannot be "+
				"compared element-wise against a fixture's `expected_rows:`, whose column layout "+
				"is the fixture's own `SELECT *` projection. Enrol metric queries only",
			len(val),
		)
	default:
		return nil, fmt.Errorf("reference engine returned unsupported value type %T", v)
	}

	sortResults(out)
	return out, nil
}

func histogramValuedErr(m labels.Labels) error {
	return fmt.Errorf(
		"reference answer for %s is histogram-valued; cerberus's result path is float-only, "+
			"so this fixture cannot be parity-checked until native-histogram encoding lands",
		m.String(),
	)
}

// sortResults orders by label set then timestamp. Deterministic ordering
// is what lets the caller compare element-wise without re-deriving
// series identity.
func sortResults(rs []Result) {
	sort.SliceStable(rs, func(i, j int) bool {
		li, lj := labels.FromMap(rs[i].Labels).String(), labels.FromMap(rs[j].Labels).String()
		if li != lj {
			return li < lj
		}
		return rs[i].TMillis < rs[j].TMillis
	})
}

// EqualValues reports whether two float samples agree.
//
// NaN==NaN is TRUE here. LogQL produces NaN as a legitimate answer
// (0/0, a quantile over an empty window), and Go's float comparison
// would call two such answers different, failing fixtures that actually
// agree.
//
// Everything else is EXACT. This is deliberate and is the reason no
// epsilon appears anywhere in this package: cerberus and Loki evaluate
// the same IEEE-754 doubles, so a real disagreement is a real bug, and
// an epsilon would be a tolerance — the shape invariant 7 forbids —
// dressed up as a numeric constant.
func EqualValues(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}
