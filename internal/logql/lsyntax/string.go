package lsyntax

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/prometheus/common/model"
)

// The String methods render an AST back into canonical LogQL. They back
// the `/loki/api/v1/format_query` endpoint and the `%s` formatting of
// matchers; they are not used by the SQL lowering.

// String renders the selector as `{` + its matchers joined by ", " + `}`.
func (e *MatchersExpr) String() string {
	rendered := make([]string, len(e.Mts))
	for i, m := range e.Mts {
		rendered[i] = m.String()
	}
	return "{" + strings.Join(rendered, ", ") + "}"
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

// String renders the chain the filter closes, then its own operator (or
// `or` for an alternate), its quoted operand, and its next alternate.
func (e *LineFilterExpr) String() string {
	var words []string
	if e.Left != nil {
		words = append(words, e.Left.String())
	}
	if e.IsOrChild {
		words = append(words, OpTypeOr)
	} else {
		words = append(words, e.Ty.String())
	}
	operand := strconv.Quote(e.Match)
	if e.Op == OpFilterIP {
		operand = OpFilterIP + "(" + operand + ")"
	}
	words = append(words, operand)
	if e.Or != nil {
		words = append(words, e.Or.String())
	}
	return strings.Join(words, " ")
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

// String renders ` | unwrap <target>` followed by ` | <filter>` for each
// post-unwrap filter, where the target is the label, or the conversion
// applied to it.
func (u *UnwrapExpr) String() string {
	target := u.Identifier
	if u.Operation != "" {
		target = u.Operation + "(" + u.Identifier + ")"
	}
	words := []string{"", OpPipe, OpUnwrap, target}
	for _, f := range u.PostFilters {
		words = append(words, OpPipe, f.String())
	}
	return strings.Join(words, " ")
}

func (e *OffsetExpr) String() string {
	return fmt.Sprintf(" %s %s", OpOffset, model.Duration(e.Offset).String())
}

// String renders the selector, any unwrap, the bracketed range, and any
// offset.
func (e *LogRangeExpr) String() string {
	rendered := e.Left.String()
	if e.Unwrap != nil {
		rendered += e.Unwrap.String()
	}
	rendered += "[" + model.Duration(e.Interval).String() + "]"
	if e.Offset == 0 {
		return rendered
	}
	return rendered + newOffsetExpr(e.Offset).String()
}

// String renders ` by (a,b)` or ` without (a,b)`.
func (g Grouping) String() string {
	keyword := "by"
	if g.Without {
		keyword = "without"
	}
	return " " + keyword + " (" + strings.Join(g.Groups, ",") + ")"
}

func (g Grouping) singleton() bool { return len(g.Groups) == 0 && !g.Without }

// String renders `op(range)` or `op(param,range)`, then the grouping.
func (e *RangeAggregationExpr) String() string {
	args := e.Left.String()
	if e.Params != nil {
		args = sixDecimals(*e.Params) + "," + args
	}
	out := e.Operation + "(" + args + ")"
	if e.Grouping != nil {
		out += e.Grouping.String()
	}
	return out
}

// sixDecimals renders v rounded to six decimal places, without trailing
// zeros or a trailing decimal point.
func sixDecimals(v float64) string {
	fixed := strings.TrimRight(strconv.FormatFloat(v, 'f', 6, 64), "0")
	return strings.TrimSuffix(fixed, ".")
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
