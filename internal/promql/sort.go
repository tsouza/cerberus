package promql

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// naturalSortKeyPadWidth is the fixed width each digit run is left-padded
// to inside the sort_by_label natural-sort key. It must exceed the digit
// count of the largest int64 (19) so every value natsort compares
// numerically (`strconv.Atoi` within int64 range) is ordered numerically
// by the padded key; see [naturalSortKeyExpr].
const naturalSortKeyPadWidth = 20

// lowerSort implements PromQL `sort(v)` / `sort_desc(v)`: return the
// input instant vector sorted by SAMPLE VALUE — ascending for `sort`,
// descending for `sort_desc`. Both take a single instant-vector
// argument; Prom rejects a range-vector argument at type-check time, so
// the inner expression is always instant-shaped here.
//
// PromQL semantics (prometheus/promql/functions.go::funcSort /
// funcSortDesc): a stable sort on the sample value alone — labels are
// preserved, only row order changes. We lower to an OrderBy on the
// Value column over the inner vector. NaN ordering follows CH's default
// (NaN sorts last in ASC, first in DESC); Prom places NaN last in both
// directions, but the wire renderer keys series by label set rather
// than positional order, so the residual NaN-ordering difference is not
// observable through cerberus's vector/matrix response shapes.
//
// `sort`/`sort_desc` preserve `__name__` — they don't derive a new
// sample, they reorder existing ones — so no MetricName rewrite applies
// (unlike the instant-math fns); the inner plan's columns flow through
// the OrderBy unchanged.
func lowerSort(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects 1 argument, got %d", c.Func.Name, len(c.Args))
	}
	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	desc := c.Func.Name == "sort_desc"
	return &chplan.OrderBy{
		Input: inner,
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Desc: desc},
		},
	}, nil
}

// lowerSortByLabel implements PromQL `sort_by_label(v, label, …)` /
// `sort_by_label_desc(v, label, …)`: return the input instant vector
// sorted by the VALUE of the named label(s) — ascending for
// `sort_by_label`, descending for `sort_by_label_desc`. The first arg
// is the instant vector; every subsequent arg is a string-literal label
// name. Later labels act as tie-breakers, so they lower to additional
// ORDER BY slots in the same direction.
//
// PromQL semantics (prometheus/promql/functions.go::funcSortByLabel /
// funcSortByLabelDesc): a NATURAL-ORDER sort on the named label values,
// NOT lexicographic. Reference Prometheus compares label values with
// `github.com/facette/natsort` (natsort.Compare) — it splits each value
// into alternating digit / non-digit runs and compares digit runs
// NUMERICALLY, so `v1 < v2 < v10` (byte order would give `v1 < v10 < v2`).
// We reproduce that ordering with a derived sort KEY: see
// [naturalSortKeyExpr]. The series set and the sample values are
// unchanged; only row order changes, so — exactly like `sort`/`sort_desc`
// — `__name__` is preserved and no MetricName rewrite applies. The inner
// plan's columns flow through the OrderBy untouched; we only add sort keys
// over the natural-sort key of each label-value expression.
//
// A label absent on a row resolves to the empty string (CH `Map`
// default / Prom's "absent label → empty value" rule). natsort's own
// handling of the empty string is degenerate (its Compare loop never
// runs for an empty operand, so `Compare("", x)` and `Compare(x, "")`
// are both false — empty is neither-less-than every value), which makes
// reference Prometheus's empty-vs-present ordering itself undefined; our
// key sorts empty first in ASC, the only transitive choice.
func lowerSortByLabel(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < 2 {
		return nil, fmt.Errorf("promql: %s expects at least 2 arguments (vector, label[, label…]), got %d", c.Func.Name, len(c.Args))
	}
	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	desc := c.Func.Name == "sort_by_label_desc"
	keys := make([]chplan.OrderKey, 0, len(c.Args)-1)
	for i := 1; i < len(c.Args); i++ {
		name, err := stringArg(c.Args[i], c.Func.Name, fmt.Sprintf("label_%d", i))
		if err != nil {
			return nil, err
		}
		keys = append(keys, chplan.OrderKey{Expr: naturalSortKeyExpr(labelValueExpr(name, s)), Desc: desc})
	}
	return &chplan.OrderBy{Input: inner, Keys: keys}, nil
}

