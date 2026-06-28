package lsyntax

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/dustin/go-humanize"
	"github.com/prometheus/prometheus/model/labels"
)

// This file holds the in-house leaf value types for the LogQL AST — the
// label-filter family, parser extraction expressions, label_format
// specs, and the line-match / label-filter operator enums. They were
// previously the `grafana/loki/v3/pkg/logql/log` types the upstream
// parser constructed; cerberus reimplements them here (Apache-2.0) so
// the binary does not link the AGPL `pkg/logql/log` package. The String
// methods are byte-for-byte faithful to upstream's so the
// `/loki/api/v1/format_query` round-trip is unchanged.

// LineMatchType enumerates the line-filter operators (`|=`, `!=`, `|~`,
// `!~`, `|>`, `!>`).
type LineMatchType int

// Line-filter operators, in upstream's iota order.
const (
	LineMatchEqual LineMatchType = iota
	LineMatchNotEqual
	LineMatchRegexp
	LineMatchNotRegexp
	LineMatchPattern
	LineMatchNotPattern
)

func (t LineMatchType) String() string {
	switch t {
	case LineMatchEqual:
		return "|="
	case LineMatchNotEqual:
		return "!="
	case LineMatchRegexp:
		return "|~"
	case LineMatchNotRegexp:
		return "!~"
	case LineMatchPattern:
		return "|>"
	case LineMatchNotPattern:
		return "!>"
	default:
		return ""
	}
}

// LabelFilterType enumerates the comparison operators a label filter can
// carry (`==`, `!=`, `>`, `>=`, `<`, `<=`).
type LabelFilterType int

// Label-filter comparison operators, in upstream's iota order.
const (
	LabelFilterEqual LabelFilterType = iota
	LabelFilterNotEqual
	LabelFilterGreaterThan
	LabelFilterGreaterThanOrEqual
	LabelFilterLesserThan
	LabelFilterLesserThanOrEqual
)

func (f LabelFilterType) String() string {
	switch f {
	case LabelFilterEqual:
		return "=="
	case LabelFilterNotEqual:
		return "!="
	case LabelFilterGreaterThan:
		return ">"
	case LabelFilterGreaterThanOrEqual:
		return ">="
	case LabelFilterLesserThan:
		return "<"
	case LabelFilterLesserThanOrEqual:
		return "<="
	default:
		return ""
	}
}

// LabelFilterer is a parsed `| label op value` filter. Cerberus only
// inspects the concrete types during lowering (it never executes them
// Go-side), so the interface is a sealed marker plus a Stringer for the
// format_query round-trip.
type LabelFilterer interface {
	fmt.Stringer
	isLabelFilterer()
}

// BinaryLabelFilter joins two filterers with `and` (And==true) or `or`.
type BinaryLabelFilter struct {
	Left  LabelFilterer
	Right LabelFilterer
	And   bool
}

// NewAndLabelFilter joins two filterers with `and`.
func NewAndLabelFilter(left, right LabelFilterer) *BinaryLabelFilter {
	return &BinaryLabelFilter{Left: left, Right: right, And: true}
}

// NewOrLabelFilter joins two filterers with `or`.
func NewOrLabelFilter(left, right LabelFilterer) *BinaryLabelFilter {
	return &BinaryLabelFilter{Left: left, Right: right}
}

func (b *BinaryLabelFilter) isLabelFilterer() {}

func (b *BinaryLabelFilter) String() string {
	var sb strings.Builder
	sb.WriteString("( ")
	sb.WriteString(b.Left.String())
	if b.And {
		sb.WriteString(" , ")
	} else {
		sb.WriteString(" or ")
	}
	sb.WriteString(b.Right.String())
	sb.WriteString(" )")
	return sb.String()
}

// StringLabelFilter matches a label value against a string matcher
// (`=`, `!=`, `=~`, `!~`).
type StringLabelFilter struct {
	*labels.Matcher
}

// NewStringLabelFilter builds a string label filter from a matcher.
func NewStringLabelFilter(m *labels.Matcher) *StringLabelFilter {
	return &StringLabelFilter{Matcher: m}
}

func (s *StringLabelFilter) isLabelFilterer() {}

// String is promoted from the embedded *labels.Matcher.

