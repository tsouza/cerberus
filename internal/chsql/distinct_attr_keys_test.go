package chsql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chsql"
)

// TestDistinctAttrKeys pins the two renderings chsql.DistinctAttrKeys
// resolves to — the Map `.keys`-subcolumn shape every pre-#3063 call site
// emitted byte-for-byte, and the JSONAllPaths shape a JSON-strategy column
// takes — and that the strategy lookup is per-column: an entry for a
// different column, or a nil strategies map, leaves the Map shape intact.
func TestDistinctAttrKeys(t *testing.T) {
	t.Parallel()

	const col = "ResourceAttributes"
	const wantMap = "DISTINCT arrayJoin(`ResourceAttributes`.`keys`)"
	const wantJSON = "DISTINCT arrayJoin(JSONAllPaths(`ResourceAttributes`))"

	for _, tc := range []struct {
		name       string
		strategies chsql.AttrStrategies
		want       string
	}{
		{"nil strategies", nil, wantMap},
		{"explicit Map strategy", chsql.AttrStrategies{col: chsql.AttrStrategyMap}, wantMap},
		{"JSON strategy on another column", chsql.AttrStrategies{"SpanAttributes": chsql.AttrStrategyJSON}, wantMap},
		{"JSON strategy on the column", chsql.AttrStrategies{col: chsql.AttrStrategyJSON}, wantJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, args := chsql.Render(chsql.DistinctAttrKeys(tc.strategies, col))
			if got != tc.want {
				t.Errorf("DistinctAttrKeys(%v, %q) = %q, want %q", tc.strategies, col, got, tc.want)
			}
			if len(args) != 0 {
				t.Errorf("DistinctAttrKeys must bind no args (identifiers only), got %v", args)
			}
		})
	}
}
