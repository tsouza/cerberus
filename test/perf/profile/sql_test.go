//go:build chdb

package profile

import (
	"reflect"
	"testing"
)

func TestPlanOperatorDetection(t *testing.T) {
	tests := []struct {
		name  string
		plan  string
		cross bool
		array bool
		rcte  bool
	}{
		{
			name:  "cross join",
			plan:  "  Join (JOIN FillRightFirst)\n  Type: CROSS\n  Strictness: UNSPECIFIED",
			cross: true,
		},
		{
			name: "inner join is not cross",
			plan: "  Join (JOIN FillRightFirst)\n  Type: INNER\n",
		},
		{
			name:  "array join",
			plan:  "  ArrayJoin (ARRAY JOIN)\n  ARRAY JOIN __array_join_exp_1\n",
			array: true,
		},
		{
			name: "recursive cte",
			plan: `  ReadFromRecursiveCTEStep ((WITH RECURSIVE cte AS (...)) AS __table1)`,
			rcte: true,
		},
		{
			name: "plain scan, no fan-out operators",
			plan: "Expression\n  ReadFromMergeTree (default.t)\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := planHasCrossJoin(tc.plan); got != tc.cross {
				t.Errorf("planHasCrossJoin = %v, want %v", got, tc.cross)
			}
			if got := planHasArrayJoin(tc.plan); got != tc.array {
				t.Errorf("planHasArrayJoin = %v, want %v", got, tc.array)
			}
			if got := planHasRecursiveCTE(tc.plan); got != tc.rcte {
				t.Errorf("planHasRecursiveCTE = %v, want %v", got, tc.rcte)
			}
		})
	}
}

func TestFromSourceLevels(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "bare scan, no nesting",
			query: "SELECT a FROM t WHERE a > 1",
			want:  []string{"SELECT a FROM t WHERE a > 1"},
		},
		{
			name:  "one nested level",
			query: "SELECT x FROM (SELECT a AS x FROM t) WHERE x > 1",
			want: []string{
				"SELECT x FROM (SELECT a AS x FROM t) WHERE x > 1",
				"SELECT a AS x FROM t",
			},
		},
		{
			name:  "two nested levels descend leftmost",
			query: "SELECT z FROM (SELECT y AS z FROM (SELECT a AS y FROM t))",
			want: []string{
				"SELECT z FROM (SELECT y AS z FROM (SELECT a AS y FROM t))",
				"SELECT y AS z FROM (SELECT a AS y FROM t)",
				"SELECT a AS y FROM t",
			},
		},
		{
			name:  "WITH-prefixed kept at depth 0 only",
			query: "WITH c AS (SELECT 1) SELECT * FROM c",
			want:  []string{"WITH c AS (SELECT 1) SELECT * FROM c"},
		},
		{
			name:  "FROM source is a table function, not a subquery",
			query: "SELECT a FROM merge(currentDatabase(), '^t$') WHERE a > 1",
			want:  []string{"SELECT a FROM merge(currentDatabase(), '^t$') WHERE a > 1"},
		},
		{
			name:  "stops descending at a CTE-prefixed inner level",
			query: "SELECT x FROM (WITH c AS (SELECT 1) SELECT n AS x FROM c)",
			want:  []string{"SELECT x FROM (WITH c AS (SELECT 1) SELECT n AS x FROM c)"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fromSourceLevels(tc.query)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fromSourceLevels mismatch\n got = %#v\nwant = %#v", got, tc.want)
			}
		})
	}
}

func TestLeftmostFromSubquery(t *testing.T) {
	tests := []struct {
		query  string
		want   string
		wantOK bool
	}{
		{"SELECT a FROM (SELECT b FROM t)", "SELECT b FROM t", true},
		{"SELECT a FROM t", "", false},
		{"SELECT a FROM merge(db, 'x')", "", false},
		// IN-list parens after FROM are not a subquery source.
		{"SELECT a FROM ('x','y')", "", false},
		// FROM inside a single-quoted literal is shielded.
		{"SELECT 'a FROM b' AS s FROM (SELECT 1)", "SELECT 1", true},
	}
	for _, tc := range tests {
		got, ok := leftmostFromSubquery(tc.query)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("leftmostFromSubquery(%q) = (%q, %v), want (%q, %v)",
				tc.query, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestInlineArgs(t *testing.T) {
	tests := []struct {
		query string
		args  []any
		want  string
	}{
		{"SELECT * FROM t WHERE a = ?", []any{int64(5)}, "SELECT * FROM t WHERE a = 5"},
		{"WHERE name = ?", []any{"up"}, "WHERE name = 'up'"},
		{"WHERE x = ? AND y = ?", []any{1.5, int64(2)}, "WHERE x = 1.5 AND y = 2"},
		// `?` inside a string literal is not a placeholder.
		{"WHERE s = 'a?b' AND c = ?", []any{int64(7)}, "WHERE s = 'a?b' AND c = 7"},
		// String arg with embedded quote is escaped.
		{"WHERE s = ?", []any{"a'b"}, `WHERE s = 'a\'b'`},
		// No args, no change.
		{"SELECT 1", nil, "SELECT 1"},
	}
	for _, tc := range tests {
		if got := inlineArgs(tc.query, tc.args); got != tc.want {
			t.Errorf("inlineArgs(%q, %#v) = %q, want %q", tc.query, tc.args, got, tc.want)
		}
	}
}

func TestParseSingleCount(t *testing.T) {
	body := `{"meta":[{"name":"count()","type":"UInt64"}],"data":[{"count()":42}],"rows":1}`
	n, err := parseSingleCount(body)
	if err != nil {
		t.Fatalf("parseSingleCount: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}

	// Empty data → 0, no error.
	n, err = parseSingleCount(`{"data":[],"rows":0}`)
	if err != nil || n != 0 {
		t.Errorf("empty data: got (%d, %v), want (0, nil)", n, err)
	}
}
