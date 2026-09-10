package chplan

import "slices"

func (s *Scan) RowType() Schema {
	if len(s.Columns) == 0 {
		return Schema{Columns: slices.Clone(s.Roles), Open: true}
	}
	return selectNames(Schema{}, s.Columns, s.Roles)
}

func (*OneRow) RowType() Schema { return Schema{Columns: []Column{{Name: "1"}}} }
func (*StepGrid) RowType() Schema {
	return Schema{Columns: []Column{{RangeWindowAnchorColumn, RoleAnchor}}}
}
func (f *Filter) RowType() Schema             { return f.Input.RowType() }
func (l *Limit) RowType() Schema              { return l.Input.RowType() }
func (o *OrderBy) RowType() Schema            { return o.Input.RowType() }
func (s *SearchTraceLimit) RowType() Schema   { return s.Input.RowType() }
func (m *MetricsSecondStage) RowType() Schema { return m.Input.RowType() }
func (p *Project) RowType() Schema            { return projectSchema(p.Input.RowType(), p.Projections, p.Roles) }

func (t *TopK) RowType() Schema {
	input := t.Input.RowType()
	if len(t.Columns) == 0 {
		return input
	}
	return selectNames(input, t.Columns, nil)
}

func (a *Aggregate) RowType() Schema {
	input := a.Input.RowType()
	return appendReducers(groupSchema(input, a.GroupBy, a.GroupByAliases, a.Roles), input, a.AggFuncs, a.Roles)
}

func (c *CrossJoin) RowType() Schema {
	l, r := c.Left.RowType(), c.Right.RowType()
	out := Schema{Columns: slices.Clone(l.Columns), Open: l.Open || r.Open}
	for _, column := range r.Columns {
		if _, duplicate := l.ByName(column.Name); duplicate {
			// The emitter aliases the right relation R; SELECT * qualifies
			// colliding right-hand names with that relation alias.
			column.Name = "R." + column.Name
		} else if l.Open {
			// An undeclared left storage column could collide. Neither the
			// bare right name nor R.name is a proven output in this case;
			// an open schema only advertises names that are certain.
			continue
		}
		out.Columns = append(out.Columns, column)
	}
	return out
}

func (u *UnionAll) RowType() Schema {
	if len(u.Inputs) == 0 {
		return Schema{}
	}
	return u.Inputs[0].RowType()
}
func (s *SetOperation) RowType() Schema { return s.Left.RowType() }
func (n *NestedSetAnnotate) RowType() Schema {
	out := n.Input.RowType()
	out.Columns = append(slices.Clone(out.Columns), Column{Name: NestedSetLeftColumn}, Column{Name: NestedSetRightColumn}, Column{Name: NestedSetParentColumn})
	return out
}

func (r *RangeLWR) RowType() Schema {
	return sampleSchema(r.MetricNameCol, r.AttributesCol, r.TimestampCol, r.ValueCol)
}

func (r *RangeWindowStaleResample) RowType() Schema {
	return sampleSchema(r.MetricNameCol, r.AttributesCol, r.TimestampCol, r.ValueCol)
}

func (v *VectorJoin) RowType() Schema {
	return sampleSchema(v.MetricNameColumn, v.AttributesColumn, v.TimestampColumn, v.ValueColumn)
}

func payloadSchema(out Schema, histogram, mixed bool) Schema {
	if histogram || mixed {
		out.Columns = append(out.Columns, histogramColumns()...)
	}
	if mixed {
		out.Columns = append(out.Columns, Column{MixedDiscriminatorColumn, RoleDiscriminator})
	}
	return out
}

func (v *VectorSetOp) RowType() Schema {
	return payloadSchema(sampleSchema(v.MetricNameColumn, v.AttributesColumn, v.TimestampColumn, v.ValueColumn), v.Histogram, v.Mixed)
}

func (v *NaryVectorSetOp) RowType() Schema {
	return payloadSchema(sampleSchema(v.MetricNameColumn, v.AttributesColumn, v.TimestampColumn, v.ValueColumn), v.Histogram, false)
}

func (v *InfoJoin) RowType() Schema {
	return payloadSchema(sampleSchema(v.MetricNameColumn, v.AttributesColumn, v.TimestampColumn, v.ValueColumn), v.Histogram, false)
}

