package lsyntax

import (
	"fmt"
	"strings"

	"github.com/prometheus/common/model"
)

// The String methods render an AST back into canonical LogQL. They back
// the `/loki/api/v1/format_query` endpoint and the `%s` formatting of
// matchers; they are not used by the SQL lowering.

func (e *MatchersExpr) String() string {
	var sb strings.Builder
	sb.WriteString("{")
	for i, m := range e.Mts {
		sb.WriteString(m.String())
		if i+1 != len(e.Mts) {
			sb.WriteString(", ")
		}
	}
	sb.WriteString("}")
	return sb.String()
}

func (e *PipelineExpr) String() string {
	var sb strings.Builder
	sb.WriteString(e.Left.String())
	for _, s := range e.MultiStages {
		sb.WriteString(" ")
		sb.WriteString(s.String())
	}
	return sb.String()
}

func (e *LineFilterExpr) String() string {
	var sb strings.Builder
	if e.Left != nil {
		sb.WriteString(e.Left.String())
		sb.WriteString(" ")
	}
	if e.IsOrChild {
		sb.WriteString("or ")
	} else {
		// LineMatchType.String() renders `|=`, `!=`, `|~`, `!~`, `|>`, `!>`.
		sb.WriteString(e.Ty.String())
		sb.WriteString(" ")
	}
	if e.Op == OpFilterIP {
		fmt.Fprintf(&sb, "%s(%q)", OpFilterIP, e.Match)
	} else {
		fmt.Fprintf(&sb, "%q", e.Match)
	}
	if e.Or != nil {
		sb.WriteString(" ")
		sb.WriteString(e.Or.String())
	}
	return sb.String()
}

func (e *LogfmtParserExpr) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s", OpPipe, OpParserTypeLogfmt)
	if e.Strict {
		sb.WriteString(" " + OpStrict)
	}
	if e.KeepEmpty {
		sb.WriteString(" " + OpKeepEmpty)
	}
	return sb.String()
}

func (e *LineParserExpr) String() string {
	if e.Param == "" {
		return fmt.Sprintf("%s %s", OpPipe, e.Op)
	}
	return fmt.Sprintf("%s %s %q", OpPipe, e.Op, e.Param)
}

func (e *JSONExpressionParserExpr) String() string {
	return fmt.Sprintf("%s %s %s", OpPipe, OpParserTypeJSON, extractionList(e.Expressions))
}

func extractionList(exprs []LabelExtractionExpr) string {
	parts := make([]string, 0, len(exprs))
	for _, ext := range exprs {
		if ext.Expression == ext.Identifier {
			parts = append(parts, ext.Identifier)
		} else {
			parts = append(parts, fmt.Sprintf("%s=%q", ext.Identifier, ext.Expression))
		}
	}
	return strings.Join(parts, ", ")
}

func (e *LogfmtExpressionParserExpr) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s", OpPipe, OpParserTypeLogfmt)
	if e.Strict {
		sb.WriteString(" " + OpStrict)
	}
	if e.KeepEmpty {
		sb.WriteString(" " + OpKeepEmpty)
	}
	sb.WriteString(" ")
	sb.WriteString(extractionList(e.Expressions))
	return sb.String()
}

func (e *LabelFilterExpr) String() string {
	return fmt.Sprintf("%s %s", OpPipe, e.LabelFilterer.String())
}

func (e *LineFmtExpr) String() string {
	return fmt.Sprintf("%s %s %q", OpPipe, OpFmtLine, e.Value)
}

func (e *DecolorizeExpr) String() string {
	return fmt.Sprintf("%s %s", OpPipe, OpDecolorize)
}

func (e *LabelFmtExpr) String() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s ", OpPipe, OpFmtLabel)
	for i, f := range e.Formats {
		if f.Rename {
			fmt.Fprintf(&sb, "%s=%s", f.Name, f.Value)
		} else {
			fmt.Fprintf(&sb, "%s=%q", f.Name, f.Value)
		}
		if i+1 != len(e.Formats) {
			sb.WriteString(",")
		}
	}
	return sb.String()
}

func (e *DropLabelsExpr) String() string { return namedMatcherStage(OpDrop, e.dropLabels) }
func (e *KeepLabelsExpr) String() string { return namedMatcherStage(OpKeep, e.keepLabels) }

func namedMatcherStage(op string, names []NamedLabelMatcher) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s ", OpPipe, op)
	for i, n := range names {
		if n.Matcher != nil {
			sb.WriteString(n.Matcher.String())
		} else {
			sb.WriteString(n.Name)
		}
		if i+1 != len(names) {
			sb.WriteString(",")
		}
	}
	return sb.String()
}

