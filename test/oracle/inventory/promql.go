// Package inventory derives the per-QL feature inventories that the
// showcase dashboards (test/e2e/grafana/compose/dashboards/showcase-*.json)
// must cover, and the AST-level matcher the coverage meta-test uses to
// decide whether a dashboard panel target exercises a given feature.
//
// The PromQL inventory is generated mechanically where the pinned
// upstream parser exports a table (parser.Functions) and hand-curated
// with parser-pinned existence checks where it doesn't (aggregation
// ops, binary operators, vector-matching keywords, selector features —
// none of which the parser exports as an enumerable table). Every row,
// mechanical or curated, carries a pin expression that MUST parse with
// the pinned parser AND must be matched back to the row's ID by
// [CollectPromQLFeatureIDs]; a generation that violates either
// invariant fails loudly instead of emitting an inventory the coverage
// meta-test can't enforce.
//
// The checked-in artifacts live under test/e2e/grafana/ql-inventory/:
//
//	promql-feature-inventory.json  — generated; verified by
//	                                 TestPromQLInventoryIsRegenerable.
//	promql-feature-exclusions.json — hand-written; rows that are
//	                                 genuinely inapplicable, each with a
//	                                 rationale. Shrink-only: an exclusion
//	                                 whose row a dashboard now covers is
//	                                 stale and fails the meta-test.
package inventory

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// Row is one coverable feature of a query language.
type Row struct {
	// ID is the stable identifier panels are matched against, e.g.
	// "fn:rate", "agg:topk", "op:and", "match:group_left",
	// "feature:subquery".
	ID string `json:"id"`
	// Class groups rows for dashboard-row organisation and reporting.
	Class string `json:"class"`
	// Token is the user-facing spelling of the feature.
	Token string `json:"token"`
	// Pin is a representative expression that exercises the feature.
	// It is parse-verified at generation time and doubles as
	// documentation of what "covering this row" means.
	Pin string `json:"pin"`
	// Experimental mirrors parser.Functions[...].Experimental for
	// function rows (informational; cerberus's heads parse with
	// EnableExperimentalFunctions=true).
	Experimental bool `json:"experimental,omitempty"`
}

// Inventory is the checked-in JSON artifact shape.
type Inventory struct {
	QL     string `json:"ql"`
	Source string `json:"source"`
	Rows   []Row  `json:"rows"`
}

// Exclusion is one documented-inapplicable inventory row.
type Exclusion struct {
	ID        string `json:"id"`
	Rationale string `json:"rationale"`
}

// Exclusions is the checked-in exclusions artifact shape.
type Exclusions struct {
	QL         string      `json:"ql"`
	Exclusions []Exclusion `json:"exclusions"`
}

// promQLSource documents where the mechanical half of the inventory
// comes from. The version is pinned by go.mod's replace directive; the
// generator does not embed it so a parser bump only changes the JSON
// when the feature surface actually changed.
const promQLSource = "github.com/prometheus/prometheus/promql/parser " +
	"(parser.Functions + parser-pinned existence checks; tsouza fork pin in go.mod)"

// newPromQLParser builds the same parser configuration cerberus's
// Prometheus head uses (EnableExperimentalFunctions=true — see
// internal/api/prom/lang.go), so the inventory enumerates exactly the
// surface cerberus exposes on the wire.
func newPromQLParser() parser.Parser {
	return parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
}

