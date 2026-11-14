package chclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestParseUnknownName pins the rejected name read off each wording
// ClickHouse uses, as captured from real servers: the native protocol, HTTP on
// 26.6 (which appends the build), the analyzer's and the legacy analyzer's
// unknown-function messages. The spelling suggestions a message lists after
// the rejected name are never read as the name.
func TestParseUnknownName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ex   *clickhouse.Exception
		want unknownName
	}{
		{
			"native, setting",
			&clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Unknown setting 'enable_adaptive_aggregator'"},
			unknownName{kind: unknownNameSetting, name: "enable_adaptive_aggregator"},
		},
		{
			"HTTP 26.6, setting",
			&clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Setting enable_adaptive_aggregator is neither a builtin setting nor started with the prefix 'SQL_' " +
				"registered for user-defined settings: Maybe you meant ['enable_sharding_aggregator']. (UNKNOWN_SETTING) (version 26.6.8.7 (official build))"},
			unknownName{kind: unknownNameSetting, name: "enable_adaptive_aggregator", nodeVersion: "26.6.8.7"},
		},
		{
			"analyzer, function",
			&clickhouse.Exception{Code: chCodeUnknownFunction, Message: "Function with name `argAndMax` does not exist. In scope SELECT argAndMax(1, 2). Maybe you meant: ['argMax','argMin']"},
			unknownName{kind: unknownNameFunction, name: "argAndMax"},
		},
		{
			"24.8 analyzer, function",
			&clickhouse.Exception{Code: chCodeUnknownFunction, Message: "Function with name 'argAndMax' does not exist. In scope SELECT argAndMax(1, 2). Maybe you meant: ['arrayMax']"},
			unknownName{kind: unknownNameFunction, name: "argAndMax"},
		},
		{
			"legacy analyzer over HTTP, function",
			&clickhouse.Exception{Code: chCodeUnknownFunction, Message: "Unknown function argAndMax. Maybe you meant: ['arrayMax']: Or unknown aggregate function argAndMax. " +
				"Maybe you meant: ['argMax']: While processing argAndMax(1, 2). (UNKNOWN_FUNCTION) (version 25.10.7.6 (official build))"},
			unknownName{kind: unknownNameFunction, name: "argAndMax", nodeVersion: "25.10.7.6"},
		},
		{
			"suggestion naming a droppable setting",
			&clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Unknown setting 'enable_adaptive_aggregatorx'. Maybe you meant ['enable_adaptive_aggregator']"},
			unknownName{kind: unknownNameSetting, name: "enable_adaptive_aggregatorx"},
		},
	}
	for _, tc := range cases {
		got, ok := parseUnknownName(fmt.Errorf("wrapped: %w", tc.ex))
		if !ok || got != tc.want {
			t.Errorf("%s: parseUnknownName = %+v, %v; want %+v", tc.name, got, ok, tc.want)
		}
	}

	for name, err := range map[string]error{
		"other code":       &clickhouse.Exception{Code: chCodeMemoryLimitExceeded, Message: "Unknown setting 'enable_adaptive_aggregator'"},
		"unparsed message": &clickhouse.Exception{Code: chCodeUnknownSetting, Message: "something else entirely"},
		"no exception":     errors.New("dial: refused"),
		"nil":              nil,
	} {
		if got, ok := parseUnknownName(err); ok {
			t.Errorf("%s: parseUnknownName = %+v; want no rejection", name, got)
		}
	}
}

