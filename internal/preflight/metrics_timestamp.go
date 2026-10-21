package preflight

import (
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/schema"
)

// metricsTimestampPrecisionKnob names the setting that types the timestamp
// columns of the metrics tables cerberus creates, which a mixed-precision
// finding points the operator at.
const metricsTimestampPrecisionKnob = "CERBERUS_SCHEMA_METRICS_TIMESTAMP_PRECISION (schema.metricsTimestampPrecision)"

// arrayTypePrefix / arrayTypeSuffix wrap a Nested sub-column's element type
// in system.columns.
const (
	arrayTypePrefix = "Array("
	arrayTypeSuffix = ")"
)

// deployedColumn is one metrics timestamp column and the type system.columns
// reports for it.
type deployedColumn struct {
	table  string
	column string
	typ    string
}

func (c deployedColumn) String() string {
	return fmt.Sprintf("%s.%s %s", c.table, c.column, c.typ)
}

// metricsTimestampColumns names the timestamp columns of a metrics table
// whose deployed type decides the schema.MetricsTimestampPrecision: the
// sample timestamp, the start timestamp and the exemplar timestamp.
func metricsTimestampColumns(m schema.Metrics) []string {
	cols := nonEmpty(m.TimestampColumn, m.StartTimeColumn)
	if m.ExemplarsColumn != "" {
		cols = append(cols, m.ExemplarsColumn+"."+schema.ExemplarTimestampField)
	}
	return cols
}

// metricsTimestampPrecisionOf classifies one deployed timestamp column type
// (schema.MetricsTimestampPrecisionOfType), reporting false for a type that
// is not a ClickHouse timestamp. An Array is the Nested exemplar column and
// classifies by its element type.
func metricsTimestampPrecisionOf(deployedType string) (schema.MetricsTimestampPrecision, bool) {
	typ := strings.TrimSpace(deployedType)
	if strings.HasPrefix(typ, arrayTypePrefix) && strings.HasSuffix(typ, arrayTypeSuffix) {
		typ = strings.TrimSuffix(strings.TrimPrefix(typ, arrayTypePrefix), arrayTypeSuffix)
	}
	return schema.MetricsTimestampPrecisionOfType(typ)
}

// resolveMetricsTimestamp resolves the deployed metrics timestamp columns to
// the one schema.MetricsTimestampPrecision every PromQL statement renders
// for, or to the FATAL problems that make that impossible: a column whose
// type is not a timestamp, or columns of different precisions. One statement
// can read every metrics table — a merge() over gauge and sum, a union across
// the histogram tables — so a single precision has to hold for all of them.
// No columns (no metric table introspected) resolves to the zero value.
func resolveMetricsTimestamp(cols []deployedColumn) (schema.MetricsTimestampPrecision, []string) {
	var (
		problems []string
		byPrec   = map[schema.MetricsTimestampPrecision][]string{}
	)
	for _, c := range cols {
		p, ok := metricsTimestampPrecisionOf(c.typ)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"table %s column %s: expected DateTime or DateTime64, found %s",
				c.table, c.column, c.typ,
			))
			continue
		}
		byPrec[p] = append(byPrec[p], c.String())
	}
	nanos, seconds := byPrec[schema.MetricsTimestampNanos], byPrec[schema.MetricsTimestampSeconds]
	if len(nanos) > 0 && len(seconds) > 0 {
		problems = append(problems, fmt.Sprintf(
			"metrics timestamp columns mix precisions — nanosecond: %s; coarser: %s — every metrics "+
				"table must store its timestamps at one precision. %s types only the tables cerberus "+
				"creates, so it must match the precision of the tables that already exist",
			strings.Join(nanos, ", "), strings.Join(seconds, ", "), metricsTimestampPrecisionKnob,
		))
	}
	if len(problems) > 0 {
		return schema.MetricsTimestampNanos, problems
	}
	if len(seconds) > 0 {
		return schema.MetricsTimestampSeconds, nil
	}
	return schema.MetricsTimestampNanos, nil
}

// sumTimestampTimezone returns the timezone the sum table's deployed sample
// timestamp column names, "" when it names none or was not introspected.
func sumTimestampTimezone(m schema.Metrics, cols []deployedColumn) string {
	for _, c := range cols {
		if c.table == m.SumTable && c.column == m.TimestampColumn {
			return schema.TimestampTimezoneOfType(c.typ)
		}
	}
	return ""
}
