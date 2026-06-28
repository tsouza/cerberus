package ast

import "fmt"

// StaticType tags the value carried by a Static. The ordering matches the
// reference parser so that an A/B oracle comparing raw enum values stays
// aligned; cerberus code only ever refers to the named constants.
type StaticType int

const (
	TypeNil StaticType = iota
	TypeSpanset
	TypeAttribute // resolved at query time
	TypeInt
	TypeFloat
	TypeString
	TypeBoolean
	TypeIntArray
	TypeFloatArray
	TypeStringArray
	TypeBooleanArray
	TypeDuration
	TypeStatus
	TypeKind
)

func (t StaticType) isNumeric() bool {
	return t == TypeInt || t == TypeFloat || t == TypeDuration
}

// staticTypeNames maps each StaticType to its constant name; the slice index
// is the iota value, so lookups are a bounds-checked index rather than a
// switch.
var staticTypeNames = [...]string{
	TypeNil:          "TypeNil",
	TypeSpanset:      "TypeSpanset",
	TypeAttribute:    "TypeAttribute",
	TypeInt:          "TypeInt",
	TypeFloat:        "TypeFloat",
	TypeString:       "TypeString",
	TypeBoolean:      "TypeBoolean",
	TypeIntArray:     "TypeIntArray",
	TypeFloatArray:   "TypeFloatArray",
	TypeStringArray:  "TypeStringArray",
	TypeBooleanArray: "TypeBooleanArray",
	TypeDuration:     "TypeDuration",
	TypeStatus:       "TypeStatus",
	TypeKind:         "TypeKind",
}

// String renders the constant name (e.g. "TypeInt"), matching the reference
// parser's spelling so diagnostics and any A/B oracle stay aligned.
func (t StaticType) String() string {
	if int(t) >= 0 && int(t) < len(staticTypeNames) {
		return staticTypeNames[t]
	}
	return fmt.Sprintf("StaticType(%d)", int(t))
}

// Operator enumerates every binary, unary, and spanset-structural operator
// in the language, plus the AST-only operators (exists / in / regex-any)
// that have no concrete surface syntax but are produced during parsing.
type Operator int

const (
	OpNone Operator = iota
	OpAdd
	OpSub
	OpDiv
	OpMod
	OpMult
	OpEqual
	OpNotEqual
	OpRegex
	OpNotRegex
	OpGreater
	OpGreaterEqual
	OpLess
	OpLessEqual
	OpPower
	OpAnd
	OpOr
	OpNot
	OpSpansetChild
	OpSpansetParent
	OpSpansetDescendant
	OpSpansetAncestor
	OpSpansetAnd
	OpSpansetUnion
	OpSpansetSibling
	OpSpansetNotChild
	OpSpansetNotParent
	OpSpansetNotSibling
	OpSpansetNotAncestor
	OpSpansetNotDescendant
	OpSpansetUnionChild
	OpSpansetUnionParent
	OpSpansetUnionSibling
	OpSpansetUnionAncestor
	OpSpansetUnionDescendant
	// AST-only operators below — produced internally, not parseable.
	OpExists
	OpNotExists
	OpIn
	OpNotIn
	OpRegexMatchAny
	OpRegexMatchNone
)

// isBoolean reports whether the operator yields a boolean result
// independent of its operands.
func (op Operator) isBoolean() bool {
	switch op {
	case OpOr, OpAnd, OpEqual, OpNotEqual, OpRegex, OpNotRegex,
		OpGreater, OpGreaterEqual, OpLess, OpLessEqual, OpNot,
		OpExists, OpNotExists, OpIn, OpNotIn,
		OpRegexMatchAny, OpRegexMatchNone:
		return true
	default:
		return false
	}
}

// operatorSymbols renders each Operator to its source token.
var operatorSymbols = map[Operator]string{
	OpAdd:                    "+",
	OpSub:                    "-",
	OpDiv:                    "/",
	OpMod:                    "%",
	OpMult:                   "*",
	OpEqual:                  "=",
	OpNotEqual:               "!=",
	OpRegex:                  "=~",
	OpNotRegex:               "!~",
	OpGreater:                ">",
	OpGreaterEqual:           ">=",
	OpLess:                   "<",
	OpLessEqual:              "<=",
	OpPower:                  "^",
	OpAnd:                    "&&",
	OpOr:                     "||",
	OpNot:                    "!",
	OpSpansetChild:           ">",
	OpSpansetParent:          "<",
	OpSpansetDescendant:      ">>",
	OpSpansetAncestor:        "<<",
	OpSpansetAnd:             "&&",
	OpSpansetSibling:         "~",
	OpSpansetUnion:           "||",
	OpSpansetNotChild:        "!>",
	OpSpansetNotParent:       "!<",
	OpSpansetNotSibling:      "!~",
	OpSpansetNotAncestor:     "!<<",
	OpSpansetNotDescendant:   "!>>",
	OpSpansetUnionChild:      "&>",
	OpSpansetUnionParent:     "&<",
	OpSpansetUnionSibling:    "&~",
	OpSpansetUnionAncestor:   "&<<",
	OpSpansetUnionDescendant: "&>>",
	OpExists:                 "!= nil",
	OpNotExists:              "= nil",
	OpIn:                     "in",
	OpNotIn:                  "not in",
}

