// Package lsyntax is cerberus's clean-room, Apache-licensed LogQL parser.
//
// It replaces the AGPLv3 grafana/loki `pkg/logql/syntax` parser. Only
// the Go *source* of that parser is AGPL; the LogQL grammar/language it
// implements is not copyrightable, so this package reimplements the
// language from the published grammar (the goyacc productions) and from
// its own hand-written lexer + recursive-descent / Pratt parser.
//
// The native AST node types (MatchersExpr, PipelineExpr, the label /
// line filter family, the metric-form expressions, …) intentionally
// mirror the exported field names and shapes of the upstream AST so the
// existing cerberus lowering (internal/logql/lower.go and friends)
// consumes them via a single import-alias substitution.
//
// Leaf values that belong to the *runtime evaluation* surface — the
// label filterers, label formatters, label-extraction expressions and
// named-label matchers — are still constructed from the upstream
// grafana/loki `pkg/logql/log` package. That package is the evaluation
// runtime, not the parser, and stays imported until a later change
// reimplements it too. This package only removes the AGPL *parser*.
package lsyntax

// Operation identifier strings. These mirror the LogQL operator
// vocabulary; they are the stable string values cerberus's lowering
// switches on (e.g. RangeAggregationExpr.Operation == OpRangeTypeRate).
const (
	// Range vector operations.
	OpRangeTypeCount       = "count_over_time"
	OpRangeTypeRate        = "rate"
	OpRangeTypeRateCounter = "rate_counter"
	OpRangeTypeBytes       = "bytes_over_time"
	OpRangeTypeBytesRate   = "bytes_rate"
	OpRangeTypeAvg         = "avg_over_time"
	OpRangeTypeSum         = "sum_over_time"
	OpRangeTypeMin         = "min_over_time"
	OpRangeTypeMax         = "max_over_time"
	OpRangeTypeStdvar      = "stdvar_over_time"
	OpRangeTypeStddev      = "stddev_over_time"
	OpRangeTypeQuantile    = "quantile_over_time"
	OpRangeTypeFirst       = "first_over_time"
	OpRangeTypeLast        = "last_over_time"
	OpRangeTypeAbsent      = "absent_over_time"

	// Vector aggregation operations.
	OpTypeSum        = "sum"
	OpTypeAvg        = "avg"
	OpTypeMax        = "max"
	OpTypeMin        = "min"
	OpTypeCount      = "count"
	OpTypeStddev     = "stddev"
	OpTypeStdvar     = "stdvar"
	OpTypeBottomK    = "bottomk"
	OpTypeTopK       = "topk"
	OpTypeSort       = "sort"
	OpTypeSortDesc   = "sort_desc"
	OpTypeApproxTopK = "approx_topk"

	OpTypeVector = "vector"

	// Logical / set binary operators.
	OpTypeOr     = "or"
	OpTypeAnd    = "and"
	OpTypeUnless = "unless"

	// Arithmetic binary operators.
	OpTypeAdd = "+"
	OpTypeSub = "-"
	OpTypeMul = "*"
	OpTypeDiv = "/"
	OpTypeMod = "%"
	OpTypePow = "^"

	// Comparison binary operators.
	OpTypeCmpEQ = "=="
	OpTypeNEQ   = "!="
	OpTypeGT    = ">"
	OpTypeGTE   = ">="
	OpTypeLT    = "<"
	OpTypeLTE   = "<="

	// Parser stages.
	OpParserTypeJSON    = "json"
	OpParserTypeLogfmt  = "logfmt"
	OpParserTypeRegexp  = "regexp"
	OpParserTypeUnpack  = "unpack"
	OpParserTypePattern = "pattern"

	// Format / misc stages.
	OpFmtLine    = "line_format"
	OpFmtLabel   = "label_format"
	OpDecolorize = "decolorize"

	OpPipe   = "|"
	OpUnwrap = "unwrap"
	OpOffset = "offset"

	OpOn       = "on"
	OpIgnoring = "ignoring"

	OpGroupLeft  = "group_left"
	OpGroupRight = "group_right"

	// Unwrap conversion operations.
	OpConvBytes           = "bytes"
	OpConvDuration        = "duration"
	OpConvDurationSeconds = "duration_seconds"

	OpLabelReplace = "label_replace"

	OpFilterIP = "ip"

	OpDrop = "drop"
	OpKeep = "keep"

	// Parser flags.
	OpStrict    = "--strict"
	OpKeepEmpty = "--keep-empty"

	// Variants.
	OpVariants = "variants"
	VariantsOf = "of"
)
