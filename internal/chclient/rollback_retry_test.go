package chclient

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// rolledBackConn models a ClickHouse node rolled back below the release that
// introduced some settings: it rejects every statement whose settings carry
// one of unknown with UNKNOWN_SETTING, in the wording the native protocol
// uses, and runs every other statement. The settings it judges are the ones
// Client.querySettings derives from the statement's context — the map
// queryContext hands the driver.
type rolledBackConn struct {
	staleConn
	client  *Client
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
	settings := c.client.querySettings(ctx)
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
	c := newWithConn(conn)
	conn.client = c
	return c, conn
}

// TestQueryStrings_RolledBackNodeAnswersThroughTheRetry is a rolling
// rollback from 26.8 to 26.6 seen from one query: the set in force was
// resolved against 26.8, so the engine stamps enable_adaptive_aggregator=0 on
// an exponential-histogram window query, and the rolled-back node does not
// know the setting. The setting only bounds memory, so the query must answer
// — sent once more without the droppable settings it carried — rather than
// fail until the next capability re-probe. Every other setting rides the
// second attempt unchanged.
func TestQueryStrings_RolledBackNodeAnswersThroughTheRetry(t *testing.T) {
	t.Parallel()
	c, conn := newRolledBackClient("enable_adaptive_aggregator")

	ctx := WithQuerySetting(context.Background(), "enable_adaptive_aggregator", 0)
	ctx = WithQuerySetting(ctx, "max_bytes_before_external_join", 1<<20)
	ctx = WithQuerySetting(ctx, "enable_analyzer", 0)
	if _, err := c.QueryStrings(ctx, "SELECT 1"); err != nil {
		t.Fatalf("QueryStrings on a node that does not know enable_adaptive_aggregator: %v; want it answered", err)
	}

	sent := conn.attempts()
	if len(sent) != 2 {
		t.Fatalf("statements sent = %d; want 2 (the rejected one and one retry)", len(sent))
	}
	retry := sent[1]
	for _, dropped := range []string{"enable_adaptive_aggregator", "max_bytes_before_external_join"} {
		if _, ok := retry[dropped]; ok {
			t.Errorf("the retry still carries droppable setting %s: %v", dropped, retry)
		}
	}
	for name, value := range sent[0] {
		if name == "enable_adaptive_aggregator" || name == "max_bytes_before_external_join" {
			continue
		}
		if got, ok := retry[name]; !ok || got != value {
			t.Errorf("the retry's %s = %v (present %v); want the first attempt's %v", name, got, ok, value)
		}
	}
	if len(retry) != len(sent[0])-2 {
		t.Errorf("the retry carries %d settings; want the first attempt's %d less the 2 droppable ones: %v", len(retry), len(sent[0]), retry)
	}
	if st := c.br.currentState(); st != "closed" {
		t.Errorf("breaker state = %q; want closed", st)
	}
}