func (op Operator) String() string {
	if s, ok := operatorSymbols[op]; ok {
		return s
	}
	return fmt.Sprintf("operator(%d)", op)
}

// AttributeScope is the lexical scope qualifier on an attribute reference
// (resource., span., event., link., …).
type AttributeScope int8

const (
	AttributeScopeNone AttributeScope = iota
	AttributeScopeTrace
	AttributeScopeResource
	AttributeScopeSpan
	AttributeScopeEvent
	AttributeScopeLink
	AttributeScopeInstrumentation
	AttributeScopeUnknown
)

func (s AttributeScope) String() string {
	switch s {
	case AttributeScopeNone:
		return "none"
	case AttributeScopeTrace:
		return "trace"
	case AttributeScopeResource:
		return "resource"
	case AttributeScopeSpan:
		return "span"
	case AttributeScopeEvent:
		return "event"
	case AttributeScopeLink:
		return "link"
	case AttributeScopeInstrumentation:
		return "instrumentation"
	default:
		return fmt.Sprintf("att(%d).", s)
	}
}

// AttributeScopeFromString is the inverse of AttributeScope.String for the
// scopes that have surface syntax.
func AttributeScopeFromString(s string) AttributeScope {
	switch s {
	case "trace":
		return AttributeScopeTrace
	case "span":
		return AttributeScopeSpan
	case "resource":
		return AttributeScopeResource
	case "event":
		return AttributeScopeEvent
	case "link":
		return AttributeScopeLink
	case "instrumentation":
		return AttributeScopeInstrumentation
	case "", "none":
		return AttributeScopeNone
	default:
		return AttributeScopeUnknown
	}
}

// Intrinsic identifies a built-in span/trace property that is addressable
// without a scope prefix (duration, name, status, kind, …) as well as the
// scoped intrinsics (span:duration, trace:rootName, …) and the structural
// markers used internally to request parent/child/sibling resolution.
type Intrinsic int8

const (
	IntrinsicNone Intrinsic = iota
	IntrinsicDuration
	IntrinsicName
	IntrinsicStatus
	IntrinsicStatusMessage
	IntrinsicKind
	IntrinsicChildCount
	IntrinsicTraceRootService
	IntrinsicTraceRootSpan
	IntrinsicTraceDuration
	IntrinsicNestedSetLeft
	IntrinsicNestedSetRight
	IntrinsicNestedSetParent
	IntrinsicEventName
	IntrinsicEventTimeSinceStart
	IntrinsicLinkSpanID
	IntrinsicLinkTraceID
	IntrinsicInstrumentationName
	IntrinsicInstrumentationVersion
	IntrinsicParent
	IntrinsicStructuralDescendant
	IntrinsicStructuralSibling
	IntrinsicStructuralChild
	IntrinsicTraceID
	IntrinsicSpanID
	IntrinsicParentID
	ScopedIntrinsicSpanStatus
	ScopedIntrinsicSpanStatusMessage
	ScopedIntrinsicSpanDuration
	ScopedIntrinsicSpanName
	ScopedIntrinsicSpanKind
	ScopedIntrinsicTraceRootName
	ScopedIntrinsicTraceRootService
	ScopedIntrinsicTraceDuration
	IntrinsicTraceStartTime
	IntrinsicSpanStartTime
	IntrinsicServiceStats
)

// intrinsicNames maps each intrinsic to its TraceQL identifier. Used by
// both String and intrinsicFromString so the two stay in lockstep.
var intrinsicNames = map[Intrinsic]string{
	IntrinsicNone:                    "none",
	IntrinsicDuration:                "duration",
	IntrinsicName:                    "name",
	IntrinsicStatus:                  "status",
	IntrinsicStatusMessage:           "statusMessage",
	IntrinsicKind:                    "kind",
	IntrinsicChildCount:              "span:childCount",
	IntrinsicEventName:               "event:name",
	IntrinsicEventTimeSinceStart:     "event:timeSinceStart",
	IntrinsicLinkSpanID:              "link:spanID",
	IntrinsicLinkTraceID:             "link:traceID",
	IntrinsicParent:                  "parent",
	IntrinsicTraceRootService:        "rootServiceName",
	IntrinsicTraceRootSpan:           "rootName",
	IntrinsicTraceDuration:           "traceDuration",
	IntrinsicTraceID:                 "trace:id",
	IntrinsicTraceStartTime:          "traceStartTime",
	ScopedIntrinsicSpanStatus:        "span:status",
	ScopedIntrinsicSpanStatusMessage: "span:statusMessage",
	ScopedIntrinsicSpanDuration:      "span:duration",
	ScopedIntrinsicSpanName:          "span:name",
	ScopedIntrinsicSpanKind:          "span:kind",
	ScopedIntrinsicTraceRootName:     "trace:rootName",
	ScopedIntrinsicTraceRootService:  "trace:rootService",
	ScopedIntrinsicTraceDuration:     "trace:duration",
	IntrinsicSpanID:                  "span:id",
	IntrinsicParentID:                "span:parentID",
	IntrinsicInstrumentationName:     "instrumentation:name",
	IntrinsicInstrumentationVersion:  "instrumentation:version",
	IntrinsicSpanStartTime:           "spanStartTime",
	IntrinsicNestedSetLeft:           "nestedSetLeft",
	IntrinsicNestedSetRight:          "nestedSetRight",
	IntrinsicNestedSetParent:         "nestedSetParent",
}

