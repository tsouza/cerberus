//go:build integration

package chserver

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
)

// droppableSettingBuilds bracket each chopt.DroppableSettings entry's first
// release: 25.3 predates the lazy-materialisation settings (25.4), 25.10
// predates min_table_rows_to_use_projection_index (25.11), 26.3 predates
// max_bytes_before_external_join (26.4), 26.6 — the rollback target of the
// supported 26.6 to 26.8 upgrade — predates enable_adaptive_aggregator
// (26.8), 26.8 predates max_bytes_before_external_distinct (26.9), and 26.9
// knows every one. Each is a build this package already pins.
var droppableSettingBuilds = []string{
	"clickhouse/clickhouse-server:25.3.14.14-alpine",
	"clickhouse/clickhouse-server:25.10.7.6-alpine",
	"clickhouse/clickhouse-server:26.3.17.56-alpine",
	"clickhouse/clickhouse-server:26.6.1.1193-alpine",
	"clickhouse/clickhouse-server:26.8.10.6-alpine",
	"clickhouse/clickhouse-server:26.9.25.15-alpine",
}

// unknownFunctionImage predates argAndMax (25.11), which arg_and_max_fusion
// emits.
const unknownFunctionImage = "clickhouse/clickhouse-server:25.10.7.6-alpine"

// codeUnknownFunction is ClickHouse's UNKNOWN_FUNCTION error code.
const codeUnknownFunction = 46

// rejections records what a client's unknown-name hook receives.
type rejections struct {
	mu   sync.Mutex
	seen []chclient.UnknownNameRejection
}

func (r *rejections) record(rej chclient.UnknownNameRejection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, rej)
}

func (r *rejections) take() []chclient.UnknownNameRejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.seen
	r.seen = nil
	return out
}

// withDroppableSettings stamps every droppable setting on ctx, the way the
// engine's settings rules do.
func withDroppableSettings(ctx context.Context) context.Context {
	for _, d := range chopt.DroppableSettings() {
		ctx = chclient.WithQuerySetting(ctx, d.Name, 0)
	}
	return ctx
}

// TestDroppableSettings_RolledBackNodeAnswers pins chopt.DroppableSettings
// against real servers, the way production sends a setting, over the native
// protocol:
//
//   - each setting's first release: a build below Since rejects it with
//     UNKNOWN_SETTING, a build at or above accepts it;
//   - a query carrying every droppable setting answers on every build. On a
//     build too old for one of them, the client reports one retried
//     rejection and sends the query again without them; on a build that
//     knows them all, it reports nothing;
//   - the columnar matrix decode retries the same way.
func TestDroppableSettings_RolledBackNodeAnswers(t *testing.T) {
	for _, image := range droppableSettingBuilds {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s := startServer(ctx, t, image)

			tooOld := false
			for _, d := range chopt.DroppableSettings() {
				rows, err := s.admin.Conn().Query(clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{d.Name: 0})), "SELECT 1")
				if err == nil {
					_ = rows.Close()
				}
				if !s.version.Less(d.Since) {
					if err != nil {
						t.Errorf("%s=0 on %s (at or above %s): %v", d.Name, s.version, d.Since, err)
					}
					continue
				}
				tooOld = true
				if code := exceptionCode(err); code != codeUnknownSetting {
					t.Errorf("%s=0 on %s (below %s): err = %v, code %d; want UNKNOWN_SETTING", d.Name, s.version, d.Since, err, code)
				}
			}

			for _, columnar := range []bool{false, true} {
				c := s.client(t, adminUser, adminPassword, chclient.Config{ColumnarMatrixDecode: columnar})
				rec := &rejections{}
				c.SetUnknownNameHook(rec.record)
				cur, err := c.QueryCursor(withDroppableSettings(ctx),
					"SELECT 'm' AS MetricName, map('k', 'v') AS Attributes, toDateTime64('2026-01-01 00:00:00', 9, 'UTC') AS TimeUnix, 1.5 AS Value")
				if err != nil {
					t.Fatalf("columnar=%v: query carrying every droppable setting on %s: %v; want it answered", columnar, s.version, err)
				}
				n := 0
				for cur.Next() {
					n++
				}
				if err := cur.Err(); err != nil || n != 1 {
					t.Errorf("columnar=%v on %s: %d samples, err %v; want 1 sample", columnar, s.version, n, err)
				}
				_ = cur.Close()

				got := rec.take()
				if !tooOld {
					if len(got) != 0 {
						t.Errorf("columnar=%v on %s, which knows every droppable setting: rejections %+v; want none", columnar, s.version, got)
					}
					continue
				}
				if len(got) != 1 || !got[0].Retried || got[0].Kind != "setting" {
					t.Fatalf("columnar=%v on %s: rejections %+v; want one retried setting rejection", columnar, s.version, got)
				}
				if d, ok := chopt.LookupDroppableSetting(got[0].Name); !ok || !s.version.Less(d.Since) {
					t.Errorf("columnar=%v on %s: rejected %q; want a droppable setting newer than the node", columnar, s.version, got[0].Name)
				}
			}
		})
	}
}