func (u *UnwrapExpr) String() string {
	var sb strings.Builder
	if u.Operation != "" {
		fmt.Fprintf(&sb, " %s %s %s(%s)", OpPipe, OpUnwrap, u.Operation, u.Identifier)
	} else {
		fmt.Fprintf(&sb, " %s %s %s", OpPipe, OpUnwrap, u.Identifier)
	}
	for _, f := range u.PostFilters {
		fmt.Fprintf(&sb, " %s %s", OpPipe, f)
	}
	return sb.String()
}

func (e *OffsetExpr) String() string {
	return fmt.Sprintf(" %s %s", OpOffset, model.Duration(e.Offset).String())
}

func (e *LogRangeExpr) String() string {
	var sb strings.Builder
	sb.WriteString(e.Left.String())
	if e.Unwrap != nil {
		sb.WriteString(e.Unwrap.String())
	}
	fmt.Fprintf(&sb, "[%v]", model.Duration(e.Interval))
	if e.Offset != 0 {
		off := OffsetExpr{Offset: e.Offset}
		sb.WriteString(off.String())
	}
	return sb.String()
}

func (g Grouping) String() string {
	var sb strings.Builder
	if g.Without {
		sb.WriteString(" without ")
	} else {
		sb.WriteString(" by ")
	}
	sb.WriteString("(")
	sb.WriteString(strings.Join(g.Groups, ","))
	sb.WriteString(")")
	return sb.String()
}

func (g Grouping) singleton() bool { return len(g.Groups) == 0 && !g.Without }

func (e *RangeAggregationExpr) String() string {
	var sb strings.Builder
	sb.WriteString(e.Operation)
	sb.WriteString("(")
	if e.Params != nil {
		fmt.Fprintf(&sb, "%s,", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", *e.Params), "0"), "."))
	}
	sb.WriteString(e.Left.String())
	sb.WriteString(")")
	if e.Grouping != nil {
		sb.WriteString(e.Grouping.String())
	}
	return sb.String()
}

func (e *VectorAggregationExpr) String() string {
	var params []string
	switch e.Operation {
	case OpTypeBottomK, OpTypeTopK, OpTypeApproxTopK:
		params = []string{fmt.Sprintf("%d", e.Params), e.Left.String()}
	default:
		params = []string{e.Left.String()}
	}
	var sb strings.Builder
	sb.WriteString(e.Operation)
	if e.Grouping != nil && !e.Grouping.singleton() {
		sb.WriteString(e.Grouping.String())
	}
	sb.WriteString("(")
	sb.WriteString(strings.Join(params, ","))
	sb.WriteString(")")
	return sb.String()
}

func (e *BinOpExpr) String() string {
	if e.SampleExpr == nil || e.RHS == nil {
		return ""
	}
	op := e.Op
	if e.Opts != nil {
		if e.Opts.ReturnBool {
			op += " bool"
		}
		if e.Opts.VectorMatching != nil {
			op += vectorMatchingString(e.Opts.VectorMatching)
		}
	}
	return fmt.Sprintf("%s %s %s", e.SampleExpr.String(), op, e.RHS.String())
}

func vectorMatchingString(vm *VectorMatching) string {
	var sb strings.Builder
	if vm.On {
		fmt.Fprintf(&sb, " on(%s)", strings.Join(vm.MatchingLabels, ","))
	} else if len(vm.MatchingLabels) > 0 {
		fmt.Fprintf(&sb, " ignoring(%s)", strings.Join(vm.MatchingLabels, ","))
	}
	switch vm.Card {
	case CardManyToOne:
		fmt.Fprintf(&sb, " group_left(%s)", strings.Join(vm.Include, ","))
	case CardOneToMany:
		fmt.Fprintf(&sb, " group_right(%s)", strings.Join(vm.Include, ","))
	}
	return sb.String()
}

func (e *LiteralExpr) String() string { return fmt.Sprint(e.Val) }

func (e *LabelReplaceExpr) String() string {
	return fmt.Sprintf("%s(%s,%q,%q,%q,%q)", OpLabelReplace, e.Left.String(), e.Dst, e.Replacement, e.Src, e.Regex)
}

func (e *VectorExpr) String() string {
	return fmt.Sprintf("%s(%f)", OpTypeVector, e.Val)
}

func (m *MultiVariantExpr) String() string {
	parts := make([]string, 0, len(m.variants))
	for _, v := range m.variants {
		parts = append(parts, v.String())
	}
	return fmt.Sprintf("%s(%s) %s (%s)", OpVariants, strings.Join(parts, ", "), VariantsOf, m.logRange.String())
}
