package solver

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// slice decomposes the eval grid into k disjoint, on-grid anchor sub-grids
// and re-anchors a share-immutable-off-spine view of plan onto each (docs
// §Decomposition "Primary dimension"). It is the geometry half of the
// Planner: pure arithmetic over the anchor grid plus a ReanchorRange per
// slice. ReanchorRange clones only the O(spine-depth) re-gridded spine nodes
// and shares the immutable off-spine subtrees, so the input plan is never
// mutated and the K shards never alias a mutable node.
//
// Anchors are defined backward from End: a_i = End - Offset - i*Step,
// i in [0, N), N = OuterRange/Step + 1. With m = ceil(N/K) anchors per slice,
// slice j owns indices [j*m, min((j+1)*m, N)); End_j = End - j*m*Step;
// OuterRange_j = (count_j - 1)*Step. Because End_j sits on the original grid
// and OuterRange_j is a Step-multiple, the union of slice anchor sets equals
// the original set EXACTLY, pairwise disjoint — no compose-time reconciliation.
//
// Singleton-tail rule: a slice with count_j < 2 MERGES into its (older)
// neighbor — an OuterRange_j == 0 slice would flip the emitter from the
// matrix template to the instant template, and keeping every shard on the
// identical template keeps the parity argument trivial.
//
// Returned slices are ordered OLDEST-FIRST (the composition order): slice 0 is
// the oldest sub-grid, the last slice ends at the original End.
func (p *Planner) slice(plan chplan.Node, meta RequestMeta, k int) ([]Slice, error) {
	if k < 2 {
		return nil, fmt.Errorf("solver: slice K must be >= 2, got %d", k)
	}

	end := meta.End.UTC()
	start := meta.Start.UTC()
	step := meta.Step
	if step <= 0 {
		return nil, fmt.Errorf("solver: slice requires Step > 0, got %s", step)
	}
	outerRange := end.Sub(start)
	if outerRange <= 0 {
		return nil, fmt.Errorf("solver: slice requires End > Start, got [%v,%v]", start, end)
	}

	n := int(outerRange/step) + 1 // total anchor count
	if n < 2 {
		return nil, fmt.Errorf("solver: slice requires N >= 2 anchors, got %d", n)
	}
	// Cap K so every slice owns at least 2 anchors: with m = ceil(N/K) the
	// per-slice count is >= 2 iff K <= floor(N/2). Beyond that, m would be 1
	// and every slice a singleton — the singleton-tail rule generalised. The
	// Planner's clamp already keeps K within this bound for routed plans; the
	// cap makes slice() correct even when called with an out-of-range K
	// (the geometry unit tests exercise arbitrary K directly).
	if maxK := n / 2; k > maxK {
		k = maxK
	}
	if k < 2 {
		k = 2
	}

	// How far back before each slice's oldest anchor its input scan must reach
	// (Offset + lookback, compounded along nested spines, max across join arms).
	reach := spineReach(plan)

	// The plan that reaches the slicer is pinned at the full request grid
	// [Start, End] (the Planner's grid-prediction guard already verified it
	// sits exactly there). ReanchorRange only re-anchors a node whose bounds
	// are either unpinned or already equal to the target grid, so to re-grid
	// each slice onto a SUB-window we first build one spine-UNPINNED, share-
	// immutable-off-spine view of the plan; ReanchorRange then fills each
	// slice's grid into it. The original plan is never touched — unpinSpine
	// clones only the spine path and shares the off-spine subtrees.
	base := unpinSpine(plan)

	// m = ceil(N/K) anchors per slice (newest-first index space).
	m := (n + k - 1) / k

	// Build slices newest-first by anchor index, then flip to oldest-first
	// for composition. Index space: anchor i (i in [0,N)) has timestamp
	// End - Offset - i*Step; the emitted/grid anchor (Offset aside) is
	// End - i*Step. Slice over the grid bounds.
	type span struct {
		startIdx, count int // [startIdx, startIdx+count) over i in [0,N)
	}
	var spans []span
	for j := 0; j*m < n; j++ {
		lo := j * m
		hi := lo + m
		if hi > n {
			hi = n
		}
		spans = append(spans, span{startIdx: lo, count: hi - lo})
	}

	// Singleton-tail merge: the last span (oldest, largest index) is the
	// only one that can have count < m; if it has count < 2, fold it into
	// its newer neighbor so no OuterRange_j == 0 slice is emitted.
	if len(spans) >= 2 {
		last := spans[len(spans)-1]
		if last.count < 2 {
			spans[len(spans)-2].count += last.count
			spans = spans[:len(spans)-1]
		}
	}

	// Each span [lo, lo+count) over i maps to grid bounds:
	//   End_j   = End - lo*Step                         (newest anchor of slice)
	//   Start_j = End - (lo+count-1)*Step               (oldest anchor of slice)
	// Build oldest-first: iterate spans in reverse index order (largest lo
	// is the oldest slice).
	slices := make([]Slice, 0, len(spans))
	for si := len(spans) - 1; si >= 0; si-- {
		sp := spans[si]
		endJ := end.Add(-time.Duration(sp.startIdx) * step)
		startJ := end.Add(-time.Duration(sp.startIdx+sp.count-1) * step)

		// ScanFrom_j = Start_j - reach, where reach folds each spine's Offset
		// (with its sign) and lookback and takes the deepest join/union arm.
		// ScanFrom is solver-owned and sign-aware (docs §Decomposition); the
		// re-anchored plan carries the grid, and the executor (a later PR)
		// consumes ScanFrom for the offset-aware pushdown.
		scanFrom := startJ.Add(-reach)

		shardPlan, err := chplan.ReanchorRange(base, startJ, endJ)
		if err != nil {
			return nil, fmt.Errorf("solver: re-anchor slice %d [%v,%v]: %w", si, startJ, endJ, err)
		}

		slices = append(slices, Slice{
			Start:    startJ,
			End:      endJ,
			ScanFrom: scanFrom,
			Plan:     shardPlan,
		})
	}

	// Re-index oldest-first.
	for i := range slices {
		slices[i].Index = i
	}
	return slices, nil
}