// TestClassifyDriverErr_UnknownNameIsTyped: a rejection of a name the node
// does not know reaches the caller as an *UnknownNameError naming the kind,
// the name and the node's build, unwrapping to ErrNodeTooOld and the
// exception. A gate setting keeps its own error, which unwraps to
// ErrNodeTooOld as well.
func TestClassifyDriverErr_UnknownNameIsTyped(t *testing.T) {
	t.Parallel()
	c := newWithConn(&staleConn{})

	ex := &clickhouse.Exception{Code: chCodeUnknownFunction, Message: "Function with name `timeSeriesThrowDuplicateSeriesIf` does not exist. (UNKNOWN_FUNCTION) (version 26.1.12.23 (official build))"}
	err := c.classifyDriverErr(context.Background(), ex)
	var unknown *UnknownNameError
	if !errors.As(err, &unknown) || unknown.Kind != unknownNameFunction || unknown.Name != "timeSeriesThrowDuplicateSeriesIf" || unknown.NodeVersion != "26.1.12.23" {
		t.Fatalf("err = %#v; want an *UnknownNameError for function timeSeriesThrowDuplicateSeriesIf on 26.1.12.23", err)
	}
	for _, want := range []string{"function", "timeSeriesThrowDuplicateSeriesIf", "26.1.12.23"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %q", err, want)
		}
	}
	if !errors.Is(err, ErrNodeTooOld) {
		t.Error("an *UnknownNameError does not unwrap to ErrNodeTooOld")
	}
	var cause *clickhouse.Exception
	if !errors.As(err, &cause) || cause != ex {
		t.Error("the exception is not reachable through the *UnknownNameError")
	}

	gate := c.classifyDriverErr(context.Background(), &clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Unknown setting '" + chopt.SettingUseQueryConditionCache + "'"})
	var gateErr *GateSettingUnsupportedError
	if !errors.As(gate, &gateErr) || errors.As(gate, &unknown) && unknown.Name == chopt.SettingUseQueryConditionCache {
		t.Fatalf("gate rejection = %#v; want a *GateSettingUnsupportedError and no *UnknownNameError", gate)
	}
	if !errors.Is(gate, ErrNodeTooOld) {
		t.Error("a *GateSettingUnsupportedError does not unwrap to ErrNodeTooOld")
	}
}

// rejectionRecorder collects what the client's unknown-name hook receives.
type rejectionRecorder struct {
	mu   sync.Mutex
	seen []UnknownNameRejection
}

func (r *rejectionRecorder) record(rej UnknownNameRejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, rej)
}

func (r *rejectionRecorder) all() []UnknownNameRejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]UnknownNameRejection(nil), r.seen...)
}

// withObservedRejections gives c a reader-backed counter and a recording hook.
func withObservedRejections(c *Client) (*rejectionRecorder, *sdkmetric.ManualReader) {
	reader := sdkmetric.NewManualReader()
	c.rejections = newRejectionReporter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	rec := &rejectionRecorder{}
	c.SetUnknownNameHook(rec.record)
	return rec, reader
}

const unknownNameCounter = "cerberus_ch_unknown_name_rejections_total"

// TestQueryStrings_RetryIsReported: the retried rejection reaches the hook
// once, marked retried, and counts on the retried series, which was exported
// at zero before any rejection.
func TestQueryStrings_RetryIsReported(t *testing.T) {
	t.Parallel()
	c, _ := newRolledBackClient(chopt.SettingEnableAdaptiveAggregator)
	rec, reader := withObservedRejections(c)
	retried := map[string]string{"kind": unknownNameSetting, "name": chopt.SettingEnableAdaptiveAggregator, "action": rejectionActionRetried}

	if sum, exported, points := counterSum(t, reader, unknownNameCounter, retried); !exported || points != 1 || sum != 0 {
		t.Fatalf("before any rejection: retried series sum=%d exported=%v points=%d; want one series at 0", sum, exported, points)
	}
	ctx := WithQuerySetting(context.Background(), chopt.SettingEnableAdaptiveAggregator, 0)
	if _, err := c.QueryStrings(ctx, "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings: %v", err)
	}
	want := []UnknownNameRejection{{Kind: unknownNameSetting, Name: chopt.SettingEnableAdaptiveAggregator, Retried: true}}
	if got := rec.all(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("hook received %+v; want %+v", got, want)
	}
	if sum, _, _ := counterSum(t, reader, unknownNameCounter, retried); sum != 1 {
		t.Errorf("retried series = %d; want 1", sum)
	}
}

