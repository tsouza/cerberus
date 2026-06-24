package routerrules

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chsql"
)

// Conservative resource caps stamped on EVERY corpus SELECT, mirroring
// optcorpus's discipline: this is an offline report generator, so it biases hard
// toward never disturbing the data plane. Single-threaded, deprioritised behind
// data-plane queries, hard-capped in wall-clock and in rows/bytes read, flagged
// read-only so it can never mutate state.
const (
	chCorpusMaxExecutionTime = 30.0 // seconds; offline, so a touch more generous than the inline reconciler
	chCorpusMaxThreads       = 1
	chCorpusPriority         = 10
	chCorpusMaxRowsToRead    = 200_000_000
	chCorpusMaxBytesToRead   = 8 << 30 // 8 GiB
	chCorpusTimeout          = 60 * time.Second
)

// chCorpusSource evaluates the catalog against the live cerberus_router_corpus
// table. It builds every query with the typed chsql QueryBuilder/Frag API (no
// raw SQL strings) and stamps the conservative settings above on each scan.
type chCorpusSource struct {
	conn  CHConn
	since float64 // event_time floor (unix seconds); 0 disables windowing
}

// NewCHCorpusSource builds a ClickHouse-table source over the narrow CHConn
// (typically *chclient.Client.Conn() from cmd/). sinceUnix bounds event_time.
func NewCHCorpusSource(conn CHConn, sinceUnix float64) CorpusSource {
	return &chCorpusSource{conn: conn, since: sinceUnix}
}

func (s *chCorpusSource) query(ctx context.Context, sql string, args []any) (rows, error) {
	// The timeout context must outlive the returned rows: callers iterate and
	// Scan AFTER query() returns. Cancelling here (defer cancel()) would tear
	// the context down before the first Scan — under the chdb-go database/sql
	// driver that surfaces as "context canceled" on Scan and poisons the
	// single global chdb session for every later query. clickhouse-go's native
	// driver buffers rows so it tolerated the premature cancel, masking the
	// bug. Tie cancel to rows.Close() instead so the context spans the scan and
	// is never leaked (every caller already defers Close).
	ctx, cancel := context.WithTimeout(ctx, chCorpusTimeout)
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{
		"max_execution_time":    chCorpusMaxExecutionTime,
		"timeout_overflow_mode": "throw",
		"max_threads":           chCorpusMaxThreads,
		"priority":              chCorpusPriority,
		"max_rows_to_read":      chCorpusMaxRowsToRead,
		"max_bytes_to_read":     chCorpusMaxBytesToRead,
		"read_overflow_mode":    "break",
		"readonly":              1,
	}))
	r, err := s.conn.Query(ctx, sql, args...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("routerrules: query %s: %w", CorpusTableName, err)
	}
	return &cancelRows{rows: r, cancel: cancel}, nil
}

// rows is the subset of driver.Rows this source consumes.
type rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// cancelRows binds a context.CancelFunc to a rows lifetime: the cancel fires on
// Close (which every consumer defers), so the query timeout context spans the
// whole scan rather than being torn down the instant query() returns.
type cancelRows struct {
	rows
	cancel context.CancelFunc
}

func (c *cancelRows) Close() error {
	err := c.rows.Close()
	c.cancel()
	return err
}

// groupKeyFrag is the SELECT/GROUP BY expression for a group-key column.
//
// Group keys are scanned into *string. Reference (Enum8) and LowCardinality
// columns both carry string names, but the chdb-go database/sql driver cannot
// cast a bare Enum8 column into *string ("could not cast to type: ENUM").
// Wrapping every group key in toString() yields the enum's string name (and is
// a no-op on String / LowCardinality(String)), so the CH group key equals the
// JSONL path's value (which reads the same string name) — parity holds.
func groupKeyFrag(col string) chsql.Frag {
	return chsql.Call("toString", chsql.BareIdent(col))
}

// sinceFrag returns the event_time window predicate, or nil when unwindowed.
func (s *chCorpusSource) sinceFrag() chsql.Frag {
	if s.since <= 0 {
		return nil
	}
	// event_time >= toDateTime(<sinceUnix>)
	return chsql.Gte(
		chsql.BareIdent("event_time"),
		chsql.Call("toDateTime", chsql.InlineLit(int64(s.since))),
	)
}

