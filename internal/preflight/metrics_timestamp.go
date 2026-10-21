package preflight

import (
	"fmt"
	"strings"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// The two deployed types an OTel-CH metrics timestamp column takes, as
// system.columns reports them: the exporter's schema before v0.157 types
// TimeUnix / StartTimeUnix / Exemplars.TimeUnix `DateTime64(9)`, and from
// v0.157 `DateTime`. The Nested exemplar column reports the Array of
// either.
const (
	metricsTimestampTypeDateTime64 = "DateTime64(9)"
	metricsTimestampTypeDateTime   = "DateTime"
)

// exemplarTimestampField is the timestamp field of the OTel-CH metrics
// `Exemplars` Nested column, which system.columns reports flattened as
// `Exemplars.TimeUnix`.
const exemplarTimestampField = "TimeUnix"

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
// whose deployed type picks the MetricsTimestampStrategy: the sample
// timestamp, the start timestamp and the exemplar timestamp.
func metricsTimestampColumns(m schema.Metrics) []string {
	cols := nonEmpty(m.TimestampColumn, m.StartTimeColumn)
	if m.ExemplarsColumn != "" {
		cols = append(cols, m.ExemplarsColumn+"."+exemplarTimestampField)
	}
	return cols
}

// metricsTimestampStrategyFor classifies one deployed timestamp column type,
// reporting false for a type cerberus renders no SQL for. An Array is the
// Nested exemplar column and classifies by its element type.
//
// Detection only: which SQL each strategy renders is chsql's decision
// (chsql.MetricsTimestampStrategy).
func metricsTimestampStrategyFor(deployedType string) (chsql.MetricsTimestampStrategy, bool) {
	typ := strings.TrimSpace(deployedType)
	if strings.HasPrefix(typ, arrayTypePrefix) && strings.HasSuffix(typ, arrayTypeSuffix) {
		typ = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(typ, arrayTypePrefix), arrayTypeSuffix))
	}
	switch typ {
	case metricsTimestampTypeDateTime64:
		return chsql.MetricsTimestampDateTime64, true
	case metricsTimestampTypeDateTime:
		return chsql.MetricsTimestampDateTime, true
	default:
		return chsql.MetricsTimestampDateTime64, false
	}
}

// resolveMetricsTimestamp resolves the deployed metrics timestamp columns to
// the one MetricsTimestampStrategy every PromQL statement renders against,
// or to the FATAL problems that make that impossible: a column of a type
// metricsTimestampStrategyFor does not classify, or columns that classify
// differently. One statement can read every metrics table — a merge() over
// gauge and sum, a union across the histogram tables — so a single strategy
// has to hold for all of them. No columns (no metric table introspected)
// resolves to the zero value.
func resolveMetricsTimestamp(cols []deployedColumn) (chsql.MetricsTimestampStrategy, []string) {
	var (
		problems []string
		first    *deployedColumn
		resolved chsql.MetricsTimestampStrategy
	)
	for i, c := range cols {
		s, ok := metricsTimestampStrategyFor(c.typ)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"table %s column %s: expected %s or %s, found %s",
				c.table, c.column, metricsTimestampTypeDateTime64, metricsTimestampTypeDateTime, c.typ,
			))
			continue
		}
		if first == nil {
			first, resolved = &cols[i], s
			continue
		}
		if s != resolved {
			problems = append(problems, fmt.Sprintf(
				"metrics timestamp columns disagree: %s, but %s — every metrics table must type its "+
					"timestamp columns %s, or every one %s",
				first, c, metricsTimestampTypeDateTime64, metricsTimestampTypeDateTime,
			))
		}
	}
	if len(problems) > 0 {
		return chsql.MetricsTimestampDateTime64, problems
	}
	return resolved, nil
}
