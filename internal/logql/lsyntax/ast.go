package lsyntax

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/logql/logpattern"
)

// Expr is the root LogQL expression interface. Every AST node satisfies
// it. The isExpr marker keeps the interface closed to this package's
// node set so unrelated types can't accidentally satisfy it.
type Expr interface {
	fmt.Stringer
	isExpr()
}

// LogSelectorExpr is an expression that selects and filters log lines
// (a stream selector, optionally followed by a pipeline).
type LogSelectorExpr interface {
	Expr
	Matchers() []*labels.Matcher
	isLogSelectorExpr()
}

// SampleExpr is an expression that produces a numeric sample series
// (range / vector aggregations, binary ops, literals, …).
type SampleExpr interface {
	Expr
	// Selector returns the inner log selector the sample expression
	// reads from, so callers (e.g. /index/stats) can recover its
	// stream matchers.
	Selector() (LogSelectorExpr, error)
	isSampleExpr()
}

// StageExpr is one stage of a log pipeline. The label-projection stages
// (`| drop` / `| keep`) expose their matchers via accessors on the
// concrete types; every other stage builds its SQL shape directly in the
// lowering, so the interface carries only the marker.
type StageExpr interface {
	Expr
	isStageExpr()
}

// MultiStageExpr is an ordered log pipeline.
type MultiStageExpr []StageExpr

// stageBase supplies the StageExpr marker methods.
type stageBase struct{}

func (stageBase) isExpr()      {}
func (stageBase) isStageExpr() {}

// -------------------------------------------------------------------
// Stream selector
// -------------------------------------------------------------------

// MatchersExpr is a bare stream selector: `{job="api", env=~"prod"}`.
type MatchersExpr struct {
	Mts []*labels.Matcher
}

func newMatcherExpr(matchers []*labels.Matcher) *MatchersExpr {
	return &MatchersExpr{Mts: matchers}
}

func (e *MatchersExpr) isExpr()                     {}
func (e *MatchersExpr) isLogSelectorExpr()          {}
func (e *MatchersExpr) Matchers() []*labels.Matcher { return e.Mts }

// PipelineExpr is a stream selector followed by pipeline stages.
type PipelineExpr struct {
	Left        *MatchersExpr
	MultiStages MultiStageExpr
}

func newPipelineExpr(left *MatchersExpr, pipeline MultiStageExpr) *PipelineExpr {
	return &PipelineExpr{Left: left, MultiStages: pipeline}
}

func (e *PipelineExpr) isExpr()                     {}
func (e *PipelineExpr) isLogSelectorExpr()          {}
func (e *PipelineExpr) Matchers() []*labels.Matcher { return e.Left.Matchers() }

// -------------------------------------------------------------------
// Line filters (`|=`, `!=`, `|~`, `!~`, `|>`, `!>`)
// -------------------------------------------------------------------

// LineFilter is a single line-filter clause.
type LineFilter struct {
	Ty    LineMatchType
	Match string
	Op    string
}

// LineFilterExpr is a (possibly chained / or-joined) line filter stage.
// Left holds earlier filters in the same pipeline; Or holds alternates
// joined by `or`.
type LineFilterExpr struct {
	stageBase
	LineFilter
	Left      *LineFilterExpr
	Or        *LineFilterExpr
	IsOrChild bool
}

// lineFilterClause is one line-filter operator with its operands: the
// first operand and then every `or` alternate, in source order.
type lineFilterClause struct {
	ty       LineMatchType
	operands []LineFilter
}

// excludes reports whether the clause's operator drops the lines its
// operands match (`!=`, `!~`, `!>`) rather than keeping them.
func (c lineFilterClause) excludes() bool {
	switch c.ty {
	case LineMatchNotEqual, LineMatchNotRegexp, LineMatchNotPattern:
		return true
	default:
		return false
	}
}

