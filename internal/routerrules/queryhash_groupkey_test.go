package routerrules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// normalized_query_hash is a UInt64 and a real hash is uniform over that whole
// range, but a float64 holds an integer exactly only below 2^53. These are the
// three measured collapse cases from issue #3189: the first value above the
// float64 integer boundary, and an adjacent pair above 2^63 that both render
// as "1.7e+19".
const (
	hashAtFloat64Boundary = uint64(9007199254740992)     // 2^53, still exact
	hashPastFloat64Bound  = uint64(9007199254740993)     // 2^53+1, rounds down to 2^53
	hashAbove2p63A        = uint64(17000000000000000001) // both render "1.7e+19"
	hashAbove2p63B        = uint64(17000000000000000002)
)

// groupKeysByHash evaluates a match-everything rule grouped by
// normalized_query_hash and returns the group keys, sorted.
func groupKeysByHash(t *testing.T, src CorpusSource) []string {
	t.Helper()
	groups, err := src.EvalRule(context.Background(), RuleQuery{
		Condition: &EnumCmp{Column: "language", Op: OpEq, Values: []string{"promql"}},
		GroupBy:   []string{"normalized_query_hash"},
		Env:       Env{},
	})
	if err != nil {
		t.Fatalf("eval rule: %v", err)
	}
	keys := make([]string, 0, len(groups))
	for _, g := range groups {
		if len(g.GroupKey) != 1 {
			t.Fatalf("group key = %v, want exactly one column", g.GroupKey)
		}
		keys = append(keys, g.GroupKey[0])
	}
	sort.Strings(keys)
	return keys
}

// wantHashKeys is the exact decimal rendering of each hash — the same string
// the ClickHouse backend produces, which groups by
// toString(normalized_query_hash).
func wantHashKeys(hashes ...uint64) []string {
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, strconv.FormatUint(h, 10))
	}
	sort.Strings(out)
	return out
}

// TestQueryHashGroupKeysAreExactAboveFloat64Range pins that both in-Go corpus
// backends key a class on the EXACT UInt64 hash, over the whole domain of the
// column rather than the part of it a float64 happens to survive.
//
// Rendering the column through float64 — which both backends used to do —
// silently rounds every value at or above 2^53, so two distinct hot shapes
// collapse into a single class. The ClickHouse backend groups by
// toString(normalized_query_hash) and never had the defect, so the two backends
// produced DIFFERENT findings from the same corpus while docs/router-rules.md
// claimed they produce identical ones.
//
// Every fixture in this package used to sit below 2^53, which is exactly why
// the harness could not see it: the two renderings agree everywhere the corpus
// was ever tested. These values are above it, in both directions that break —
// past the float64 integer boundary, and past 2^63 where the int64 fast path in
// a generic numeric formatter overflows and falls through to "1.7e+19".
func TestQueryHashGroupKeysAreExactAboveFloat64Range(t *testing.T) {
	t.Parallel()

	hashes := []uint64{hashAtFloat64Boundary, hashPastFloat64Bound, hashAbove2p63A, hashAbove2p63B}
	want := wantHashKeys(hashes...)

	t.Run("jsonl", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "corpus.jsonl")
		var buf []byte
		for _, h := range hashes {
			buf = append(buf, fmt.Sprintf(
				`{"event_time":1,"shape_id":"prom:s","language":"promql","route":"A",`+
					`"exit_status":"ok","normalized_query_hash":%d,"memory_usage":1}`+"\n", h,
			)...)
		}
		if err := os.WriteFile(path, buf, 0o600); err != nil {
			t.Fatalf("write corpus: %v", err)
		}
		assertHashKeys(t, groupKeysByHash(t, NewJSONLCorpusSource(path, 0)), want)
	})

	t.Run("in-memory", func(t *testing.T) {
		t.Parallel()
		rows := make([]BenchRow, 0, len(hashes))
		for _, h := range hashes {
			rows = append(rows, BenchRow{
				ShapeID: "prom:s", Language: "promql", Route: "A", ExitStatus: "ok",
				NormalizedQueryHash: h, MemoryUsage: 1,
			})
		}
		assertHashKeys(t, groupKeysByHash(t, (&BenchCorpus{Rows: rows}).AsCorpusSource()), want)
	})
}

func assertHashKeys(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d classes %v, want %d %v; distinct hashes collapsed into one class, so two "+
			"hot shapes are reported as one", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group key %d = %q, want %q; the key must be the exact decimal the ClickHouse "+
				"backend's toString(normalized_query_hash) produces", i, got[i], want[i])
		}
	}
}

// TestFormatQueryHashIsExact pins the renderer itself at the boundary, so a
// failure names the cause directly rather than through a collapsed class count.
func TestFormatQueryHashIsExact(t *testing.T) {
	t.Parallel()

	for _, h := range []uint64{0, 1, hashAtFloat64Boundary, hashPastFloat64Bound, hashAbove2p63A, hashAbove2p63B, ^uint64(0)} {
		if got, want := formatQueryHash(h), strconv.FormatUint(h, 10); got != want {
			t.Errorf("formatQueryHash(%d) = %q, want %q", h, got, want)
		}
	}
}
