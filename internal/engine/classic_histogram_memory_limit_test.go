package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
)

// classicHistogramMemoryLimitCap is the per-query cap the fixture's
// memory-limit errors carry.
const classicHistogramMemoryLimitCap int64 = 1 << 30

// classicHistogramCHMemoryLimit is the error chain chclient surfaces when
// ClickHouse aborts a statement with code 241.
func classicHistogramCHMemoryLimit() error {
	return &chclient.MemoryLimitError{
		Limit: classicHistogramMemoryLimitCap,
		Cause: &clickhouse.Exception{Code: 241, Name: "MEMORY_LIMIT_EXCEEDED", Message: "Memory limit (for query) exceeded"},
	}
}

// classicHistogramShapes are real PromQL lowerings, each marked with whether
// it carries a classic-histogram window or bucket-merge stage under the
// fan-out lowerers (classic) and under the auto selection on 26.8
// (classicAuto). Auto lowers a range classic `rate` quantile through the
// ts-grid per-rung path, which carries neither stage.
var classicHistogramShapes = []struct {
	name                 string
	query                string
	rangeQuery           bool
	classic, classicAuto bool
}{
	{"instant classic quantile", `histogram_quantile(0.5, sum by (le) (rate(` + adaptiveClassicMetric + `[5m])))`, false, true, true},
	{"instant classic quantile over bare selector", `histogram_quantile(0.5, sum by (le) (` + adaptiveClassicMetric + `))`, false, true, true},
	{"range classic rate quantile", `histogram_quantile(0.5, sum by (le) (rate(` + adaptiveClassicMetric + `[5m])))`, true, true, false},
	{"range classic increase quantile", `histogram_quantile(0.5, sum by (le) (increase(` + adaptiveClassicMetric + `[5m])))`, true, true, true},
	{"instant native quantile", `histogram_quantile(0.5, sum(rate(` + adaptiveExpMetric + `[5m])))`, false, false, false},
	{"range native quantile", `histogram_quantile(0.5, sum(rate(` + adaptiveExpMetric + `[5m])))`, true, false, false},
	{"instant bucket rate without a quantile", `sum by (le) (rate(` + adaptiveClassicMetric + `[5m]))`, false, false, false},
	{"range counter rate", `sum(rate(http_requests_total[5m]))`, true, false, false},
}

// TestClassicHistogramStage_RealLowerings pins the plan fact on what the
// production lowerers build, after the optimizer the engine runs before it
// inspects the plan, under both the fan-out lowerers and the auto selection
// on 26.8.
func TestClassicHistogramStage_RealLowerings(t *testing.T) {
	t.Parallel()
	autoLowerers, _ := adaptiveDispatch(t, adaptiveServer268)
	for _, lowerers := range []struct {
		name  string
		table promql.RangeLowerers
		auto  bool
	}{
		{"fan-out", promql.RangeLowerers{}, false},
		{"auto 26.8", autoLowerers, true},
	} {
		for _, tc := range classicHistogramShapes {
			t.Run(lowerers.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				want := tc.classic
				if lowerers.auto {
					want = tc.classicAuto
				}
				plan := optimizer.Default().Run(context.Background(), lowerPromQLWith(t, lowerers.table, tc.query, tc.rangeQuery))
				if got := engine.ClassicHistogramStageForTest(plan); got != want {
					t.Fatalf("classicHistogramStage = %v, want %v", got, want)
				}
			})
		}
	}
}

// classicHistogramLang lowers query as an instant or range PromQL query with
// the fan-out lowerers.
func classicHistogramLang(t *testing.T, query string, rangeQuery bool) *fakeLang {
	t.Helper()
	plan := lowerPromQLWith(t, promql.RangeLowerers{}, query, rangeQuery)
	return &fakeLang{
		name: "promql",
		parseFn: func(context.Context, string) (chplan.Node, engine.Meta, error) {
			return plan, engine.Meta{IsMetric: true}, nil
		},
	}
}

// memoryLimitCursorClient is a route-A client whose eager query or cursor
// open fails with openErr, or whose cursor streams one row and then fails its
// drain with drainErr.
type memoryLimitCursorClient struct {
	openErr  error
	drainErr error
}

func (c *memoryLimitCursorClient) Query(context.Context, string, ...any) ([]chclient.Sample, error) {
	return nil, c.openErr
}

func (c *memoryLimitCursorClient) QueryCursor(context.Context, string, ...any) (chclient.Cursor, error) {
	if c.openErr != nil {
		return nil, c.openErr
	}
	return &drainErrCursor{remaining: 1, err: c.drainErr}, nil
}

// drainErrCursor streams remaining empty rows, then reports err.
type drainErrCursor struct {
	remaining int
	err       error
}

func (c *drainErrCursor) Next() bool {
	if c.remaining <= 0 {
		return false
	}
	c.remaining--
	return true
}
func (c *drainErrCursor) Sample() chclient.Sample { return chclient.Sample{} }
func (c *drainErrCursor) Err() error {
	if c.remaining > 0 {
		return nil
	}
	return c.err
}
func (c *drainErrCursor) Close() error     { return nil }
func (c *drainErrCursor) Inspected() int64 { return 0 }

