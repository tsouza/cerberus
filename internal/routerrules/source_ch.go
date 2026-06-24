package routerrules

import (
	"context"
	"fmt"
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
	ctx, cancel := context.WithTimeout(ctx, chCorpusTimeout)
	defer cancel()
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
		return nil, fmt.Errorf("routerrules: query %s: %w", CorpusTableName, err)
	}
	return r, nil
}

// rows is the subset of driver.Rows this source consumes.
type rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
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
	qb := chsql.NewQuery().From(chsql.BareIdent(CorpusTableName))
	conds := scopeConds(spec.Scope)
	if sf := s.sinceFrag(); sf != nil {
		conds = append(conds, sf)
	}
	if len(conds) > 0 {
		qb = qb.Where(conds...)
	}

	if len(spec.PartitionBy) == 0 {
		qb = qb.SelectAs(aggExpr, "v")
		sql, args := qb.Build()
		r, err := s.query(ctx, sql, args)
		if err != nil {
			return Value{}, err
		}
		defer func() { _ = r.Close() }()
		var v float64
		if r.Next() {
			if err := r.Scan(&v); err != nil {
				return Value{}, fmt.Errorf("routerrules: scan aggregate: %w", err)
			}
		}
		if err := r.Err(); err != nil {
			return Value{}, err
		}
		return Value{Scalar: v}, nil
	}

	partCol := spec.PartitionBy[0]
	qb = qb.Select(chsql.BareIdent(partCol)).SelectAs(aggExpr, "v").GroupBy(chsql.BareIdent(partCol))
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
		part[key] = v
	}
	if err := r.Err(); err != nil {
		return Value{}, err
	}
	return Value{Partition: part, PartitionCol: partCol}, nil
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
		qb = qb.Select(chsql.BareIdent(col))
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
		qb = qb.GroupBy(chsql.BareIdent(col))
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
