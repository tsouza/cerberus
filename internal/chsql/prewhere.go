package chsql

import "github.com/tsouza/cerberus/internal/chplan"

// flattenAnd walks a chplan.Expr tree, returning the conjunction's
// flattened list of leaves. `Binary{OpAnd, A, B}` decomposes into
// flattenAnd(A) ++ flattenAnd(B); every other Expr kind is returned
// as a single-element slice. This mirrors the FilterFusion optimizer
// rule's view of a Filter chain: conjuncts compose left-associatively,
// and the codegen wants the flat set to partition.
func flattenAnd(e chplan.Expr) []chplan.Expr {
	bin, ok := e.(*chplan.Binary)
	if !ok || bin.Op != chplan.OpAnd {
		return []chplan.Expr{e}
	}
	out := flattenAnd(bin.Left)
	out = append(out, flattenAnd(bin.Right)...)
	return out
}

// collectColumnRefs walks an Expr tree and returns the set of bare
// column names referenced. Map-access keys, regex patterns and other
// literals do not contribute. Qualified column refs (those carrying a
// Qualifier) are skipped — they reach into a join side and don't
// describe the underlying MergeTree column the codegen needs.
func collectColumnRefs(e chplan.Expr) []string {
	seen := make(map[string]struct{})
	var walk func(chplan.Expr)
	walk = func(x chplan.Expr) {
		switch v := x.(type) {
		case *chplan.ColumnRef:
			if v.Qualifier == "" {
				seen[v.Name] = struct{}{}
			}
		case *chplan.Binary:
			walk(v.Left)
			walk(v.Right)
		case *chplan.FuncCall:
			for _, a := range v.Args {
				walk(a)
			}
		case *chplan.MapAccess:
			walk(v.Map)
			walk(v.Key)
		case *chplan.MapWithoutKeys:
			walk(v.Map)
		case *chplan.MapWithoutEmptyValues:
			walk(v.Map)
		case *chplan.LabelReplace:
			walk(v.Map)
		case *chplan.LabelJoin:
			walk(v.Map)
		case *chplan.LineContent:
			walk(v.Source)
		case *chplan.FieldAccess:
			walk(v.Source)
		case *chplan.NestedArrayExists:
			walk(v.Value)
			if v.Column != "" {
				seen[v.Column] = struct{}{}
			}
		}
	}
	walk(e)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out
}

// isCheapPredicate reports whether the Expr tree consists only of
// "cheap to evaluate" shapes — comparison and boolean operators,
// column refs, literals, map accesses, regex matches (CH's `match`
// is a hash-prefix check on a constant pattern, cheap), and the
// LineContent / FieldAccess shapes Loki / Tempo emit for label-style
// predicates.
//
// FuncCall is treated conservatively: arbitrary user functions may
// be expensive (e.g. a JSON-parse, a regex over the body column),
// so they fall out of the cheap set and stay in WHERE.
// NestedArrayExists invokes arrayExists on a Nested column —
// also not promoted, since the per-row cost is unbounded by the
// nested-array length.
func isCheapPredicate(e chplan.Expr) bool {
	switch v := e.(type) {
	case *chplan.ColumnRef, *chplan.LitString, *chplan.LitInt, *chplan.LitFloat, *chplan.LitBool:
		return true
	case *chplan.Binary:
		return isCheapPredicate(v.Left) && isCheapPredicate(v.Right)
	case *chplan.MapAccess:
		return isCheapPredicate(v.Map) && isCheapPredicate(v.Key)
	case *chplan.LineContent:
		return isCheapPredicate(v.Source)
	case *chplan.FieldAccess:
		return isCheapPredicate(v.Source)
	case *chplan.MapWithoutKeys, *chplan.MapWithoutEmptyValues, *chplan.LabelReplace, *chplan.LabelJoin, *chplan.FuncCall, *chplan.NestedArrayExists:
		return false
	}
	return false
}

// classifyPredicate returns the columns referenced, whether the
// predicate is cheap, and whether it touches a wide column on shape.
// The cols slice is returned even for non-safe predicates because the
// sort-key ordering pass inspects it independently.
func classifyPredicate(e chplan.Expr, shape TableShape) (cols []string, cheap, touchesWide bool) {
	cols = collectColumnRefs(e)
	cheap = isCheapPredicate(e)
	for _, c := range cols {
		if shape.IsWideColumn(c) {
			touchesWide = true
			break
		}
	}
	return cols, cheap, touchesWide
}

