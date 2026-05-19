// Package spec — chplan IR pretty-printer used by TXTAR `-- chplan --` golden
// sections.
//
// Renders an internal/chplan tree as a deterministic, human-readable string
// with one node per line and two-space indentation showing parent/child
// relationships. Expression operands are inlined on the same line as the node
// whose field they belong to.
//
// The printer is intentionally stable: no map iteration, no time.Now, no
// pointer addresses — same input → same output across calls and across runs.

package spec

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// PrintChplan returns the IR tree as a deterministic multi-line string.
// Each line ends with `\n`; the result has a trailing newline.
func PrintChplan(n chplan.Node) string {
	if n == nil {
		return "<nil>\n"
	}
	var b strings.Builder
	printNode(&b, n, 0)
	return b.String()
}

func printNode(b *strings.Builder, n chplan.Node, depth int) {
	indent := strings.Repeat("  ", depth)
	if n == nil {
		fmt.Fprintf(b, "%s<nil>\n", indent)
		return
	}
	switch v := n.(type) {
	case *chplan.Scan:
		tbl := v.Table
		if v.Database != "" {
			tbl = v.Database + "." + v.Table
		}
		if len(v.Columns) == 0 {
			fmt.Fprintf(b, "%sScan(%s)\n", indent, tbl)
		} else {
			fmt.Fprintf(b, "%sScan(%s, columns=[%s])\n", indent, tbl, strings.Join(v.Columns, ", "))
		}
	case *chplan.OneRow:
		fmt.Fprintf(b, "%sOneRow\n", indent)
		_ = v
	case *chplan.StepGrid:
		fmt.Fprintf(b, "%sStepGrid start=%s end=%s step=%s\n",
			indent, v.Start.Format("2006-01-02T15:04:05.000000000Z"),
			v.End.Format("2006-01-02T15:04:05.000000000Z"), v.Step)
	case *chplan.CrossJoin:
		fmt.Fprintf(b, "%sCrossJoin\n", indent)
		printNode(b, v.Left, depth+1)
		printNode(b, v.Right, depth+1)
	case *chplan.Filter:
		fmt.Fprintf(b, "%sFilter predicate=%s\n", indent, printExpr(v.Predicate))
		printNode(b, v.Input, depth+1)
	case *chplan.Project:
		parts := make([]string, len(v.Projections))
		for i, p := range v.Projections {
			if p.Alias != "" {
				parts[i] = fmt.Sprintf("%s AS %s", printExpr(p.Expr), p.Alias)
			} else {
				parts[i] = printExpr(p.Expr)
			}
		}
		if len(parts) == 0 {
			fmt.Fprintf(b, "%sProject *\n", indent)
		} else {
			fmt.Fprintf(b, "%sProject [%s]\n", indent, strings.Join(parts, ", "))
		}
		printNode(b, v.Input, depth+1)
	case *chplan.Aggregate:
		gb := make([]string, len(v.GroupBy))
		for i, e := range v.GroupBy {
			if i < len(v.GroupByAliases) && v.GroupByAliases[i] != "" {
				gb[i] = fmt.Sprintf("%s AS %s", printExpr(e), v.GroupByAliases[i])
			} else {
				gb[i] = printExpr(e)
			}
		}
		aggs := make([]string, len(v.AggFuncs))
		for i, f := range v.AggFuncs {
			aggs[i] = printAggFunc(f)
		}
		fmt.Fprintf(b, "%sAggregate groupBy=[%s] funcs=[%s]\n",
			indent, strings.Join(gb, ", "), strings.Join(aggs, ", "))
		printNode(b, v.Input, depth+1)
	case *chplan.Limit:
		fmt.Fprintf(b, "%sLimit %d\n", indent, v.Count)
		printNode(b, v.Input, depth+1)
	case *chplan.TopK:
		dir := "ASC"
		if v.Desc {
			dir = "DESC"
		}
		if v.KExpr != nil {
			fmt.Fprintf(b, "%sTopK k=<expr> sort=%s %s", indent, printExpr(v.SortExpr), dir)
		} else {
			fmt.Fprintf(b, "%sTopK k=%d sort=%s %s", indent, v.K, printExpr(v.SortExpr), dir)
		}
		if len(v.By) > 0 {
			bys := make([]string, len(v.By))
			for i, e := range v.By {
				bys[i] = printExpr(e)
			}
			fmt.Fprintf(b, " by=[%s]", strings.Join(bys, ", "))
		}
		if len(v.Columns) > 0 {
			fmt.Fprintf(b, " columns=[%s]", strings.Join(v.Columns, ", "))
		}
		b.WriteString("\n")
		printNode(b, v.Input, depth+1)
		if v.KExpr != nil {
			fmt.Fprintf(b, "%s  KExpr:\n", indent)
			printNode(b, v.KExpr, depth+2)
		}
	case *chplan.OrderBy:
		keys := make([]string, len(v.Keys))
		for i, k := range v.Keys {
			dir := "ASC"
			if k.Desc {
				dir = "DESC"
			}
			keys[i] = fmt.Sprintf("%s %s", printExpr(k.Expr), dir)
		}
		fmt.Fprintf(b, "%sOrderBy [%s]\n", indent, strings.Join(keys, ", "))
		printNode(b, v.Input, depth+1)
	case *chplan.RangeWindow:
		fmt.Fprintf(b, "%sRangeWindow func=%s range=%s step=%s",
			indent, v.Func, v.Range, v.Step)
		if v.OuterRange != 0 {
			fmt.Fprintf(b, " outerRange=%s", v.OuterRange)
		}
		if v.Offset != 0 {
			fmt.Fprintf(b, " offset=%s", v.Offset)
		}
		if v.Identity {
			b.WriteString(" identity=true")
		}
		if v.TimestampColumn != "" {
			fmt.Fprintf(b, " ts=%s", v.TimestampColumn)
		}
		if v.ValueColumn != "" {
			fmt.Fprintf(b, " value=%s", v.ValueColumn)
		}
		if len(v.GroupBy) > 0 {
			gb := make([]string, len(v.GroupBy))
			for i, e := range v.GroupBy {
				gb[i] = printExpr(e)
			}
			fmt.Fprintf(b, " groupBy=[%s]", strings.Join(gb, ", "))
		}
		if len(v.Scalars) > 0 {
			ss := make([]string, len(v.Scalars))
			for i, s := range v.Scalars {
				ss[i] = strconv.FormatFloat(s, 'g', -1, 64)
			}
			fmt.Fprintf(b, " scalars=[%s]", strings.Join(ss, ", "))
		}
		if !v.Start.IsZero() || !v.End.IsZero() {
			fmt.Fprintf(b, " start=%s end=%s", v.Start.UTC().Format("2006-01-02T15:04:05Z"), v.End.UTC().Format("2006-01-02T15:04:05Z"))
		}
		b.WriteString("\n")
		printNode(b, v.Input, depth+1)
	case *chplan.VectorJoin:
		fmt.Fprintf(b, "%sVectorJoin op=%s match=%s card=%s",
			indent, v.Op, printVectorMatch(v.Match), printVectorCard(v.Card))
		if len(v.Include) > 0 {
			fmt.Fprintf(b, " include=[%s]", strings.Join(v.Include, ", "))
		}
		if v.ReturnBool {
			b.WriteString(" bool")
		}
		b.WriteString("\n")
		printNode(b, v.Left, depth+1)
		printNode(b, v.Right, depth+1)
	case *chplan.VectorSetOp:
		fmt.Fprintf(b, "%sVectorSetOp op=%s match=%s\n",
			indent, v.Op, printVectorMatch(v.Match))
		printNode(b, v.Left, depth+1)
		printNode(b, v.Right, depth+1)
	case *chplan.StructuralJoin:
		fmt.Fprintf(b, "%sStructuralJoin op=%s", indent, v.Op)
		if v.MaxDepth != 0 {
			fmt.Fprintf(b, " maxDepth=%d", v.MaxDepth)
		}
		b.WriteString("\n")
		printNode(b, v.Left, depth+1)
		printNode(b, v.Right, depth+1)
	case *chplan.SetOperation:
		fmt.Fprintf(b, "%sSetOperation op=%s\n", indent, v.Op)
		printNode(b, v.Left, depth+1)
		printNode(b, v.Right, depth+1)
	case *chplan.MetricsAggregate:
		gb := make([]string, len(v.GroupBy))
		for i, e := range v.GroupBy {
			alias := ""
			if i < len(v.GroupByAliases) {
				alias = v.GroupByAliases[i]
			}
			display := ""
			if i < len(v.GroupByDisplayNames) {
				display = v.GroupByDisplayNames[i]
			}
			gb[i] = formatGroupByEntry(printExpr(e), alias, display)
		}
		fmt.Fprintf(b, "%sMetricsAggregate op=%s", indent, v.Op)
		if v.Attr != nil {
			fmt.Fprintf(b, " attr=%s", printExpr(v.Attr))
		}
		if len(gb) > 0 {
			fmt.Fprintf(b, " groupBy=[%s]", strings.Join(gb, ", "))
		}
		if len(v.Quantiles) > 0 {
			qs := make([]string, len(v.Quantiles))
			for i, q := range v.Quantiles {
				qs[i] = strconv.FormatFloat(q, 'g', -1, 64)
			}
			fmt.Fprintf(b, " quantiles=[%s]", strings.Join(qs, ", "))
		}
		if v.IsDuration {
			b.WriteString(" duration=true")
		}
		if v.ValueAlias != "" {
			fmt.Fprintf(b, " valueAlias=%s", v.ValueAlias)
		}
		b.WriteString("\n")
		if v.Inner != nil {
			printNode(b, v.Inner, depth+1)
		}
	case *chplan.MetricsSecondStage:
		fmt.Fprintf(b, "%sMetricsSecondStage op=%s", indent, v.Op)
		switch v.Op {
		case chplan.SecondStageTopK, chplan.SecondStageBottomK:
			fmt.Fprintf(b, " k=%d", v.K)
		case chplan.SecondStageThreshold:
			fmt.Fprintf(b, " threshold=(%s %s)", v.ThresholdOp,
				strconv.FormatFloat(v.ThresholdValue, 'g', -1, 64))
		}
		if len(v.PartitionBy) > 0 {
			fmt.Fprintf(b, " partitionBy=[%s]", strings.Join(v.PartitionBy, ", "))
		}
		if v.ValueAlias != "" {
			fmt.Fprintf(b, " valueAlias=%s", v.ValueAlias)
		}
		b.WriteString("\n")
		if v.Input != nil {
			printNode(b, v.Input, depth+1)
		}
	case *chplan.MetricsHistogramOverTime:
		gb := make([]string, len(v.GroupBy))
		for i, e := range v.GroupBy {
			alias := ""
			if i < len(v.GroupByAliases) {
				alias = v.GroupByAliases[i]
			}
			display := ""
			if i < len(v.GroupByDisplayNames) {
				display = v.GroupByDisplayNames[i]
			}
			gb[i] = formatGroupByEntry(printExpr(e), alias, display)
		}
		fmt.Fprintf(b, "%sMetricsHistogramOverTime", indent)
		if v.Attr != nil {
			fmt.Fprintf(b, " attr=%s", printExpr(v.Attr))
		}
		if v.IsDuration {
			b.WriteString(" duration=true")
		}
		if len(gb) > 0 {
			fmt.Fprintf(b, " groupBy=[%s]", strings.Join(gb, ", "))
		}
		if v.BucketAlias != "" {
			fmt.Fprintf(b, " bucketAlias=%s", v.BucketAlias)
		}
		if v.ValueAlias != "" {
			fmt.Fprintf(b, " valueAlias=%s", v.ValueAlias)
		}
		b.WriteString("\n")
		if v.Inner != nil {
			printNode(b, v.Inner, depth+1)
		}
	case *chplan.AbsentOverTime:
		fmt.Fprintf(b, "%sAbsentOverTime range=%s step=%s", indent, v.Range, v.Step)
		if v.Offset != 0 {
			fmt.Fprintf(b, " offset=%s", v.Offset)
		}
		if v.TimestampColumn != "" {
			fmt.Fprintf(b, " ts=%s", v.TimestampColumn)
		}
		if v.ValueColumn != "" {
			fmt.Fprintf(b, " value=%s", v.ValueColumn)
		}
		if v.MetricNameColumn != "" {
			fmt.Fprintf(b, " name=%s", v.MetricNameColumn)
		}
		if v.AttributesColumn != "" {
			fmt.Fprintf(b, " attrs=%s", v.AttributesColumn)
		}
		if len(v.SynthLabels) > 0 {
			parts := make([]string, len(v.SynthLabels))
			for i, kv := range v.SynthLabels {
				parts[i] = fmt.Sprintf("%s=%q", kv.Key, kv.Value)
			}
			fmt.Fprintf(b, " synth=[%s]", strings.Join(parts, ", "))
		}
		if !v.Start.IsZero() || !v.End.IsZero() {
			fmt.Fprintf(b, " start=%s end=%s", v.Start.UTC().Format("2006-01-02T15:04:05Z"), v.End.UTC().Format("2006-01-02T15:04:05Z"))
		}
		b.WriteString("\n")
		printNode(b, v.Input, depth+1)
	case *chplan.HistogramQuantile:
		gb := make([]string, len(v.GroupBy))
		for i, e := range v.GroupBy {
			if i < len(v.GroupByAliases) && v.GroupByAliases[i] != "" {
				gb[i] = fmt.Sprintf("%s AS %s", printExpr(e), v.GroupByAliases[i])
			} else {
				gb[i] = printExpr(e)
			}
		}
		fmt.Fprintf(b, "%sHistogramQuantile phi=%s", indent, strconv.FormatFloat(v.Phi, 'g', -1, 64))
		if len(gb) > 0 {
			fmt.Fprintf(b, " groupBy=[%s]", strings.Join(gb, ", "))
		}
		b.WriteString("\n")
		printNode(b, v.Input, depth+1)
	case *chplan.HistogramQuantileNative:
		gb := make([]string, len(v.GroupBy))
		for i, e := range v.GroupBy {
			if i < len(v.GroupByAliases) && v.GroupByAliases[i] != "" {
				gb[i] = fmt.Sprintf("%s AS %s", printExpr(e), v.GroupByAliases[i])
			} else {
				gb[i] = printExpr(e)
			}
		}
		fmt.Fprintf(b, "%sHistogramQuantileNative phi=%s", indent, strconv.FormatFloat(v.Phi, 'g', -1, 64))
		if len(gb) > 0 {
			fmt.Fprintf(b, " groupBy=[%s]", strings.Join(gb, ", "))
		}
		b.WriteString("\n")
		printNode(b, v.Input, depth+1)
	default:
		fmt.Fprintf(b, "%s<unknown:%T>\n", indent, n)
	}
}

