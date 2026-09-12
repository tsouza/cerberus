package chplan

// InputTimestampColumn resolves the physical timestamp owned by the nested
// relation that MetricsAggregate reads. The wrapping RangeWindow's
// TimestampColumn is an output name and is deliberately not consulted.
func (m *MetricsAggregate) InputTimestampColumn() (string, bool) {
	if m == nil {
		return "", false
	}
	return metricsNestedTimestampColumn(m.Inner)
}

// InputTimestampColumn resolves the physical timestamp owned by the nested
// relation that MetricsHistogramOverTime reads.
func (m *MetricsHistogramOverTime) InputTimestampColumn() (string, bool) {
	if m == nil {
		return "", false
	}
	return metricsNestedTimestampColumn(m.Inner)
}

func metricsNestedTimestampColumn(inner Node) (string, bool) {
	if inner == nil {
		return "", false
	}
	row := inner.RowType()
	nameCount := make(map[string]int, len(row.Columns))
	var timestamp string
	for _, column := range row.Columns {
		nameCount[column.Name]++
		if column.Role != RoleTimestamp {
			continue
		}
		if column.Name == "" || timestamp != "" {
			return "", false
		}
		timestamp = column.Name
	}
	if timestamp == "" || nameCount[timestamp] != 1 {
		return "", false
	}
	return timestamp, true
}