// TestUnknownFunction_SurfacesTyped: a node older than a function cerberus
// emits answers the query with an *UnknownNameError naming the function,
// reported once and never retried — the function is part of the statement.
func TestUnknownFunction_SurfacesTyped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	s := startServer(ctx, t, unknownFunctionImage)
	c := s.client(t, adminUser, adminPassword, chclient.Config{})
	rec := &rejections{}
	c.SetUnknownNameHook(rec.record)

	_, err := c.QueryStrings(ctx, "SELECT toString(argAndMax(number, number)) FROM numbers(3)")
	if code := exceptionCode(err); code != codeUnknownFunction {
		t.Fatalf("argAndMax on %s: err = %v, code %d; want UNKNOWN_FUNCTION", s.version, err, code)
	}
	var unknown *chclient.UnknownNameError
	if !errors.As(err, &unknown) || unknown.Kind != "function" || unknown.Name != "argAndMax" {
		t.Errorf("argAndMax on %s: err = %v; want an *UnknownNameError for function argAndMax", s.version, err)
	}
	if !errors.Is(err, chclient.ErrNodeTooOld) {
		t.Errorf("argAndMax on %s: err does not unwrap to ErrNodeTooOld", s.version)
	}
	if got := rec.take(); len(got) != 1 || got[0].Retried || got[0].Name != "argAndMax" {
		t.Errorf("argAndMax on %s: rejections %+v; want one unretried argAndMax rejection", s.version, got)
	}
}

// columnarPoolBuilds are the two ways a server leaves a connection after
// rejecting a statement before answering it: 25.10 keeps it open and reads
// the next statement out of step, 26.6 drops it.
var columnarPoolBuilds = []string{
	"clickhouse/clickhouse-server:25.10.7.6-alpine",
	"clickhouse/clickhouse-server:26.6.1.1193-alpine",
}

// TestColumnar_RejectedStatementDoesNotPoisonThePool: on a one-connection
// columnar pool, a statement the server rejects outright is followed by
// statements that answer. The connection the rejection left behind must not
// be handed to them.
func TestColumnar_RejectedStatementDoesNotPoisonThePool(t *testing.T) {
	const matrix = "SELECT 'm' AS MetricName, map('k', 'v') AS Attributes, toDateTime64('2026-01-01 00:00:00', 9, 'UTC') AS TimeUnix, 1.5 AS Value"
	for _, image := range columnarPoolBuilds {
		t.Run(image, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s := startServer(ctx, t, image)
			c := s.client(t, adminUser, adminPassword, chclient.Config{ColumnarMatrixDecode: true, MaxOpenConns: 1})

			_, err := c.QueryCursor(chclient.WithQuerySetting(ctx, "cerberus_no_such_setting", 0), matrix)
			if code := exceptionCode(err); code != codeUnknownSetting {
				t.Fatalf("premise: an unknown setting on %s: err = %v, code %d; want UNKNOWN_SETTING", s.version, err, code)
			}
			for i := range 2 {
				cur, err := c.QueryCursor(ctx, matrix)
				if err != nil {
					t.Fatalf("statement %d after the rejection on %s: %v; want it answered", i+1, s.version, err)
				}
				_ = cur.Close()
			}
		})
	}
}