// unpinSpine returns a copy-on-write view of plan whose windowed-spine bounds
// (RangeWindow / RangeLWR Start, End, and the matrix OuterRange) are zeroed,
// so ReanchorRange treats every spine node as the unpinned subquery-inner
// shape and fills each slice's grid in.
//
// The original plan is never mutated. unpinSpine clones ONLY the spine-path
// nodes it actually zeroes (and their ancestors back to the root, the
// O(spine-depth) chain) and SHARES every immutable off-spine subtree verbatim
// — the structural-sharing companion to ReanchorRange's off-spine sharing.
// The returned tree therefore aliases the input's off-spine nodes; it is fed
// straight into ReanchorRange (which shares them again onto each shard) and is
// never mutated in place.
//
// Zeroing is safe because the Planner has already proven (signal 4) that
// every spine node sits exactly on the grid the request predicts — so the
// information being dropped is exactly the grid ReanchorRange recomputes.
//
// GUARDRAIL B (nested subqueries). Blanket off-spine sharing is exact only
// when an off-spine subtree carries no windowed node that unpinSpine must
// zero. An off-spine subtree reachable via a non-spine Node child (e.g. a
// TopK.KExpr computed-K plan) CAN itself contain a RangeWindow / RangeLWR
// that needs zeroing; sharing that subtree and zeroing it in place would
// corrupt the caller's plan. So unpinSpine DESCENDS into off-spine children,
// cloning the path to any inner windowed node it must zero, and shares only
// the genuinely window-free subtrees. (ScalarSubquery interiors are reached
// through Expr slots, not Node children; chplan.Walk and the Children() walk
// here never descend into them, so they are carried by value inside the
// shared/cloned node exactly as before — unpinSpine never zeroed them.)
func unpinSpine(plan chplan.Node) chplan.Node {
	out, _ := unpinSpineCOW(plan)
	return out
}

// unpinSpineCOW returns a copy-on-write rewrite of n with the windowed-spine
// bounds zeroed. The second return reports whether the returned node is a
// fresh clone (true) or the shared original (false), so a parent can decide
// whether it too must clone (it must clone iff any child changed).
//
// Invariant: the returned node is `Equal` to the old CloneNode-then-zero
// result, but allocates only along the path to a zeroed spine node.
func unpinSpineCOW(n chplan.Node) (chplan.Node, bool) {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		input, _ := unpinSpineCOW(v.Input)
		// A matrix window (Step > 0) is on the spine: clone + zero its grid.
		// An instant window is not zeroed but must still clone if its input
		// changed so the zeroing does not leak into the shared original.
		if v.Step > 0 {
			c := *v
			c.Start = time.Time{}
			c.End = time.Time{}
			c.OuterRange = 0
			c.Input = input
			return &c, true
		}
		c := *v
		c.Input = input
		return &c, true
	case *chplan.RangeLWR:
		input, _ := unpinSpineCOW(v.Input)
		c := *v
		c.Start = time.Time{}
		c.End = time.Time{}
		c.Input = input
		return &c, true
	case *chplan.Filter:
		input, changed := unpinSpineCOW(v.Input)
		if !changed {
			return v, false
		}
		c := *v
		c.Input = input
		return &c, true
	case *chplan.Project:
		input, changed := unpinSpineCOW(v.Input)
		if !changed {
			return v, false
		}
		c := *v
		c.Input = input
		return &c, true
	}

	// Off the recognised spine. Descend into every Node child (GUARDRAIL B:
	// a child subtree -- e.g. a TopK.KExpr computed-K plan -- can itself carry
	// a windowed node that must be zeroed). If no child carries one, share the
	// original verbatim (the COW fast path). If one does, fall back to the
	// pre-COW behavior for THIS subtree only: deep-copy it and zero the spine
	// of the copy in place. That is byte-for-byte the old semantics, confined
	// to the rare off-spine-window subtree, so blanket-sharing never mutates a
	// node the caller still owns.
	return descendOffSpine(n)
}