func (i Intrinsic) String() string {
	if s, ok := intrinsicNames[i]; ok {
		return s
	}
	return fmt.Sprintf("intrinsic(%d)", i)
}

// intrinsicFromString resolves a bare identifier to its intrinsic, or
// IntrinsicNone if the identifier is an ordinary attribute name.
func intrinsicFromString(s string) Intrinsic {
	for in, name := range intrinsicNames {
		if in == IntrinsicNone || in == IntrinsicParent {
			continue
		}
		if name == s {
			return in
		}
	}
	return IntrinsicNone
}

// Status is the span status enum (the value space of the `status`
// intrinsic and `status` keyword literals).
type Status int

const (
	StatusError Status = iota
	StatusOk
	StatusUnset
)

func (s Status) String() string {
	switch s {
	case StatusError:
		return "error"
	case StatusOk:
		return "ok"
	case StatusUnset:
		return "unset"
	default:
		return fmt.Sprintf("status(%d)", s)
	}
}

// Kind is the span kind enum (the value space of the `kind` intrinsic and
// `kind` keyword literals).
type Kind int

const (
	KindUnspecified Kind = iota
	KindInternal
	KindClient
	KindServer
	KindProducer
	KindConsumer
)

func (k Kind) String() string {
	switch k {
	case KindUnspecified:
		return "unspecified"
	case KindInternal:
		return "internal"
	case KindClient:
		return "client"
	case KindServer:
		return "server"
	case KindProducer:
		return "producer"
	case KindConsumer:
		return "consumer"
	default:
		return fmt.Sprintf("kind(%d)", k)
	}
}

// AggregateOp is the pipeline aggregate function (count / min / max / sum /
// avg) applied by an `| count()`-style stage.
type AggregateOp int

const (
	AggregateCount AggregateOp = iota
	AggregateMax
	AggregateMin
	AggregateSum
	AggregateAvg
)

func (a AggregateOp) String() string {
	switch a {
	case AggregateCount:
		return "count"
	case AggregateMax:
		return "max"
	case AggregateMin:
		return "min"
	case AggregateSum:
		return "sum"
	case AggregateAvg:
		return "avg"
	default:
		return fmt.Sprintf("aggregate(%d)", a)
	}
}

// MetricsAggregateOp is the first-stage metrics function in a metrics
// query (rate / count_over_time / quantile_over_time / …).
type MetricsAggregateOp int

const (
	MetricsAggregateRate MetricsAggregateOp = iota
	MetricsAggregateCountOverTime
	MetricsAggregateMinOverTime
	MetricsAggregateMaxOverTime
	MetricsAggregateAvgOverTime
	MetricsAggregateSumOverTime
	MetricsAggregateQuantileOverTime
	MetricsAggregateHistogramOverTime
)

func (a MetricsAggregateOp) String() string {
	switch a {
	case MetricsAggregateRate:
		return "rate"
	case MetricsAggregateCountOverTime:
		return "count_over_time"
	case MetricsAggregateMinOverTime:
		return "min_over_time"
	case MetricsAggregateMaxOverTime:
		return "max_over_time"
	case MetricsAggregateAvgOverTime:
		return "avg_over_time"
	case MetricsAggregateSumOverTime:
		return "sum_over_time"
	case MetricsAggregateQuantileOverTime:
		return "quantile_over_time"
	case MetricsAggregateHistogramOverTime:
		return "histogram_over_time"
	default:
		return fmt.Sprintf("metricsAggregate(%d)", a)
	}
}

// SecondStageOp is the second-stage metrics selector (topk / bottomk).
type SecondStageOp int

const (
	OpTopK SecondStageOp = iota
	OpBottomK
)

func (op SecondStageOp) String() string {
	switch op {
	case OpTopK:
		return "topk"
	case OpBottomK:
		return "bottomk"
	default:
		return "unknown"
	}
}