// attach appends the clause to the filters parsed before it in the same
// stage (prev, nil for the first clause) and returns the new last node.
//
// An excluding clause drops a line that matches ANY operand, which is the
// conjunction of one excluding filter per operand: each operand becomes
// its own node on the Left chain. A keeping clause is a single node whose
// alternates hang off Or; see keptAlternates for which of them the
// predicate carries.
func (c lineFilterClause) attach(prev *LineFilterExpr) *LineFilterExpr {
	if c.excludes() {
		for _, operand := range c.operands {
			prev = &LineFilterExpr{LineFilter: operand, Left: prev}
		}
		return prev
	}
	node := &LineFilterExpr{LineFilter: c.operands[0], Left: prev}
	link := &node.Or
	for _, alt := range c.keptAlternates() {
		*link = &LineFilterExpr{LineFilter: alt, IsOrChild: true}
		link = &(*link).Or
	}
	return node
}

// keptAlternates returns the `or` alternates a keeping clause matches on.
// The reference evaluates only the alternates after the last `ip(...)`
// alternate that another alternate follows; that alternate and the ones
// before it are not part of its predicate, so they are not part of
// cerberus's either.
func (c lineFilterClause) keptAlternates() []LineFilter {
	alts := c.operands[1:]
	from := 0
	for i := range len(alts) - 1 {
		if alts[i].Op == OpFilterIP {
			from = i + 1
		}
	}
	return alts[from:]
}

// -------------------------------------------------------------------
// Parser stages
// -------------------------------------------------------------------

// LogfmtParserExpr is a bare `| logfmt` (optionally flagged) parser.
type LogfmtParserExpr struct {
	stageBase
	Strict    bool
	KeepEmpty bool
}

func newLogfmtParserExpr(flags []string) *LogfmtParserExpr {
	strict, keepEmpty := logfmtFlags(flags)
	return &LogfmtParserExpr{Strict: strict, KeepEmpty: keepEmpty}
}

// logfmtFlags reports whether `--strict` and `--keep-empty` are among a
// logfmt stage's flags. Any other flag is ignored.
func logfmtFlags(flags []string) (strict, keepEmpty bool) {
	return slices.Contains(flags, OpStrict), slices.Contains(flags, OpKeepEmpty)
}

// LineParserExpr is a `| json` / `| regexp "..."` / `| unpack` /
// `| pattern "..."` parser stage.
type LineParserExpr struct {
	stageBase
	Op    string
	Param string
}

// errMissingCapture mirrors upstream's parse-time rejection of a
// `| regexp` stage that carries no named captures.
var errMissingCapture = errors.New("at least one named capture must be supplied")

// validateRegexpParser reimplements the parse-time validation upstream's
// log.NewRegexpParser performs: the pattern must compile, carry at least
// one named capture, and have no duplicate capture names. (Upstream also
// checks each name is a valid label name; Go's regexp grammar already
// constrains named groups to `[A-Za-z_][A-Za-z0-9_]*`, which is always a
// valid label name, so that check is a no-op here.)
func validateRegexpParser(re string) error {
	regex, err := regexp.Compile(re)
	if err != nil {
		return err
	}
	if regex.NumSubexp() == 0 {
		return errMissingCapture
	}
	seen := map[string]struct{}{}
	named := 0
	for _, n := range regex.SubexpNames() {
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			return fmt.Errorf("duplicate extracted label name '%s'", n)
		}
		seen[n] = struct{}{}
		// `named--` here is indistinguishable from `named++`: the only
		// read is the `named == 0` post-condition below, and +k and -k
		// are zero for the same k. See ast_test.go's NOT KILLABLE footer.
		named++
	}
	if named == 0 {
		return errMissingCapture
	}
	return nil
}

func newLabelParserExpr(op, param string) *LineParserExpr {
	// Validate the regexp / pattern argument at parse time, matching the
	// upstream parser's eager validation. A bad pattern panics with a
	// ParseError that ParseExprWithoutValidation recovers into an error.
	switch op {
	case OpParserTypeRegexp:
		if err := validateRegexpParser(param); err != nil {
			panic(NewParseError(fmt.Sprintf("invalid regexp parser: %s", err.Error()), 0, 0))
		}
	case OpParserTypePattern:
		if _, err := logpattern.New(param); err != nil {
			panic(NewParseError(fmt.Sprintf("invalid pattern parser: %s", err.Error()), 0, 0))
		}
	}
	return &LineParserExpr{Op: op, Param: param}
}