// descendOffSpine handles the off-spine case of unpinSpineCOW. It probes each
// Node child for a windowed node that unpinSpine must zero; if none is found
// the original node is shared verbatim. If one is found, the whole node is
// deep-copied and its spine zeroed in place by zeroSpineInPlace -- exactly the
// pre-COW path -- so the shared original is never touched.
func descendOffSpine(n chplan.Node) (chplan.Node, bool) {
	if !subtreeHasZeroableSpine(n) {
		return n, false
	}
	cloned := chplan.CloneNode(n)
	zeroSpineInPlace(cloned)
	return cloned, true
}

// subtreeHasZeroableSpine reports whether the subtree rooted at n (descending
// only through Node children, never through Expr-embedded ScalarSubquery
// interiors -- which unpinSpine never zeroed) contains a windowed node whose
// grid unpinSpine would zero: any RangeLWR, or a matrix RangeWindow (Step > 0).
func subtreeHasZeroableSpine(n chplan.Node) bool {
	found := false
	chplan.Walk(n, func(node chplan.Node) bool {
		switch v := node.(type) {
		case *chplan.RangeLWR:
			found = true
		case *chplan.RangeWindow:
			if v.Step > 0 {
				found = true
			}
		}
		return !found
	})
	return found
}

// zeroSpineInPlace zeroes the windowed-spine bounds of an OWNED node tree in
// place. It is the original (pre-COW) unpinSpine walk, retained for the
// GUARDRAIL B off-spine fallback where unpinSpineCOW has already deep-copied
// the subtree and may safely mutate the copy.
func zeroSpineInPlace(n chplan.Node) {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		if v.Step > 0 {
			v.Start = time.Time{}
			v.End = time.Time{}
			v.OuterRange = 0
		}
		zeroSpineInPlace(v.Input)
		return
	case *chplan.RangeLWR:
		v.Start = time.Time{}
		v.End = time.Time{}
		zeroSpineInPlace(v.Input)
		return
	case *chplan.Filter:
		zeroSpineInPlace(v.Input)
		return
	case *chplan.Project:
		zeroSpineInPlace(v.Input)
		return
	}
	for _, ch := range n.Children() {
		zeroSpineInPlace(ch)
	}
}

// spineReach returns how far BACK before a slice's oldest anchor the slice's
// input scan must reach: the maximum, over every windowed spine PATH in plan, of
// (Σ Range/Lookback + Σ Offset along that path). The caller derives
// ScanFrom_j = Start_j - reach; the matrix emitters are offset-blind, so the
// solver owns this floor (docs §Decomposition).
//
// Range and Offset COMPOUND along a NESTED spine (a subquery's inner window is
// evaluated at the outer window's shifted sub-anchors), so a path sums them.
// PARALLEL arms — a VectorJoin's two arms, a companion UnionAll's arms — are
// independent, so the deepest arm wins (max). This is why the reach cannot
// collapse to one global (Σ Range, single Offset): summing parallel arms
// over-scans the shallower one, and — the real hazard — a single
// "last-offset-wins" scalar can pick the shallower arm's Offset and UNDER-scan
// the deeper arm (e.g. rate(a[5m]) / rate(b[5m] offset 1h)), silently dropping
// rows the deeper arm needs. A negative Offset shifts the window toward the
// future and shrinks a path's reach; max keeps whichever path reaches furthest
// into the past.
func spineReach(plan chplan.Node) time.Duration {
	if plan == nil {
		return 0
	}
	switch v := plan.(type) {
	case *chplan.RangeWindow:
		return v.Offset + v.Range + spineReach(v.Input)
	case *chplan.RangeLWR:
		return v.Offset + v.Lookback + spineReach(v.Input)
	}
	// Off-window node: Filter / Project / Aggregate pass through to their single
	// spine child; a VectorJoin / UnionAll fans out to parallel arms. The reach
	// is the deepest child's, or 0 at a leaf (Scan).
	first := true
	var reach time.Duration
	for _, c := range plan.Children() {
		if r := spineReach(c); first || r > reach {
			reach, first = r, false
		}
	}
	return reach
}

// gcdDuration returns gcd(|a|,|b|) as a Duration (nanosecond granularity).
func gcdDuration(a, b time.Duration) time.Duration {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// lcmDuration returns lcm(|a|,|b|) as a Duration. lcm(x,0)=x by convention so
// it folds cleanly over a resolution list seeded at 1ns.
func lcmDuration(a, b time.Duration) time.Duration {
	if a == 0 || b == 0 {
		if a == 0 {
			return b
		}
		return a
	}
	g := gcdDuration(a, b)
	if g == 0 {
		return 0
	}
	return (a / g) * b
}
