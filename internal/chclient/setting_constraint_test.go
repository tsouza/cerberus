package chclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestParseSettingConstraint pins the setting read off each wording a
// settings profile's constraint refuses a value with, as captured from 24.8
// and 26.8 servers: a CONST or READONLY constraint, a MIN and a MAX. The
// analyzer-mandatory refusal shares the code and is not a profile's.
func TestParseSettingConstraint(t *testing.T) {
	t.Parallel()
	for message, want := range map[string]string{
		"Setting query_plan_merge_filters should not be changed. (SETTING_CONSTRAINT_VIOLATION)":                                       "query_plan_merge_filters",
		"Setting query_plan_merge_filters should not be changed. (SETTING_CONSTRAINT_VIOLATION) (version 24.8.14.39 (official build))": "query_plan_merge_filters",
		"Setting max_threads shouldn't be less than 2. (SETTING_CONSTRAINT_VIOLATION)":                                                 "max_threads",
		"Setting max_threads shouldn't be greater than 8. (SETTING_CONSTRAINT_VIOLATION)":                                              "max_threads",
	} {
		got, ok := parseSettingConstraint(fmt.Errorf("wrapped: %w", &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: message}))
		if !ok || got != want {
			t.Errorf("%q: parseSettingConstraint = %q, %v; want %q", message, got, ok, want)
		}
	}
	for name, err := range map[string]error{
		"analyzer mandatory": &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Message: "Setting 'enable_analyzer' cannot be disabled: the analyzer is mandatory since v26.9"},
		"other code":         &clickhouse.Exception{Code: chCodeUnknownSetting, Message: "Setting query_plan_merge_filters should not be changed."},
		"no exception":       errors.New("dial: refused"),
		"nil":                nil,
	} {
		if got, ok := parseSettingConstraint(err); ok {
			t.Errorf("%s: parseSettingConstraint = %q; want no refusal", name, got)
		}
	}
}

// constrainedConn models a node whose user's settings profile holds refused
// at its server default: a statement that sets it is refused with
// SETTING_CONSTRAINT_VIOLATION in the server's wording, and every other
// statement runs.
type constrainedConn struct {
	staleConn
	refused string

	calls atomic.Int32
	mu    sync.Mutex
	sent  []clickhouse.Settings
}

func (c *constrainedConn) Query(ctx context.Context, _ string, _ ...any) (driver.Rows, error) {
	c.calls.Add(1)
	settings := driverSettingsFromContext(ctx)
	c.mu.Lock()
	c.sent = append(c.sent, settings)
	c.mu.Unlock()
	if _, carried := settings[c.refused]; carried {
		return nil, &clickhouse.Exception{Code: chCodeSettingConstraintViolation, Name: "DB::Exception", Message: "Setting " + c.refused + " should not be changed."}
	}
	return &chaosRows{}, nil
}

func (c *constrainedConn) attempts() []clickhouse.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clickhouse.Settings(nil), c.sent...)
}

// TestQueryStrings_ProfileRefusingADroppableSettingAnswers: a settings
// profile that refuses query_plan_merge_filters gets the statement again
// without that setting alone, and the query answers. The refusal is counted
// on the constrained series and never reaches the re-probe hook: the node's
// build is not what refused it.
func TestQueryStrings_ProfileRefusingADroppableSettingAnswers(t *testing.T) {
	t.Parallel()
	conn := &constrainedConn{refused: chopt.SettingQueryPlanMergeFilters}
	c := newWithConn(conn)
	rec, reader := withObservedRejections(c)

	ctx := withEveryDroppableSetting(context.Background())
	ctx = WithQuerySetting(ctx, chopt.SettingQueryPlanMergeFilters, 1)
	if _, err := c.QueryStrings(ctx, "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings under a profile refusing %s: %v; want it answered", chopt.SettingQueryPlanMergeFilters, err)
	}
	sent := conn.attempts()
	if len(sent) != 2 {
		t.Fatalf("statements sent = %d; want 2 (the refused one and one retry)", len(sent))
	}
	assertRetryKeeps(t, sent[0], sent[1], chopt.SettingQueryPlanMergeFilters)
	if got := rec.all(); len(got) != 0 {
		t.Errorf("hook received %+v; want nothing: a profile's refusal asks for no re-probe", got)
	}
	for action, want := range map[string]int64{rejectionActionConstrained: 1, rejectionActionRetried: 0, rejectionActionSurfaced: 0} {
		series := map[string]string{"name": chopt.SettingQueryPlanMergeFilters, "action": action}
		if sum, _, _ := counterSum(t, reader, unknownNameCounter, series); sum != want {
			t.Errorf("%s series = %d; want %d", action, sum, want)
		}
	}
}