func (h *HistogramProjection) RowType() Schema {
	out := groupSchema(h.Input.RowType(), h.GroupBy, h.GroupByAliases, nil)
	out.Columns = append(out.Columns, histogramColumns()...)
	return out
}

func windowSchema(input Schema, keys []Expr, matrix bool, timestamp, value string) Schema {
	out := groupSchema(input, keys, nil, nil)
	if matrix {
		out.Columns = append(out.Columns, Column{RangeWindowAnchorColumn, RoleAnchor}, Column{timestamp, RoleTimestamp})
	}
	out.Columns = append(out.Columns, Column{value, RoleValue})
	return out
}

func (r *RangeWindowGridNativeInstant) RowType() Schema {
	return windowSchema(r.Input.RowType(), r.GroupBy, false, r.TimestampColumn, r.ValueColumn)
}

func (r *RangeWindowGridNative) RowType() Schema {
	out := windowSchema(r.Input.RowType(), r.GroupBy, true, r.TimestampColumn, r.ValueColumn)
	if len(r.Recollapse) == 0 {
		return out
	}
	input := r.Input.RowType()
	groupNames := make([]string, len(r.GroupBy))
	for i, key := range r.GroupBy {
		groupNames[i] = ProjectionOutputName(Projection{Expr: key})
	}
	pass, _ := r.PartitionRecollapseGroupBy(groupNames)
	keys := Schema{}
	for _, i := range pass {
		keys.Columns = append(keys.Columns, roleColumn(groupNames[i], input, nil))
	}
	keys.Columns = append(keys.Columns, projectSchema(input, r.Recollapse, nil).Columns...)
	keys.Columns = append(keys.Columns, Column{RangeWindowAnchorColumn, RoleAnchor}, Column{r.TimestampColumn, RoleTimestamp}, Column{r.ValueColumn, RoleValue})
	return keys
}

func (r *RangeWindow) RowType() Schema {
	if r.DownsampleTier {
		return r.variantWindowSchema(r.DownsampleTierInput.RowType(), false)
	}
	switch m := r.Input.(type) {
	case *MetricsAggregate:
		return metricsWindowSchema(m)
	case *MetricsHistogramOverTime:
		return histogramWindowSchema(m)
	case *MetricsCompare:
		out := m.RowType()
		value := out.Columns[len(out.Columns)-1]
		out.Columns = append(out.Columns[:len(out.Columns)-1], Column{RangeWindowAnchorColumn, RoleAnchor}, value)
		return out
	}
	input := r.Input.RowType()
	if len(r.Variants) != 0 {
		return r.variantWindowSchema(input, true)
	}
	out := groupSchema(input, r.GroupBy, nil, nil)
	if r.OuterRange > 0 {
		out.Columns = append(out.Columns, Column{RangeWindowAnchorColumn, RoleAnchor})
		if r.TimestampColumn != "" {
			timestamp := r.TimestampColumn
			if timestamp == RangeWindowAnchorColumn {
				timestamp = "TimeUnix"
			}
			out.Columns = append(out.Columns, Column{timestamp, RoleTimestamp})
		}
	}
	out.Columns = append(out.Columns, Column{r.ValueColumn, RoleValue})
	if r.Func == "predict_linear" && !r.Identity && r.OuterRange == 0 && r.PredictLinearSlopeColumn != "" {
		out.Columns = append(out.Columns, Column{Name: r.PredictLinearSlopeColumn})
	}
	return out
}

func (r *RangeWindow) variantWindowSchema(input Schema, variants bool) Schema {
	out := groupSchema(input, r.GroupBy, nil, nil)
	if r.OuterRange > 0 || r.DownsampleTier {
		out.Columns = append(out.Columns, Column{RangeWindowAnchorColumn, RoleAnchor})
		if r.TimestampColumn != RangeWindowAnchorColumn {
			out.Columns = append(out.Columns, Column{r.TimestampColumn, RoleTimestamp})
		}
	}
	out.Columns = append(out.Columns, Column{r.ValueColumn, RoleValue})
	if variants {
		out.Columns = append(out.Columns, Column{Name: r.VariantColumn})
	}
	return out
}