// TestQueryStrings_RetryRejectionIsFinalAndReported: a node that rejects the
// retry for the setting the retry no longer carries gets no third statement;
// that second rejection is the typed answer, and it is reported too, as
// surfaced, so the counter sees every rejection.
func TestQueryStrings_RetryRejectionIsFinalAndReported(t *testing.T) {
	t.Parallel()
	c, conn := newRolledBackClient(chopt.SettingEnableAdaptiveAggregator)
	conn.rejectAlways = true
	rec, reader := withObservedRejections(c)

	ctx := WithQuerySetting(context.Background(), chopt.SettingEnableAdaptiveAggregator, 0)
	_, err := c.QueryStrings(ctx, "SELECT 1")
	var unknown *UnknownNameError
	if !errors.As(err, &unknown) || unknown.Name != chopt.SettingEnableAdaptiveAggregator {
		t.Fatalf("err = %v; want an *UnknownNameError for %s", err, chopt.SettingEnableAdaptiveAggregator)
	}
	if got := conn.calls.Load(); got != 2 {
		t.Errorf("statements sent = %d; want 2: a rejection of a setting the retry no longer carries ends it", got)
	}
	want := []UnknownNameRejection{
		{Kind: unknownNameSetting, Name: chopt.SettingEnableAdaptiveAggregator, Retried: true},
		{Kind: unknownNameSetting, Name: chopt.SettingEnableAdaptiveAggregator},
	}
	if got := rec.all(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("hook received %+v; want %+v", got, want)
	}
	for action, n := range map[string]int64{rejectionActionRetried: 1, rejectionActionSurfaced: 1} {
		series := map[string]string{"name": chopt.SettingEnableAdaptiveAggregator, "action": action}
		if sum, _, _ := counterSum(t, reader, unknownNameCounter, series); sum != n {
			t.Errorf("%s series = %d; want %d", action, sum, n)
		}
	}
}

// TestQueryStrings_NonDroppableRejectionSurfaces: a rejection of a name that
// is not a droppable setting the statement carried is never retried. It is
// returned typed and still reported, so the re-probe still runs early.
func TestQueryStrings_NonDroppableRejectionSurfaces(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setting string
		stamp   bool
		wantErr func(error) bool
	}{
		{
			name: "a setting outside the droppable table", setting: SettingWorkload, stamp: true,
			wantErr: func(err error) bool {
				var u *UnknownNameError
				return errors.As(err, &u) && u.Kind == unknownNameSetting && u.Name == SettingWorkload
			},
		},
		{
			name: "a gate setting", setting: chopt.SettingUseQueryConditionCache, stamp: true,
			wantErr: func(err error) bool {
				var g *GateSettingUnsupportedError
				return errors.As(err, &g) && g.Gate.Name == chopt.SettingUseQueryConditionCache
			},
		},
		{
			name: "a droppable setting the statement did not carry", setting: chopt.SettingEnableAdaptiveAggregator, stamp: false,
			wantErr: func(err error) bool {
				var u *UnknownNameError
				return errors.As(err, &u) && u.Name == chopt.SettingEnableAdaptiveAggregator
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, conn := newRolledBackClient(tc.setting)
			conn.rejectAlways = !tc.stamp
			rec, reader := withObservedRejections(c)
			ctx := context.Background()
			if tc.stamp {
				ctx = WithQuerySetting(ctx, tc.setting, 0)
			}
			_, err := c.QueryStrings(ctx, "SELECT 1")
			if !tc.wantErr(err) {
				t.Fatalf("err = %#v; want the typed rejection of %s", err, tc.setting)
			}
			if got := conn.calls.Load(); got != 1 {
				t.Errorf("statements sent = %d; want 1, no retry", got)
			}
			want := UnknownNameRejection{Kind: unknownNameSetting, Name: tc.setting}
			if got := rec.all(); len(got) != 1 || got[0] != want {
				t.Errorf("hook received %+v; want [%+v]", got, want)
			}
			surfaced := map[string]string{"name": tc.setting, "action": rejectionActionSurfaced}
			if sum, _, _ := counterSum(t, reader, unknownNameCounter, surfaced); sum != 1 {
				t.Errorf("surfaced series for %s = %d; want 1", tc.setting, sum)
			}
		})
	}
}

// functionRejectingConn rejects every statement for an unknown function.
type functionRejectingConn struct {
	staleConn
	function string
}

func (c *functionRejectingConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	c.calls.Add(1)
	return nil, &clickhouse.Exception{Code: chCodeUnknownFunction, Message: "Function with name `" + c.function + "` does not exist. Maybe you meant: ['argMax']"}
}