// JSONExpressionParserExpr is a typed `| json a="x.y", b="z"` parser.
type JSONExpressionParserExpr struct {
	stageBase
	Expressions []LabelExtractionExpr
}

func newJSONExpressionParser(expressions []LabelExtractionExpr) *JSONExpressionParserExpr {
	return &JSONExpressionParserExpr{Expressions: expressions}
}

// LogfmtExpressionParserExpr is a typed `| logfmt a="x", b="y"` parser.
type LogfmtExpressionParserExpr struct {
	stageBase
	Expressions       []LabelExtractionExpr
	Strict, KeepEmpty bool
}

func newLogfmtExpressionParser(expressions []LabelExtractionExpr, flags []string) *LogfmtExpressionParserExpr {
	strict, keepEmpty := logfmtFlags(flags)
	return &LogfmtExpressionParserExpr{Expressions: expressions, Strict: strict, KeepEmpty: keepEmpty}
}

// -------------------------------------------------------------------
// Label filter / format / fmt / decolorize / drop / keep
// -------------------------------------------------------------------

// LabelFilterExpr is a `| label op value` filter stage. It embeds the
// runtime filterer so cerberus reads it as `expr.LabelFilterer`.
type LabelFilterExpr struct {
	stageBase
	LabelFilterer
}

func newLabelFilterExpr(filterer LabelFilterer) *LabelFilterExpr {
	return &LabelFilterExpr{LabelFilterer: filterer}
}

// LineFmtExpr is a `| line_format "..."` stage.
type LineFmtExpr struct {
	stageBase
	Value string
}

func newLineFmtExpr(value string) *LineFmtExpr { return &LineFmtExpr{Value: value} }

// DecolorizeExpr is a `| decolorize` stage.
type DecolorizeExpr struct {
	stageBase
}

func newDecolorizeExpr() *DecolorizeExpr { return &DecolorizeExpr{} }

// LabelFmtExpr is a `| label_format new=old, x="{{.y}}"` stage.
type LabelFmtExpr struct {
	stageBase
	Formats []LabelFmt
}

func newLabelFmtExpr(fmts []LabelFmt) *LabelFmtExpr { return &LabelFmtExpr{Formats: fmts} }

// DropLabelsExpr is a `| drop a, b="v"` stage.
type DropLabelsExpr struct {
	stageBase
	dropLabels []NamedLabelMatcher
}

func newDropLabelsExpr(dropLabels []NamedLabelMatcher) *DropLabelsExpr {
	return &DropLabelsExpr{dropLabels: dropLabels}
}

// Matchers returns the drop entries (bare names + value matchers) so the
// Go-side post-processing can apply the projection over a row's label map.
func (e *DropLabelsExpr) Matchers() []NamedLabelMatcher { return e.dropLabels }

// HasNamedMatchers reports whether any drop entry is a value matcher
// (`| drop env="prod"`) rather than a bare label name.
func (e *DropLabelsExpr) HasNamedMatchers() bool {
	for _, d := range e.dropLabels {
		if d.Matcher != nil {
			return true
		}
	}
	return false
}

// Names returns the label names targeted by the drop stage.
func (e *DropLabelsExpr) Names() []string {
	names := []string{}
	for _, d := range e.dropLabels {
		if d.Name != "" {
			names = append(names, d.Name)
		} else if d.Matcher != nil {
			names = append(names, d.Matcher.Name)
		}
	}
	return names
}

// KeepLabelsExpr is a `| keep a, b="v"` stage.
type KeepLabelsExpr struct {
	stageBase
	keepLabels []NamedLabelMatcher
}

func newKeepLabelsExpr(keepLabels []NamedLabelMatcher) *KeepLabelsExpr {
	return &KeepLabelsExpr{keepLabels: keepLabels}
}

// Matchers returns the keep entries so the Go-side post-processing can
// apply the projection over a row's label map.
func (e *KeepLabelsExpr) Matchers() []NamedLabelMatcher { return e.keepLabels }