// Aggregate resolves a corpus param with a single grouped/scoped SELECT.
func (s *chCorpusSource) Aggregate(ctx context.Context, spec AggSpec) (Value, error) {
	switch {
	case spec.CountRatio:
		return s.countRatio(ctx, spec)
	case spec.Percentile != nil:
		return s.scalarOrPartition(ctx, spec, quantileFrag(*spec.Percentile, spec.Column))
	case spec.Agg != "":
		return s.scalarOrPartition(ctx, spec, chsql.Call(string(spec.Agg), chsql.BareIdent(spec.Column)))
	default:
		return Value{}, fmt.Errorf("routerrules: ch aggregate: empty AggSpec for column %q", spec.Column)
	}
}

// quantileFrag builds quantileExact(<frac>)(<column>) via the typed parametric
// aggregate constructor, so the CH path uses the same exact-quantile family the
// JSONL path replicates.
func quantileFrag(frac float64, column string) chsql.Frag {
	return chsql.Parametric("quantileExact", []chsql.Frag{chsql.InlineLit(frac)}, chsql.BareIdent(column))
}

func (s *chCorpusSource) scalarOrPartition(ctx context.Context, spec AggSpec, aggExpr chsql.Frag) (Value, error) {
	// An empty sub-population is no signal, not a watermark of 0. The scalar
	// path carries a count() companion column so an empty population resolves to
	// Value{NoSignal: true} (mirroring the JSONL backend's len(all)==0 guard),
	// and a fire-gate that depends on it is skipped rather than firing on
	// everything. A non-grouped aggregate (quantileExact/avg/stddevPop/…) over a
	// zero-row group returns NaN, so the aggregate is still wrapped in
	// ifNull(<agg>, 0) and folded through normalizeEmptyAgg to keep a non-empty
	// group's value finite — but with count() driving NoSignal, that folded 0 is
	// never consumed as a watermark in the empty case.
	aggExpr = chsql.Call("ifNull", aggExpr, chsql.InlineLit(int64(0)))
	qb := chsql.NewQuery().From(chsql.BareIdent(CorpusTableName))
	conds := scopeConds(spec.Scope)
	if sf := s.sinceFrag(); sf != nil {
		conds = append(conds, sf)
	}
	if len(conds) > 0 {
		qb = qb.Where(conds...)
	}

	if len(spec.PartitionBy) == 0 {
		qb = qb.SelectAs(aggExpr, "v").SelectAs(chsql.Call("count"), "n")
		sql, args := qb.Build()
		r, err := s.query(ctx, sql, args)
		if err != nil {
			return Value{}, err
		}
		defer func() { _ = r.Close() }()
		var (
			v float64
			n uint64
		)
		if r.Next() {
			if err := r.Scan(&v, &n); err != nil {
				return Value{}, fmt.Errorf("routerrules: scan aggregate: %w", err)
			}
		}
		if err := r.Err(); err != nil {
			return Value{}, err
		}
		if n == 0 {
			return Value{NoSignal: true}, nil
		}
		return Value{Scalar: normalizeEmptyAgg(v)}, nil
	}

	partCol := spec.PartitionBy[0]
	qb = qb.Select(groupKeyFrag(partCol)).SelectAs(aggExpr, "v").GroupBy(groupKeyFrag(partCol))
	sql, args := qb.Build()
	r, err := s.query(ctx, sql, args)
	if err != nil {
		return Value{}, err
	}
	defer func() { _ = r.Close() }()
	part := map[string]float64{}
	for r.Next() {
		var (
			key string
			v   float64
		)
		if err := r.Scan(&key, &v); err != nil {
			return Value{}, fmt.Errorf("routerrules: scan partitioned aggregate: %w", err)
		}
		part[key] = normalizeEmptyAgg(v)
	}
	if err := r.Err(); err != nil {
		return Value{}, err
	}
	return Value{Partition: part, PartitionCol: partCol}, nil
}

