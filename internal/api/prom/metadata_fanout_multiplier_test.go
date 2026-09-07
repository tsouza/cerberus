package prom_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// fanoutRecordingQuerier records, per dispatched statement, the data-shard
// fan-out multiplier the handler stamped on the ctx it dispatched under
// (chclient.WithDataShardFanoutMultiplier) alongside that statement's SQL.
// It embeds stubQuerier for every method it does not need to observe.
type fanoutRecordingQuerier struct {
	stubQuerier

	mu    sync.Mutex
	stamp []fanoutStamp
}

// fanoutStamp is one dispatch: the SQL and the multiplier its ctx carried
// (ok=false meaning the dispatch stamped nothing, which charges the gate one
// single fan-out no matter how many tables the statement really scans).
type fanoutStamp struct {
	sql        string
	multiplier int
	ok         bool
}

func (q *fanoutRecordingQuerier) record(ctx context.Context, sql string) {
	n, ok := chclient.DataShardFanoutMultiplierFromContext(ctx)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.stamp = append(q.stamp, fanoutStamp{sql: sql, multiplier: n, ok: ok})
}

func (q *fanoutRecordingQuerier) snapshot() []fanoutStamp {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]fanoutStamp(nil), q.stamp...)
}

func (q *fanoutRecordingQuerier) QueryStrings(ctx context.Context, sql string, args ...any) ([]string, error) {
	q.record(ctx, sql)
	return q.stubQuerier.QueryStrings(ctx, sql, args...)
}

func (q *fanoutRecordingQuerier) Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.record(ctx, sql)
	return q.stubQuerier.Query(ctx, sql, args...)
}

func (q *fanoutRecordingQuerier) QueryMetricMeta(
	ctx context.Context, sql, metricType string, args ...any,
) ([]chclient.MetricMetaRow, error) {
	q.record(ctx, sql)
	return q.stubQuerier.QueryMetricMeta(ctx, sql, metricType, args...)
}

// TestMetadataDispatch_StampsItsOwnPhysicalScanCount pins the fan-out weight
// for the metadata endpoints, which dispatch to ClickHouse WITHOUT going
// through the engine and so must stamp the weight themselves (cerberus issue
// #3128's audit). Each of these statements UNION-ALLs one arm per configured
// metric table — several physical scans — and on a multi-data-shard
// deployment every scan is a Distributed fan-out, so charging the gate a
// flat 1 would let several of the widest statements this head builds admit
// concurrently. The assertion is deliberately relative, not a pinned
// constant: the count must equal the number of physical table references the
// dispatched SQL actually contains, so it tracks a schema with more (or
// fewer) metric tables instead of freezing today's table list.
func TestMetadataDispatch_StampsItsOwnPhysicalScanCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		path string
	}{
		{"labels", "/api/v1/labels"},
		{"label_values", "/api/v1/label/job/values"},
		{"series", "/api/v1/series?match[]=up"},
		{"metric_metadata", "/api/v1/metadata"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &fanoutRecordingQuerier{}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}

			stamps := q.snapshot()
			if len(stamps) == 0 {
				t.Fatalf("%s issued no ClickHouse statement — the test would assert nothing", tc.path)
			}
			for i, s := range stamps {
				if !s.ok {
					t.Fatalf("statement %d dispatched with no fan-out multiplier stamped; the gate would charge one fan-out for %d physical scan(s):\n%s",
						i, physicalTableRefs(t, s.sql), s.sql)
				}
				if want := physicalTableRefs(t, s.sql); s.multiplier != want {
					t.Errorf("statement %d stamped multiplier %d, want its own physical-table reference count %d:\n%s",
						i, s.multiplier, want, s.sql)
				}
			}
		})
	}
}

// physicalTableRefs counts the physical table references in a rendered
// statement, independently of the emitter's own counter — which is what
// makes this test a real cross-check rather than a restatement of that
// counter. Two spellings reach ClickHouse and both are a real per-table
// Distributed fan-out on a multi-data-shard deployment:
//
//   - a backtick-quoted table name in FROM position, one scan each;
//   - `merge(currentDatabase(), '^(a|b)$')`, which fans out to every member
//     of its alternation — the shape the metrics UNION-table selector
//     lowering builds, and the reason a "one statement, one scan" reading
//     undercounts.
func physicalTableRefs(t *testing.T, sql string) int {
	t.Helper()
	n := 0
	for _, table := range metricTablesForCounting {
		n += strings.Count(sql, "`"+table+"`")
	}
	for _, m := range mergeMembersPattern.FindAllStringSubmatch(sql, -1) {
		n += len(strings.Split(m[1], "|"))
	}
	return n
}

// metricTablesForCounting is the default OTel metrics table set this
// handler's endpoints can reach. Listed rather than derived so the test
// fails loudly if the schema grows a table the endpoints scan but nobody
// taught this oracle about.
var metricTablesForCounting = []string{
	"otel_metrics_gauge",
	"otel_metrics_sum",
	"otel_metrics_histogram",
	"otel_metrics_exponential_histogram",
}

// mergeMembersPattern captures the alternation inside a rendered
// `merge(currentDatabase(), '^(a|b)$')` table function.
var mergeMembersPattern = regexp.MustCompile(`merge\(currentDatabase\(\), '\^\(([^)]*)\)\$'\)`)
