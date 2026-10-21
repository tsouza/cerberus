package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestMountAPIHeads_EveryHeadSharesTheStrategiesCell pins that every head
// renders under the one SchemaStrategies cell runRequirementsCheck returns,
// so a strategy the schema re-probe stores reaches each of them.
func TestMountAPIHeads_EveryHeadSharesTheStrategiesCell(t *testing.T) {
	t.Setenv("CERBERUS_ENABLED_HEADS", "prom,loki,tempo")
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logger := quietLogger()
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	heads, err := mountAPIHeads(ctx, http.NewServeMux(), lazyClient(t), cfg, chopt.EnabledSet{}, nil,
		newAdmitLimiters(cfg, logger), logger, engine.ResourceBoundOverrides{}, promql.ResourceBounds{},
		preflightProbes{Strategies: cell})
	if err != nil {
		t.Fatalf("mountAPIHeads: %v", err)
	}
	if heads.consumers.prom.Strategies != cell {
		t.Error("prom head does not render under the shared strategies cell")
	}
	if heads.loki.Strategies != cell {
		t.Error("loki head does not render under the shared strategies cell")
	}
	if heads.tempo.Strategies != cell {
		t.Error("tempo head does not render under the shared strategies cell")
	}
}

// TestRunRequirementsCheck_DisabledRendersConfiguredPrecision pins that with
// the requirements check disabled the metrics head renders for the
// configured timestamp precision — the one cerberus provisions the tables
// with — since nothing detects the deployed one.
func TestRunRequirementsCheck_DisabledRendersConfiguredPrecision(t *testing.T) {
	t.Parallel()
	for _, precision := range []schema.MetricsTimestampPrecision{schema.MetricsTimestampNanos, schema.MetricsTimestampSeconds} {
		var cfg config.Config
		cfg.SchemaProvisioning.MetricsTimestampPrecision = precision
		_, probes, err := runRequirementsCheck(context.Background(), quietLogger(), nil, cfg)
		if err != nil {
			t.Fatalf("disabled check: %v", err)
		}
		if got := probes.Strategies.Load().Metrics.MetricsTimestamp; got != precision {
			t.Errorf("configured %v: heads render for %v", precision, got)
		}
	}
}

// columnsQuerier answers the boot probe from a fixed system.columns map,
// keyed by the table the introspection SQL binds as its second argument.
type columnsQuerier struct {
	columns map[string][]chclient.NameTypePair
}

func (columnsQuerier) QueryStrings(context.Context, string, ...any) ([]string, error) {
	return []string{"25.8.2.1"}, nil
}

func (q columnsQuerier) QueryNameTypePairs(_ context.Context, _ string, args ...any) ([]chclient.NameTypePair, error) {
	if len(args) < 2 {
		return nil, errors.New("columnsQuerier: expected (database, table) args")
	}
	table, _ := args[1].(string)
	return q.columns[table], nil
}

// lateSchemaColumns is the system.columns answer for tables created after
// boot with JSON-typed logs and traces resource attributes and DateTime
// metrics timestamps.
func lateSchemaColumns() map[string][]chclient.NameTypePair {
	m, l, tr := schema.DefaultOTelMetrics(), schema.DefaultOTelLogs(), schema.DefaultOTelTraces()
	col := func(name, typ string) chclient.NameTypePair { return chclient.NameTypePair{Name: name, Type: typ} }
	const mapType, jsonType, timestampType = "Map(String, String)", "JSON", "DateTime"
	metricCols := func(valueCols ...chclient.NameTypePair) []chclient.NameTypePair {
		return append([]chclient.NameTypePair{
			col(m.MetricNameColumn, "String"), col(m.TimestampColumn, timestampType),
			col(m.ServiceNameColumn, "LowCardinality(String)"),
			col(m.AttributesColumn, mapType), col(m.ResourceAttributesColumn, mapType),
		}, valueCols...)
	}
	sample := metricCols(col(m.ValueColumn, "Float64"))
	hist := metricCols(col(m.CountColumn, "UInt64"), col(m.SumColumn, "Float64"))
	return map[string][]chclient.NameTypePair{
		m.GaugeTable: sample, m.SumTable: sample, m.HistogramTable: hist, m.ExpHistogramTable: hist,
		l.LogsTable: {
			col(l.TimestampColumn, "DateTime64(9)"), col(l.BodyColumn, "String"),
			col(l.ServiceNameColumn, "LowCardinality(String)"),
			col(l.AttributesColumn, mapType), col(l.ResourceAttributesColumn, jsonType),
		},
		tr.SpansTable: {
			col(tr.TraceIDColumn, "String"), col(tr.SpanIDColumn, "String"),
			col(tr.SpanNameColumn, "LowCardinality(String)"), col(tr.ServiceNameColumn, "LowCardinality(String)"),
			col(tr.DurationColumn, "UInt64"), col(tr.StartTimeColumn, "DateTime64(9)"),
			col(tr.AttributesColumn, mapType), col(tr.ResourceAttributesColumn, jsonType),
		},
	}
}

