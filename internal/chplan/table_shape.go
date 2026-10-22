package chplan

import "slices"

// TableShape captures the ClickHouse-side facts the codegen needs to
// produce sort-key-aware filter orderings and PREWHERE promotion.
//
// SortColumns lists the columns the table's ORDER BY sorts by, in key
// order; the codegen prefers emitting predicates whose LHS references
// columns that appear earlier in this list. WideColumns names the "fat"
// columns (large strings, Maps, Arrays) that PREWHERE wants to avoid
// reading until predicates have culled the row set.
//
// IntegerDiscriminatorColumns names narrow enum-like columns whose integer
// equality predicates should stay in PREWHERE even when no WHERE predicate
// remains. SkipIndexColumns names columns covered by registered Bloom-filter
// or set indexes. The default tables expose none — the field is plumbed so an
// override can populate it without a schema migration.
//
// Declared here rather than in internal/chsql because SchemaStrategies, which
// carries one per deployed table, is.
type TableShape struct {
	SortColumns                 []string
	WideColumns                 []string
	IntegerDiscriminatorColumns []string
	SkipIndexColumns            []string
}

// IsSortColumn reports whether name is listed in SortColumns.
func (s TableShape) IsSortColumn(name string) bool {
	return slices.Contains(s.SortColumns, name)
}

// SortRank returns the position of name in SortColumns, or -1 when
// the column is not part of the sort key. Lower rank = earlier in the
// ORDER BY, i.e. more selective for granule pruning.
func (s TableShape) SortRank(name string) int {
	return slices.Index(s.SortColumns, name)
}

// IsWideColumn reports whether name is listed in WideColumns.
func (s TableShape) IsWideColumn(name string) bool {
	return slices.Contains(s.WideColumns, name)
}

// IsIntegerDiscriminatorColumn reports whether name is an explicitly
// registered narrow enum-like integer discriminator.
func (s TableShape) IsIntegerDiscriminatorColumn(name string) bool {
	return slices.Contains(s.IntegerDiscriminatorColumns, name)
}

// IsSkipIndexColumn reports whether name has a registered skip index.
// Always false today (no defaults populate SkipIndexColumns) — kept as
// part of the public surface so future schema overrides can plug in.
func (s TableShape) IsSkipIndexColumn(name string) bool {
	return slices.Contains(s.SkipIndexColumns, name)
}