// assertClassicHistogramWrap pins err's classification: wrapped as a
// classic-histogram memory-limit abort exactly when want, and in both cases
// still a *chclient.MemoryLimitError matching chclient.ErrMemoryLimitExceeded,
// so every reader of the unwrapped error reads the wrapped one alike.
func assertClassicHistogramWrap(t *testing.T, err error, want bool) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want the memory-limit abort")
	}
	var classic *engine.ClassicHistogramMemoryLimitError
	if got := errors.As(err, &classic); got != want {
		t.Fatalf("wrapped as ClassicHistogramMemoryLimitError = %v, want %v: %v", got, want, err)
	}
	if want && classic.Limit != classicHistogramMemoryLimitCap {
		t.Fatalf("Limit = %d, want the aborting client's %d", classic.Limit, classicHistogramMemoryLimitCap)
	}
	if !errors.Is(err, chclient.ErrMemoryLimitExceeded) {
		t.Fatalf("errors.Is(ErrMemoryLimitExceeded) = false: %v", err)
	}
	var memLimit *chclient.MemoryLimitError
	if !errors.As(err, &memLimit) {
		t.Fatalf("errors.As(*chclient.MemoryLimitError) = false: %v", err)
	}
}

// TestClassicHistogramMemoryLimit_WrapsOnlyAClassicStage drives the eager
// path (Engine.Query), the cursor open (Engine.QueryCursor) and the cursor
// drain over a classic-histogram statement and over other statements: only
// the classic one's memory-limit abort is wrapped, on every path.
func TestClassicHistogramMemoryLimit_WrapsOnlyAClassicStage(t *testing.T) {
	t.Parallel()
	for _, tc := range classicHistogramShapes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			t.Run("eager", func(t *testing.T) {
				eng := &engine.Engine{Optimizer: optimizer.Default(), Client: &memoryLimitCursorClient{openErr: classicHistogramCHMemoryLimit()}}
				_, err := eng.Query(context.Background(), classicHistogramLang(t, tc.query, tc.rangeQuery), tc.query)
				assertClassicHistogramWrap(t, err, tc.classic)
			})
			t.Run("cursor open", func(t *testing.T) {
				eng := &engine.Engine{Optimizer: optimizer.Default(), Client: &memoryLimitCursorClient{openErr: classicHistogramCHMemoryLimit()}}
				_, err := eng.QueryCursor(context.Background(), classicHistogramLang(t, tc.query, tc.rangeQuery), tc.query)
				assertClassicHistogramWrap(t, err, tc.classic)
			})
			t.Run("cursor drain", func(t *testing.T) {
				eng := &engine.Engine{Optimizer: optimizer.Default(), Client: &memoryLimitCursorClient{drainErr: classicHistogramCHMemoryLimit()}}
				res, err := eng.QueryCursor(context.Background(), classicHistogramLang(t, tc.query, tc.rangeQuery), tc.query)
				if err != nil {
					t.Fatalf("QueryCursor: %v", err)
				}
				defer func() { _ = res.Cursor.Close() }()
				rows := 0
				for res.Cursor.Next() {
					rows++
				}
				if rows != 1 {
					t.Fatalf("drained %d rows, want the cursor's one row passed through", rows)
				}
				assertClassicHistogramWrap(t, res.Cursor.Err(), tc.classic)
			})
		})
	}
}

// TestClassicHistogramMemoryLimit_OtherErrorsPassThrough pins that a
// classic-histogram statement's failure other than a memory-limit abort is
// never wrapped, at the open and in the drain.
func TestClassicHistogramMemoryLimit_OtherErrorsPassThrough(t *testing.T) {
	t.Parallel()
	query := classicHistogramShapes[0].query
	other := &clickhouse.Exception{Code: 159, Name: "TIMEOUT_EXCEEDED", Message: "Timeout exceeded"}
	var classic *engine.ClassicHistogramMemoryLimitError

	eng := &engine.Engine{Optimizer: optimizer.Default(), Client: &memoryLimitCursorClient{openErr: other}}
	if _, err := eng.Query(context.Background(), classicHistogramLang(t, query, false), query); err == nil || errors.As(err, &classic) {
		t.Fatalf("eager error = %v, want the unwrapped timeout", err)
	}
	eng = &engine.Engine{Optimizer: optimizer.Default(), Client: &memoryLimitCursorClient{drainErr: other}}
	res, err := eng.QueryCursor(context.Background(), classicHistogramLang(t, query, false), query)
	if err != nil {
		t.Fatalf("QueryCursor: %v", err)
	}
	for res.Cursor.Next() {
	}
	if err := res.Cursor.Err(); err == nil || errors.As(err, &classic) {
		t.Fatalf("drain error = %v, want the unwrapped timeout", err)
	}
	var ex *clickhouse.Exception
	if !errors.As(res.Cursor.Err(), &ex) || ex.Code != other.Code {
		t.Fatalf("drain error = %v, want the original exception", res.Cursor.Err())
	}
}