// TestQueryCursor_UnknownFunctionSurfacesTyped: a function is part of the
// statement, so a node that does not know it is answered with the typed
// error on the first attempt, reported for the early re-probe.
func TestQueryCursor_UnknownFunctionSurfacesTyped(t *testing.T) {
	t.Parallel()
	conn := &functionRejectingConn{function: "argAndMax"}
	c := newWithConn(conn)
	rec, _ := withObservedRejections(c)

	ctx := WithQuerySetting(context.Background(), chopt.SettingEnableAdaptiveAggregator, 0)
	_, err := c.QueryCursor(ctx, "SELECT argAndMax(Value, TimeUnix) FROM t")
	var unknown *UnknownNameError
	if !errors.As(err, &unknown) || unknown.Kind != unknownNameFunction || unknown.Name != "argAndMax" {
		t.Fatalf("err = %v; want an *UnknownNameError for function argAndMax", err)
	}
	if got := conn.calls.Load(); got != 1 {
		t.Errorf("statements sent = %d; want 1, no retry", got)
	}
	if got := rec.all(); len(got) != 1 || got[0] != (UnknownNameRejection{Kind: unknownNameFunction, Name: "argAndMax"}) {
		t.Errorf("hook received %+v; want one unretried argAndMax rejection", got)
	}
}

// TestQueryCursor_RolledBackNodeAnswersThroughTheRetry: the row cursor opens
// through the same seam as every other query method.
func TestQueryCursor_RolledBackNodeAnswersThroughTheRetry(t *testing.T) {
	t.Parallel()
	c, conn := newRolledBackClient(chopt.SettingMinTableRowsToUseProjectionIndex)
	ctx := WithQuerySetting(context.Background(), chopt.SettingMinTableRowsToUseProjectionIndex, 0)
	cur, err := c.QueryCursor(ctx, "SELECT 1")
	if err != nil {
		t.Fatalf("QueryCursor: %v; want it answered through the retry", err)
	}
	_ = cur.Close()
	if got := conn.calls.Load(); got != 2 {
		t.Errorf("statements sent = %d; want 2", got)
	}
}

// TestSetUnknownNameHook_SharedByHeadViews: the hook cmd/cerberus installs on
// the root client reaches a rejection on any head's view.
func TestSetUnknownNameHook_SharedByHeadViews(t *testing.T) {
	t.Parallel()
	c, _ := newRolledBackClient(chopt.SettingEnableAdaptiveAggregator)
	rec, _ := withObservedRejections(c)
	ctx := WithQuerySetting(context.Background(), chopt.SettingEnableAdaptiveAggregator, 0)
	if _, err := c.ForHead(HeadProm).QueryStrings(ctx, "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings on the prom view: %v", err)
	}
	if got := len(rec.all()); got != 1 {
		t.Errorf("hook calls through the prom view = %d; want 1", got)
	}
}

// TestCapabilityCanaries_RejectionIsNotReported: a capability canary sends a
// setting to learn whether the server accepts it, so its rejection is the
// verdict it asks for — never counted or handed to the hook as a node older
// than the set in force, which would keep the re-probe requesting itself.
func TestCapabilityCanaries_RejectionIsNotReported(t *testing.T) {
	t.Parallel()
	canaries := map[string]func(*Client) chopt.Capability{
		"workload": func(c *Client) chopt.Capability {
			return c.ProbeQueryWorkloadCapability(context.Background(), "cerberus_queries")
		},
		"result cache": func(c *Client) chopt.Capability { return c.ProbeResultCacheCapability(context.Background()) },
		"ts grid":      func(c *Client) chopt.Capability { return c.ProbeTSGridCapability(context.Background()) },
		"query-log union": func(c *Client) chopt.Capability {
			return c.ProbeQueryLogUnionCapability(context.Background())
		},
	}
	for name, probe := range canaries {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, conn := newRolledBackClient("cerberus_canary_setting")
			conn.rejectAlways = true
			rec, reader := withObservedRejections(c)
			if got := probe(c); got != chopt.CapabilityForbidden {
				t.Fatalf("verdict = %v; want Forbidden for a rejected canary", got)
			}
			if got := rec.all(); len(got) != 0 {
				t.Errorf("hook received %+v; want nothing from a canary", got)
			}
			if sum, _, _ := counterSum(t, reader, unknownNameCounter, map[string]string{"action": rejectionActionSurfaced}); sum != 0 {
				t.Errorf("surfaced rejections = %d; want 0 from a canary", sum)
			}
			if got := conn.calls.Load(); got != 1 {
				t.Errorf("statements sent = %d; want 1", got)
			}
		})
	}
}