// sortRankFor returns the lowest SortColumns rank among the predicate's
// referenced columns, or -1 if none match. Lower rank means the predicate
// touches a column closer to the front of the ORDER BY, which is the
// position CH's granule-skipping cares about.
func sortRankFor(cols []string, shape TableShape) int {
	best := -1
	for _, c := range cols {
		r := shape.SortRank(c)
		if r < 0 {
			continue
		}
		if best < 0 || r < best {
			best = r
		}
	}
	return best
}

// orderedConjuncts partitions conjuncts into the three buckets described
// in docs/optimizer-research.md § 3 — sort-prefix predicates first,
// then skip-index predicates, then everything else — preserving input
// order within each bucket. Within the sort-prefix bucket conjuncts are
// further ordered by ascending SortColumns rank so the earliest sort
// column emits first; ties are broken by input order to keep the SQL
// deterministic for the goldens.
//
// shape may be the zero value; the function then returns the conjuncts
// unchanged (every predicate falls into the "rest" bucket in input
// order).
func orderedConjuncts(conjuncts []chplan.Expr, shape TableShape) []chplan.Expr {
	if len(conjuncts) <= 1 {
		return conjuncts
	}
	type ranked struct {
		expr chplan.Expr
		rank int
		idx  int
	}
	var prefix []ranked
	var skip []chplan.Expr
	var rest []chplan.Expr
	for i, c := range conjuncts {
		cols := collectColumnRefs(c)
		if r := sortRankFor(cols, shape); r >= 0 {
			prefix = append(prefix, ranked{expr: c, rank: r, idx: i})
			continue
		}
		hitsSkip := false
		for _, col := range cols {
			if shape.IsSkipIndexColumn(col) {
				hitsSkip = true
				break
			}
		}
		if hitsSkip {
			skip = append(skip, c)
			continue
		}
		rest = append(rest, c)
	}
	// Stable insertion sort by rank; ties broken by input order.
	for i := 1; i < len(prefix); i++ {
		for j := i; j > 0; j-- {
			if prefix[j-1].rank > prefix[j].rank ||
				(prefix[j-1].rank == prefix[j].rank && prefix[j-1].idx > prefix[j].idx) {
				prefix[j-1], prefix[j] = prefix[j], prefix[j-1]
				continue
			}
			break
		}
	}
	out := make([]chplan.Expr, 0, len(conjuncts))
	for _, r := range prefix {
		out = append(out, r.expr)
	}
	out = append(out, skip...)
	out = append(out, rest...)
	return out
}

// partitionPrewhere splits ordered conjuncts into a PREWHERE bucket
// and a WHERE bucket given the table shape. A conjunct lands in
// PREWHERE iff it is cheap and references no wide column.
//
// When *every* conjunct qualifies for PREWHERE, the last cheap-but-
// non-wide-touching predicate is kept in WHERE so CH's executor
// doesn't degenerate to a no-op WHERE clause. The behaviour is purely
// cosmetic — CH happily accepts a query with only PREWHERE — but the
// retained predicate matches the PREWHERE-promotion design note.
//
// When the shape has no wide columns registered (e.g. an unknown
// table), partitionPrewhere returns empty PREWHERE and all conjuncts
// in WHERE: there's no benefit to promotion when no wide column would
// be deferred.
func partitionPrewhere(conjuncts []chplan.Expr, shape TableShape) (prewhere, where []chplan.Expr) {
	if len(shape.WideColumns) == 0 {
		return nil, conjuncts
	}
	for _, c := range conjuncts {
		_, cheap, wide := classifyPredicate(c, shape)
		if cheap && !wide {
			prewhere = append(prewhere, c)
		} else {
			where = append(where, c)
		}
	}
	if len(where) == 0 && len(prewhere) > 0 {
		last := len(prewhere) - 1
		where = []chplan.Expr{prewhere[last]}
		prewhere = prewhere[:last]
	}
	return prewhere, where
}

// projectionTouchesWide reports whether the SELECT list named by cols
// would pull any wide column on shape. An empty cols slice (the
// `SELECT *` case) is treated as touching every column — CH reads the
// full row, including the wide ones.
func projectionTouchesWide(cols []string, shape TableShape) bool {
	if len(shape.WideColumns) == 0 {
		return false
	}
	if len(cols) == 0 {
		return true
	}
	for _, c := range cols {
		if shape.IsWideColumn(c) {
			return true
		}
	}
	return false
}
