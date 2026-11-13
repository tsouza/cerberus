package chclient

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/chopt"
)

// rolledBackConn models a ClickHouse node rolled back below the release that
// introduced some settings. It judges each statement by the settings the
// driver was handed for it (driverSettingsFromContext), the settings a real
// driver would send. A statement carrying one of unknown is rejected with
// UNKNOWN_SETTING, in the native protocol's wording, naming the first of
// unknown it carries; every other statement runs.
type rolledBackConn struct {
	staleConn
	unknown []string
	// rejectAlways rejects every statement for unknown[0], whatever it
	// carries: a node that refuses the retry too.
	rejectAlways bool

	calls atomic.Int32
	mu    sync.Mutex
	sent  []clickhouse.Settings
}

func (c *rolledBackConn) Query(ctx context.Context, _ string, _ ...any) (driver.Rows, error) {
	c.calls.Add(1)
	settings := driverSettingsFromContext(ctx)
	c.mu.Lock()
	c.sent = append(c.sent, settings)
	c.mu.Unlock()
	for _, name := range c.unknown {
		if _, carried := settings[name]; carried || c.rejectAlways {
			return nil, &clickhouse.Exception{Code: chCodeUnknownSetting, Name: "DB::Exception", Message: "Unknown setting '" + name + "'"}
		}
	}
	return &chaosRows{}, nil
}

func (c *rolledBackConn) attempts() []clickhouse.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]clickhouse.Settings(nil), c.sent...)
}

// newRolledBackClient builds a client over a rolledBackConn rejecting unknown.
func newRolledBackClient(unknown ...string) (*Client, *rolledBackConn) {
	conn := &rolledBackConn{unknown: unknown}
	return newWithConn(conn), conn
}

// unknownTo lists, in table order, the droppable settings a node running
// build node does not know.
func unknownTo(node chopt.Version) []string {
	var out []string
	for _, d := range chopt.DroppableSettings() {
		if node.Less(d.Since) {
			out = append(out, d.Name)
		}
	}
	return out
}

// withEveryDroppableSetting stamps every droppable setting at a value it may
// be dropped at, the way the engine's settings rules would on a set resolved
// against a build that knows them all.
func withEveryDroppableSetting(ctx context.Context) context.Context {
	for _, d := range chopt.DroppableSettings() {
		value := any(0)
		if d.OnlyAt != nil {
			value = d.OnlyAt
		}
		ctx = WithQuerySetting(ctx, d.Name, value)
	}
	return ctx
}

// assertRetryKeeps fails unless retry carries exactly first's settings less
// dropped.
func assertRetryKeeps(t *testing.T, first, retry clickhouse.Settings, dropped ...string) {
	t.Helper()
	gone := map[string]bool{}
	for _, name := range dropped {
		gone[name] = true
		if _, ok := retry[name]; ok {
			t.Errorf("the retry still carries %s, which the node does not know", name)
		}
	}
	for name, value := range first {
		if gone[name] {
			continue
		}
		if got, ok := retry[name]; !ok || got != value {
			t.Errorf("the retry's %s = %v (present %v); want the first attempt's %v", name, got, ok, value)
		}
	}
	if len(retry) != len(first)-len(dropped) {
		t.Errorf("the retry carries %d settings; want the first attempt's %d less %d dropped: %v", len(retry), len(first), len(dropped), retry)
	}
}

// Builds a rolled-back node can run: the rollback target of the supported
// 26.6 to 26.8 upgrade, and a node old enough to lack two release lines of
// droppable settings.
var (
	rollbackTarget = chopt.Version{Major: 26, Minor: 6, Patch: 8, Build: 7}
	olderNode      = chopt.Version{Major: 26, Minor: 3}
)

