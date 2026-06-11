package surfaceparity

import (
	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// Domain-aware LogQL operands from the showcase seed
// (test/e2e/seed/cmd/seed/showcase_logql.go). The gateway stream is
// logfmt-formatted with real extractable fields; using it keeps parser
// stages (logfmt/json/...) and unwrap fed structured input.
const (
	// logSelector selects a real seeded log stream.
	logSelector = `{service_name="gateway"}`
	// logUnwrapField is a real numeric field extractable from the
	// gateway logfmt stream, suitable for unwrap + range aggregations.
	logUnwrapField = "status"
	// logShopSelector selects the JSON-formatted shop stream.
	logShopSelector = `{service_name="shop"}`
)

// logProbe is one LogQL grammar symbol + its canonical probe. Probes
// are full pipeline expressions (a stream selector is mandatory in
// LogQL), shaped per op family.
type logProbe struct {
	symbol string
	kind   string
	probe  string
}

// logProbes enumerates the LogQL Op* surface — range aggregations,
// vector aggregations, parser stages, conversion/label functions, line
// + label filters, and binary ops — each with a domain-aware probe.
// Internal/sharding-only ops (the __…__ -prefixed consts that have no
// LogQL surface) are deliberately excluded: they are not part of the
// accepted grammar, so they carry no parity meaning.
var logProbes = []logProbe{
	// Range aggregations over a log range (no unwrap).
	{"range:" + syntax.OpRangeTypeCount, "range-agg", `count_over_time(` + logSelector + `[5m])`},
	{"range:" + syntax.OpRangeTypeRate, "range-agg", `rate(` + logSelector + `[5m])`},
	{"range:" + syntax.OpRangeTypeBytes, "range-agg", `bytes_over_time(` + logSelector + `[5m])`},
	{"range:" + syntax.OpRangeTypeBytesRate, "range-agg", `bytes_rate(` + logSelector + `[5m])`},
	{"range:" + syntax.OpRangeTypeAbsent, "range-agg", `absent_over_time(` + logSelector + `[5m])`},
	// Range aggregations requiring an unwrap (numeric sample stream).
	{"range:" + syntax.OpRangeTypeAvg, "range-agg", unwrapRange("avg_over_time")},
	{"range:" + syntax.OpRangeTypeSum, "range-agg", unwrapRange("sum_over_time")},
	{"range:" + syntax.OpRangeTypeMin, "range-agg", unwrapRange("min_over_time")},
	{"range:" + syntax.OpRangeTypeMax, "range-agg", unwrapRange("max_over_time")},
	{"range:" + syntax.OpRangeTypeStdvar, "range-agg", unwrapRange("stdvar_over_time")},
	{"range:" + syntax.OpRangeTypeStddev, "range-agg", unwrapRange("stddev_over_time")},
	{"range:" + syntax.OpRangeTypeQuantile, "range-agg", `quantile_over_time(0.9, ` + logSelector + ` | logfmt | unwrap ` + logUnwrapField + ` [5m])`},
	{"range:" + syntax.OpRangeTypeFirst, "range-agg", unwrapRange("first_over_time")},
	{"range:" + syntax.OpRangeTypeLast, "range-agg", unwrapRange("last_over_time")},
	{"range:" + syntax.OpRangeTypeRateCounter, "range-agg", `rate_counter(` + logSelector + ` | logfmt | unwrap ` + logUnwrapField + ` [5m])`},

	// Vector aggregations over a range-aggregation result.
	{"vector:" + syntax.OpTypeSum, "vector-agg", vecAgg("sum")},
	{"vector:" + syntax.OpTypeAvg, "vector-agg", vecAgg("avg")},
	{"vector:" + syntax.OpTypeMax, "vector-agg", vecAgg("max")},
	{"vector:" + syntax.OpTypeMin, "vector-agg", vecAgg("min")},
	{"vector:" + syntax.OpTypeCount, "vector-agg", vecAgg("count")},
	{"vector:" + syntax.OpTypeStddev, "vector-agg", vecAgg("stddev")},
	{"vector:" + syntax.OpTypeStdvar, "vector-agg", vecAgg("stdvar")},
	{"vector:" + syntax.OpTypeBottomK, "vector-agg", `bottomk(3, ` + rangeBody() + `)`},
	{"vector:" + syntax.OpTypeTopK, "vector-agg", `topk(3, ` + rangeBody() + `)`},
	{"vector:" + syntax.OpTypeSort, "vector-agg", `sort(` + rangeBody() + `)`},
	{"vector:" + syntax.OpTypeSortDesc, "vector-agg", `sort_desc(` + rangeBody() + `)`},
	{"vector:" + syntax.OpTypeApproxTopK, "vector-agg", `approx_topk(3, ` + rangeBody() + `)`},

	// Parser stages.
	{"parser:" + syntax.OpParserTypeJSON, "parser-stage", logShopSelector + ` | json`},
	{"parser:" + syntax.OpParserTypeLogfmt, "parser-stage", logSelector + ` | logfmt`},
	{"parser:" + syntax.OpParserTypeRegexp, "parser-stage", logSelector + ` | regexp "(?P<lvl>\\w+)"`},
	{"parser:" + syntax.OpParserTypeUnpack, "parser-stage", `{service_name="packer"} | unpack`},
	{"parser:" + syntax.OpParserTypePattern, "parser-stage", `{service_name="proxy"} | pattern "<method> <path>"`},

	// Conversion functions (in an unwrap position).
	{"conv:" + syntax.OpConvBytes, "conv-fn", `sum_over_time(` + logSelector + ` | logfmt | unwrap bytes(size) [5m])`},
	{"conv:" + syntax.OpConvDuration, "conv-fn", `sum_over_time(` + logSelector + ` | logfmt | unwrap duration(took) [5m])`},
	{"conv:" + syntax.OpConvDurationSeconds, "conv-fn", `sum_over_time(` + logSelector + ` | logfmt | unwrap duration_seconds(took) [5m])`},

	// Label / line functions + label mutators.
	{"label:" + syntax.OpFmtLine, "label-fn", logSelector + ` | line_format "{{.status}}"`},
	{"label:" + syntax.OpFmtLabel, "label-fn", logSelector + ` | logfmt | label_format lvl="{{.level}}"`},
	{"label:" + syntax.OpDrop, "label-fn", logSelector + ` | logfmt | drop level`},
	{"label:" + syntax.OpKeep, "label-fn", logSelector + ` | logfmt | keep level`},
	{"label:" + syntax.OpDecolorize, "label-fn", `{service_name="painter"} | decolorize`},

	// Line filters.
	{"linefilter:eq", "line-filter", logSelector + ` |= "error"`},
	{"linefilter:neq", "line-filter", logSelector + ` != "error"`},
	{"linefilter:re", "line-filter", logSelector + ` |~ "err.*"`},
	{"linefilter:nre", "line-filter", logSelector + ` !~ "err.*"`},

	// Label filters.
	{"labelfilter:match", "label-filter", logSelector + ` | logfmt | level = "error"`},
	{"labelfilter:ip", "label-filter", logSelector + ` | logfmt | remote_addr = ip("192.168.0.0/16")`},

	// Binary ops between two metric queries.
	{"op:" + syntax.OpTypeAdd, "binary-op", binOp("+")},
	{"op:" + syntax.OpTypeSub, "binary-op", binOp("-")},
	{"op:" + syntax.OpTypeMul, "binary-op", binOp("*")},
	{"op:" + syntax.OpTypeDiv, "binary-op", binOp("/")},
	{"op:" + syntax.OpTypeMod, "binary-op", binOp("%")},
	{"op:" + syntax.OpTypePow, "binary-op", binOp("^")},
	{"op:" + syntax.OpTypeCmpEQ, "binary-op", binOp("==")},
	{"op:" + syntax.OpTypeNEQ, "binary-op", binOp("!=")},
	{"op:" + syntax.OpTypeGT, "binary-op", binOp(">")},
	{"op:" + syntax.OpTypeGTE, "binary-op", binOp(">=")},
	{"op:" + syntax.OpTypeLT, "binary-op", binOp("<")},
	{"op:" + syntax.OpTypeLTE, "binary-op", binOp("<=")},
	{"op:" + syntax.OpTypeOr, "binary-op", setOp("or")},
	{"op:" + syntax.OpTypeAnd, "binary-op", setOp("and")},
	{"op:" + syntax.OpTypeUnless, "binary-op", setOp("unless")},

	// label_replace function.
	{"label:" + syntax.OpLabelReplace, "label-fn", `label_replace(` + rangeBody() + `, "dst", "$1", "service_name", "(.*)")`},
}

func unwrapRange(fn string) string {
	return fn + `(` + logSelector + ` | logfmt | unwrap ` + logUnwrapField + ` [5m])`
}

func rangeBody() string {
	return `count_over_time(` + logSelector + `[5m])`
}

func vecAgg(fn string) string {
	return fn + `(` + rangeBody() + `)`
}

func binOp(op string) string {
	return rangeBody() + ` ` + op + ` ` + rangeBody()
}

func setOp(op string) string {
	return rangeBody() + ` ` + op + ` ` + rangeBody()
}

// referenceVerdictLogQL models reference Loki acceptance: the wire path
// accepts exactly what syntax.ParseExpr (parse + validate) accepts. We
// run that gate in-process — the LIGHT path, no compat container.
func referenceVerdictLogQL(query string) Verdict {
	if _, err := syntax.ParseExpr(query); err != nil {
		return VerdictReject
	}
	return VerdictAccept
}

// probeLogQL enumerates the LogQL Op* surface and classifies each
// symbol against cerberus + the in-process reference oracle.
func probeLogQL() ([]Entry, error) {
	var entries []Entry
	for _, p := range logProbes {
		cv, cerr := cerberusVerdictLogQL(p.probe)
		ref := referenceVerdictLogQL(p.probe)
		entries = append(entries, Entry{
			Head:          "logql",
			Symbol:        p.symbol,
			Kind:          p.kind,
			Probe:         p.probe,
			Cerberus:      cv,
			Reference:     ref,
			Class:         classify(cv, ref),
			CerberusError: cerr,
		})
	}
	return entries, nil
}