// normalizeEmptyAgg folds a CH aggregate over an empty/TTL'd population to the
// JSONL backend's 0-contract. The ifNull(<agg>, 0) Frag wrap handles
// NULL-returning aggregates; this guard additionally catches the NaN/±Inf a
// non-grouped aggregate (avg/quantileExact/stddevPop) yields over a zero-row
// group, which ifNull does not. Empty corpus = no signal = no findings.
func normalizeEmptyAgg(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func (s *chCorpusSource) countRatio(ctx context.Context, spec AggSpec) (Value, error) {
	numExpr := chsql.Call("countIf", chsql.And(scopeConds(spec.NumScope)...))
	denExpr := chsql.Call("countIf", chsql.And(scopeConds(spec.DenScope)...))
	qb := chsql.NewQuery().
		From(chsql.BareIdent(CorpusTableName)).
		SelectAs(numExpr, "num").
		SelectAs(denExpr, "den")
	if sf := s.sinceFrag(); sf != nil {
		qb = qb.Where(sf)
	}
	sql, args := qb.Build()
	r, err := s.query(ctx, sql, args)
	if err != nil {
		return Value{}, err
	}
	defer func() { _ = r.Close() }()
	var num, den float64
	if r.Next() {
		if err := r.Scan(&num, &den); err != nil {
			return Value{}, fmt.Errorf("routerrules: scan count ratio: %w", err)
		}
	}
	if err := r.Err(); err != nil {
		return Value{}, err
	}
	if den == 0 {
		return Value{Scalar: 0}, nil
	}
	return Value{Scalar: num / den}, nil
}

// EvalRule pushes the match filter, group-by, support count, and evidence
// aggregates into one CH SELECT. min_support filtering is applied by the
// evaluator (not a HAVING clause) so both backends share identical thin-class
// semantics.
func (s *chCorpusSource) EvalRule(ctx context.Context, q RuleQuery) ([]GroupResult, error) {
	condFrag, err := q.Condition.frag(q.Env)
	if err != nil {
		return nil, err
	}

	qb := chsql.NewQuery().From(chsql.BareIdent(CorpusTableName))
	for _, col := range q.GroupBy {
		qb = qb.Select(groupKeyFrag(col))
	}
	qb = qb.SelectAs(chsql.Call("count"), "support")
	for _, ev := range q.Evidence {
		qb = qb.Select(chsql.Call(string(ev.fn), chsql.BareIdent(ev.column)))
	}

	conds := []chsql.Frag{condFrag}
	if sf := s.sinceFrag(); sf != nil {
		conds = append(conds, sf)
	}
	qb = qb.Where(conds...)
	for _, col := range q.GroupBy {
		qb = qb.GroupBy(groupKeyFrag(col))
	}

	sql, args := qb.Build()
	r, err := s.query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()

	var out []GroupResult
	for r.Next() {
		dest, gkPtrs, support, evPtrs := scanTargets(len(q.GroupBy), len(q.Evidence))
		if err := r.Scan(dest...); err != nil {
			return nil, fmt.Errorf("routerrules: scan rule group: %w", err)
		}
		gk := make([]string, len(gkPtrs))
		for i := range gkPtrs {
			gk[i] = *gkPtrs[i]
		}
		ev := make([]float64, len(evPtrs))
		for i := range evPtrs {
			ev[i] = *evPtrs[i]
		}
		out = append(out, GroupResult{GroupKey: gk, Support: *support, Evidence: ev})
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanTargets builds the Scan destination slice for a rule group row: nGroup
// string group keys, one int64 support count, and nEv float64 evidence values.
func scanTargets(nGroup, nEv int) (dest []any, gk []*string, support *int64, ev []*float64) {
	gk = make([]*string, nGroup)
	support = new(int64)
	ev = make([]*float64, nEv)
	dest = make([]any, 0, nGroup+1+nEv)
	for i := range gk {
		gk[i] = new(string)
		dest = append(dest, gk[i])
	}
	dest = append(dest, support)
	for i := range ev {
		ev[i] = new(float64)
		dest = append(dest, ev[i])
	}
	return dest, gk, support, ev
}

// scopeConds lowers an enum-equality scope to typed comparison Frags.
func scopeConds(scope Scope) []chsql.Frag {
	if len(scope) == 0 {
		return nil
	}
	// Deterministic order so generated SQL is stable.
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	conds := make([]chsql.Frag, 0, len(scope))
	for _, k := range keys {
		conds = append(conds, chsql.Eq(chsql.BareIdent(k), chsql.InlineLit(scope[k])))
	}
	return conds
}