// GeneratePromQL builds the PromQL feature inventory from the pinned
// parser. It returns an error (rather than panicking or silently
// dropping rows) when any pin expression fails to parse or fails to
// round-trip through CollectPromQLFeatureIDs — both indicate the
// pinned parser drifted out from under the inventory's assumptions.
func GeneratePromQL() (*Inventory, error) {
	p := newPromQLParser()
	rows := make([]Row, 0, 128)

	// --- Mechanical half: every function the pinned parser exports. ---
	for name, fn := range parser.Functions {
		pin, err := pinExprForFunction(p, name, fn)
		if err != nil {
			return nil, err
		}
		rows = append(rows, Row{
			ID:           "fn:" + name,
			Class:        functionClass(name),
			Token:        name,
			Pin:          pin,
			Experimental: fn.Experimental,
		})
	}

	// --- Curated half: constructs the parser has no exported table for. ---
	rows = append(rows, curatedPromQLRows()...)

	// Invariants: every pin parses, and the matcher attributes the pin
	// back to its own row ID.
	for _, r := range rows {
		expr, err := p.ParseExpr(r.Pin)
		if err != nil {
			return nil, fmt.Errorf("inventory row %s: pin %q does not parse: %w", r.ID, r.Pin, err)
		}
		got := CollectPromQLFeatureIDs(expr)
		if !got[r.ID] {
			return nil, fmt.Errorf(
				"inventory row %s: pin %q is not matched back to its own ID (matcher saw %v)",
				r.ID, r.Pin, sortedKeys(got),
			)
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return &Inventory{QL: "promql", Source: promQLSource, Rows: rows}, nil
}

// functionClass buckets a parser function for reporting. The
// native-histogram family gets its own class per the program's feature
// taxonomy; everything else is a plain "function".
func functionClass(name string) string {
	if strings.HasPrefix(name, "histogram_") {
		return "function-histogram"
	}
	return "function"
}

// pinExprForFunction synthesises a representative call for a parser
// function from its declared ArgTypes. `start` and `end` are special:
// they are only grammatical inside an @ modifier. Variadic functions
// are tried at the full declared arity first, then with trailing
// optional arguments dropped, because some (e.g. `info`) constrain the
// SHAPE of their optional argument beyond its declared value type.
func pinExprForFunction(p parser.Parser, name string, fn *parser.Function) (string, error) {
	switch name {
	case "start":
		return "up @ start()", nil
	case "end":
		return "up @ end()", nil
	}
	args := make([]string, 0, len(fn.ArgTypes))
	for _, at := range fn.ArgTypes {
		switch at {
		case parser.ValueTypeScalar:
			args = append(args, "0.5")
		case parser.ValueTypeVector:
			args = append(args, "up")
		case parser.ValueTypeMatrix:
			args = append(args, "up[5m]")
		case parser.ValueTypeString:
			args = append(args, `"x"`)
		default:
			return "", fmt.Errorf("function %s: unhandled arg type %q", name, at)
		}
	}
	minArgs := len(args)
	if fn.Variadic != 0 && minArgs > 0 {
		minArgs--
	}
	var lastErr error
	for n := len(args); n >= minArgs; n-- {
		pin := fmt.Sprintf("%s(%s)", name, strings.Join(args[:n], ", "))
		_, err := p.ParseExpr(pin)
		if err == nil {
			return pin, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("function %s: no synthesised pin parses (last attempt error: %w)", name, lastErr)
}

// curatedPromQLRows returns the hand-curated rows. Each row's Pin is
// the parser-pinned existence check: GeneratePromQL fails if the
// pinned parser stops accepting it.
func curatedPromQLRows() []Row {
	mk := func(id, class, token, pin string) Row {
		return Row{ID: id, Class: class, Token: token, Pin: pin}
	}
	rows := []Row{}

	// Aggregation operators (incl. the experimental limitk /
	// limit_ratio, which parse because cerberus's heads enable
	// experimental functions).
	for _, a := range []struct{ name, pin string }{
		{"sum", "sum(up)"},
		{"avg", "avg(up)"},
		{"min", "min(up)"},
		{"max", "max(up)"},
		{"count", "count(up)"},
		{"group", "group(up)"},
		{"stddev", "stddev(up)"},
		{"stdvar", "stdvar(up)"},
		{"topk", "topk(2, up)"},
		{"bottomk", "bottomk(2, up)"},
		{"quantile", "quantile(0.9, up)"},
		{"count_values", `count_values("v", up)`},
		{"limitk", "limitk(2, up)"},
		{"limit_ratio", "limit_ratio(0.5, up)"},
	} {
		rows = append(rows, mk("agg:"+a.name, "aggregation", a.name, a.pin))
	}
	rows = append(
		rows,
		mk("agg-mod:by", "aggregation-modifier", "by", "sum by (job) (up)"),
		mk("agg-mod:without", "aggregation-modifier", "without", "sum without (job) (up)"),
	)

	// Binary operators.
	for _, o := range []struct{ op, pin string }{
		{"+", "up + up"},
		{"-", "up - up"},
		{"*", "up * 2"},
		{"/", "up / 2"},
		{"%", "up % 2"},
		{"^", "up ^ 2"},
		{"atan2", "up atan2 up"},
	} {
		rows = append(rows, mk("op:"+o.op, "binary-arithmetic", o.op, o.pin))
	}
	for _, o := range []struct{ op, pin string }{
		{"==", "up == 1"},
		{"!=", "up != 1"},
		{">", "up > 0"},
		{"<", "up < 2"},
		{">=", "up >= 0"},
		{"<=", "up <= 1"},
	} {
		rows = append(rows, mk("op:"+o.op, "binary-comparison", o.op, o.pin))
	}
	for _, o := range []struct{ op, pin string }{
		{"and", "up and up"},
		{"or", "up or up"},
		{"unless", "up unless up"},
	} {
		rows = append(rows, mk("op:"+o.op, "binary-set", o.op, o.pin))
	}
	rows = append(
		rows,
		mk("op-mod:bool", "binary-modifier", "bool", "up == bool 1"),
		mk("match:on", "vector-matching", "on", "up * on (job) up"),
		mk("match:ignoring", "vector-matching", "ignoring", "up * ignoring (instance) up"),
		mk("match:group_left", "vector-matching", "group_left",
			"foo_total * on (job) group_left up"),
		mk("match:group_right", "vector-matching", "group_right",
			"up * on (job) group_right foo_total"),
	)

	// Selector / expression features.
	rows = append(
		rows,
		mk("feature:subquery", "feature", "subquery",
			"max_over_time(rate(foo_total[1m])[10m:1m])"),
		mk("feature:offset", "feature", "offset", "up offset 5m"),
		mk("feature:negative-offset", "feature", "offset -",
			"up offset -5m"),
		mk("feature:native-histogram-selector", "feature", "_exp_hist",
			"histogram_quantile(0.9, foo_exp_hist)"),
	)
	return rows
}

// CollectPromQLFeatureIDs walks a parsed PromQL expression and returns
// the set of inventory row IDs the expression exercises. This is the
// strongest matcher shape: token-level substring matching would let
// "abs" match "absent"; an AST walk cannot.
func CollectPromQLFeatureIDs(expr parser.Expr) map[string]bool {
	ids := map[string]bool{}
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		switch n := node.(type) {
		case *parser.Call:
			ids["fn:"+n.Func.Name] = true
		case *parser.VectorSelector:
			collectSelectorFeatures(ids, n.StartOrEnd, n.OriginalOffset, n.Name)
		case *parser.SubqueryExpr:
			ids["feature:subquery"] = true
			collectSelectorFeatures(ids, n.StartOrEnd, n.OriginalOffset, "")
		case *parser.AggregateExpr:
			ids["agg:"+strings.ToLower(n.Op.String())] = true
			if len(n.Grouping) > 0 {
				if n.Without {
					ids["agg-mod:without"] = true
				} else {
					ids["agg-mod:by"] = true
				}
			}
		case *parser.BinaryExpr:
			ids["op:"+n.Op.String()] = true
			if n.ReturnBool {
				ids["op-mod:bool"] = true
			}
			if vm := n.VectorMatching; vm != nil {
				if vm.On {
					ids["match:on"] = true
				} else if len(vm.MatchingLabels) > 0 {
					ids["match:ignoring"] = true
				}
				switch vm.Card {
				case parser.CardManyToOne:
					ids["match:group_left"] = true
				case parser.CardOneToMany:
					ids["match:group_right"] = true
				case parser.CardOneToOne, parser.CardManyToMany:
					// No grouping modifier to record: one-to-one is
					// the default vector-matching cardinality and
					// many-to-many is the set-operator cardinality —
					// both already captured by the op:* row above.
				}
			}
		}
		return nil
	})
	return ids
}

// collectSelectorFeatures records the modifier-derived feature IDs
// shared by vector selectors and subqueries. metricName is "" for
// subqueries (their inner selector is visited separately).
func collectSelectorFeatures(ids map[string]bool, startOrEnd parser.ItemType, originalOffset time.Duration, metricName string) {
	switch startOrEnd {
	case parser.START:
		ids["fn:start"] = true
	case parser.END:
		ids["fn:end"] = true
	}
	if originalOffset > 0 {
		ids["feature:offset"] = true
	}
	if originalOffset < 0 {
		ids["feature:negative-offset"] = true
	}
	if strings.HasSuffix(metricName, "_exp_hist") {
		ids["feature:native-histogram-selector"] = true
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MarshalInventory renders the canonical on-disk JSON form (2-space
// indent + trailing newline) so the regenerate-and-diff test compares
// byte-for-byte.
func MarshalInventory(inv *Inventory) ([]byte, error) {
	b, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
