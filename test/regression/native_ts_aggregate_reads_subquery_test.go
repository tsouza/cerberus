package regression

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/tsouza/cerberus/test/spec"
)

// nativeTSScalarFunctions are the timeSeries* names that are ordinary
// functions, not aggregates: they carry no partial state, so where they sit
// relative to a table is irrelevant here.
var nativeTSScalarFunctions = map[string]bool{
	"timeSeriesRange":       true,
	"timeSeriesFromGrid":    true,
	"timeSeriesTagsToGroup": true,
	"timeSeriesGroupToTags": true,
}

// specSQLSections are the fixture sections that hold emitted SQL.
var specSQLSections = []string{"sql", "sql_optimized"}

// TestNativeTSAggregatesNeverReadATableDirectly pins the property that keeps
// the native timeSeries* aggregates independent of the ClickHouse versions a
// Distributed deployment's shards run.
//
// ClickHouse pushes an aggregation to the shards only when the aggregate sits
// at the same query level as the Distributed table; the shards then ship
// partial states, and the timeSeries*ToGrid state format changes between
// minor releases (format 2 through 26.6, 3 in 26.7, 4 from 26.8), so a
// cross-minor pair refuses the state with INCORRECT_DATA. Cerberus renders
// every scan as its own subquery, so the Distributed table sends rows and the
// whole aggregation runs on the initiator — which
// TestTSGridStateFormatBoundary_Distributed_RealCH proves on real mixed-version
// clusters for representative shapes. This test extends the property to every
// emitted SQL golden: in each SELECT that calls a timeSeries* aggregate, the
// FROM (and any JOIN) source must be a subquery, never a table.
func TestNativeTSAggregatesNeverReadATableDirectly(t *testing.T) {
	checked := 0
	var violations []string
	err := filepath.WalkDir(specCorpusDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".txtar" {
			return nil
		}
		c, err := spec.Load(path)
		if err != nil {
			return err
		}
		for _, section := range specSQLSections {
			sql, ok := c.Section(section)
			if !ok || !strings.Contains(sql, "timeSeries") {
				continue
			}
			checked++
			for _, v := range nativeTSAggregatesOverTables(sql) {
				violations = append(violations, path+" ("+section+"): "+v)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", specCorpusDir, err)
	}
	if checked == 0 {
		t.Fatal("no emitted SQL golden calls a timeSeries* function — the scan is looking in the wrong place " +
			"and would pass vacuously")
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}

// TestNativeTSAggregatesOverTablesDetector pins the detector itself, so the
// corpus test above cannot pass because the detector stopped seeing anything.
func TestNativeTSAggregatesOverTablesDetector(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want int
	}{
		{
			"aggregate directly over a table",
			"SELECT a, timeSeriesRateToGrid(1, 2, 3, 4)(ts, v) FROM `otel_metrics_sum` GROUP BY a",
			1,
		},
		{
			"aggregate over a subquery",
			"SELECT a, timeSeriesRateToGrid(1, 2, 3, 4)(ts, v) FROM (SELECT * FROM `otel_metrics_sum` WHERE x) GROUP BY a",
			0,
		},
		{
			"merge over a subquery whose state level reads a table",
			"SELECT timeSeriesRateToGridMerge(1, 2, 3, 4)(s) FROM (SELECT timeSeriesRateToGridState(1, 2, 3, 4)(ts, v) AS s " +
				"FROM otel_metrics_sum GROUP BY a)",
			1,
		},
		{
			"scalar timeSeriesRange over a table is not an aggregate",
			"SELECT timeSeriesRange(1, 2, 3) FROM `otel_metrics_sum`",
			0,
		},
		{
			"aggregate joined to a table",
			"SELECT timeSeriesGroupArray(ts, v) FROM (SELECT 1) AS l JOIN otel_metrics_gauge AS r ON 1",
			1,
		},
		{
			"a later UNION arm reading a table does not taint the aggregate arm",
			"SELECT timeSeriesGroupArray(ts, v) FROM (SELECT 1) UNION ALL SELECT x FROM otel_metrics_sum",
			0,
		},
		{
			"parentheses and keywords inside string literals are ignored",
			"SELECT timeSeriesGroupArray(ts, v) FROM (SELECT ' FROM otel_metrics_sum (' AS s)",
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeTSAggregatesOverTables(tc.sql); len(got) != tc.want {
				t.Fatalf("nativeTSAggregatesOverTables = %q, want %d violation(s)", got, tc.want)
			}
		})
	}
}

// sqlSelectState is what one SELECT has shown so far: the first timeSeries*
// aggregate it calls and the first table it reads directly.
type sqlSelectState struct {
	aggregate string
	table     string
}

func (s *sqlSelectState) violation() string {
	if s.aggregate == "" || s.table == "" {
		return ""
	}
	return s.aggregate + " reads table " + s.table + " directly"
}

// nativeTSAggregatesOverTables returns one description per SELECT that calls a
// timeSeries* aggregate while reading a table (not a subquery) through FROM or
// JOIN. It tracks one SELECT per parenthesis level, which is how the emitter
// nests queries: every subquery is parenthesised, and a SELECT keyword starts a
// fresh select at its level (a UNION arm).
func nativeTSAggregatesOverTables(sql string) []string {
	var out []string
	stack := []*sqlSelectState{{}}
	flush := func(s *sqlSelectState) {
		if v := s.violation(); v != "" {
			out = append(out, v)
		}
		*s = sqlSelectState{}
	}
	tokens := sqlTokens(sql)
	for i, tok := range tokens {
		top := stack[len(stack)-1]
		switch {
		case tok == "(":
			stack = append(stack, &sqlSelectState{})
		case tok == ")":
			if len(stack) > 1 {
				flush(top)
				stack = stack[:len(stack)-1]
			}
		case strings.EqualFold(tok, "SELECT"):
			flush(top)
		case strings.EqualFold(tok, "FROM") || strings.EqualFold(tok, "JOIN"):
			if i+1 < len(tokens) && tokens[i+1] != "(" && (i+2 >= len(tokens) || tokens[i+2] != "(") {
				if top.table == "" {
					top.table = tokens[i+1]
				}
			}
		case strings.HasPrefix(tok, "timeSeries") && i+1 < len(tokens) && tokens[i+1] == "(":
			if !nativeTSScalarFunctions[tok] && top.aggregate == "" {
				top.aggregate = tok
			}
		}
	}
	for len(stack) > 0 {
		flush(stack[len(stack)-1])
		stack = stack[:len(stack)-1]
	}
	return out
}

// sqlTokens splits sql into parentheses and words (identifiers, keywords,
// backtick-quoted names with the backticks stripped). String literals and
// every other character are dropped, so nothing inside a literal can open a
// scope or look like a keyword.
func sqlTokens(sql string) []string {
	var tokens []string
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\'':
			for i++; i < len(runes); i++ {
				if runes[i] == '\\' {
					i++
					continue
				}
				if runes[i] == '\'' {
					if i+1 < len(runes) && runes[i+1] == '\'' {
						i++
						continue
					}
					break
				}
			}
		case r == '`':
			j := i + 1
			for j < len(runes) && runes[j] != '`' {
				j++
			}
			tokens = append(tokens, string(runes[i+1:min(j, len(runes))]))
			i = j
		case r == '(' || r == ')':
			tokens = append(tokens, string(r))
		case unicode.IsLetter(r) || r == '_':
			j := i
			for j < len(runes) && (unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) || runes[j] == '_' || runes[j] == '.') {
				j++
			}
			tokens = append(tokens, string(runes[i:j]))
			i = j - 1
		}
	}
	return tokens
}
