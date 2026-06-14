package inventory

import (
	"fmt"
	"sort"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// logQLSource documents where the LogQL inventory comes from. Unlike
// PromQL — whose parser exports an enumerable parser.Functions table —
// the pinned grafana/loki syntax package exports no feature table at
// all, so every row is hand-curated and parser-pinned: a row whose
// canonical pin expression stops parsing under a parser bump fails
// generation loudly, keeping the inventory honest against upstream
// drift.
const logQLSource = "github.com/grafana/loki/v3/pkg/logql/syntax " +
	"(hand-curated, parser-pinned existence checks; tsouza fork pin in go.mod)"

// GenerateLogQL builds the LogQL feature inventory from the curated
// row table. It returns an error (rather than panicking or silently
// dropping rows) when any pin expression fails to parse or fails to
// round-trip through CollectLogQLFeatureIDs — both indicate the
// pinned parser drifted out from under the inventory's assumptions.
func GenerateLogQL() (*Inventory, error) {
	rows := curatedLogQLRows()

	// Invariants: every pin parses with the same syntax.ParseExpr
	// entrypoint cerberus's Loki head uses (internal/logql/lang.go),
	// and the matcher attributes the pin back to its own row ID.
	for _, r := range rows {
		expr, err := syntax.ParseExpr(r.Pin)
		if err != nil {
			return nil, fmt.Errorf("inventory row %s: pin %q does not parse: %w", r.ID, r.Pin, err)
		}
		got := CollectLogQLFeatureIDs(expr)
		if !got[r.ID] {
			return nil, fmt.Errorf(
				"inventory row %s: pin %q is not matched back to its own ID (matcher saw %v)",
				r.ID, r.Pin, sortedKeys(got),
			)
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return &Inventory{QL: "logql", Source: logQLSource, Rows: rows}, nil
}

// mkRow is the Row literal shorthand shared by the curated-row
// builders below.
func mkRow(id, class, token, pin string) Row {
	return Row{ID: id, Class: class, Token: token, Pin: pin}
}

// curatedLogQLRows returns the hand-curated LogQL feature rows. Each
// row's Pin is the parser-pinned existence check: GenerateLogQL fails
// if the pinned parser stops accepting it.
func curatedLogQLRows() []Row {
	rows := []Row{}
	rows = append(rows, logQLFilterRows()...)
	rows = append(rows, logQLPipelineStageRows()...)
	rows = append(rows, logQLAggregationRows()...)
	rows = append(rows, logQLBinaryAndFeatureRows()...)
	return rows
}

// logQLFilterRows covers the line-filter and label-filter families.
func logQLFilterRows() []Row {
	mk := mkRow
	rows := []Row{}

	// Line filters — `|=` / `!=` / `|~` / `!~`, the Loki 3.x pattern
	// match filters `|>` / `!>`, `or`-chained alternates, and the
	// `ip(...)` function filter.
	for _, f := range []struct{ tok, pin string }{
		{"|=", `{service_name="api"} |= "handled"`},
		{"!=", `{service_name="api"} != "refused"`},
		{"|~", `{service_name="api"} |~ "handled|cache"`},
		{"!~", `{service_name="api"} !~ "refused|slow"`},
		{"|>", `{service_name="api"} |> "<_> id=<_>"`},
		{"!>", `{service_name="api"} !> "<_> id=<_>"`},
		{"or", `{service_name="api"} |= "handled" or "cache"`},
		{"ip", `{service_name="api"} |= ip("192.168.0.0/16")`},
	} {
		rows = append(rows, mk("linefilter:"+f.tok, "line-filter", f.tok, f.pin))
	}

	// Label filters — string matchers, the typed numeric / duration /
	// bytes comparisons, `and` / `or` conjunctions, and `ip(...)`.
	for _, f := range []struct{ tok, pin string }{
		{"string", `{service_name="api"} | detected_level="error"`},
		{"number", `{service_name="api"} | logfmt | status >= 500`},
		{"duration", `{service_name="api"} | logfmt | took > 100ms`},
		{"bytes", `{service_name="api"} | logfmt | size > 1KB`},
		{"and", `{service_name="api"} | logfmt | status >= 200 and status < 300`},
		{"or", `{service_name="api"} | detected_level="error" or detected_level="warn"`},
		{"ip", `{service_name="api"} | logfmt | remote_addr = ip("10.0.0.0/8")`},
	} {
		rows = append(rows, mk("labelfilter:"+f.tok, "label-filter", f.tok, f.pin))
	}

	return rows
}

// logQLPipelineStageRows covers parser stages, format / label-set
// manipulation stages, and unwrap conversions.
func logQLPipelineStageRows() []Row {
	mk := mkRow
	rows := []Row{}

	// Parser stages — bare and parameterised forms.
	for _, p := range []struct{ tok, pin string }{
		{"json", `{service_name="api"} | json`},
		{"json-expressions", `{service_name="api"} | json status="response.status"`},
		{"logfmt", `{service_name="api"} | logfmt`},
		{"logfmt-expressions", `{service_name="api"} | logfmt lvl="level"`},
		{"pattern", `{service_name="api"} | pattern "<verb> <path> id=<id>"`},
		{"regexp", `{service_name="api"} | regexp "id=(?P<id>\\d+)"`},
		{"unpack", `{service_name="api"} | unpack`},
	} {
		rows = append(rows, mk("parser:"+p.tok, "parser", p.tok, p.pin))
	}

	// Format + label-set manipulation stages.
	rows = append(
		rows,
		mk("fmt:line_format", "format", "line_format",
			`{service_name="api"} | line_format "{{.service_name}}: {{__line__}}"`),
		mk("fmt:label_format", "format", "label_format",
			`{service_name="api"} | logfmt | label_format lvl=level`),
		mk("stage:decolorize", "stage", "decolorize",
			`{service_name="api"} | decolorize`),
		mk("stage:drop", "stage", "drop",
			`{service_name="api"} | logfmt | drop took`),
		mk("stage:keep", "stage", "keep",
			`{service_name="api"} | logfmt | keep level, status`),
	)

	// Unwrap — bare plus the duration / duration_seconds / bytes
	// conversion functions.
	for _, u := range []struct{ tok, pin string }{
		{"bare", `sum_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{"duration", `avg_over_time({service_name="api"} | logfmt | unwrap duration(took) [5m])`},
		{"duration_seconds", `avg_over_time({service_name="api"} | logfmt | unwrap duration_seconds(took) [5m])`},
		{"bytes", `max_over_time({service_name="api"} | logfmt | unwrap bytes(size) [5m])`},
	} {
		rows = append(rows, mk("unwrap:"+u.tok, "unwrap", u.tok, u.pin))
	}

	return rows
}

// logQLAggregationRows covers range aggregations, vector aggregations,
// and the by / without grouping modifiers.
func logQLAggregationRows() []Row {
	mk := mkRow
	rows := []Row{}

	// Range aggregations.
	for _, r := range []struct{ tok, pin string }{
		{syntax.OpRangeTypeRate, `rate({service_name="api"}[5m])`},
		{syntax.OpRangeTypeRateCounter, `rate_counter({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeCount, `count_over_time({service_name="api"}[5m])`},
		{syntax.OpRangeTypeBytes, `bytes_over_time({service_name="api"}[5m])`},
		{syntax.OpRangeTypeBytesRate, `bytes_rate({service_name="api"}[5m])`},
		{syntax.OpRangeTypeAvg, `avg_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeSum, `sum_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeMin, `min_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeMax, `max_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeStddev, `stddev_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeStdvar, `stdvar_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeQuantile, `quantile_over_time(0.95, {service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeFirst, `first_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeLast, `last_over_time({service_name="api"} | logfmt | unwrap status [5m])`},
		{syntax.OpRangeTypeAbsent, `absent_over_time({service_name="cerberus-showcase-never-logged"}[5m])`},
	} {
		rows = append(rows, mk("range:"+r.tok, "range-aggregation", r.tok, r.pin))
	}

	// Vector aggregations (incl. the K-shaped and ordering operators
	// and the probabilistic approx_topk, all of which parse).
	for _, a := range []struct{ tok, pin string }{
		{syntax.OpTypeSum, `sum(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeAvg, `avg(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeMin, `min(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeMax, `max(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeCount, `count(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeStddev, `stddev(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeStdvar, `stdvar(rate({service_name="api"}[5m]))`},
		{syntax.OpTypeTopK, `topk(3, rate({service_name="api"}[5m]))`},
		{syntax.OpTypeBottomK, `bottomk(3, rate({service_name="api"}[5m]))`},
		{syntax.OpTypeSort, `sort(sum by (service_name) (rate({service_name="api"}[5m])))`},
		{syntax.OpTypeSortDesc, `sort_desc(sum by (service_name) (rate({service_name="api"}[5m])))`},
		{syntax.OpTypeApproxTopK, `approx_topk(3, rate({service_name="api"}[5m]))`},
	} {
		rows = append(rows, mk("agg:"+a.tok, "aggregation", a.tok, a.pin))
	}
	rows = append(
		rows,
		mk("agg-mod:by", "aggregation-modifier", "by",
			`sum by (service_name) (rate({service_name=~".+"}[5m]))`),
		mk("agg-mod:without", "aggregation-modifier", "without",
			`sum without (service_name) (rate({service_name=~".+"}[5m]))`),
	)

	return rows
}

// logQLBinaryAndFeatureRows covers the binary-operator families, the
// vector-matching modifiers, and the standalone expression features.
func logQLBinaryAndFeatureRows() []Row {
	mk := mkRow
	rows := []Row{}

	// Binary operators.
	for _, o := range []struct{ op, pin string }{
		{"+", `rate({service_name="api"}[5m]) + 1`},
		{"-", `rate({service_name="api"}[5m]) - 1`},
		{"*", `rate({service_name="api"}[5m]) * 2`},
		{"/", `rate({service_name="api"}[5m]) / 2`},
		{"%", `count_over_time({service_name="api"}[5m]) % 2`},
		{"^", `rate({service_name="api"}[5m]) ^ 2`},
	} {
		rows = append(rows, mk("op:"+o.op, "binary-arithmetic", o.op, o.pin))
	}
	for _, o := range []struct{ op, pin string }{
		{"==", `count_over_time({service_name="api"}[5m]) == 8`},
		{"!=", `count_over_time({service_name="api"}[5m]) != 8`},
		{">", `count_over_time({service_name="api"}[5m]) > 0`},
		{"<", `count_over_time({service_name="api"}[5m]) < 1000000`},
		{">=", `count_over_time({service_name="api"}[5m]) >= 0`},
		{"<=", `count_over_time({service_name="api"}[5m]) <= 1000000`},
	} {
		rows = append(rows, mk("op:"+o.op, "binary-comparison", o.op, o.pin))
	}
	for _, o := range []struct{ op, pin string }{
		{"and", `rate({service_name="api"}[5m]) and rate({service_name="api"}[5m])`},
		{"or", `rate({service_name="api"}[5m]) or rate({service_name="api"}[5m])`},
		{"unless", `rate({service_name="api"}[5m]) unless rate({service_name="db"}[5m])`},
	} {
		rows = append(rows, mk("op:"+o.op, "binary-set", o.op, o.pin))
	}
	rows = append(
		rows,
		mk("op-mod:bool", "binary-modifier", "bool",
			`count_over_time({service_name="api"}[5m]) > bool 10`),
		mk("match:on", "vector-matching", "on",
			`sum by (service_name) (rate({service_name=~".+"}[5m])) * on (service_name) sum by (service_name) (count_over_time({service_name=~".+"}[5m]))`),
		mk("match:ignoring", "vector-matching", "ignoring",
			`rate({service_name="api"}[5m]) * ignoring (thread) rate({service_name="api"}[5m])`),
		mk("match:group_left", "vector-matching", "group_left",
			`sum by (service_name, detected_level) (rate({service_name=~".+"}[5m])) * on (service_name) group_left sum by (service_name) (count_over_time({service_name=~".+"}[5m]))`),
		mk("match:group_right", "vector-matching", "group_right",
			`sum by (service_name) (count_over_time({service_name=~".+"}[5m])) * on (service_name) group_right sum by (service_name, detected_level) (rate({service_name=~".+"}[5m]))`),
	)

	// Selector / expression features.
	rows = append(
		rows,
		mk("feature:offset", "feature", "offset",
			`count_over_time({service_name="api"}[5m] offset 5m)`),
		mk("feature:label_replace", "feature", "label_replace",
			`label_replace(rate({service_name="api"}[5m]), "svc", "$1", "service_name", "(.*)")`),
		mk("feature:vector", "feature", "vector", `vector(1)`),
		mk("feature:variants", "feature", "variants",
			`variants(count_over_time({service_name="api"}[5m]), bytes_over_time({service_name="api"}[5m])) of ({service_name="api"}[5m])`),
	)
	return rows
}

// CollectLogQLFeatureIDs walks a parsed LogQL expression and returns
// the set of inventory row IDs the expression exercises. This is the
// strongest matcher shape: token-level substring matching would let
// "rate" match "bytes_rate"; an AST walk cannot.
//
// The walk uses the syntax package's DepthFirstTraversal. NOTE: a set
// VisitXFn REPLACES the default child recursion for that node type, so
// every Fn below re-implements the recursion the default branch would
// have performed (see syntax/visit.go).
func CollectLogQLFeatureIDs(expr syntax.Expr) map[string]bool {
	ids := map[string]bool{}
	v := &syntax.DepthFirstTraversal{}

	v.VisitLineFilterFn = func(rv syntax.RootVisitor, e *syntax.LineFilterExpr) {
		collectLineFilterPart(ids, &e.LineFilter)
		if e.Or != nil {
			ids["linefilter:or"] = true
			e.Or.Accept(rv)
		}
		if e.Left != nil {
			e.Left.Accept(rv)
		}
	}
	v.VisitLabelFilterFn = func(_ syntax.RootVisitor, e *syntax.LabelFilterExpr) {
		collectLabelFilterer(ids, e.LabelFilterer)
	}
	v.VisitLabelParserFn = func(_ syntax.RootVisitor, e *syntax.LineParserExpr) {
		ids["parser:"+e.Op] = true
	}
	v.VisitLogfmtParserFn = func(_ syntax.RootVisitor, _ *syntax.LogfmtParserExpr) {
		ids["parser:logfmt"] = true
	}
	v.VisitLogfmtExpressionParserFn = func(_ syntax.RootVisitor, _ *syntax.LogfmtExpressionParserExpr) {
		ids["parser:logfmt-expressions"] = true
	}
	v.VisitJSONExpressionParserFn = func(_ syntax.RootVisitor, _ *syntax.JSONExpressionParserExpr) {
		ids["parser:json-expressions"] = true
	}
	v.VisitLineFmtFn = func(_ syntax.RootVisitor, _ *syntax.LineFmtExpr) {
		ids["fmt:line_format"] = true
	}
	v.VisitLabelFmtFn = func(_ syntax.RootVisitor, _ *syntax.LabelFmtExpr) {
		ids["fmt:label_format"] = true
	}
	v.VisitDecolorizeFn = func(_ syntax.RootVisitor, _ *syntax.DecolorizeExpr) {
		ids["stage:decolorize"] = true
	}
	v.VisitDropLabelsFn = func(_ syntax.RootVisitor, _ *syntax.DropLabelsExpr) {
		ids["stage:drop"] = true
	}
	v.VisitKeepLabelFn = func(_ syntax.RootVisitor, _ *syntax.KeepLabelsExpr) {
		ids["stage:keep"] = true
	}
	v.VisitLogRangeFn = func(rv syntax.RootVisitor, e *syntax.LogRangeExpr) {
		if e.Unwrap != nil {
			switch e.Unwrap.Operation {
			case "":
				ids["unwrap:bare"] = true
			case syntax.OpConvDuration:
				ids["unwrap:"+syntax.OpConvDuration] = true
			case syntax.OpConvDurationSeconds:
				ids["unwrap:"+syntax.OpConvDurationSeconds] = true
			case syntax.OpConvBytes:
				ids["unwrap:"+syntax.OpConvBytes] = true
			}
		}
		if e.Offset != 0 {
			ids["feature:offset"] = true
		}
		e.Left.Accept(rv)
	}
	v.VisitRangeAggregationFn = func(rv syntax.RootVisitor, e *syntax.RangeAggregationExpr) {
		ids["range:"+e.Operation] = true
		collectGrouping(ids, e.Grouping)
		e.Left.Accept(rv)
	}
	v.VisitVectorAggregationFn = func(rv syntax.RootVisitor, e *syntax.VectorAggregationExpr) {
		ids["agg:"+e.Operation] = true
		collectGrouping(ids, e.Grouping)
		e.Left.Accept(rv)
	}
	v.VisitBinOpFn = func(rv syntax.RootVisitor, e *syntax.BinOpExpr) {
		ids["op:"+e.Op] = true
		if e.Opts != nil {
			if e.Opts.ReturnBool {
				ids["op-mod:bool"] = true
			}
			if vm := e.Opts.VectorMatching; vm != nil {
				if vm.On {
					ids["match:on"] = true
				} else if len(vm.MatchingLabels) > 0 {
					ids["match:ignoring"] = true
				}
				switch vm.Card {
				case syntax.CardManyToOne:
					ids["match:group_left"] = true
				case syntax.CardOneToMany:
					ids["match:group_right"] = true
				case syntax.CardOneToOne:
					// The default vector-matching cardinality carries no
					// grouping modifier — already captured by op:* above.
				}
			}
		}
		e.SampleExpr.Accept(rv)
		e.RHS.Accept(rv)
	}
	v.VisitLabelReplaceFn = func(rv syntax.RootVisitor, e *syntax.LabelReplaceExpr) {
		ids["feature:label_replace"] = true
		e.Left.Accept(rv)
	}
	v.VisitVectorFn = func(_ syntax.RootVisitor, _ *syntax.VectorExpr) {
		ids["feature:vector"] = true
	}
	v.VisitVariantsFn = func(rv syntax.RootVisitor, e *syntax.MultiVariantExpr) {
		ids["feature:variants"] = true
		// Mirror the default recursion so the variant arms and the shared
		// `of (...)` selector still contribute their own feature IDs.
		e.LogRange().Accept(rv)
		for _, vrt := range e.Variants() {
			vrt.Accept(rv)
		}
	}

	expr.Accept(v)
	return ids
}

// collectLineFilterPart records the row IDs for one LineFilter leg —
// the match-type token plus the `ip(...)` function-filter marker when
// the leg carries it.
func collectLineFilterPart(ids map[string]bool, lf *syntax.LineFilter) {
	switch lf.Ty {
	case loglib.LineMatchEqual:
		ids["linefilter:|="] = true
	case loglib.LineMatchNotEqual:
		ids["linefilter:!="] = true
	case loglib.LineMatchRegexp:
		ids["linefilter:|~"] = true
	case loglib.LineMatchNotRegexp:
		ids["linefilter:!~"] = true
	case loglib.LineMatchPattern:
		ids["linefilter:|>"] = true
	case loglib.LineMatchNotPattern:
		ids["linefilter:!>"] = true
	}
	if lf.Op == syntax.OpFilterIP {
		ids["linefilter:ip"] = true
	}
}

// collectLabelFilterer walks the (possibly nested) label-filter tree.
// The LabelFilterer node kinds live in pkg/logql/log, not in the
// syntax package, so this is a manual recursion rather than a visitor
// dispatch.
func collectLabelFilterer(ids map[string]bool, lf loglib.LabelFilterer) {
	switch f := lf.(type) {
	case *loglib.StringLabelFilter:
		ids["labelfilter:string"] = true
	case *loglib.LineFilterLabelFilter:
		ids["labelfilter:string"] = true
	case *loglib.NumericLabelFilter:
		ids["labelfilter:number"] = true
	case *loglib.DurationLabelFilter:
		ids["labelfilter:duration"] = true
	case *loglib.BytesLabelFilter:
		ids["labelfilter:bytes"] = true
	case *loglib.IPLabelFilter:
		ids["labelfilter:ip"] = true
	case *loglib.BinaryLabelFilter:
		if f.And {
			ids["labelfilter:and"] = true
		} else {
			ids["labelfilter:or"] = true
		}
		collectLabelFilterer(ids, f.Left)
		collectLabelFilterer(ids, f.Right)
	}
}

// collectGrouping records the by / without modifier rows shared by
// vector and range aggregations. A nil Grouping (or an empty `by ()`
// shape, which Loki parses as zero groups without the Without flag)
// records nothing.
func collectGrouping(ids map[string]bool, g *syntax.Grouping) {
	if g == nil {
		return
	}
	if g.Without {
		ids["agg-mod:without"] = true
		return
	}
	if len(g.Groups) > 0 {
		ids["agg-mod:by"] = true
	}
}
