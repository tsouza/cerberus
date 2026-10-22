package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// CanonicalMap renders the canonical, key-ordered form of a label Map —
// the series-identity key every GROUP BY, ORDER BY, PARTITION BY and
// match key over a whole label set binds to:
//
//	CAST((mapSort(<m>) AS `_typed_<h>`), toTypeName(`_typed_<h>`))
//
// The ordering is [chplan.CanonicalMapFunc]'s; [SelfTypedCast] re-declares
// the result under the type ClickHouse parses from its own name, which is
// the type a spilled block of that column is read back as.
//
// On ClickHouse 24.8–24.12, mapSort (like every Map function that derives
// its result type from the underlying Array(Tuple(key, value))) returns a
// Map whose inner Tuple has no element names, while the Map parsed from
// the name `Map(K, V)` has its Tuple named (keys, values). The two print
// the same name but are unequal types. An aggregation or sort that
// spills writes its blocks under the type name and reads them back as
// the parsed type; the reader finds that type unequal to the in-memory
// header and has no conversion between two Maps, so the read fails with
// `Cannot convert: Map(String, String) to Map(String, String)`
// (TYPE_MISMATCH). With the cast the key already carries the parsed
// type, so the header and the spilled block agree. From 25.1 mapSort
// returns the parsed type itself and the cast is between equal types,
// which ClickHouse executes as the identity.
//
// The type comes from the expression rather than a literal, so a
// LowCardinality key type, or any value type, passes through unchanged
// on every server version.
func CanonicalMap(m Frag) Frag {
	return SelfTypedCast(Call(chplan.CanonicalMapFunc, m))
}

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