// -------------------------------------------------------------------
// Unwrap / log range / offset
// -------------------------------------------------------------------

// UnwrapExpr is a `| unwrap field` (optionally `| unwrap bytes(field)`)
// clause with any trailing post-filters.
type UnwrapExpr struct {
	Identifier  string
	Operation   string
	PostFilters []LabelFilterer
}

func newUnwrapExpr(id, operation string) *UnwrapExpr {
	return &UnwrapExpr{Identifier: id, Operation: operation}
}

func (u *UnwrapExpr) addPostFilter(f LabelFilterer) *UnwrapExpr {
	u.PostFilters = append(u.PostFilters, f)
	return u
}

// LogRangeExpr is a ranged log selector: `{...} | pipeline [5m] offset 1h`.
type LogRangeExpr struct {
	Left     LogSelectorExpr
	Interval time.Duration
	Offset   time.Duration
	Unwrap   *UnwrapExpr
}

func newLogRange(left LogSelectorExpr, interval time.Duration, u *UnwrapExpr, o *OffsetExpr) *LogRangeExpr {
	r := &LogRangeExpr{Left: left, Interval: interval, Unwrap: u}
	if o != nil {
		r.Offset = o.Offset
	}
	return r
}

// OffsetExpr carries an `offset <duration>` modifier.
type OffsetExpr struct {
	Offset time.Duration
}

func newOffsetExpr(offset time.Duration) *OffsetExpr { return &OffsetExpr{Offset: offset} }

// -------------------------------------------------------------------
// Grouping
// -------------------------------------------------------------------

// Grouping is a `by (...)` / `without (...)` clause.
type Grouping struct {
	Groups  []string
	Without bool
}

// -------------------------------------------------------------------
// Range aggregation
// -------------------------------------------------------------------

// RangeAggregationExpr is `rate(...)`, `count_over_time(...)`, etc.
type RangeAggregationExpr struct {
	Left      *LogRangeExpr
	Operation string
	Params    *float64
	Grouping  *Grouping
	err       error
}

func newRangeAggregationExpr(left *LogRangeExpr, operation string, gr *Grouping, stringParams *string) *RangeAggregationExpr {
	var params *float64
	if stringParams != nil {
		if operation != OpRangeTypeQuantile {
			return &RangeAggregationExpr{
				err: NewParseError(fmt.Sprintf("parameter %s not supported for operation %s", *stringParams, operation), 0, 0),
			}
		}
		p, err := strconv.ParseFloat(*stringParams, 64)
		if err != nil {
			return &RangeAggregationExpr{
				err: NewParseError(fmt.Sprintf("invalid parameter for operation %s: %s", operation, *stringParams), 0, 0),
			}
		}
		params = &p
	} else if operation == OpRangeTypeQuantile {
		return &RangeAggregationExpr{
			err: NewParseError(fmt.Sprintf("parameter required for operation %s", operation), 0, 0),
		}
	}
	e := &RangeAggregationExpr{
		Left:      left,
		Operation: operation,
		Grouping:  gr,
		Params:    params,
	}
	if err := e.validate(); err != nil {
		e.err = err
	}
	return e
}

// validate enforces the grouping and unwrap requirements for the range
// operation, matching the upstream LogQL parser. Operations that read a
// numeric value (avg/sum/min/max/stddev/stdvar/quantile/first/last over
// time) require a `| unwrap`; the line-counting operations (count/rate/
// bytes/bytes_rate/absent) reject one.
func (e *RangeAggregationExpr) validate() error {
	if e.Grouping != nil {
		switch e.Operation {
		case OpRangeTypeAvg, OpRangeTypeStddev, OpRangeTypeStdvar, OpRangeTypeQuantile,
			OpRangeTypeMax, OpRangeTypeMin, OpRangeTypeFirst, OpRangeTypeLast:
		default:
			return NewParseError(fmt.Sprintf("grouping not allowed for %s aggregation", e.Operation), 0, 0)
		}
	}
	if e.Left != nil && e.Left.Unwrap != nil {
		switch e.Operation {
		case OpRangeTypeAvg, OpRangeTypeSum, OpRangeTypeMax, OpRangeTypeMin, OpRangeTypeStddev,
			OpRangeTypeStdvar, OpRangeTypeQuantile, OpRangeTypeRate, OpRangeTypeRateCounter,
			OpRangeTypeAbsent, OpRangeTypeFirst, OpRangeTypeLast:
			return nil
		default:
			return NewParseError(fmt.Sprintf("invalid aggregation %s with unwrap", e.Operation), 0, 0)
		}
	}
	switch e.Operation {
	case OpRangeTypeBytes, OpRangeTypeBytesRate, OpRangeTypeCount, OpRangeTypeRate, OpRangeTypeAbsent:
		return nil
	default:
		return NewParseError(fmt.Sprintf("invalid aggregation %s without unwrap", e.Operation), 0, 0)
	}
}

