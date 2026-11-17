package tempo

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// TestDrainsChosenTraces pins which lowered search shapes drain their chosen
// traces in a phase B of their own: the monotone spanset operations,
// pipeline selects and positive or union structural relations do; the plain
// shape (drained inside its own plan), a negated relation anywhere in the
// plan, the nested-set annotation and a top-level spanset aggregate do not.
func TestDrainsChosenTraces(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	start := time.Unix(1777629600, 0).UTC()
	ctx := traceql_lower.WithSearchWindow(traceql_lower.WithSearchTraceLimit(context.Background(), 20), start, start.Add(time.Hour))
	lang := &traceqlLang{schema: s}
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{`{ resource.service.name = "a" } && { name = "b" }`, true},
		{`{ name = "a" } || { name = "b" }`, true},
		{`({ name = "a" } && { name = "b" }) || { name = "c" }`, true},
		{`{ name = "a" } | select(span.x)`, true},
		{`{ rootName = name } && { kind = server }`, true},
		{`({ span.region = "us" } | count() > 1) && ({ name = "b" })`, true},
		{`{ name = "a" } >> { name = "b" }`, true},
		{`{ name = "a" } > { name = "b" }`, true},
		{`{ name = "a" } ~ { name = "b" }`, true},
		{`{ name = "a" } &>> { name = "b" }`, true},
		{`{ name = "a" } >> { name = "b" } | select(span.x)`, true},
		{`({ name = "a" } > { name = "b" }) || { name = "c" }`, true},
		{`{ name = "a" }`, false},
		{`{ rootName = "checkout" }`, false},
		{`{ name = "a" } !>> { name = "b" }`, false},
		{`{ name = "a" } !> { name = "b" }`, false},
		{`({ name = "a" } !>> { name = "b" }) || { name = "c" }`, false},
		{`{ name = "a" } | count() > 1`, false},
		{`{ } | select(nestedSetParent)`, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			plan, _, err := lang.Parse(ctx, tc.query)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got := drainsChosenTraces(plan); got != tc.want {
				t.Errorf("drainsChosenTraces = %v, want %v (plan %T)", got, tc.want, plan)
			}
		})
	}
}

// TestDrainBatches pins how a drain splits its chosen trace ids: in rank
// order, every id once, each batch's literal list within the byte budget at
// the given number of splice sites, and one batch when there is no site.
func TestDrainBatches(t *testing.T) {
	t.Parallel()
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = fmt.Sprintf("%032x", i+1)
	}
	if got := drainBatches(ids, 0); len(got) != 1 || len(got[0]) != len(ids) {
		t.Fatalf("no splice site: %d batches, want one holding every id", len(got))
	}
	for _, sites := range []int{1, 2, 7, 5000} {
		batches := drainBatches(ids, sites)
		var joined []string
		for _, b := range batches {
			if len(b) == 0 {
				t.Fatalf("sites %d: empty batch", sites)
			}
			if len(b) > 1 && estimatedLiteralBytes(b)*sites > traceIDLiteralByteBudget {
				t.Errorf("sites %d: batch of %d ids splices %d bytes, over the %d budget", sites, len(b), estimatedLiteralBytes(b)*sites, traceIDLiteralByteBudget)
			}
			joined = append(joined, b...)
		}
		if !slices.Equal(joined, ids) {
			t.Errorf("sites %d: batches do not concatenate to the ids in rank order", sites)
		}
		if sites == 7 && len(batches) < 2 {
			t.Errorf("sites 7: %d batch for %d bytes of lists, want a split", len(batches), estimatedLiteralBytes(ids)*sites)
		}
	}
}

// TestDrainLiteralSites pins the splice count drainBatches sizes its batches
// by: one per leaf span scan chplan.PushLeafPredicate restricts — per-trace
// subqueries, lookups and structural-join operands included — plus
// traceIDRestrictionSiteCount per structural join without an external-table
// push. It is checked against the restriction a phase B actually applies.
func TestDrainLiteralSites(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	start := time.Unix(1777629600, 0).UTC()
	ctx := traceql_lower.WithSearchWindow(traceql_lower.WithSearchTraceLimit(context.Background(), 20), start, start.Add(time.Hour))
	lang := &traceqlLang{schema: s}
	for _, tc := range []struct {
		query  string
		leaves int
		joins  int
	}{
		{`{ name = "a" } && { name = "b" }`, 2, 0},
		{`{ name = "a" } || { name = "b" } || { name = "c" }`, 3, 0},
		{`{ name = "a" } | select(span.x)`, 1, 0},
		{`({ span.region = "us" } | count() > 1) && ({ name = "b" })`, 3, 0},
		{`{ name = "a" } >> { name = "b" }`, 2, 1},
		{`({ name = "a" } > { name = "b" }) || { name = "c" }`, 3, 1},
	} {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			plan, _, err := lang.Parse(ctx, tc.query)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			marker := &chplan.InList{Left: &chplan.ColumnRef{Name: s.TraceIDColumn}, List: []chplan.Expr{&chplan.LitString{V: "marker"}}}
			if got := countSplices(chplan.PushLeafPredicate(chplan.CloneNode(plan), marker), marker); got != tc.leaves {
				t.Errorf("PushLeafPredicate spliced the restriction %d times, want %d", got, tc.leaves)
			}
			if got, want := drainLiteralSites(plan, true), tc.leaves; got != want {
				t.Errorf("sites with an external-table push = %d, want %d", got, want)
			}
			if got, want := drainLiteralSites(plan, false), tc.leaves+tc.joins*traceIDRestrictionSiteCount; got != want {
				t.Errorf("sites without an external-table push = %d, want %d", got, want)
			}
		})
	}
}
