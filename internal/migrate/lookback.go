package migrate

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/prometheus/common/model"
	promparser "github.com/prometheus/prometheus/promql/parser"
)

// LookbackVersion is the schema version stamped into every emitted Lookback.
// Readers reject a version they do not understand rather than misparsing a
// retention figure a cutover decision rests on.
const LookbackVersion = 1

// Reach is how far back in time one query reads, measured from the instant it
// evaluates at. It is the quantity a retention runway has to cover: provision
// less than the longest reach in the corpus and the query that needs it goes
// blank the day after cutover.
//
// Reach is defined structurally, from the PromQL AST alone — no data, no clock:
//
//   - a vector selector reaches back by its offset;
//   - a matrix selector reaches back by its range plus its offset;
//   - a subquery reaches back by its own range and offset PLUS the reach of the
//     expression it evaluates at every step, because the inner expression is
//     re-evaluated at the oldest anchor of the outer window;
//   - every other node reaches back as far as its farthest-reaching child.
//
// A negative offset (`offset -1h`, which looks FORWARD) contributes a negative
// reach; the corpus-level maximum clamps at zero, so a corpus of nothing but
// forward-looking queries requires no runway rather than a negative one.
//
// The staleness lookback the server applies to a bare instant selector is
// deliberately NOT folded in: it is a server setting, not a property of the
// operator's query, and adding it here would make the retention figure move
// when an unrelated flag changes.
func reachOf(node promparser.Node) time.Duration {
	switch n := node.(type) {
	case *promparser.VectorSelector:
		return n.OriginalOffset
	case *promparser.MatrixSelector:
		return n.Range + reachOf(n.VectorSelector)
	case *promparser.SubqueryExpr:
		return n.Range + n.OriginalOffset + reachOf(n.Expr)
	default:
		var max time.Duration
		for i, child := range promparser.Children(node) {
			r := reachOf(child)
			if i == 0 || r > max {
				max = r
			}
		}
		return max
	}
}

// QueryLookback is one PromQL corpus query's reach, carried with the provenance
// of the rule or dashboard panel that needs it, so an operator reading the
// longest figure can go straight to the query that produced it.
type QueryLookback struct {
	Expr   string         `json:"expr"`
	Source string         `json:"source"`
	Kind   string         `json:"kind"`
	Reach  model.Duration `json:"reach"`
}

// Lookback is the retention runway the corpus requires, plus a full accounting
// of every corpus entry that is NOT part of the relative-lookback walk. An entry
// the walk cannot handle is listed with a reason — never folded in as reaching
// back no distance, which would silently shrink the runway the operator
// provisions.
//
//   - ByQuery     — one entry per PromQL query whose reach is a relative lookback.
//   - Anchored    — PromQL queries pinned by an `@` modifier. Their reach is
//     measured from a fixed instant rather than from evaluation time, so it is
//     not a lookback and cannot be folded into Longest without inventing a clock.
//   - NonPromQL   — LogQL / TraceQL entries; this walk is the PromQL AST's.
//   - Unparseable — entries the PromQL parser rejected.
//
// len(ByQuery) + len(Anchored) + len(NonPromQL) + len(Unparseable) always equals
// the number of corpus queries.
type Lookback struct {
	SchemaVersion int             `json:"schema_version"`
	Longest       model.Duration  `json:"longest"`
	ByQuery       []QueryLookback `json:"by_query"`
	Anchored      []SkippedEntry  `json:"anchored"`
	NonPromQL     []SkippedEntry  `json:"non_promql"`
	Unparseable   []SkippedEntry  `json:"unparseable"`
}