// TestQueryStrings_ProfileRefusingAnotherSettingSurfaces: a profile's refusal
// of a setting outside the droppable table, or of a droppable setting sent at
// a value it may not be dropped at (the condition cache's gate value), is the
// caller's answer, untouched and unreported.
func TestQueryStrings_ProfileRefusingAnotherSettingSurfaces(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setting string
		value   any
	}{
		{name: "a setting outside the droppable table", setting: SettingWorkload, value: "w"},
		{name: "a gate setting at its gate value", setting: chopt.SettingUseQueryConditionCache, value: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := &constrainedConn{refused: tc.setting}
			c := newWithConn(conn)
			rec, _ := withObservedRejections(c)
			_, err := c.QueryStrings(WithQuerySetting(context.Background(), tc.setting, tc.value), "SELECT 1")
			var ex *clickhouse.Exception
			if !errors.As(err, &ex) || ex.Code != chCodeSettingConstraintViolation {
				t.Fatalf("err = %v; want the SETTING_CONSTRAINT_VIOLATION itself", err)
			}
			if got := conn.calls.Load(); got != 1 {
				t.Errorf("statements sent = %d; want 1, no retry", got)
			}
			if got := rec.all(); len(got) != 0 {
				t.Errorf("hook received %+v; want nothing", got)
			}
		})
	}
}

// TestDroppableRetry_ProfileRefusalNotRetried covers the two ways a profile's
// refusal of a droppable setting is not retried. A capability canary's
// refusal is the answer it asks for, so it is neither retried nor reported. A
// statement whose retry the caller vetoes (the columnar path, once the server
// has answered a packet) returns the refusal itself, counted as surfaced and
// still kept from the re-probe hook.
func TestDroppableRetry_ProfileRefusalNotRetried(t *testing.T) {
	t.Parallel()
	ctx := WithQuerySetting(context.Background(), chopt.SettingQueryPlanMergeFilters, 1)

	t.Run("capability canary", func(t *testing.T) {
		t.Parallel()
		conn := &constrainedConn{refused: chopt.SettingQueryPlanMergeFilters}
		c := newWithConn(conn)
		rec, reader := withObservedRejections(c)
		if _, err := c.QueryStrings(withCapabilityCanary(ctx), "SELECT 1"); err == nil {
			t.Fatal("a canary's refused statement answered; want the refusal")
		}
		if got := conn.calls.Load(); got != 1 {
			t.Errorf("statements sent = %d; want 1, no retry", got)
		}
		if got := rec.all(); len(got) != 0 {
			t.Errorf("hook received %+v; want nothing from a canary", got)
		}
		if sum, _, _ := counterSum(t, reader, unknownNameCounter, map[string]string{"name": chopt.SettingQueryPlanMergeFilters}); sum != 0 {
			t.Errorf("rejections counted = %d; want 0 from a canary", sum)
		}
	})

	t.Run("retry vetoed", func(t *testing.T) {
		t.Parallel()
		conn := &constrainedConn{refused: chopt.SettingQueryPlanMergeFilters}
		c := newWithConn(conn)
		rec, reader := withObservedRejections(c)
		open := func(ctx context.Context) (driver.Rows, error) { return conn.Query(ctx, "SELECT 1") }
		_, _, err := runDroppableRetry(c.rejections, withDriverSettings(ctx, c.querySettings(ctx)), open, c.droppableRetryContext, func() bool { return false })
		var ex *clickhouse.Exception
		if !errors.As(err, &ex) || ex.Code != chCodeSettingConstraintViolation {
			t.Fatalf("err = %v; want the SETTING_CONSTRAINT_VIOLATION itself", err)
		}
		if got := conn.calls.Load(); got != 1 {
			t.Errorf("statements sent = %d; want 1, the retry vetoed", got)
		}
		if got := rec.all(); len(got) != 0 {
			t.Errorf("hook received %+v; want nothing: a profile's refusal asks for no re-probe", got)
		}
		surfaced := map[string]string{"name": chopt.SettingQueryPlanMergeFilters, "action": rejectionActionSurfaced}
		if sum, _, _ := counterSum(t, reader, unknownNameCounter, surfaced); sum != 1 {
			t.Errorf("surfaced series = %d; want 1", sum)
		}
	})
}
