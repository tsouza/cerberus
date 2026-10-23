package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The canonical series key and its type cast ([CanonicalMap]).
//
// On ClickHouse 24.8–24.12, mapSort (like every Map function that derives
// its result type from the underlying Array(Tuple(key, value))) returns a
// Map whose inner Tuple has no element names, while the Map parsed from
// the name `Map(K, V)` has its Tuple named (keys, values). The two print
// the same name but are unequal types. An aggregation or sort that spills
// writes its blocks under the type name and reads them back as the parsed
// type; the reader finds that type unequal to the in-memory header and has
// no conversion between two Maps, so the read fails with
// `Cannot convert: Map(String, String) to Map(String, String)`
// (TYPE_MISMATCH). Cast to the type parsed from its operand's type name,
// the key already carries the type a spilled block reads back as, so the
// header and the block agree. From 25.1 mapSort returns the parsed type
// itself, and chopt.FeatureCanonicalMapTypeCast leaves the cast off there.
//
// The type comes from the operand rather than a literal, so a
// LowCardinality key type, or any value type, passes through unchanged. The
// operand appears twice rather than being named once by an inline alias:
// the projection a key feeds is routinely aliased back to the operand's own
// column name (`... AS Attributes`), and under enable_analyzer=0 an inline
// alias inside that projection whose expression reads the column resolves
// the column to the projection's alias, which ClickHouse rejects as
// CYCLIC_ALIASES.

// exprCanonicalMap renders chplan.FnMapSort through [CanonicalMap].
// exprFunc dispatches here ahead of the fnResolutions lookup for the same
// package-init-cycle reason exprMapContainsKey is special-cased: a
// fnRender hook in that table that calls b.Expr would make the table's
// initializer depend on itself.
func (b *Builder) exprCanonicalMap(args []chplan.Expr) error {
	if len(args) != 1 {
		return fmt.Errorf("%w: chplan.FnMapSort takes exactly one Map argument, got %d", ErrUnsupported, len(args))
	}
	CanonicalMap(func(b *Builder) { _ = b.Expr(args[0]) })(b)
	return b.err
}
