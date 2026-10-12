package tempo

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// rootLookupEnvelope reads the cohort once, before the spans query. Literal
// bounds let ClickHouse prune partitions without separate scalar reads for
// Start and End. Coverage counts distinct normalized IDs, not MV batches.
// Present bounds must still be complete: this is the existing operator-owned
// TraceIDTsEnabled contract, not a proof that historical spans were backfilled.
func (h *Handler) rootLookupEnvelope(ctx context.Context, traceIDs []string) (start, end time.Time, covered bool, err error) {
	s := h.Schema
	if s.TraceIDTsTable == "" || s.TraceIDTsStartColumn == "" || s.TraceIDTsEndColumn == "" {
		return start, end, false, nil
	}
	ids := padTraceIDs(traceIDs)
	unique := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		unique[id] = struct{}{}
	}
	agg := &chplan.Aggregate{
		Input: &chplan.Filter{
			Input: &chplan.Scan{Table: s.TraceIDTsTable},
			Predicate: &chplan.Binary{Op: chplan.OpAnd,
				Left: inStringList(s.TraceIDColumn, ids),
				Right: &chplan.Binary{Op: chplan.OpLe,
					Left:  &chplan.ColumnRef{Name: s.TraceIDTsStartColumn},
					Right: &chplan.ColumnRef{Name: s.TraceIDTsEndColumn}},
			},
		},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnMin, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDTsStartColumn}}, Alias: "TraceStart"},
			{Fn: chplan.FnMax, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDTsEndColumn}}, Alias: "TraceEnd"},
			{Fn: chplan.FnUniqExact, Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}}, Alias: "TraceCount"},
		},
	}
	// String-encode nanoseconds so decoding never passes through float64.
	const timestampPrecisionNano = 9
	nanos := func(column string) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnToUnixNanos, Args: []chplan.Expr{
				&chplan.FuncCall{Fn: chplan.FnToDateTime64, Args: []chplan.Expr{
					&chplan.ColumnRef{Name: column}, &chplan.LitInt{V: timestampPrecisionNano},
				}},
			}},
		}}
	}
	plan := &chplan.Project{Input: agg, Projections: []chplan.Projection{{
		Expr: &chplan.FuncCall{Fn: chplan.FnToJSONString, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnTuple, Args: []chplan.Expr{
				nanos("TraceStart"), nanos("TraceEnd"),
				&chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{&chplan.ColumnRef{Name: "TraceCount"}}},
			}},
		}}, Alias: "Envelope",
	}}}
	sql, args, scans, err := chsql.EmitCounted(ctx, plan)
	if err != nil {
		return start, end, false, err
	}
	rows, err := h.Client.QueryStrings(chclient.WithDataShardFanoutMultiplier(ctx, scans), sql, args...)
	if err != nil {
		return start, end, false, err
	}
	if len(rows) == 0 {
		return start, end, false, nil
	}
	const envelopeFieldCount = 3
	var fields []string
	if len(rows) != 1 {
		return start, end, false, fmt.Errorf("expected one envelope row, got %d", len(rows))
	}
	if err := json.Unmarshal([]byte(rows[0]), &fields); err != nil {
		return start, end, false, fmt.Errorf("decode envelope: %w", err)
	}
	if len(fields) != envelopeFieldCount {
		return start, end, false, fmt.Errorf("expected %d envelope fields, got %d", envelopeFieldCount, len(fields))
	}
	count, err := strconv.Atoi(fields[2])
	if err != nil {
		return start, end, false, fmt.Errorf("decode coverage: %w", err)
	}
	if count != len(unique) {
		return start, end, false, nil
	}
	lo, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return start, end, false, fmt.Errorf("decode Start: %w", err)
	}
	hi, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return start, end, false, fmt.Errorf("decode End: %w", err)
	}
	pad := int64(chplan.TraceIDTsEndPadSeconds) * int64(time.Second)
	if lo > hi || hi > math.MaxInt64-pad {
		return start, end, false, nil
	}
	return time.Unix(0, lo), time.Unix(0, hi+pad), true, nil
}
