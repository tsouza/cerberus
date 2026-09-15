package chplan

import "strconv"

func (h *HistogramQuantile) RowType() Schema {
	out := groupSchema(h.Input.RowType(), h.GroupBy, h.GroupByAliases, nil)
	out.Columns = append(out.Columns, Column{Name: "Value", Role: RoleValue})
	return out
}

func (h *HistogramQuantileNative) RowType() Schema {
	out := groupSchema(h.Input.RowType(), h.GroupBy, h.GroupByAliases, nil)
	out.Columns = append(out.Columns, Column{Name: "Value", Role: RoleValue})
	return out
}

func (a *AbsentOverTime) RowType() Schema {
	return sampleSchema(a.MetricNameColumn, a.AttributesColumn, a.TimestampColumn, a.ValueColumn)
}

func (r *RangeBucketFanout) RowType() Schema {
	input := r.Input.RowType()
	out := Schema{Columns: []Column{{Name: r.AnchorAlias, Role: RoleAnchor}}}
	out.Columns = append(out.Columns, groupSchema(input, r.GroupBy, r.GroupByAliases, nil).Columns...)
	return appendReducers(out, input, r.AggFuncs, nil)
}

func (r *RangeBucketGridNative) RowType() Schema {
	input := r.Input.RowType()
	out := Schema{Columns: []Column{{Name: r.AnchorAlias, Role: RoleAnchor}}}
	out.Columns = append(out.Columns, groupSchema(input, r.GroupBy, r.GroupByAliases, nil).Columns...)
	out.Columns = append(
		out.Columns,
		Column{Name: r.BucketCountsCol, Role: RoleHistogramField, HistogramField: HistogramFieldBucketCounts},
		Column{Name: r.ExplicitBoundsCol, Role: RoleHistogramField, HistogramField: HistogramFieldExplicitBounds},
	)
	return out
}

func (r *RangeWindowGridNativeVectorAgg) RowType() Schema {
	input := r.Input.RowType()
	out := groupSchema(input, r.GroupBy, r.GroupByAliases, nil)
	out.Columns = append(out.Columns, Column{Name: RangeWindowAnchorColumn, Role: RoleAnchor})
	if r.AnchorAlias != RangeWindowAnchorColumn {
		out.Columns = append(out.Columns, Column{Name: r.AnchorAlias, Role: RoleTimestamp})
	}
	if value, ok := input.Find(RoleValue); ok {
		out.Columns = append(out.Columns, value)
	}
	if r.PartialCountAlias != "" {
		out.Columns = append(out.Columns, Column{Name: r.PartialCountAlias})
	}
	return out
}

func (m *MetricsAggregate) RowType() Schema {
	aliases := m.GroupByAliases
	multi := m.Op == MetricsOpQuantileOverTime && len(m.Quantiles) > 1
	if multi {
		aliases = make([]string, len(m.GroupBy))
		for i := range aliases {
			if i < len(m.GroupByAliases) {
				aliases[i] = m.GroupByAliases[i]
			}
			if aliases[i] == "" {
				aliases[i] = "g" + strconv.Itoa(i)
			}
		}
	}
	out := groupSchema(m.Inner.RowType(), m.GroupBy, aliases, nil)
	if multi {
		out.Columns = append(out.Columns, Column{Name: "__phi__"})
	}
	out.Columns = append(out.Columns, Column{Name: m.ValueAlias, Role: RoleValue})
	return out
}

func (m *MetricsHistogramOverTime) RowType() Schema {
	out := groupSchema(m.Inner.RowType(), m.GroupBy, m.GroupByAliases, nil)
	out.Columns = append(out.Columns, Column{Name: outputDefault(m.BucketAlias, "__bucket")}, Column{Name: outputDefault(m.ValueAlias, "Value"), Role: RoleValue})
	return out
}

func (m *MetricsCompare) RowType() Schema {
	return Schema{Columns: []Column{
		{Name: outputDefault(m.SelAlias, "is_selection")},
		{Name: outputDefault(m.AttrAlias, "attr")},
		{Name: outputDefault(m.ValAlias, "val")},
		{Name: outputDefault(m.ValueAlias, "Value"), Role: RoleValue},
	}}
}

