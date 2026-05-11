package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// emitVectorJoin renders a PromQL vector-vector binary expression as an
// INNER JOIN of per-series latest samples. Shape:
//
//	SELECT
//	    L.<MetricName>, L.<Attributes>, L.<TimeUnix>,
//	    (L.<Value> <Op> R.<Value>) AS <Value>
//	FROM (
//	    SELECT <MetricName>, <Attributes>,
//	           max(<TimeUnix>) AS <TimeUnix>,
//	           argMax(<Value>, <TimeUnix>) AS <Value>
//	    FROM (<left>) GROUP BY <MetricName>, <Attributes>
//	) AS L
//	INNER JOIN (
//	    SELECT <MetricName>, <Attributes>,
//	           max(<TimeUnix>) AS <TimeUnix>,
//	           argMax(<Value>, <TimeUnix>) AS <Value>
//	    FROM (<right>) GROUP BY <MetricName>, <Attributes>
//	) AS R
//	ON <match-key-on-L> = <match-key-on-R>
//
// The match-key expression depends on `Match`:
//   - default (Labels empty, On false) → `L.Attributes = R.Attributes`
//   - on(labels)                       → AND of `L.Attributes['lbl'] = R.Attributes['lbl']`
//   - ignoring(labels)                 → mapFilter-stripped Attributes equality
func (e *emitter) emitVectorJoin(j *chplan.VectorJoin) error {
	if err := e.validateVectorJoinCols(j); err != nil {
		return err
	}

	attrsCol := quoteIdent(j.AttributesColumn)
	metricCol := quoteIdent(j.MetricNameColumn)
	tsCol := quoteIdent(j.TimestampColumn)
	valCol := quoteIdent(j.ValueColumn)

	// Outer SELECT.
	fmt.Fprintf(&e.b, "SELECT L.%s, L.%s, L.%s, (L.%s %s R.%s) AS %s FROM ",
		metricCol, attrsCol, tsCol, valCol, string(j.Op), valCol, valCol)

	// Left side — argMax per series.
	if err := e.emitLatestPerSeries(j.Left, metricCol, attrsCol, tsCol, valCol); err != nil {
		return err
	}
	e.b.WriteString(" AS L INNER JOIN ")
	if err := e.emitLatestPerSeries(j.Right, metricCol, attrsCol, tsCol, valCol); err != nil {
		return err
	}
	e.b.WriteString(" AS R ON ")
	return e.writeVectorMatchPredicate(j.Match, attrsCol)
}

func (e *emitter) validateVectorJoinCols(j *chplan.VectorJoin) error {
	switch {
	case j.AttributesColumn == "":
		return fmt.Errorf("%w: VectorJoin.AttributesColumn unset", ErrUnsupported)
	case j.MetricNameColumn == "":
		return fmt.Errorf("%w: VectorJoin.MetricNameColumn unset", ErrUnsupported)
	case j.TimestampColumn == "":
		return fmt.Errorf("%w: VectorJoin.TimestampColumn unset", ErrUnsupported)
	case j.ValueColumn == "":
		return fmt.Errorf("%w: VectorJoin.ValueColumn unset", ErrUnsupported)
	}
	return nil
}

// emitLatestPerSeries wraps a subquery in a per-series latest aggregation:
// (SELECT metric, attrs, max(ts) AS ts, argMax(value, ts) AS value FROM <n>
//
//	GROUP BY metric, attrs).
func (e *emitter) emitLatestPerSeries(n chplan.Node, metricCol, attrsCol, tsCol, valCol string) error {
	e.b.WriteByte('(')
	fmt.Fprintf(&e.b, "SELECT %s, %s, max(%s) AS %s, argMax(%s, %s) AS %s FROM ",
		metricCol, attrsCol, tsCol, tsCol, valCol, tsCol, valCol)
	if err := e.emitSubquery(n); err != nil {
		return err
	}
	fmt.Fprintf(&e.b, " GROUP BY %s, %s)", metricCol, attrsCol)
	return nil
}

func (e *emitter) writeVectorMatchPredicate(m chplan.VectorMatch, attrsCol string) error {
	// Default: full-Attributes equality.
	if len(m.Labels) == 0 && !m.On {
		fmt.Fprintf(&e.b, "L.%s = R.%s", attrsCol, attrsCol)
		return nil
	}

	if m.On {
		// on(l1, l2): conjunction of per-label key equalities.
		for i, lbl := range m.Labels {
			if i > 0 {
				e.b.WriteString(" AND ")
			}
			fmt.Fprintf(&e.b, "L.%s[", attrsCol)
			if err := e.bindArg(lbl); err != nil {
				return err
			}
			fmt.Fprintf(&e.b, "] = R.%s[", attrsCol)
			if err := e.bindArg(lbl); err != nil {
				return err
			}
			e.b.WriteByte(']')
		}
		return nil
	}

	// ignoring(l1, l2): mapFilter-stripped Attributes equality on each side.
	for _, side := range []string{"L", "R"} {
		if side == "R" {
			e.b.WriteString(" = ")
		}
		fmt.Fprintf(&e.b, "mapFilter((k, v) -> NOT (k IN (")
		for i, lbl := range m.Labels {
			if i > 0 {
				e.b.WriteString(", ")
			}
			if err := e.bindArg(lbl); err != nil {
				return err
			}
		}
		fmt.Fprintf(&e.b, ")), %s.%s)", side, attrsCol)
	}
	return nil
}
