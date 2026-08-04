package loki

import (
	"strconv"
	"time"

	"github.com/dustin/go-humanize"

	"github.com/tsouza/cerberus/internal/logql"
	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// errLabelFilterKind mirrors internal/logql/duration.go's
// errLabelFilterKind constant — the `__error__` value reference Loki
// stamps when a numeric/duration/bytes label filter's value fails to
// parse. Kept as a package-local copy rather than shared: the two
// sites implement the same wire contract over two different execution
// substrates (SQL vs Go — see [labelFiltererEval]'s doc comment), not
// a shared abstraction worth coupling the packages over.
const errLabelFilterKind = "LabelFilterErr"

// labelFiltererEval evaluates a parsed LogQL label filterer against a
// row's Go-side label map. It is the execution-time counterpart to
// internal/logql/lower.go's SQL lowering (labelFiltererLower et al.)
// for filters that follow a `| unpack` / `| pattern` stage: those two
// parser stages extract labels in Go after the rows return (see
// unpackStep / newPatternStep in post_process.go), so any label filter
// downstream of them can't be resolved by SQL — the labels it tests
// may not exist until the dynamic stage's transform runs.
// lowerPipelineWithLabels detects this (its dynamicLabels gate) and
// skips SQL lowering entirely for such filters; [newLabelFilterStep]
// is where they're actually applied instead.
//
// Mirrors reference Loki's per-filter-kind Process contract, matching
// the semantics cerberus's SQL lowering already documents for the
// kinds that have SQL visibility (see internal/logql/{numbytes,
// duration,ip}.go's doc comments):
//
//   - StringLabelFilter: matches the label's value (absent → "")
//     against the embedded *labels.Matcher.
//   - BinaryLabelFilter: short-circuiting and/or.
//   - NumericLabelFilter / DurationLabelFilter / BytesLabelFilter:
//     label absent → row DROPPED, no error; value present but
//     unparseable → row KEPT with `__error__="LabelFilterErr"`
//     stamped (unless the row already carries an error — "don't
//     overwrite what might be a more useful error"); otherwise the
//     parsed value is compared against the literal.
//   - IPLabelFilter: a row already carrying `__error__` is kept
//     unconditionally; label absent → row DROPPED, even for `!=`;
//     otherwise the label value is scanned for an IP-shaped substring
//     inside the pattern's match set.
//
// Returns (keep, labels) — labels is the input map, or a copy carrying
// a freshly stamped error pair.
func labelFiltererEval(lf syntax.LabelFilterer, labels map[string]string) (bool, map[string]string) {
	switch f := lf.(type) {
	case *syntax.StringLabelFilter:
		return f.Matches(labels[f.Name]), labels
	case *syntax.BinaryLabelFilter:
		if f.And {
			keep, labels := labelFiltererEval(f.Left, labels)
			if !keep {
				return false, labels
			}
			return labelFiltererEval(f.Right, labels)
		}
		keep, labels := labelFiltererEval(f.Left, labels)
		if keep {
			return true, labels
		}
		return labelFiltererEval(f.Right, labels)
	case *syntax.NumericLabelFilter:
		raw, ok := labels[f.Name]
		if !ok {
			return false, labels
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return true, stampLabelFilterErr(labels, err)
		}
		return compareOrdered(f.Type, v, f.Value), labels
	case *syntax.DurationLabelFilter:
		raw, ok := labels[f.Name]
		if !ok {
			return false, labels
		}
		v, err := time.ParseDuration(raw)
		if err != nil {
			return true, stampLabelFilterErr(labels, err)
		}
		return compareOrdered(f.Type, v, f.Value), labels
	case *syntax.BytesLabelFilter:
		raw, ok := labels[f.Name]
		if !ok {
			return false, labels
		}
		v, err := humanize.ParseBytes(raw)
		if err != nil {
			return true, stampLabelFilterErr(labels, err)
		}
		return compareOrdered(f.Type, v, f.Value), labels
	case *syntax.IPLabelFilter:
		return ipLabelFilterEval(f, labels)
	default:
		// Unknown filterer kind — keep the row rather than silently
		// dropping data the filter was never meant to affect.
		return true, labels
	}
}

// orderedFilterValue is the set of value kinds NumericLabelFilter /
// DurationLabelFilter / BytesLabelFilter compare against their literal.
type orderedFilterValue interface {
	~float64 | ~int64 | ~uint64
}

// compareOrdered applies a LabelFilterType comparison to a parsed
// filter value against its literal.
func compareOrdered[T orderedFilterValue](op syntax.LabelFilterType, v, literal T) bool {
	switch op {
	case syntax.LabelFilterEqual:
		return v == literal
	case syntax.LabelFilterNotEqual:
		return v != literal
	case syntax.LabelFilterGreaterThan:
		return v > literal
	case syntax.LabelFilterGreaterThanOrEqual:
		return v >= literal
	case syntax.LabelFilterLesserThan:
		return v < literal
	case syntax.LabelFilterLesserThanOrEqual:
		return v <= literal
	default:
		return false
	}
}

// stampLabelFilterErr stamps `__error__="LabelFilterErr"` /
// `__error_details__=<err>` onto a copy of labels, unless the row
// already carries an error — reference Loki's "don't overwrite what
// might be a more useful error" rule (see internal/logql/numbytes.go).
func stampLabelFilterErr(labels map[string]string, err error) map[string]string {
	if labels[syntax.ErrorLabel] != "" {
		return labels
	}
	out := make(map[string]string, len(labels)+2)
	for k, v := range labels {
		out[k] = v
	}
	out[syntax.ErrorLabel] = errLabelFilterKind
	out[syntax.ErrorDetailsLabel] = err.Error()
	return out
}

// ipLabelFilterEval implements IPLabelFilter's distinct per-row
// contract (see internal/logql/ip.go's ipLabelFilterExpr doc comment):
// rows already carrying an error are kept unconditionally, an absent
// label drops the row even for `!=`, and otherwise the label value is
// scanned for an IP-shaped substring inside the pattern's match set.
func ipLabelFilterEval(f *syntax.IPLabelFilter, labels map[string]string) (bool, map[string]string) {
	if labels[syntax.ErrorLabel] != "" {
		return true, labels
	}
	raw, ok := labels[f.Label]
	if !ok {
		return false, labels
	}
	matched, err := logql.MatchesIPPattern(raw, f.Pattern)
	if err != nil {
		// Invalid patterns are rejected at lowering time via the same
		// parser (internal/logql's ipLabelFilterExpr / MatchesIPPattern
		// share parseIPPattern), so this is unreachable in production;
		// keep the row rather than dropping data over a bug elsewhere.
		return true, labels
	}
	if f.Ty == syntax.LabelFilterNotEqual {
		matched = !matched
	}
	return matched, labels
}
