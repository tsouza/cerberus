package routerrules

import "sort"

// CorpusTableName is the ClickHouse table the router corpus is written to. It is
// re-declared here verbatim rather than imported from internal/optcorpus so this
// package depends on neither optcorpus nor chclient (mirroring how optcorpus
// itself re-declares its narrow read surface rather than importing chclient, to
// avoid an import cycle). The two declarations are a stable wire contract: if
// optcorpus ever renames the table, this constant and the column allow-list
// below must be updated in lockstep.
const CorpusTableName = "cerberus_router_corpus"

// ColumnKind classifies each corpus column so the validator can tell which
// columns may legitimately carry an enum literal in a rule condition (the three
// Enum-typed columns) versus which may only be compared against a resolved
// numeric parameter (the cost/feature columns).
type ColumnKind uint8

const (
	// ColumnNumeric is a scalar cost or feature column (UInt/Float). A
	// condition may compare it only against a resolved ${param}, never against
	// an inline number.
	ColumnNumeric ColumnKind = iota
	// ColumnEnum is one of the three Enum-typed columns. A condition may
	// compare it against a domain category literal (eq / in) — these are
	// classifier categories, not tunable numbers.
	ColumnEnum
	// ColumnTime is the event_time column. It is window-bounded by the source
	// (via --since), never used as a rule-condition operand.
	ColumnTime
	// ColumnGroup is an identity/grouping column (shape id, query hash,
	// decision reason). It is used as a group key or carried as finding
	// context, never compared numerically.
	ColumnGroup
)

// corpusColumn pairs a column name with its kind. The slice is the single
// source of truth the validator checks every ColRef against, so a typo'd or
// drifted column name fails at catalog load, not at query time.
type corpusColumn struct {
	name string
	kind ColumnKind
	// runtimeCost marks a numeric column that ClickHouse MEASURES while the
	// query runs, as opposed to one the solver computes from the query shape
	// before dispatch. The distinction decides how a percentile over the column
	// must be scoped — see runtimeCostColumns.
	runtimeCost bool
}

// corpusColumns is the canonical column contract of cerberus_router_corpus,
// column-for-column aligned with the MergeTree DDL in internal/optcorpus
// (corpusCreateTableSQL) and the Row JSON tags in internal/optcorpus
// (optcorpus.Row). Keep these in lockstep with that DDL.
var corpusColumns = []corpusColumn{
	{name: "event_time", kind: ColumnTime},
	{name: "shape_id", kind: ColumnGroup},
	{name: "language", kind: ColumnEnum},
	{name: "normalized_query_hash", kind: ColumnGroup},
	{name: "n_anchors", kind: ColumnNumeric},
	{name: "fanout", kind: ColumnNumeric},
	{name: "cumulative_d", kind: ColumnNumeric},
	{name: "outer_range", kind: ColumnNumeric},
	{name: "step", kind: ColumnNumeric},
	{name: "route", kind: ColumnEnum},
	{name: "k_shards", kind: ColumnNumeric},
	{name: "decision_reason", kind: ColumnGroup},
	{name: "read_rows", kind: ColumnNumeric, runtimeCost: true},
	{name: "read_bytes", kind: ColumnNumeric, runtimeCost: true},
	{name: "query_duration_ms", kind: ColumnNumeric, runtimeCost: true},
	{name: "memory_usage", kind: ColumnNumeric, runtimeCost: true},
	{name: "exit_status", kind: ColumnEnum},
}

// columnKinds indexes corpusColumns by name for O(1) validation lookups.
var columnKinds = func() map[string]ColumnKind {
	m := make(map[string]ColumnKind, len(corpusColumns))
	for _, c := range corpusColumns {
		m[c.name] = c.kind
	}
	return m
}()

// runtimeCostColumns indexes the columns ClickHouse measures during execution:
// read_rows, read_bytes, query_duration_ms, memory_usage. Every other numeric
// column (n_anchors, fanout, cumulative_d, outer_range, step, k_shards) is
// GEOMETRY — the solver derives it from the query shape before dispatch — and
// its value is the same whether the query then finished or died.
//
// The split decides how a percentile over the column must be scoped, which is
// why it is enforced at catalog-validation time (see validateHealthScope):
//
//   - A percentile over a runtime-cost column is a "what does a normal query
//     cost here?" watermark, so it MUST be scoped to exit_status: ok. An
//     abnormal termination truncates the counters at whatever ClickHouse had
//     accumulated when it gave up — a timeout's duration is the deadline, an
//     OOM's memory is the cap, a killed scan's read_rows is however far it got —
//     so those rows describe the failure, not the query. Folding them in drags
//     the watermark toward the pathologies the rules exist to detect, and the
//     rules stop firing exactly where they are needed.
//
//   - A percentile over a geometry column MUST NOT be scoped that way. There is
//     no outcome bias to remove, so the scope only shrinks the sample; on a
//     deployment whose heavy shapes are precisely the ones that fail, it can
//     empty the population outright and collapse the watermark to a fire-on-
//     everything floor.
var runtimeCostColumns = func() map[string]struct{} {
	m := make(map[string]struct{}, len(corpusColumns))
	for _, c := range corpusColumns {
		if c.runtimeCost {
			m[c.name] = struct{}{}
		}
	}
	return m
}()

// enumDomains is the closed set of accepted category tokens per Enum column,
// matching the optcorpus Enum8 DDL. A condition that compares an enum column
// against a token outside its domain fails validation, catching typos like
// route='C' or exit_status='killed'.
var enumDomains = map[string]map[string]struct{}{
	"route":       setOf("A", "B"),
	"exit_status": setOf("ok", "oom", "timeout", "sample_budget", "breaker", "rejected", "aborted", "error"),
	"language":    setOf("promql", "logql", "traceql"),
}

func setOf(xs ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		m[x] = struct{}{}
	}
	return m
}

// EnumDomain returns the accepted category tokens for an Enum-typed corpus
// column, sorted, or nil for a column that is not Enum-typed. It exposes the
// closed set so a lockstep test can compare it against the writer's member
// list without routerrules taking a build-time dependency on the writer.
func EnumDomain(column string) []string {
	dom, ok := enumDomains[column]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(dom))
	for tok := range dom {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// knownColumn reports whether name is a corpus column.
func knownColumn(name string) bool {
	_, ok := columnKinds[name]
	return ok
}

// isEnumColumn reports whether name is one of the three Enum-typed columns.
func isEnumColumn(name string) bool {
	return columnKinds[name] == ColumnEnum
}

// validEnumValue reports whether tok is an accepted category for enum column.
func validEnumValue(column, tok string) bool {
	dom, ok := enumDomains[column]
	if !ok {
		return false
	}
	_, ok = dom[tok]
	return ok
}