func outputDefault(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

func outerGroupNames(keys []Expr, aliases []string) []string {
	names := make([]string, len(keys))
	for i := range names {
		if i < len(aliases) {
			names[i] = aliases[i]
		}
		if names[i] == "" {
			names[i] = "g" + strconv.Itoa(i)
		}
	}
	return names
}

func metricsWindowSchema(m *MetricsAggregate) Schema {
	out := groupSchema(m.Inner.RowType(), m.GroupBy, outerGroupNames(m.GroupBy, m.GroupByAliases), nil)
	out.Columns = append(out.Columns, Column{Name: RangeWindowAnchorColumn, Role: RoleAnchor})
	if m.Op == MetricsOpQuantileOverTime {
		out.Columns = append(out.Columns, Column{Name: "__bucket"})
	}
	out.Columns = append(out.Columns, Column{Name: m.ValueAlias, Role: RoleValue})
	return out
}

func histogramWindowSchema(m *MetricsHistogramOverTime) Schema {
	out := groupSchema(m.Inner.RowType(), m.GroupBy, outerGroupNames(m.GroupBy, m.GroupByAliases), nil)
	out.Columns = append(out.Columns, Column{Name: outputDefault(m.BucketAlias, "__bucket")}, Column{Name: RangeWindowAnchorColumn, Role: RoleAnchor}, Column{Name: outputDefault(m.ValueAlias, "Value"), Role: RoleValue})
	return out
}

// JoinSideAlias is the public output name of a two-sided payload join.
func JoinSideAlias(prefix, side, column string) string { return prefix + "_" + side + "_" + column }

func histogramJoinColumns(metric, attributes, timestamp string) []string {
	return []string{
		metric, attributes, timestamp, HistogramScaleColumn, HistogramCountColumn, HistogramSumColumn,
		HistogramZeroCountColumn, HistogramZeroThresholdColumn, HistogramPositiveOffsetColumn,
		HistogramPositiveBucketCountsColumn, HistogramNegativeOffsetColumn, HistogramNegativeBucketCountsColumn,
	}
}

func joinSideSchema(prefix string, names []string) Schema {
	out := Schema{}
	for _, name := range names {
		for _, side := range []string{"L", "R"} {
			out.Columns = append(out.Columns, Column{Name: JoinSideAlias(prefix, side, name)})
		}
	}
	return out
}

func (h *HistogramVectorJoin) RowType() Schema {
	return joinSideSchema("_hq", histogramJoinColumns(h.MetricNameColumn, h.AttributesColumn, h.TimestampColumn))
}

func (m *MixedVectorJoin) RowType() Schema {
	s := payloadSchema(sampleSchema(m.MetricNameColumn, m.AttributesColumn, m.TimestampColumn, m.ValueColumn), false, true)
	names := make([]string, len(s.Columns))
	for i, c := range s.Columns {
		names[i] = c.Name
	}
	return joinSideSchema("_mvj", names)
}

func (h *HistogramFloatVectorJoin) RowType() Schema {
	roles := sampleSchema(h.MetricNameColumn, h.AttributesColumn, h.TimestampColumn, h.ValueColumn)
	out := selectNames(roles, histogramJoinColumns(h.MetricNameColumn, h.AttributesColumn, h.TimestampColumn), nil)
	out.Columns = append(out.Columns, Column{Name: h.ValueColumn, Role: RoleValue})
	return out
}

func (j *StructuralJoin) RowType() Schema {
	input := j.Right.RowType()
	keys := []Column{{Name: j.TraceIDColumn, Role: RoleTraceID}, {Name: j.SpanIDColumn, Role: RoleSpanID}, {Name: j.ParentSpanIDColumn, Role: RoleParentSpanID}}
	out := Schema{Columns: keys}
	if len(j.ExtraProjectionColumns) != 0 {
		out.Columns = append(out.Columns, selectNames(input, j.ExtraProjectionColumns, nil).Columns...)
		return out
	}
	out.Open = input.Open
	for _, c := range input.Columns {
		if c.Name != j.TraceIDColumn && c.Name != j.SpanIDColumn && c.Name != j.ParentSpanIDColumn {
			out.Columns = append(out.Columns, c)
		}
	}
	return out
}
