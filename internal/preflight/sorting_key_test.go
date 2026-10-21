package preflight

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// Stock exporter metrics sorting keys as system.tables reports them.
const (
	stockMetricsKeyBeforeV0157 = "ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix)"
	stockMetricsKeyV0157       = "ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix"
	stockLogsKey               = "ServiceName, TimestampTime, Timestamp"
	cerberusTracesKey          = "ServiceName, SpanName, toDateTime(Timestamp)"
)

// TestRunResolvesDeployedSortingKeys pins that the boot probe reads every
// table's sorting key from system.tables and resolves it, per signal, to the
// bare-column prefix internal/engine matches a GROUP BY against — a different
// key per metrics table included.
func TestRunResolvesDeployedSortingKeys(t *testing.T) {
	t.Parallel()
	m, l, tr := schema.DefaultOTelMetrics(), schema.DefaultOTelLogs(), schema.DefaultOTelTraces()
	q := &stubQuerier{Version: "25.8.2.1", Columns: healthyColumns(), SortingKeys: map[string]string{
		m.GaugeTable:        stockMetricsKeyV0157,
		m.SumTable:          stockMetricsKeyBeforeV0157,
		m.HistogramTable:    "MetricName, Attributes, ServiceName, toUnixTimestamp64Nano(TimeUnix)",
		m.ExpHistogramTable: "",
		l.LogsTable:         stockLogsKey,
		tr.SpansTable:       cerberusTracesKey,
	}}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("Run: %v", res.Fatal)
	}
	want := map[string]map[string][]string{
		"metrics": {
			m.GaugeTable:        {"ServiceName", "MetricName"},
			m.SumTable:          {"ServiceName", "MetricName", "Attributes"},
			m.HistogramTable:    {"MetricName", "Attributes", "ServiceName"},
			m.ExpHistogramTable: {},
		},
		"logs":   {l.LogsTable: {"ServiceName", "TimestampTime", "Timestamp"}},
		"traces": {tr.SpansTable: {"ServiceName", "SpanName"}},
	}
	got := map[string]map[string][]string{
		"metrics": res.Strategies.Metrics.SortingKeyPrefixes,
		"logs":    res.Strategies.Logs.SortingKeyPrefixes,
		"traces":  res.Strategies.Traces.SortingKeyPrefixes,
	}
	for signal, tables := range want {
		if len(got[signal]) != len(tables) {
			t.Errorf("%s prefixes = %v, want %v", signal, got[signal], tables)
		}
		for table, prefix := range tables {
			p, ok := got[signal][table]
			if !ok || p == nil || !slices.Equal(p, prefix) {
				t.Errorf("%s table %s prefix = %#v (present %v), want %#v", signal, table, p, ok, prefix)
			}
		}
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "sorting key") {
			t.Errorf("a readable sorting key raised a warning: %s", w)
		}
	}
}

// TestRunSortingKeyUnreadableFailsClosed pins that a sorting key cerberus
// cannot read resolves to an empty prefix — no GROUP BY matches it — with a
// warning naming the table, rather than to the provisioned key.
func TestRunSortingKeyUnreadableFailsClosed(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	q := &stubQuerier{Version: "25.8.2.1", Columns: healthyColumns(), SortingKeyErr: errors.New("code: 497, ACCESS_DENIED")}
	res := Run(context.Background(), q, defaultReq())
	if res.Fatal != nil {
		t.Fatalf("an unreadable sorting key must not be fatal: %v", res.Fatal)
	}
	p, ok := res.Strategies.Metrics.SortingKeyPrefixes[m.GaugeTable]
	if !ok || len(p) != 0 {
		t.Errorf("gauge prefix = %v (present %v), want an empty prefix", p, ok)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool {
		return strings.Contains(w, "sorting key of table "+m.GaugeTable) && strings.Contains(w, "ACCESS_DENIED")
	}) {
		t.Errorf("no warning names the unreadable %s sorting key: %v", m.GaugeTable, res.Warnings)
	}
}

// TestRunSortingKeyUnlistedTableFailsClosed pins the same for a table
// system.columns lists but system.tables does not.
func TestRunSortingKeyUnlistedTableFailsClosed(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	q := &unlistedTableQuerier{stubQuerier: stubQuerier{Version: "25.8.2.1", Columns: healthyColumns()}, unlisted: m.SumTable}
	res := Run(context.Background(), q, defaultReq())
	if p, ok := res.Strategies.Metrics.SortingKeyPrefixes[m.SumTable]; !ok || len(p) != 0 {
		t.Errorf("sum prefix = %v (present %v), want an empty prefix", p, ok)
	}
	if !slices.ContainsFunc(res.Warnings, func(w string) bool {
		return strings.Contains(w, "does not list table "+m.SumTable)
	}) {
		t.Errorf("no warning names the unlisted %s: %v", m.SumTable, res.Warnings)
	}
}

// unlistedTableQuerier answers the sorting-key read for unlisted with no
// rows.
type unlistedTableQuerier struct {
	stubQuerier
	unlisted string
}

func (u *unlistedTableQuerier) QueryNameTypePairs(ctx context.Context, sql string, args ...any) ([]chclient.NameTypePair, error) {
	if isSortingKeyQuery(sql) && len(args) >= 2 && args[1] == u.unlisted {
		return nil, nil
	}
	return u.stubQuerier.QueryNameTypePairs(ctx, sql, args...)
}

// TestRunSortingKeyDistributedTableKeepsProvisionedKey pins that a
// Distributed table — whose own sorting_key is empty — resolves to no entry,
// so its statements keep the key cerberus provisions the local tables with.
func TestRunSortingKeyDistributedTableKeepsProvisionedKey(t *testing.T) {
	t.Parallel()
	m := schema.DefaultOTelMetrics()
	q := &stubQuerier{
		Version: "25.8.2.1", Columns: healthyColumns(),
		Engines: map[string]string{m.GaugeTable: distributedEngine},
	}
	res := Run(context.Background(), q, defaultReq())
	if p, ok := res.Strategies.Metrics.SortingKeyPrefixes[m.GaugeTable]; ok {
		t.Errorf("Distributed gauge resolved to prefix %v, want no entry", p)
	}
	if _, ok := res.Strategies.Metrics.SortingKeyPrefixes[m.SumTable]; !ok {
		t.Errorf("MergeTree sum has no entry")
	}
}

// TestRunSortingKeyTransportFailureIsUnreachable pins that a dial failure on
// the sorting-key read is the transient unreachable state, as it is on the
// column read.
func TestRunSortingKeyTransportFailureIsUnreachable(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{Version: "25.8.2.1", Columns: healthyColumns(), SortingKeyErr: dialRefusedErr()}
	assertTransient(t, Run(context.Background(), q, defaultReq()))
}