// TestReprobeSchema_StoresEveryResolvedStrategy pins that the schema
// re-probe — the path a boot takes when its tables do not exist yet — has
// stored every strategy the late tables resolve to, for every signal, by the
// moment it reports ready: the logs and traces attribute strategies as well
// as the metrics timestamp precision. The cell is read inside markPresent,
// so a request /readyz admits right after the flip cannot render under the
// configured strategies.
func TestReprobeSchema_StoresEveryResolvedStrategy(t *testing.T) {
	t.Parallel()
	req := preflight.Requirements{
		Database: "otel",
		Metrics:  schema.DefaultOTelMetrics(),
		Logs:     schema.DefaultOTelLogs(),
		Traces:   schema.DefaultOTelTraces(),
	}
	cell := chsql.NewSchemaStrategiesCell(chsql.ResolvedSchemaStrategies{})
	signal := &cellAtReady{schemaPresentSignal: newSchemaPresentSignal("tables absent"), cell: cell}
	const reprobeInterval = time.Millisecond
	reprobeSchema(t.Context(), quietLogger(), columnsQuerier{columns: lateSchemaColumns()}, req, signal, cell, reprobeInterval)

	if present, reason := signal.Func()(); !present {
		t.Fatalf("re-probe did not report ready: %s", reason)
	}
	if signal.atReady == nil {
		t.Fatal("re-probe never flipped readiness")
	}
	got := *signal.atReady
	if s := got.Logs.Attrs.Lookup(req.Logs.ResourceAttributesColumn); s != chsql.AttrStrategyJSON {
		t.Errorf("logs %s strategy at ready = %v, want JSON", req.Logs.ResourceAttributesColumn, s)
	}
	if s := got.Traces.Attrs.Lookup(req.Traces.ResourceAttributesColumn); s != chsql.AttrStrategyJSON {
		t.Errorf("traces %s strategy at ready = %v, want JSON", req.Traces.ResourceAttributesColumn, s)
	}
	if p := got.Metrics.MetricsTimestamp; p != schema.MetricsTimestampSeconds {
		t.Errorf("metrics timestamp precision at ready = %v, want seconds", p)
	}
}

// cellAtReady is a schemaPresentSignal that records what cell holds at the
// moment readiness flips.
type cellAtReady struct {
	*schemaPresentSignal
	cell    *chsql.SchemaStrategiesCell
	atReady *chsql.ResolvedSchemaStrategies
}

func (c *cellAtReady) markPresent() {
	got := c.cell.Load()
	c.atReady = &got
	c.schemaPresentSignal.markPresent()
}

// TestRunRequirementsCheck_NotReadyRendersConfiguredPrecision pins that a
// boot whose server cannot be introspected yet renders for the configured
// metrics timestamp precision until the re-probe stores the deployed one.
func TestRunRequirementsCheck_NotReadyRendersConfiguredPrecision(t *testing.T) {
	t.Parallel()
	cfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv: %v", err)
	}
	cfg.RequirementsCheck = true
	cfg.SchemaProvisioning.MetricsTimestampPrecision = schema.MetricsTimestampSeconds
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	present, probes, err := runRequirementsCheck(ctx, quietLogger(), lazyClient(t), cfg)
	if err != nil {
		t.Fatalf("runRequirementsCheck: %v", err)
	}
	if present == nil {
		t.Fatal("an unreachable server must boot not ready")
	}
	if ready, _ := present(); ready {
		t.Fatal("an unreachable server reported ready")
	}
	if got := probes.Strategies.Load().Metrics.MetricsTimestamp; got != schema.MetricsTimestampSeconds {
		t.Errorf("not-ready heads render for %v, want the configured seconds", got)
	}
}