func (e *RangeAggregationExpr) isExpr()       {}
func (e *RangeAggregationExpr) isSampleExpr() {}
func (e *RangeAggregationExpr) Selector() (LogSelectorExpr, error) {
	return selectorBeneath(e.err, e.Left)
}

// selectorSource is a node that resolves the log selector a sample
// expression reads.
type selectorSource interface {
	Selector() (LogSelectorExpr, error)
}

// Selector returns the ranged selector.
func (r *LogRangeExpr) Selector() (LogSelectorExpr, error) { return r.Left, nil }

// selectorBeneath resolves the selector under a node whose construction may
// have failed. The error stashed at construction wins, so a failed node
// never reaches its operand, which may be unset.
func selectorBeneath(stashed error, operand selectorSource) (LogSelectorExpr, error) {
	if stashed != nil {
		return nil, stashed
	}
	return operand.Selector()
}

// -------------------------------------------------------------------
// Vector aggregation
// -------------------------------------------------------------------

// VectorAggregationExpr is `sum(...)`, `topk(5, ...)`, etc.
type VectorAggregationExpr struct {
	Left      SampleExpr
	Grouping  *Grouping
	Params    int
	Operation string
	err       error
}

func mustNewVectorAggregationExpr(left SampleExpr, operation string, gr *Grouping, params *string) *VectorAggregationExpr {
	var p int
	switch operation {
	case OpTypeBottomK, OpTypeTopK, OpTypeApproxTopK:
		if params == nil {
			return &VectorAggregationExpr{err: NewParseError(fmt.Sprintf("parameter required for operation %s", operation), 0, 0)}
		}
		var err error
		p, err = strconv.Atoi(*params)
		if err != nil {
			return &VectorAggregationExpr{err: NewParseError(fmt.Sprintf("invalid parameter %s(%s,", operation, *params), 0, 0)}
		}
		if p <= 0 {
			return &VectorAggregationExpr{err: NewParseError(fmt.Sprintf("invalid parameter (must be greater than 0) %s(%s", operation, *params), 0, 0)}
		}
		if operation == OpTypeApproxTopK && gr != nil {
			return &VectorAggregationExpr{err: NewParseError(fmt.Sprintf("grouping not allowed for %s aggregation", operation), 0, 0)}
		}
	default:
		if params != nil {
			return &VectorAggregationExpr{err: NewParseError(fmt.Sprintf("unsupported parameter for operation %s(%s,", operation, *params), 0, 0)}
		}
	}
	if gr == nil {
		gr = &Grouping{}
	}
	return &VectorAggregationExpr{Left: left, Operation: operation, Grouping: gr, Params: p}
}

func (e *VectorAggregationExpr) isExpr()       {}
func (e *VectorAggregationExpr) isSampleExpr() {}
func (e *VectorAggregationExpr) Selector() (LogSelectorExpr, error) {
	return selectorBeneath(e.err, e.Left)
}

// -------------------------------------------------------------------
// Binary op + vector matching
// -------------------------------------------------------------------

// VectorMatchCardinality describes the cardinality of a binary vector match.
type VectorMatchCardinality int

const (
	CardOneToOne VectorMatchCardinality = iota
	CardManyToOne
	CardOneToMany
)

