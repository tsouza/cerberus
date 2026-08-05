package promql

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/prometheus/prometheus/promql/parser"
)

// evalLabelReplace implements label_replace(v, dst, replacement, src,
// regex) — a pure label-set transform over an already-evaluated instant
// vector. Mirrors Prometheus's evalLabelReplace
// (promql/functions.go): for each series, match `regex` (anchored
// `^(?s:regex)$`, matching Go's ExpandString convention) against the
// src label's value (empty string when the label is absent), and when
// it matches, set dst to the expanded replacement template. Series
// whose src value doesn't match are passed through unchanged — dst is
// left as-is, not cleared.
//
// Unlike cerberus's real lowering (internal/promql/label_fns.go), this
// does not validate that dst/src are syntactically valid label names:
// cerberus's Attributes column is a generic Map(String, String), so its
// lowering never rejects a label name shape reference Prometheus's Go
// evaluator would panic on — matching cerberus's actual behavior here,
// not the upstream Go evaluator's, is the point of this oracle.
func (e *Evaluator) evalLabelReplace(c *parser.Call, evalTsMs int64) (value, error) {
	if len(c.Args) != 5 {
		return value{}, fmt.Errorf("oracle: label_replace expects 5 args, got %d", len(c.Args))
	}
	inner, err := e.evalAny(c.Args[0], evalTsMs)
	if err != nil {
		return value{}, err
	}
	if inner.Kind != kindVec {
		return value{}, fmt.Errorf("oracle: label_replace first argument must be an instant vector")
	}
	dst, err := stringLiteralArg(c.Args[1], "label_replace", "dst")
	if err != nil {
		return value{}, err
	}
	repl, err := stringLiteralArg(c.Args[2], "label_replace", "replacement")
	if err != nil {
		return value{}, err
	}
	src, err := stringLiteralArg(c.Args[3], "label_replace", "src")
	if err != nil {
		return value{}, err
	}
	regexStr, err := stringLiteralArg(c.Args[4], "label_replace", "regex")
	if err != nil {
		return value{}, err
	}
	regex, err := regexp.Compile("^(?s:" + regexStr + ")$")
	if err != nil {
		return value{}, fmt.Errorf("oracle: label_replace: invalid regular expression %q: %w", regexStr, err)
	}

	out := make([]VectorRow, len(inner.Vec))
	for i, r := range inner.Vec {
		lbls := r.Labels
		srcVal := lbls[src]
		if idx := regex.FindStringSubmatchIndex(srcVal); idx != nil {
			lbls = CopyLabels(r.Labels)
			lbls[dst] = string(regex.ExpandString(nil, repl, srcVal, idx))
		}
		out[i] = VectorRow{Labels: lbls, T: r.T, V: r.V}
	}
	if err := requireUniqueLabelSets(out); err != nil {
		return value{}, err
	}
	sortVectorRows(out)
	return value{Kind: kindVec, Vec: out}, nil
}

// evalLabelJoin implements label_join(v, dst, sep, src1, src2, ...) —
// concatenates the named source labels' values (empty string for an
// absent label) with sep and assigns the result to dst, for every
// series in the already-evaluated input vector. Mirrors Prometheus's
// evalLabelJoin (promql/functions.go). Zero src labels is valid PromQL
// (the parser allows a variadic tail of length 0); strings.Join of an
// empty slice is "", so dst becomes the empty string — matching Prom's
// documented behavior, no special case needed.
func (e *Evaluator) evalLabelJoin(c *parser.Call, evalTsMs int64) (value, error) {
	if len(c.Args) < 3 {
		return value{}, fmt.Errorf("oracle: label_join expects at least 3 args (v, dst, sep[, src…]), got %d", len(c.Args))
	}
	inner, err := e.evalAny(c.Args[0], evalTsMs)
	if err != nil {
		return value{}, err
	}
	if inner.Kind != kindVec {
		return value{}, fmt.Errorf("oracle: label_join first argument must be an instant vector")
	}
	dst, err := stringLiteralArg(c.Args[1], "label_join", "dst")
	if err != nil {
		return value{}, err
	}
	sep, err := stringLiteralArg(c.Args[2], "label_join", "sep")
	if err != nil {
		return value{}, err
	}
	srcs := make([]string, 0, len(c.Args)-3)
	for i := 3; i < len(c.Args); i++ {
		src, err := stringLiteralArg(c.Args[i], "label_join", fmt.Sprintf("src_%d", i-2))
		if err != nil {
			return value{}, err
		}
		srcs = append(srcs, src)
	}

	out := make([]VectorRow, len(inner.Vec))
	for i, r := range inner.Vec {
		vals := make([]string, len(srcs))
		for j, src := range srcs {
			vals[j] = r.Labels[src]
		}
		lbls := CopyLabels(r.Labels)
		lbls[dst] = strings.Join(vals, sep)
		out[i] = VectorRow{Labels: lbls, T: r.T, V: r.V}
	}
	if err := requireUniqueLabelSets(out); err != nil {
		return value{}, err
	}
	sortVectorRows(out)
	return value{Kind: kindVec, Vec: out}, nil
}

// stringLiteralArg extracts a static string literal from a Call
// argument. PromQL's grammar requires a literal string at these
// positions (label_replace/label_join's dst/replacement/src/regex/sep
// params); this guards against an upstream parser shape change without
// a bad-cast panic.
func stringLiteralArg(e parser.Expr, fnName, paramName string) (string, error) {
	sl, ok := e.(*parser.StringLiteral)
	if !ok {
		return "", fmt.Errorf("oracle: %s(%s) requires a string literal, got %T", fnName, paramName, e)
	}
	return sl.Val, nil
}

// requireUniqueLabelSets mirrors reference Prometheus's
// mergeSeriesWithSameLabelset / ContainsSameLabelset check for the
// instant-eval case: two output rows sharing the exact same label set
// (including __name__) at the same timestamp is the
// "vector cannot contain metrics with the same labelset" query error,
// not a silent merge (Prom's merge path only applies across DIFFERENT
// timestamps within a range query's per-series matrix, which doesn't
// arise here since every row in an instant evaluation shares one ts).
func requireUniqueLabelSets(rows []VectorRow) error {
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		k := labelKey(r.Labels)
		if _, ok := seen[k]; ok {
			return fmt.Errorf("oracle: vector cannot contain metrics with the same labelset %s", k)
		}
		seen[k] = struct{}{}
	}
	return nil
}
