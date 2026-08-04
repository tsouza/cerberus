package surfaceparity

import (
	"github.com/tsouza/cerberus/internal/logql/lsyntax"
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
	{"range:" + lsyntax.OpRangeTypeCount, "range-agg", `count_over_time(` + logSelector + `[5m])`},
	{"range:" + lsyntax.OpRangeTypeRate, "range-agg", `rate(` + logSelector + `[5m])`},
	{"range:" + lsyntax.OpRangeTypeBytes, "range-agg", `bytes_over_time(` + logSelector + `[5m])`},
	{"range:" + lsyntax.OpRangeTypeBytesRate, "range-agg", `bytes_rate(` + logSelector + `[5m])`},
	{"range:" + lsyntax.OpRangeTypeAbsent, "range-agg", `absent_over_time(` + logSelector + `[5m])`},
	// Range aggregations requiring an unwrap (numeric sample stream).
	{"range:" + lsyntax.OpRangeTypeAvg, "range-agg", unwrapRange("avg_over_time")},
	{"range:" + lsyntax.OpRangeTypeSum, "range-agg", unwrapRange("sum_over_time")},
	{"range:" + lsyntax.OpRangeTypeMin, "range-agg", unwrapRange("min_over_time")},
	{"range:" + lsyntax.OpRangeTypeMax, "range-agg", unwrapRange("max_over_time")},
	{"range:" + lsyntax.OpRangeTypeStdvar, "range-agg", unwrapRange("stdvar_over_time")},
	{"range:" + lsyntax.OpRangeTypeStddev, "range-agg", unwrapRange("stddev_over_time")},
	{"range:" + lsyntax.OpRangeTypeQuantile, "range-agg", `quantile_over_time(0.9, ` + logSelector + ` | logfmt | unwrap ` + logUnwrapField + ` [5m])`},
	{"range:" + lsyntax.OpRangeTypeFirst, "range-agg", unwrapRange("first_over_time")},
	{"range:" + lsyntax.OpRangeTypeLast, "range-agg", unwrapRange("last_over_time")},
	{"range:" + lsyntax.OpRangeTypeRateCounter, "range-agg", `rate_counter(` + logSelector + ` | logfmt | unwrap ` + logUnwrapField + ` [5m])`},

	// Vector aggregations over a range-aggregation result.
	{"vector:" + lsyntax.OpTypeSum, "vector-agg", vecAgg("sum")},
	{"vector:" + lsyntax.OpTypeAvg, "vector-agg", vecAgg("avg")},
	{"vector:" + lsyntax.OpTypeMax, "vector-agg", vecAgg("max")},
	{"vector:" + lsyntax.OpTypeMin, "vector-agg", vecAgg("min")},
	{"vector:" + lsyntax.OpTypeCount, "vector-agg", vecAgg("count")},
	{"vector:" + lsyntax.OpTypeStddev, "vector-agg", vecAgg("stddev")},
	{"vector:" + lsyntax.OpTypeStdvar, "vector-agg", vecAgg("stdvar")},
	{"vector:" + lsyntax.OpTypeBottomK, "vector-agg", `bottomk(3, ` + rangeBody() + `)`},
	{"vector:" + lsyntax.OpTypeTopK, "vector-agg", `topk(3, ` + rangeBody() + `)`},
	{"vector:" + lsyntax.OpTypeSort, "vector-agg", `sort(` + rangeBody() + `)`},
	{"vector:" + lsyntax.OpTypeSortDesc, "vector-agg", `sort_desc(` + rangeBody() + `)`},
	{"vector:" + lsyntax.OpTypeApproxTopK, "vector-agg", `approx_topk(3, ` + rangeBody() + `)`},

	// Parser stages.
	{"parser:" + lsyntax.OpParserTypeJSON, "parser-stage", logShopSelector + ` | json`},
	{"parser:" + lsyntax.OpParserTypeLogfmt, "parser-stage", logSelector + ` | logfmt`},
	{"parser:" + lsyntax.OpParserTypeRegexp, "parser-stage", logSelector + ` | regexp "(?P<lvl>\\w+)"`},
	{"parser:" + lsyntax.OpParserTypeUnpack, "parser-stage", `{service_name="packer"} | unpack`},
	{"parser:" + lsyntax.OpParserTypePattern, "parser-stage", `{service_name="proxy"} | pattern "<method> <path>"`},

	// Conversion functions (in an unwrap position).
	{"conv:" + lsyntax.OpConvBytes, "conv-fn", `sum_over_time(` + logSelector + ` | logfmt | unwrap bytes(size) [5m])`},
	{"conv:" + lsyntax.OpConvDuration, "conv-fn", `sum_over_time(` + logSelector + ` | logfmt | unwrap duration(took) [5m])`},
	{"conv:" + lsyntax.OpConvDurationSeconds, "conv-fn", `sum_over_time(` + logSelector + ` | logfmt | unwrap duration_seconds(took) [5m])`},

	// Label / line functions + label mutators.
	{"label:" + lsyntax.OpFmtLine, "label-fn", logSelector + ` | line_format "{{.status}}"`},
	{"label:" + lsyntax.OpFmtLabel, "label-fn", logSelector + ` | logfmt | label_format lvl="{{.level}}"`},
	{"label:" + lsyntax.OpDrop, "label-fn", logSelector + ` | logfmt | drop level`},
	{"label:" + lsyntax.OpKeep, "label-fn", logSelector + ` | logfmt | keep level`},
	{"label:" + lsyntax.OpDecolorize, "label-fn", `{service_name="painter"} | decolorize`},

	// Line filters.
	{"linefilter:eq", "line-filter", logSelector + ` |= "error"`},
	{"linefilter:neq", "line-filter", logSelector + ` != "error"`},
	{"linefilter:re", "line-filter", logSelector + ` |~ "err.*"`},
	{"linefilter:nre", "line-filter", logSelector + ` !~ "err.*"`},

	// Label filters.
	{"labelfilter:match", "label-filter", logSelector + ` | logfmt | level = "error"`},
	{"labelfilter:ip", "label-filter", logSelector + ` | logfmt | remote_addr = ip("192.168.0.0/16")`},

	// Binary ops between two metric queries.
	{"op:" + lsyntax.OpTypeAdd, "binary-op", binOp("+")},
	{"op:" + lsyntax.OpTypeSub, "binary-op", binOp("-")},
	{"op:" + lsyntax.OpTypeMul, "binary-op", binOp("*")},
	{"op:" + lsyntax.OpTypeDiv, "binary-op", binOp("/")},
	{"op:" + lsyntax.OpTypeMod, "binary-op", binOp("%")},
	{"op:" + lsyntax.OpTypePow, "binary-op", binOp("^")},
	{"op:" + lsyntax.OpTypeCmpEQ, "binary-op", binOp("==")},
	{"op:" + lsyntax.OpTypeNEQ, "binary-op", binOp("!=")},
	{"op:" + lsyntax.OpTypeGT, "binary-op", binOp(">")},
	{"op:" + lsyntax.OpTypeGTE, "binary-op", binOp(">=")},
	{"op:" + lsyntax.OpTypeLT, "binary-op", binOp("<")},
	{"op:" + lsyntax.OpTypeLTE, "binary-op", binOp("<=")},
	{"op:" + lsyntax.OpTypeOr, "binary-op", setOp("or")},
	{"op:" + lsyntax.OpTypeAnd, "binary-op", setOp("and")},
	{"op:" + lsyntax.OpTypeUnless, "binary-op", setOp("unless")},

	// label_replace function.
	{"label:" + lsyntax.OpLabelReplace, "label-fn", `label_replace(` + rangeBody() + `, "dst", "$1", "service_name", "(.*)")`},
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

// probeLogQL enumerates the LogQL Op* surface and classifies each
// symbol against cerberus + the reference verdict read from the pinned
// artifact (logql-reference-verdicts.json).
//
// The reference verdict used to be a live in-process
// syntax.ParseExpr(query) != nil call against upstream Loki's AGPL
// parser (github.com/grafana/loki/v3/pkg/logql/syntax). That worked (no
// compat container needed — parse+validate is data-independent) but
// meant this file — reached unconditionally by
// TestInventoryIsRegenerable and `just update-parity-ledgers`, both part
// of the default `check` gate — imported an AGPL package with no build
// tag. It now reads the frozen verdict the same live call produced,
// exactly mirroring how promql.go reads promql-reference-verdicts.json
// instead of hitting a live reference Prometheus. See
// logql_reference.go + logql_reference_agpl_test.go (the agpl_oracle
// -tagged regeneration/drift-check counterpart) and #1520.
func probeLogQL() ([]Entry, error) {
	ref, err := loadLogQLReferenceVerdicts()
	if err != nil {
		return nil, err
	}

	var entries []Entry
	for _, p := range logProbes {
		cv, cerr := cerberusVerdictLogQL(p.probe)
		rv, rerr := ref.verdict(p.symbol)
		if rerr != nil {
			return nil, rerr
		}
		entries = append(entries, Entry{
			Head:          "logql",
			Symbol:        p.symbol,
			Kind:          p.kind,
			Probe:         p.probe,
			Cerberus:      cv,
			Reference:     rv,
			Class:         classify(cv, rv),
			CerberusError: cerr,
		})
	}
	return entries, nil
}