// TestQueryStrings_RolledBackNodeAnswersThroughTheRetry is a rolling
// rollback from 26.8 to 26.6 seen from one query: the set in force was
// resolved against 26.8, so the engine stamps enable_adaptive_aggregator=0 on
// an exponential-histogram window query, and the rolled-back node does not
// know the setting. The setting only bounds memory, so the query must answer
// rather than fail until the next capability re-probe. The retry drops only
// what a 26.6 node cannot know — the droppable settings released at or after
// 26.8 — and keeps the join spill threshold (26.4), the lazy-materialisation
// settings (25.4), the projection-index threshold (25.11) and every other
// setting unchanged.
func TestQueryStrings_RolledBackNodeAnswersThroughTheRetry(t *testing.T) {
	t.Parallel()
	c, conn := newRolledBackClient(unknownTo(rollbackTarget)...)

	ctx := withEveryDroppableSetting(context.Background())
	ctx = WithQuerySetting(ctx, "enable_analyzer", 0)
	if _, err := c.QueryStrings(ctx, "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings on a node that does not know enable_adaptive_aggregator: %v; want it answered", err)
	}

	sent := conn.attempts()
	if len(sent) != 2 {
		t.Fatalf("statements sent = %d; want 2 (the rejected one and one retry)", len(sent))
	}
	assertRetryKeeps(t, sent[0], sent[1], chopt.SettingEnableAdaptiveAggregator, chopt.SettingMaxBytesBeforeExternalDistinct)
	if st := c.br.currentState(); st != "closed" {
		t.Errorf("breaker state = %q; want closed", st)
	}
}

// TestQueryStrings_OlderNodeRetriesPerRelease: a 26.3 node lacks three
// droppable settings across two release lines and reports one at a time.
// Rejected for the 26.8 setting first, the query drops what was released at
// or after 26.8; rejected again for the 26.4 join spill threshold, it drops
// that too, and answers carrying exactly the droppable settings 26.3 knows.
// Both rejections are reported, retried.
func TestQueryStrings_OlderNodeRetriesPerRelease(t *testing.T) {
	t.Parallel()
	c, conn := newRolledBackClient(unknownTo(olderNode)...)
	rec, _ := withObservedRejections(c)

	if _, err := c.QueryStrings(withEveryDroppableSetting(context.Background()), "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings on %s: %v; want it answered", olderNode, err)
	}
	sent := conn.attempts()
	if len(sent) != 3 {
		t.Fatalf("statements sent = %d; want 3", len(sent))
	}
	assertRetryKeeps(t, sent[0], sent[2],
		chopt.SettingEnableAdaptiveAggregator, chopt.SettingMaxBytesBeforeExternalDistinct, chopt.SettingMaxBytesBeforeExternalJoin)
	want := []UnknownNameRejection{
		{Kind: unknownNameSetting, Name: chopt.SettingEnableAdaptiveAggregator, Retried: true},
		{Kind: unknownNameSetting, Name: chopt.SettingMaxBytesBeforeExternalJoin, Retried: true},
	}
	if got := rec.all(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("hook received %+v; want %+v", got, want)
	}
}

// TestQueryStrings_ConditionCacheSwitchedOnIsDropped: use_query_condition_cache
// is a gate setting, but condition_cache stamping it =1 only switches the
// cache on, and a node that predates the setting has no cache. A node that
// rejects it is answered without it. Switched off by the client-wide
// override for the cache's wrong-result builds, it is the gate's stamp, and
// the rejection surfaces as *GateSettingUnsupportedError instead.
func TestQueryStrings_ConditionCacheSwitchedOnIsDropped(t *testing.T) {
	t.Parallel()
	ctx := WithQuerySetting(context.Background(), chopt.SettingUseQueryConditionCache, 1)

	c, conn := newRolledBackClient(chopt.SettingUseQueryConditionCache)
	if _, err := c.QueryStrings(ctx, "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings with the cache switched on: %v; want it answered without the setting", err)
	}
	if sent := conn.attempts(); len(sent) != 2 {
		t.Fatalf("statements sent = %d; want 2", len(sent))
	} else {
		assertRetryKeeps(t, sent[0], sent[1], chopt.SettingUseQueryConditionCache)
	}

	c, conn = newRolledBackClient(chopt.SettingUseQueryConditionCache)
	c.SetQueryConditionCacheDisabled(true)
	_, err := c.QueryStrings(ctx, "SELECT 1")
	var gate *GateSettingUnsupportedError
	if !errors.As(err, &gate) {
		t.Fatalf("err = %v; want *GateSettingUnsupportedError with the cache switched off", err)
	}
	if got := conn.calls.Load(); got != 1 {
		t.Errorf("statements sent with the cache switched off = %d; want 1, the gate's stamp is never dropped", got)
	}
}