// naturalSortKeyExpr wraps a string-valued expression in the derived
// sort KEY whose ClickHouse default (byte-order) collation reproduces
// reference Prometheus's `natsort.Compare` ordering. The emitted shape is
//
//	arrayStringConcat(
//	  arrayMap(c -> if(match(c, '^[0-9]+$'),
//	                   leftPad(c, 20, '0'),
//	                   c),
//	           extractAll(<value>, '[0-9]+|[^0-9]+')))
//
// `extractAll` splits the value into the same alternating digit /
// non-digit chunks natsort's `(\d+|\D+)` regex produces. Each pure-digit
// chunk is left-padded with '0' to a fixed width so that bytewise
// comparison of the padded chunk orders it numerically (and leading-zero
// variants like `01`/`1` collapse to the same padded form — matching
// natsort, which compares digit runs via `strconv.Atoi`). Non-digit
// chunks pass through verbatim, so they keep their byte-order comparison
// exactly where natsort applies its string-compare fallback. Concatenating
// the transformed chunks yields one key string whose byte order equals
// natsort's chunk-aligned order.
//
// Width is naturalSortKeyPadWidth (20). natsort compares digit runs
// numerically only while `strconv.Atoi` succeeds — i.e. within int64
// range (≤19 digits); beyond that natsort falls back to string compare.
// 20 > 19 covers every int64-range run faithfully; a digit run of 20+
// consecutive digits (outside any realistic OTel label value) is the
// sole residual divergence from natsort.
func naturalSortKeyExpr(value chplan.Expr) chplan.Expr {
	const chunkClass = "[0-9]+|[^0-9]+"
	const digitRunAnchor = "^[0-9]+$"
	chunks := &chplan.FuncCall{
		Name: "extractAll",
		Args: []chplan.Expr{value, &chplan.LitString{V: chunkClass}},
	}
	padDigitRun := &chplan.Lambda{
		Params: []string{"c"},
		Body: &chplan.FuncCall{
			Name: "if",
			Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "match",
					Args: []chplan.Expr{&chplan.BareIdent{Name: "c"}, &chplan.LitString{V: digitRunAnchor}},
				},
				&chplan.FuncCall{
					Name: "leftPad",
					Args: []chplan.Expr{
						&chplan.BareIdent{Name: "c"},
						&chplan.LitInt{V: naturalSortKeyPadWidth},
						&chplan.LitString{V: "0"},
					},
				},
				&chplan.BareIdent{Name: "c"},
			},
		},
	}
	return &chplan.FuncCall{
		Name: "arrayStringConcat",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "arrayMap", Args: []chplan.Expr{padDigitRun, chunks}},
		},
	}
}

// labelValueExpr resolves a Prom label NAME to the chplan expression
// that yields that label's VALUE for a row — the same resolution
// [matcherToExpr] applies to a matcher's left-hand side, minus the
// comparison. `__name__` reads the dedicated MetricName column; a label
// backed by a top-level OTel-CH column (e.g. `service_name`) coalesces
// the column with its Attributes-map fallback; everything else is an
// [attributeLookup] on the Attributes map (with the dotted-candidate
// if-chain for underscored names). Used by [lowerSortByLabel] to build
// ORDER BY keys over label values.
func labelValueExpr(name string, s schema.Metrics) chplan.Expr {
	if name == model.MetricNameLabel {
		return &chplan.ColumnRef{Name: s.MetricNameColumn}
	}
	mapLookup := attributeLookup(s.AttributesColumn, name)
	if col := schemaTopLevelColumn(s, name); col != "" {
		return &chplan.FuncCall{
			Name: "coalesce",
			Args: []chplan.Expr{
				&chplan.FuncCall{
					Name: "nullIf",
					Args: []chplan.Expr{
						&chplan.ColumnRef{Name: col},
						&chplan.LitString{V: ""},
					},
				},
				mapLookup,
			},
		}
	}
	return mapLookup
}