// TestQueryStrings_RejectionOfEveryAttemptIsBounded: a node that rejects a
// droppable setting whatever the statement carries gets at most
// maxDroppableAttempts statements, and the query is answered with the typed
// rejection of the setting.
func TestQueryStrings_RejectionOfEveryAttemptIsBounded(t *testing.T) {
	t.Parallel()
	for _, d := range chopt.DroppableSettings() {
		t.Run(d.Name, func(t *testing.T) {
			t.Parallel()
			c, conn := newRolledBackClient(d.Name)
			conn.rejectAlways = true
			_, err := c.QueryStrings(withEveryDroppableSetting(context.Background()), "SELECT 1")
			if got, limit := int(conn.calls.Load()), maxDroppableAttempts(); got > limit {
				t.Errorf("statements sent = %d; want at most %d", got, limit)
			}
			var unknown *UnknownNameError
			if !errors.As(err, &unknown) || unknown.Name != d.Name {
				t.Fatalf("err = %#v; want an *UnknownNameError for %s", err, d.Name)
			}
		})
	}
}

// TestRunDroppableRetry_NonConvergingRetryIsBounded: a classifier that keeps
// asking for a retry, the shape of a broken drop rule, stops after
// maxDroppableAttempts statements with a *DroppableRetryInvariantError naming
// the setting and the count. The last rejection is reported as surfaced, and
// the error reaches the caller unchanged through classifyDriverErr.
func TestRunDroppableRetry_NonConvergingRetryIsBounded(t *testing.T) {
	t.Parallel()
	c := newWithConn(&staleConn{})
	rec, reader := withObservedRejections(c)
	rejection := &clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Unknown setting '" + chopt.SettingEnableAdaptiveAggregator + "'"}
	opens := 0
	open := func(context.Context) (struct{}, error) {
		opens++
		return struct{}{}, rejection
	}
	stuck := func(ctx context.Context, _ error) (context.Context, UnknownNameRejection, bool) {
		return ctx, UnknownNameRejection{Kind: unknownNameSetting, Name: chopt.SettingEnableAdaptiveAggregator, Retried: true}, true
	}

	_, _, err := runDroppableRetry(c.rejections, context.Background(), open, stuck, nil)
	limit := maxDroppableAttempts()
	if opens != limit {
		t.Errorf("statements sent = %d; want %d", opens, limit)
	}
	var inv *DroppableRetryInvariantError
	if !errors.As(err, &inv) || inv.Setting != chopt.SettingEnableAdaptiveAggregator || inv.Attempts != limit {
		t.Fatalf("err = %#v; want a *DroppableRetryInvariantError for %s after %d statements", err, chopt.SettingEnableAdaptiveAggregator, limit)
	}
	for _, want := range []string{chopt.SettingEnableAdaptiveAggregator, fmt.Sprint(limit), "invariant"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not name %q", err, want)
		}
	}
	if !errors.Is(err, ErrDroppableRetryInvariant) {
		t.Error("the error does not unwrap to ErrDroppableRetryInvariant")
	}
	var cause *clickhouse.Exception
	if !errors.As(err, &cause) || cause != rejection {
		t.Error("the node's last rejection is not reachable through the error")
	}
	if got := c.classifyDriverErr(context.Background(), err); got != err {
		t.Errorf("classifyDriverErr = %#v; want the invariant error unchanged", got)
	}
	got := rec.all()
	if len(got) != limit || got[len(got)-1].Retried {
		t.Fatalf("hook received %+v; want %d rejections, the last one not retried", got, limit)
	}
	for action, n := range map[string]int64{rejectionActionRetried: int64(limit - 1), rejectionActionSurfaced: 1} {
		series := map[string]string{"name": chopt.SettingEnableAdaptiveAggregator, "action": action}
		if sum, _, _ := counterSum(t, reader, unknownNameCounter, series); sum != n {
			t.Errorf("%s series = %d; want %d", action, sum, n)
		}
	}
}
