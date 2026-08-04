// Package spec — TraceQL wrap-projection reconstruction (build-tag-free).
//
// Layer 2a's TraceQL fixtures (internal/traceql/lower_test.go) capture the
// `-- sql --` section BEFORE engine.QueryPlan's wrap-projection stage
// (ProjectSamples), which traceqlLang.ProjectSamples always applies via
// wrapWithSampleProjection before optimizing/emitting (see
// internal/api/tempo/handler.go). For a search-shaped query (everything
// except a metrics-pipeline expression), the recorded SQL is therefore SQL
// that never reaches a real ClickHouse server in production — issue #1635
// fixed this gap for the `strict-scan` differential
// (strictscan_integration_test.go); issue #1653 closes the identical gap in
// the chDB round-trip lane (RunRoundTrip, runner_chdb.go), which backs the
// required `roundtrip (traceql)` status check.
//
// ReconstructTraceQLSearchWrap is the ONE reconstruction path both lanes
// share, so a change to the wrap logic (or to how a fixture's
// search_limit/search_window sections thread through) only needs updating
// here. It is build-tag-free (unlike its two callers — chdb and integration
// respectively) because it has no execution-substrate dependency of its own:
// it only parses + lowers + re-emits, using the exact same tempo.Lang the
// production /api/search handler drives.
package spec

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	traceqllower "github.com/tsouza/cerberus/internal/traceql"
	traceqlast "github.com/tsouza/cerberus/internal/traceql/ast"
)

// traceqlWrapExplainStep is passed to tempo.NewExplainLang but never
// actually exercised: ReconstructTraceQLSearchWrap only calls lang.Parse for
// fixtures whose query is NOT a metrics pipeline, so explainLang.Parse
// always takes its non-metrics branch, which ignores step. Kept a nonzero,
// human-legible value only to satisfy NewExplainLang's documented "must be
// > 0" contract defensively.
const traceqlWrapExplainStep = time.Minute

// ReconstructTraceQLSearchWrap reconstructs the wrap-projected SQL
// production actually sends to ClickHouse for a TraceQL round-trip
// fixture's search-shaped plan. c must carry a `query.traceql` section (the
// TraceQL round-trip fixture contract); optional `search_limit` /
// `search_window` sections thread the same bounded-scan shape
// internal/traceql/lower_test.go feeds the fixture's own `-- chplan --` /
// `-- sql --` sections.
//
// ok is false — sqlStr/args are the zero value, err is nil — in two cases a
// caller must treat as "leave the fixture's original SQL/args untouched":
//   - c has no `query.traceql` section (it isn't a TraceQL fixture at all).
//   - the query IS TraceQL but lowers through a metrics pipeline (`| rate()`,
//     `| count_over_time() by (...)`, …). Metrics-pipeline queries never
//     reach traceqlLang in production — /api/metrics/query_range routes them
//     through the entirely separate metricsLang
//     (internal/api/tempo/metrics_query_range.go), whose own hand-rolled
//     wrap this reconstruction does not represent.
//
// Reconstruction mirrors /api/search's own Lang exactly: tempo.NewExplainLang
// embeds the SAME unexported traceqlLang the handler drives, so Parse +
// ProjectSamples here are byte-for-byte what engine.QueryPlan would run for
// this query.
func ReconstructTraceQLSearchWrap(c *Case) (sqlStr string, args []any, ok bool, err error) {
	query, hasQuery := c.Section("query.traceql")
	if !hasQuery {
		return "", nil, false, nil
	}
	query = strings.TrimSpace(query)

	expr, err := traceqlast.Parse(query)
	if err != nil {
		return "", nil, false, fmt.Errorf("traceql parse: %w", err)
	}
	if expr.MetricsPipeline != nil || expr.MetricsSecondStage != nil {
		return "", nil, false, nil
	}

	ctx := context.Background()
	if v, hasLimit := c.Section("search_limit"); hasLimit {
		n, perr := strconv.Atoi(strings.TrimSpace(v))
		if perr != nil {
			return "", nil, false, fmt.Errorf("bad search_limit %q: %w", v, perr)
		}
		ctx = traceqllower.WithSearchTraceLimit(ctx, n)
	}
	if v, hasWindow := c.Section("search_window"); hasWindow {
		fields := strings.Fields(v)
		if len(fields) != 2 {
			return "", nil, false, fmt.Errorf("search_window wants two Unix-second bounds, got %q", v)
		}
		startSec, err1 := strconv.ParseInt(fields[0], 10, 64)
		endSec, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			return "", nil, false, fmt.Errorf("bad search_window %q", v)
		}
		ctx = traceqllower.WithSearchWindow(ctx, time.Unix(startSec, 0).UTC(), time.Unix(endSec, 0).UTC())
	}

	lang := tempo.NewExplainLang(schema.DefaultOTelTraces(), traceqlWrapExplainStep)
	plan, meta, err := lang.Parse(ctx, query)
	if err != nil {
		return "", nil, false, fmt.Errorf("traceql lower (wrap reconstruction): %w", err)
	}
	wrapped := lang.ProjectSamples(plan, meta)

	sqlStr, args, err = chsql.Emit(context.Background(), wrapped)
	if err != nil {
		return "", nil, false, fmt.Errorf("chsql.Emit(wrapped): %w", err)
	}
	return sqlStr, args, true, nil
}
