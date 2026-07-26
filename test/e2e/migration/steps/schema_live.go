package steps

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/schema"
)

// schemaLiveDiff is what the MIG-10 tier-1 "diff the rendered schema against
// the live database" step produced: per metrics table, which of cerberus's
// own expected columns the live collector-created table is missing, plus any
// table cerberus expects that does not exist at all.
type schemaLiveDiff struct {
	ran            bool
	missingTables  []string
	missingColumns map[string][]string
}

// registerSchemaLiveSteps binds the MIG-10 tier-1 steps: diffing the
// offline-rendered schema (already established by the shared schema.go
// steps) against the live, collector-created ClickHouse tables.
func (w *World) registerSchemaLiveSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator diffs the rendered schema against the live database$`, w.whenDiffSchemaAgainstLive)
	ctx.Step(`^every rendered table exists in the live database$`, w.thenNoTableMissingLive)
	ctx.Step(`^every rendered column exists on its live table$`, w.thenNoColumnMissingLive)
}

// metricsExpectedColumns names, for each metrics table, the columns
// cerberus's own read-side schema config (schema.Metrics) expects to find
// there. It is built from the SAME typed config the live server resolves its
// column names from — schema.DefaultOTelMetrics(), the same value schema.go's
// signalTables() reads — so a table this omits is a table cerberus does not
// read a column from, not a gap in the check. Scoped to the four metrics
// tables the Tier-1 fixture actually populates (gauge, sum, classic
// histogram, exponential histogram) — logs and traces are outside this
// scenario's fixture surface and are left to a future extension of this
// check rather than asserted here on faith.
func metricsExpectedColumns(m schema.Metrics) map[string][]string {
	common := []string{
		m.ResourceAttributesColumn, m.ServiceNameColumn, m.MetricNameColumn,
		m.MetricDescriptionColumn, m.MetricUnitColumn, m.AttributesColumn,
		m.TimestampColumn, m.FlagsColumn,
	}
	valueCols := append(append([]string{}, common...), m.StartTimeColumn, m.ValueColumn)
	sumCols := append(append([]string{}, valueCols...), m.AggregationTemporalityColumn, m.IsMonotonicColumn)
	histCols := append(append([]string{}, common...),
		m.CountColumn, m.SumColumn, m.BucketCountsColumn, m.ExplicitBoundsColumn, m.AggregationTemporalityColumn)
	expHistCols := append(append([]string{}, common...),
		m.CountColumn, m.SumColumn, m.ScaleColumn, m.ZeroCountColumn,
		m.PositiveOffsetColumn, m.PositiveBucketCountsColumn,
		m.NegativeOffsetColumn, m.NegativeBucketCountsColumn, m.AggregationTemporalityColumn)
	return map[string][]string{
		m.GaugeTable:        valueCols,
		m.SumTable:          sumCols,
		m.HistogramTable:    histCols,
		m.ExpHistogramTable: expHistCols,
	}
}

// whenDiffSchemaAgainstLive dials the live Tier-1 ClickHouse and diffs
// cerberus's own expected metrics-table columns against what the collector
// actually created. The offline render (the shared schema.go When step) must
// have already run — this step diffs the SAME typed schema config that
// render exercises, against live reality, rather than re-parsing the
// rendered SQL text.
func (w *World) whenDiffSchemaAgainstLive() error {
	if !w.schema.ran {
		return fmt.Errorf("the schema has not been rendered; the scenario must render it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), livePollBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	diff := schemaLiveDiff{ran: true, missingColumns: map[string][]string{}}
	expected := metricsExpectedColumns(schema.DefaultOTelMetrics())
	tables := make([]string, 0, len(expected))
	for table := range expected {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	for _, table := range tables {
		exists, err := tableExistsLive(ctx, conn, w.live.CHDatabase, table)
		if err != nil {
			return err
		}
		if !exists {
			diff.missingTables = append(diff.missingTables, table)
			continue
		}
		liveCols, err := liveColumnSet(ctx, conn, w.live.CHDatabase, table)
		if err != nil {
			return err
		}
		var missing []string
		for _, col := range expected[table] {
			if col == "" {
				continue
			}
			if _, ok := liveCols[col]; !ok {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			diff.missingColumns[table] = missing
		}
	}
	w.schemaLive = diff
	return nil
}

// thenNoTableMissingLive asserts every metrics table cerberus's schema
// expects actually exists in the live, collector-created database.
func (w *World) thenNoTableMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if len(w.schemaLive.missingTables) > 0 {
		return fmt.Errorf("the live database is missing tables the rendered schema declares: %v", w.schemaLive.missingTables)
	}
	return nil
}

// thenNoColumnMissingLive asserts every column cerberus's schema config
// expects on a metrics table is actually present on the live table — the
// "diff vs collector-created tables lists every missing/renamed column"
// half of MIG-10's PASS assertion.
func (w *World) thenNoColumnMissingLive() error {
	if !w.schemaLive.ran {
		return fmt.Errorf("the schema has not been diffed against the live database")
	}
	if len(w.schemaLive.missingColumns) == 0 {
		return nil
	}
	tables := make([]string, 0, len(w.schemaLive.missingColumns))
	for t := range w.schemaLive.missingColumns {
		tables = append(tables, t)
	}
	sort.Strings(tables)
	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "%s missing %v; ", t, w.schemaLive.missingColumns[t])
	}
	return fmt.Errorf("the live database deviates from the rendered schema: %s", b.String())
}