// MaxLookback computes the retention runway the corpus requires: the longest
// reach across its PromQL queries, with every other entry accounted for in its
// own list. It is pure and offline — the corpus is the only input.
func MaxLookback(c Corpus) Lookback {
	lb := Lookback{
		SchemaVersion: LookbackVersion,
		ByQuery:       []QueryLookback{},
		Anchored:      []SkippedEntry{},
		NonPromQL:     []SkippedEntry{},
		Unparseable:   []SkippedEntry{},
	}
	var longest time.Duration
	for _, q := range c.Queries {
		if q.Lang != LangPromQL {
			lb.NonPromQL = append(lb.NonPromQL, SkippedEntry{
				Source: q.Source,
				Reason: fmt.Sprintf("%s entry; the lookback walk reads the PromQL AST", q.Lang),
			})
			continue
		}
		ast, err := ruleGraphParser.ParseExpr(q.Expr)
		if err != nil {
			lb.Unparseable = append(lb.Unparseable, SkippedEntry{
				Source: q.Source,
				Reason: fmt.Sprintf("unparseable PromQL: %v", err),
			})
			continue
		}
		if why := anchorReason(ast); why != "" {
			lb.Anchored = append(lb.Anchored, SkippedEntry{Source: q.Source, Reason: why})
			continue
		}
		reach := reachOf(ast)
		if reach > longest {
			longest = reach
		}
		lb.ByQuery = append(lb.ByQuery, QueryLookback{
			Expr:   q.Expr,
			Source: q.Source,
			Kind:   q.Kind,
			Reach:  model.Duration(reach),
		})
	}
	lb.Longest = model.Duration(longest)

	sortByQuery(lb.ByQuery)
	sortSkipped(lb.Anchored)
	sortSkipped(lb.NonPromQL)
	sortSkipped(lb.Unparseable)
	return lb
}

// anchorReason reports why an expression is pinned to a fixed instant, or the
// empty string when its reach is a plain relative lookback. An `@` modifier —
// absolute (`@ 1600000000`) or window-relative (`@ start()` / `@ end()`) —
// evaluates its selector at an instant that is not the query's own evaluation
// time, so the distance it reads back cannot be stated as a lookback offline.
func anchorReason(node promparser.Node) string {
	var reason string
	promparser.Inspect(node, func(n promparser.Node, _ []promparser.Node) error {
		var (
			ts         *int64
			startOrEnd promparser.ItemType
			text       string
		)
		switch v := n.(type) {
		case *promparser.VectorSelector:
			ts, startOrEnd, text = v.Timestamp, v.StartOrEnd, v.String()
		case *promparser.SubqueryExpr:
			ts, startOrEnd, text = v.Timestamp, v.StartOrEnd, v.String()
		default:
			return nil
		}
		switch {
		case ts != nil:
			reason = fmt.Sprintf(
				"pinned to an absolute instant by an @ modifier (%s); its reach is not a lookback", text,
			)
		case startOrEnd != 0:
			reason = fmt.Sprintf(
				"pinned to the request window by an @ modifier (%s); its reach is not a lookback", text,
			)
		default:
			return nil
		}
		return errAnchorFound
	})
	return reason
}

// errAnchorFound stops the anchor walk at the first `@` modifier: one is enough
// to take the whole query out of the relative-lookback walk.
var errAnchorFound = fmt.Errorf("migrate: anchored expression")

// sortByQuery orders the per-query reaches by (Source, Expr) so the artifact is
// byte-identical for identical inputs regardless of harvest order.
func sortByQuery(in []QueryLookback) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Source != in[j].Source {
			return in[i].Source < in[j].Source
		}
		return in[i].Expr < in[j].Expr
	})
}

// sortSkipped orders an accounting list by (Source, Reason), matching the order
// BuildCorpus gives the corpus's own skip list.
func sortSkipped(in []SkippedEntry) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Source != in[j].Source {
			return in[i].Source < in[j].Source
		}
		return in[i].Reason < in[j].Reason
	})
}

// Marshal renders the lookback as deterministic, indented JSON with a trailing
// newline, matching the corpus's own on-disk shape.
func (l Lookback) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(l, "", jsonIndent)
	if err != nil {
		return nil, fmt.Errorf("migrate: marshal lookback: %w", err)
	}
	return append(data, '\n'), nil
}