// NumericLabelFilter compares a label value parsed as a float64.
type NumericLabelFilter struct {
	Name  string
	Value float64
	Type  LabelFilterType
}

// NewNumericLabelFilter builds a numeric label filter.
func NewNumericLabelFilter(t LabelFilterType, name string, value float64) *NumericLabelFilter {
	return &NumericLabelFilter{Name: name, Value: value, Type: t}
}

func (n *NumericLabelFilter) isLabelFilterer() {}

func (n *NumericLabelFilter) String() string {
	return fmt.Sprintf("%s%s%s", n.Name, n.Type, strconv.FormatFloat(n.Value, 'f', -1, 64))
}

// DurationLabelFilter compares a label value parsed as a Go duration.
type DurationLabelFilter struct {
	Name  string
	Value time.Duration
	Type  LabelFilterType
}

// NewDurationLabelFilter builds a duration label filter.
func NewDurationLabelFilter(t LabelFilterType, name string, value time.Duration) *DurationLabelFilter {
	return &DurationLabelFilter{Name: name, Value: value, Type: t}
}

func (d *DurationLabelFilter) isLabelFilterer() {}

func (d *DurationLabelFilter) String() string {
	return fmt.Sprintf("%s%s%s", d.Name, d.Type, d.Value)
}

// BytesLabelFilter compares a label value parsed as a humanised byte
// size.
type BytesLabelFilter struct {
	Name  string
	Value uint64
	Type  LabelFilterType
}

// NewBytesLabelFilter builds a bytes label filter.
func NewBytesLabelFilter(t LabelFilterType, name string, value uint64) *BytesLabelFilter {
	return &BytesLabelFilter{Name: name, Value: value, Type: t}
}

func (b *BytesLabelFilter) isLabelFilterer() {}

func (b *BytesLabelFilter) String() string {
	// Render the byte count the way upstream does: humanize then strip the
	// space humanize.Bytes inserts (e.g. "5 kB" -> "5kB").
	v := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, humanize.Bytes(b.Value))
	return fmt.Sprintf("%s%s%s", b.Name, b.Type, v)
}

// IPLabelFilter matches a label value against an `ip("...")` pattern
// (single IP / CIDR / range). Only `=` / `!=` are grammatical.
type IPLabelFilter struct {
	Ty      LabelFilterType
	Label   string
	Pattern string
}

// NewIPLabelFilter builds an ip() label filter. The pattern is validated
// at lowering time (internal/logql/ip.go), matching upstream which parks
// an invalid pattern until the filter runs.
func NewIPLabelFilter(pattern, label string, ty LabelFilterType) *IPLabelFilter {
	return &IPLabelFilter{Ty: ty, Label: label, Pattern: pattern}
}

func (f *IPLabelFilter) isLabelFilterer() {}

func (f *IPLabelFilter) String() string {
	// Upstream renders ip() label filters with a single `=` for the equal
	// case (not `==`), keeping the on-the-wire form `addr=ip("...")`.
	eq := "="
	if f.Ty == LabelFilterNotEqual {
		eq = LabelFilterNotEqual.String()
	}
	return fmt.Sprintf("%s%sip(%q)", f.Label, eq, f.Pattern)
}

// LabelExtractionExpr is one `identifier="expression"` pair in a typed
// `| json` / `| logfmt` parser stage. Expression == Identifier for the
// bare `| logfmt foo` form.
type LabelExtractionExpr struct {
	Identifier string
	Expression string
}

// NewLabelExtractionExpr builds an extraction expression.
func NewLabelExtractionExpr(identifier, expression string) LabelExtractionExpr {
	return LabelExtractionExpr{Identifier: identifier, Expression: expression}
}

// LabelFmt is one `dst=src` (rename) or `dst="{{template}}"` clause of a
// `| label_format` stage.
type LabelFmt struct {
	Name   string
	Value  string
	Rename bool
}

// NewRenameLabelFmt builds a rename clause (`dst=src`).
func NewRenameLabelFmt(dst, src string) LabelFmt {
	return LabelFmt{Name: dst, Value: src, Rename: true}
}

// NewTemplateLabelFmt builds a template clause (`dst="{{...}}"`).
func NewTemplateLabelFmt(dst, template string) LabelFmt {
	return LabelFmt{Name: dst, Value: template}
}