func (vmc VectorMatchCardinality) String() string {
	switch vmc {
	case CardOneToOne:
		return "one-to-one"
	case CardManyToOne:
		return "many-to-one"
	case CardOneToMany:
		return "one-to-many"
	}
	return "unknown"
}

// VectorMatching captures the on/ignoring + group_left/right modifiers.
type VectorMatching struct {
	Card           VectorMatchCardinality
	MatchingLabels []string
	On             bool
	Include        []string
}

// BinOpOptions carries the binary-op modifiers.
type BinOpOptions struct {
	ReturnBool     bool
	VectorMatching *VectorMatching
}

// BinOpExpr is `a + b`, `a > bool c`, `a and on(x) b`, etc.
type BinOpExpr struct {
	SampleExpr
	RHS  SampleExpr
	Op   string
	Opts *BinOpOptions
	err  error
}

func mustNewBinOpExpr(op string, opts *BinOpOptions, lhs, rhs Expr) SampleExpr {
	left, ok := lhs.(SampleExpr)
	if !ok {
		return &BinOpExpr{err: NewParseError(fmt.Sprintf("unexpected type for left leg of binary operation (%s): %T", op, lhs), 0, 0)}
	}
	right, ok := rhs.(SampleExpr)
	if !ok {
		return &BinOpExpr{err: NewParseError(fmt.Sprintf("unexpected type for right leg of binary operation (%s): %T", op, rhs), 0, 0)}
	}

	leftLit, lOk := left.(*LiteralExpr)
	rightLit, rOk := right.(*LiteralExpr)
	var leftVal, rightVal float64
	if lOk {
		v, err := leftLit.Value()
		if err != nil {
			return &BinOpExpr{err: err}
		}
		leftVal = v
	}
	if rOk {
		v, err := rightLit.Value()
		if err != nil {
			return &BinOpExpr{err: err}
		}
		rightVal = v
	}

	// Logical / set operators reject literal legs.
	if IsLogicalBinOp(op) {
		if lOk {
			return &BinOpExpr{err: NewParseError(fmt.Sprintf("unexpected literal for left leg of logical/set binary operation (%s): %f", op, leftVal), 0, 0)}
		}
		if rOk {
			return &BinOpExpr{err: NewParseError(fmt.Sprintf("unexpected literal for right leg of logical/set binary operation (%s): %f", op, rightVal), 0, 0)}
		}
	}

	// Fold `(1 + 1)` → `2`: a binop with two literal legs reduces to a
	// single literal so the invariant "no binop has two literal legs"
	// holds for the lowering.
	if lOk && rOk {
		return reduceBinOp(op, leftVal, rightVal)
	}

	return &BinOpExpr{SampleExpr: left, RHS: right, Op: op, Opts: opts}
}

func (e *BinOpExpr) isExpr()       {}
func (e *BinOpExpr) isSampleExpr() {}
func (e *BinOpExpr) Selector() (LogSelectorExpr, error) {
	if e.err != nil {
		return nil, e.err
	}
	return e.SampleExpr.Selector()
}

// -------------------------------------------------------------------
// Literal / label_replace / vector
// -------------------------------------------------------------------

// LiteralExpr is a bare numeric literal used as a metric-query leg.
type LiteralExpr struct {
	Val float64
	err error
}

func mustNewLiteralExpr(s string, invert bool) *LiteralExpr {
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		err = NewParseError(fmt.Sprintf("unable to parse literal as a float: %s", err.Error()), 0, 0)
	}
	if invert {
		n = -n
	}
	return &LiteralExpr{Val: n, err: err}
}

func (e *LiteralExpr) isExpr()                            {}
func (e *LiteralExpr) isSampleExpr()                      {}
func (e *LiteralExpr) isLogSelectorExpr()                 {}
func (e *LiteralExpr) Matchers() []*labels.Matcher        { return nil }
func (e *LiteralExpr) Selector() (LogSelectorExpr, error) { return e, e.err }
func (e *LiteralExpr) Value() (float64, error)            { return scalarValue(e.Val, e.err) }

