package solver

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// slice decomposes the eval grid into k disjoint, on-grid anchor sub-grids
// and re-anchors a deep copy of plan onto each (docs §Decomposition "Primary
// dimension"). It is the geometry half of the Planner: pure arithmetic over
// the anchor grid plus a ReanchorRange per slice (which deep-copies, so the
// input plan is never mutated).
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

	// Spine geometry the scan floor needs: Offset and cumulative lookback D.
	offset, d := spineOffsetAndD(plan)

	// The plan that reaches the slicer is pinned at the full request grid
	// [Start, End] (the Planner's grid-prediction guard already verified it
	// sits exactly there). ReanchorRange only re-anchors a node whose bounds
	// are either unpinned or already equal to the target grid, so to re-grid
	// each slice onto a SUB-window we first build one deep, spine-UNPINNED
	// copy of the plan; ReanchorRange then fills each slice's grid into it.
	// The original plan is never touched — unpinSpine clones.
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

		// ScanFrom_j = Start_j - D - Offset_spine. Offset enters with its
		// sign: a negative offset widens the scan to the RIGHT past End_j,
		// and the left floor moves accordingly. ScanFrom is solver-owned and
		// sign-aware (docs §Decomposition); the re-anchored plan carries the
		// grid, and the executor (a later PR) consumes ScanFrom for the
		// offset-aware pushdown.
		scanFrom := startJ.Add(-d).Add(-offset)

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

// unpinSpine returns a deep copy of plan whose windowed-spine bounds
// (RangeWindow / RangeLWR Start, End, and the matrix OuterRange) are zeroed,
// so ReanchorRange treats every spine node as the unpinned subquery-inner
// shape and fills each slice's grid in. The original plan is never mutated:
// the copy is produced by chplan.CloneNode, and only the cloned spine nodes
// are zeroed. Off-spine nodes are carried verbatim by the clone.
//
// Zeroing is safe because the Planner has already proven (signal 4) that
// every spine node sits exactly on the grid the request predicts — so the
// information being dropped is exactly the grid ReanchorRange recomputes.
func unpinSpine(plan chplan.Node) chplan.Node {
	c := chplan.CloneNode(plan)
	var walk func(chplan.Node)
	walk = func(n chplan.Node) {
		switch v := n.(type) {
		case *chplan.RangeWindow:
			if v.Step > 0 {
				v.Start = time.Time{}
				v.End = time.Time{}
				v.OuterRange = 0
			}
			walk(v.Input)
			return
		case *chplan.RangeLWR:
			v.Start = time.Time{}
			v.End = time.Time{}
			walk(v.Input)
			return
		case *chplan.Filter:
			walk(v.Input)
			return
		case *chplan.Project:
			walk(v.Input)
			return
		}
		for _, ch := range n.Children() {
			walk(ch)
		}
	}
	walk(c)
	return c
}

// spineOffsetAndD walks the windowed spine of plan to recover the Offset
// folded onto the selector and the cumulative lookback D (Σ Range down matrix
// windows + leaf RangeLWR.Lookback). Both feed the solver-owned, sign-aware
// scan floor (docs §Decomposition): the matrix emitters are offset-blind, so
// the solver derives the input interval itself.
func spineOffsetAndD(plan chplan.Node) (offset, d time.Duration) {
	var walk func(chplan.Node)
	walk = func(n chplan.Node) {
		switch v := n.(type) {
		case *chplan.RangeWindow:
			if v.Offset != 0 {
				offset = v.Offset
			}
			d += v.Range
			walk(v.Input)
			return
		case *chplan.RangeLWR:
			if v.Offset != 0 {
				offset = v.Offset
			}
			d += v.Lookback
			walk(v.Input)
			return
		case *chplan.Filter:
			walk(v.Input)
			return
		case *chplan.Project:
			walk(v.Input)
			return
		}
		for _, c := range n.Children() {
			walk(c)
		}
	}
	walk(plan)
	return offset, d
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
