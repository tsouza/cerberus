package chsql

import (
	"fmt"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// traceLookupInputAlias is the bare table alias of a TraceLookupFilter's
// Input relation; traceLookupAliasPrefix plus the lookup's index names each
// lookup relation (`l0`, `l1`, …).
const (
	traceLookupInputAlias  = "m"
	traceLookupAliasPrefix = "l"
)

// emitTraceLookupFilter renders a chplan.TraceLookupFilter:
//
//	SELECT m.* [, l0.<value> AS <value>, …] FROM (<Input>) AS m
//	LEFT JOIN (<Lookups[0]>) AS l0 ON m.<trace id> = l0.<TraceLookupKeyColumn>
//	…
//	[WHERE <Predicate>]
//
// The lookup value columns are selected only when the node exposes them, and
// the WHERE only when it carries a Predicate.
//
// Every lookup is a separate relation built once and probed per Input row, so
// the comparison between a row and its trace's lookup value is an ordinary
// row-level predicate rather than a subquery correlated with the outer row.
// The predicate reads the lookup value columns bare: their names are unique
// across the join by the node's contract, and the key column is named
// TraceLookupKeyColumn rather than the input's trace-id name, so neither side
// makes a bare Input column ambiguous.
func (e *emitter) emitTraceLookupFilter(f *chplan.TraceLookupFilter) error {
	if f.Input == nil || (f.Predicate == nil && !f.ExposeLookups) || len(f.Lookups) == 0 {
		return fmt.Errorf("%w: TraceLookupFilter needs an input, a predicate or exposed lookups, and at least one lookup", ErrUnsupported)
	}
	traceID, err := f.Input.RowType().UniqueNamedRole(chplan.RoleTraceID)
	if err != nil {
		return fmt.Errorf("%w: TraceLookupFilter input requires a trace-id role: %w", ErrUnsupported, err)
	}
	inputSub, err := e.subqueryFrag(f.Input)
	if err != nil {
		return err
	}
	qb := NewQuery().
		Select(QualStar(traceLookupInputAlias)).
		From(aliasedFrag(inputSub, traceLookupInputAlias))
	for i, lookup := range f.Lookups {
		if lookup == nil {
			return fmt.Errorf("%w: TraceLookupFilter lookup %d is nil", ErrUnsupported, i)
		}
		lookupSub, lerr := e.subqueryFrag(lookup)
		if lerr != nil {
			return lerr
		}
		alias := traceLookupAliasPrefix + strconv.Itoa(i)
		qb.Join(LeftJoin, aliasedFrag(lookupSub, alias),
			spanIDPairFrag(traceLookupInputAlias, traceID.Name, alias, chplan.TraceLookupKeyColumn))
		if f.ExposeLookups {
			if err := exposeLookupValues(qb, lookup, alias); err != nil {
				return err
			}
		}
	}
	if f.Predicate != nil {
		qb.Where(func(b *Builder) { _ = b.Expr(f.Predicate) })
	}
	return e.emitSelect(qb)
}

// exposeLookupValues selects every value column of lookup, read through its
// join alias and renamed to its bare name, so the relation above reads it the
// way a predicate inside the node does.
func exposeLookupValues(qb *QueryBuilder, lookup chplan.Node, alias string) error {
	values, ok := chplan.LookupValueColumnsOf(lookup)
	if !ok {
		return fmt.Errorf("%w: TraceLookupFilter exposes the columns of a lookup whose row type is open", ErrUnsupported)
	}
	for _, c := range values {
		qb.SelectAs(Qual(alias, c.Name), c.Name)
	}
	return nil
}