func printExpr(e chplan.Expr) string {
	if e == nil {
		return "<nil>"
	}
	switch v := e.(type) {
	case *chplan.ColumnRef:
		if v.Qualifier != "" {
			return fmt.Sprintf("%s.%s", v.Qualifier, v.Name)
		}
		return v.Name
	case *chplan.LitString:
		return strconv.Quote(v.V)
	case *chplan.LitInt:
		return strconv.FormatInt(v.V, 10)
	case *chplan.LitFloat:
		return strconv.FormatFloat(v.V, 'g', -1, 64)
	case *chplan.LitBool:
		return strconv.FormatBool(v.V)
	case *chplan.Binary:
		return fmt.Sprintf("(%s %s %s)", printExpr(v.Left), v.Op, printExpr(v.Right))
	case *chplan.FuncCall:
		args := make([]string, len(v.Args))
		for i, a := range v.Args {
			args[i] = printExpr(a)
		}
		return fmt.Sprintf("%s(%s)", v.Name, strings.Join(args, ", "))
	case *chplan.MapAccess:
		return fmt.Sprintf("%s[%s]", printExpr(v.Map), printExpr(v.Key))
	case *chplan.FieldAccess:
		return fmt.Sprintf("%s[%q]", printExpr(v.Source), v.Path)
	case *chplan.MapWithoutKeys:
		return fmt.Sprintf("mapWithout(%s, [%s])", printExpr(v.Map), strings.Join(v.Keys, ", "))
	case *chplan.MapWithoutEmptyValues:
		return fmt.Sprintf("mapWithoutEmpty(%s)", printExpr(v.Map))
	case *chplan.LabelReplace:
		return fmt.Sprintf("labelReplace(%s, dst=%q, replacement=%q, src=%q, regex=%q, emptyReplacement=%q)",
			printExpr(v.Map), v.Dst, v.Replacement, v.Src, v.Regex, v.EmptyReplacement)
	case *chplan.LabelJoin:
		return fmt.Sprintf("labelJoin(%s, dst=%q, separator=%q, srcs=[%s])",
			printExpr(v.Map), v.Dst, v.Separator, strings.Join(v.Srcs, ", "))
	case *chplan.LineContent:
		flags := ""
		if v.IsRegex {
			flags += "regex"
		} else {
			flags += "substr"
		}
		if v.Negated {
			flags += ",not"
		}
		return fmt.Sprintf("lineContent(%s, %q, %s)", printExpr(v.Source), v.Pattern, flags)
	case *chplan.NestedArrayExists:
		return fmt.Sprintf("nestedArrayExists(%s.%s[%q] %s %s)",
			v.Column, v.SubField, v.Key, v.Op, printExpr(v.Value))
	case *chplan.Lambda:
		if len(v.Params) == 1 {
			return fmt.Sprintf("%s -> %s", v.Params[0], printExpr(v.Body))
		}
		return fmt.Sprintf("(%s) -> %s", strings.Join(v.Params, ", "), printExpr(v.Body))
	case *chplan.BareIdent:
		return v.Name
	case *chplan.Subscript:
		return fmt.Sprintf("%s[%s]", printExpr(v.Container), printExpr(v.Key))
	default:
		return fmt.Sprintf("<unknown:%T>", e)
	}
}

