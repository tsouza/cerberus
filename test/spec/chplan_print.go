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
		if len(v.Columns) == 0 {
			fmt.Fprintf(b, "%sScan(%s)\n", indent, v.Table)
		} else {
			fmt.Fprintf(b, "%sScan(%s, columns=[%s])\n", indent, v.Table, strings.Join(v.Columns, ", "))
		}
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
			if i < len(v.GroupByAliases) && v.GroupByAliases[i] != "" {
				gb[i] = fmt.Sprintf("%s AS %s", printExpr(e), v.GroupByAliases[i])
			} else {
				gb[i] = printExpr(e)
			}
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
		if v.ValueAlias != "" {
			fmt.Fprintf(b, " valueAlias=%s", v.ValueAlias)
		}
		b.WriteString("\n")
		if v.Inner != nil {
			printNode(b, v.Inner, depth+1)
		}
	case *chplan.MetricsHistogramOverTime:
		gb := make([]string, len(v.GroupBy))
		for i, e := range v.GroupBy {
			if i < len(v.GroupByAliases) && v.GroupByAliases[i] != "" {
				gb[i] = fmt.Sprintf("%s AS %s", printExpr(e), v.GroupByAliases[i])
			} else {
				gb[i] = printExpr(e)
			}
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
