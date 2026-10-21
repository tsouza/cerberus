//go:build chdb

// chDB-backed coverage of the metrics tables' deployed sorting key reaching
// the optimize_aggregation_in_order eligibility check: the boot requirements
// check (internal/preflight) reads each table's sorting_key from
// system.tables, and the engine stamps the setting exactly for the GROUP BY
// shapes that prefix that key. It lives with the metrics head's chDB suite
// because the metrics tables are the ones a stock exporter can create with a
// key cerberus does not provision.

package prom_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/preflight"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// The ORDER BY clauses of the exporter's metrics templates: the v0.157 one
// chsqltest.UpstreamDateTimeMetricsDDL renders, and the one every release
// before v0.157 shipped.
const (
	stockOrderByV0157       = "ORDER BY (ServiceName, MetricName, toStartOfHour(TimeUnix), cityHash64(Attributes), TimeUnix)"
	stockOrderByBeforeV0157 = "ORDER BY (ServiceName, MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix))"
)

// sortingKeyProbeMemCap is the statement memory cap the settings probe sizes
// its memory-bound rules from; the rule under test reads none of them.
const sortingKeyProbeMemCap = int64(1 << 30)

// cerberusMetricsDDL renders the metrics tables cerberus provisions.
func cerberusMetricsDDL(database string) ([]string, error) {
	return ddl.RenderAll(ddl.Config{Database: database}, []ddl.Signal{ddl.Metrics})
}

// stockMetricsDDLBeforeV0157 renders the stock exporter's metrics tables as
// every release before v0.157 created them: the v0.157 templates with the
// earlier sorting key, and the earlier DateTime64(9) timestamp type its
// toUnixTimestamp64Nano key element requires.
func stockMetricsDDLBeforeV0157(database string) ([]string, error) {
	stmts, err := chsqltest.UpstreamDateTimeMetricsDDL(database)
	if err != nil {
		return nil, err
	}
	for i, s := range stmts {
		if !strings.Contains(s, stockOrderByV0157) {
			return nil, &missingOrderByError{stmt: s}
		}
		s = strings.Replace(s, stockOrderByV0157, stockOrderByBeforeV0157, 1)
		stmts[i] = strings.NewReplacer(" DateTime CODEC", " DateTime64(9) CODEC", "TimeUnix DateTime,", "TimeUnix DateTime64(9),").Replace(s)
	}
	return stmts, nil
}

type missingOrderByError struct{ stmt string }

func (e *missingOrderByError) Error() string {
	return "v0.157 template carries no " + stockOrderByV0157 + ":\n" + e.stmt
}

// metricsAggOverScan builds Aggregate(GROUP BY cols) over Scan(table).
func metricsAggOverScan(table string, cols ...string) chplan.Node {
	groupBy := make([]chplan.Expr, len(cols))
	for i, c := range cols {
		groupBy[i] = &chplan.ColumnRef{Name: c}
	}
	return &chplan.Aggregate{
		Input:    &chplan.Scan{Table: table},
		GroupBy:  groupBy,
		AggFuncs: []chplan.AggFunc{{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "v"}},
	}
}

// TestDeployedMetricsSortingKeyDecidesAggregationInOrder creates the metrics
// tables at the key cerberus provisions and at both stock exporter keys,
// resolves their strategies through the boot requirements check, and asserts
// the setting is stamped exactly for the GROUP BY shapes that prefix each
// deployed key — and, over cerberus's tables, exactly where it was stamped
// before the deployed key was read.
func TestDeployedMetricsSortingKeyDecidesAggregationInOrder(t *testing.T) {
	m := schema.DefaultOTelMetrics()
	groupBys := [][]string{
		{"MetricName"},
		{"MetricName", "Attributes"},
		{"MetricName", "Attributes", "ServiceName"},
		{"ServiceName"},
		{"ServiceName", "MetricName"},
		{"ServiceName", "MetricName", "Attributes"},
	}
	worlds := []struct {
		name     string
		database string
		render   func(database string) ([]string, error)
		// stamped lists the indexes into groupBys the deployed key admits.
		stamped map[int]bool
	}{
		{"cerberus", "sk_cerberus", cerberusMetricsDDL, map[int]bool{0: true, 1: true, 2: true}},
		{"stock before v0.157", "sk_stock_before_v0157", stockMetricsDDLBeforeV0157, map[int]bool{3: true, 4: true, 5: true}},
		{"stock v0.157", "sk_stock_v0157", chsqltest.UpstreamDateTimeMetricsDDL, map[int]bool{3: true, 4: true}},
	}
	c := chclienttest.NewChDB(t)
	rules := engine.SettingsRules{OptimizeAggregationInOrder: true, Metrics: m}
	for _, w := range worlds {
		t.Run(w.name, func(t *testing.T) {
			c.Seed(t, "DROP DATABASE IF EXISTS "+w.database+";\nCREATE DATABASE "+w.database+";")
			t.Cleanup(func() { c.Seed(t, "DROP DATABASE IF EXISTS "+w.database+";") })
			stmts, err := w.render(w.database)
			if err != nil {
				t.Fatal(err)
			}
			for _, stmt := range stmts {
				c.Seed(t, stmt+";")
			}
			res := preflight.Run(context.Background(), c, preflight.Requirements{
				Database: w.database,
				Metrics:  m,
				Signals:  preflight.Signals{Metrics: true},
			})
			if res.Fatal != nil || !res.SchemaProvisioned() {
				t.Fatalf("requirements check: fatal %v, absent %v", res.Fatal, res.AbsentTables)
			}
			strategies := res.Strategies.Metrics
			for _, table := range []string{m.GaugeTable, m.SumTable, m.HistogramTable, m.ExpHistogramTable} {
				if _, ok := strategies.SortingKeyPrefixes[table]; !ok {
					t.Fatalf("no deployed sorting key resolved for %s: %v", table, strategies.SortingKeyPrefixes)
				}
				for i, cols := range groupBys {
					plan := metricsAggOverScan(table, cols...)
					got := engine.ProbeQuerySettings(plan, rules, sortingKeyProbeMemCap, strategies).
						Settings["optimize_aggregation_in_order"] == 1
					if got != w.stamped[i] {
						t.Errorf("%s GROUP BY %v: stamped = %v, want %v", table, cols, got, w.stamped[i])
					}
					if w.name != "cerberus" {
						continue
					}
					provisioned := engine.ProbeQuerySettings(plan, rules, sortingKeyProbeMemCap, chsql.SchemaStrategies{}).
						Settings["optimize_aggregation_in_order"] == 1
					if got != provisioned {
						t.Errorf("%s GROUP BY %v: stamped = %v over cerberus's tables, %v under the provisioned key",
							table, cols, got, provisioned)
					}
				}
			}
		})
	}
}