func printAggFunc(a chplan.AggFunc) string {
	args := make([]string, len(a.Args))
	for i, x := range a.Args {
		args[i] = printExpr(x)
	}
	var head string
	if len(a.Params) > 0 {
		ps := make([]string, len(a.Params))
		for i, p := range a.Params {
			ps[i] = printExpr(p)
		}
		head = fmt.Sprintf("%s(%s)(%s)", a.Name, strings.Join(ps, ", "), strings.Join(args, ", "))
	} else {
		head = fmt.Sprintf("%s(%s)", a.Name, strings.Join(args, ", "))
	}
	if a.Alias != "" {
		return head + " AS " + a.Alias
	}
	return head
}

func printVectorMatch(m chplan.VectorMatch) string {
	if len(m.Labels) == 0 && !m.On {
		return "default"
	}
	kind := "ignoring"
	if m.On {
		kind = "on"
	}
	return fmt.Sprintf("%s(%s)", kind, strings.Join(m.Labels, ","))
}

func printVectorCard(c chplan.VectorCard) string {
	switch c {
	case chplan.CardOneToOne:
		return "one-to-one"
	case chplan.CardManyToOne:
		return "many-to-one"
	case chplan.CardOneToMany:
		return "one-to-many"
	}
	return "unknown"
}

// formatGroupByEntry renders one MetricsAggregate / MetricsHistogramOverTime
// group-by entry for the IR snapshot. When the lowering populated a
// display name (the Tempo-canonical wire form, e.g.
// `resource.service.name`) that differs from the SQL alias (the bare
// `service.name`) the snapshot surfaces both via the `AS alias|display`
// suffix so a reader can tell which name the SQL emitter uses (alias)
// vs. which name the Tempo handler surfaces on the wire (display).
// When they agree (e.g. unscoped attributes, intrinsics like `kind`)
// only the alias prints, keeping legacy fixtures byte-stable.
func formatGroupByEntry(expr, alias, display string) string {
	switch {
	case alias == "" && display == "":
		return expr
	case alias == "" || alias == display:
		return fmt.Sprintf("%s AS %s", expr, display)
	case display == "":
		return fmt.Sprintf("%s AS %s", expr, alias)
	default:
		return fmt.Sprintf("%s AS %s|%s", expr, alias, display)
	}
}