// scalarValue is the value of a scalar literal, or zero and the parse
// error when its text did not parse as a float.
func scalarValue(parsed float64, stashed error) (float64, error) {
	if stashed != nil {
		return 0, stashed
	}
	return parsed, nil
}

// LabelReplaceExpr is `label_replace(v, dst, repl, src, regex)`.
type LabelReplaceExpr struct {
	Left        SampleExpr
	Dst         string
	Replacement string
	Src         string
	Regex       string
	Re          *regexp.Regexp
	err         error
}

// mustNewLabelReplaceExpr builds label_replace. Its regex matches the
// whole source value, so it is anchored at both ends; a regex that does
// not compile leaves a node carrying only the parse error.
func mustNewLabelReplaceExpr(left SampleExpr, dst, replacement, src, regex string) *LabelReplaceExpr {
	e := &LabelReplaceExpr{Left: left, Dst: dst, Replacement: replacement, Src: src, Regex: regex}
	anchored, err := regexp.Compile("^(?:" + regex + ")$")
	if err == nil {
		e.Re = anchored
		return e
	}
	return &LabelReplaceExpr{err: NewParseError("invalid regex in label_replace: "+err.Error(), 0, 0)}
}

func (e *LabelReplaceExpr) isExpr()       {}
func (e *LabelReplaceExpr) isSampleExpr() {}
func (e *LabelReplaceExpr) Selector() (LogSelectorExpr, error) {
	return selectorBeneath(e.err, e.Left)
}

// VectorExpr is `vector(<scalar>)`.
type VectorExpr struct {
	Val float64
	err error
}

// NewVectorExpr builds a VectorExpr from its scalar argument.
func NewVectorExpr(scalar string) *VectorExpr {
	n, err := strconv.ParseFloat(scalar, 64)
	if err != nil {
		err = NewParseError(fmt.Sprintf("unable to parse vectorExpr as a float: %s", err.Error()), 0, 0)
	}
	return &VectorExpr{Val: n, err: err}
}

func (e *VectorExpr) isExpr()                            {}
func (e *VectorExpr) isSampleExpr()                      {}
func (e *VectorExpr) isLogSelectorExpr()                 {}
func (e *VectorExpr) Err() error                         { return e.err }
func (e *VectorExpr) Matchers() []*labels.Matcher        { return nil }
func (e *VectorExpr) Selector() (LogSelectorExpr, error) { return e, e.err }
func (e *VectorExpr) Value() (float64, error)            { return scalarValue(e.Val, e.err) }

// -------------------------------------------------------------------
// Multi-variant
// -------------------------------------------------------------------

// MultiVariantExpr is `variants(m0, m1, …) of ({selector}[range])`.
type MultiVariantExpr struct {
	logRange *LogRangeExpr
	variants []SampleExpr
	err      error
}

func newVariantsExpr(variants []SampleExpr, logRange *LogRangeExpr) *MultiVariantExpr {
	return &MultiVariantExpr{variants: variants, logRange: logRange}
}

func (m *MultiVariantExpr) isExpr()                 {}
func (m *MultiVariantExpr) isSampleExpr()           {}
func (m *MultiVariantExpr) LogRange() *LogRangeExpr { return m.logRange }
func (m *MultiVariantExpr) Variants() []SampleExpr  { return m.variants }
func (m *MultiVariantExpr) Matchers() []*labels.Matcher {
	if m.logRange == nil || m.logRange.Left == nil {
		return nil
	}
	return m.logRange.Left.Matchers()
}

func (m *MultiVariantExpr) Selector() (LogSelectorExpr, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.logRange == nil {
		return nil, NewParseError("variants query has no log range", 0, 0)
	}
	return m.logRange.Left, nil
}

// mustNewMatcher builds a label matcher, panicking with a ParseError on
// an invalid regular expression (recovered by ParseExprWithoutValidation).
func mustNewMatcher(t labels.MatchType, n, v string) *labels.Matcher {
	m, err := labels.NewMatcher(t, n, v)
	if err != nil {
		panic(NewParseError(err.Error(), 0, 0))
	}
	return m
}
