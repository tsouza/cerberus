package preflight

import (
	"context"
	"fmt"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// distributedEngine is the system.tables engine of a Distributed table, which
// stores no rows itself and so reports no sorting key of its own.
const distributedEngine = "Distributed"

// tableSortingKeySQL reads one table's engine and sorting key from
// system.tables; a table that does not exist lists none.
func tableSortingKeySQL(database, table string) (string, []any) {
	return chsql.NewQuery().
		Select(chsql.Col("engine"), chsql.Col("sorting_key")).
		From(chsql.Qual("system", "tables")).
		Where(
			chsql.Eq(chsql.Col("database"), chsql.Lit(database)),
			chsql.Eq(chsql.Col("name"), chsql.Lit(table)),
		).
		Build()
}

// sortingKeyFinding is what probeSortingKey read for one table: whether it
// knows the table's key (known), the leading bare-column run of that key
// (prefix), the columns the key sorts by (columns: nil unless the key itself
// was read), and the warning a failed read raises.
type sortingKeyFinding struct {
	known   bool
	prefix  []string
	columns []string
	warning string
}

// probeSortingKey reads table's deployed sorting key and reduces it to its
// bare-column prefix (schema.SortingKeyPrefixOf), which the
// optimize_aggregation_in_order eligibility check in internal/engine matches
// a GROUP BY against, and to the columns it sorts by
// (schema.SortingKeyColumnsOf), which the emitter's PREWHERE promotion ranks
// a scan's filters by (chsql.TableShape).
//
//   - A transport failure is returned as unreachable, exactly as checkTable
//     reports one.
//   - Any other failure, or a table system.tables does not list, is known
//     with an empty prefix and a warning: no GROUP BY is a prefix of a key
//     cerberus could not read, so the setting is not stamped on that table.
//     Its columns stay nil: the PREWHERE ranking, which never changes a
//     result, keeps the key cerberus provisions rather than none.
//   - A Distributed table is not known: its rows, and the sorting key that
//     orders them, live in the local tables it reads, so its statements keep
//     the key cerberus provisions those with.
func probeSortingKey(ctx context.Context, q Querier, database, table string) (f sortingKeyFinding, unreachable error) {
	sql, args := tableSortingKeySQL(database, table)
	rows, err := q.QueryNameTypePairs(ctx, sql, args...)
	if err != nil {
		if isUnreachable(err) {
			return sortingKeyFinding{}, err
		}
		return sortingKeyFinding{known: true, prefix: []string{}, warning: fmt.Sprintf(
			"could not read the sorting key of table %s: %v — optimize_aggregation_in_order is not "+
				"stamped on its queries", table, err,
		)}, nil
	}
	if len(rows) == 0 {
		return sortingKeyFinding{known: true, prefix: []string{}, warning: fmt.Sprintf(
			"system.tables does not list table %s, so its sorting key is unknown — "+
				"optimize_aggregation_in_order is not stamped on its queries", table,
		)}, nil
	}
	engine, sortingKey := rows[0].Name, rows[0].Type
	if engine == distributedEngine {
		return sortingKeyFinding{}, nil
	}
	return sortingKeyFinding{
		known:   true,
		prefix:  schema.SortingKeyPrefixOf(sortingKey),
		columns: schema.SortingKeyColumnsOf(sortingKey),
	}, nil
}
