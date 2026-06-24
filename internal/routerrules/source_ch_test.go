package routerrules

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// fakeRows is a canned driver.Rows over a fixed set of column-value tuples. It
// embeds driver.Rows so the unused methods of that wide interface are satisfied
// without hand-writing them; only the four the CH source calls are overridden.
type fakeRows struct {
	driver.Rows
	data [][]any
	pos  int
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("fakeRows: scan arity %d != row arity %d", len(dest), len(row))
	}
	for i, d := range dest {
		switch p := d.(type) {
		case *string:
			*p = row[i].(string)
		case *float64:
			*p = row[i].(float64)
		case *int64:
			*p = row[i].(int64)
		default:
			return fmt.Errorf("fakeRows: unsupported scan target %T", d)
		}
	}
	return nil
}

func (r *fakeRows) Err() error   { return nil }
func (r *fakeRows) Close() error { return nil }

// recordingConn is a fake CHConn that returns scripted rows per query and
// records every SQL string it saw, so tests can assert query shape and count.
type recordingConn struct {
	queries []string
	// respond maps a substring of the SQL to the rows to return for it. The
	// first matching entry wins; an unmatched query returns empty rows.
	respond []scriptedResponse
}

type scriptedResponse struct {
	match string
	rows  [][]any
}

func (c *recordingConn) Query(_ context.Context, query string, _ ...any) (driver.Rows, error) {
	c.queries = append(c.queries, query)
	for _, r := range c.respond {
		if stringIndex(query, r.match) >= 0 {
			return &fakeRows{data: r.rows}, nil
		}
	}
	return &fakeRows{}, nil
}

func TestCHSourceBuildsTypedSQL(t *testing.T) {
	conn := &recordingConn{}
	src := NewCHCorpusSource(conn, 0).(*chCorpusSource)

	// A scalar percentile param: assert the parametric quantileExact shape and
	// the scope predicate are present, all composed by the typed builder.
	frac := 0.9
	_, err := src.Aggregate(context.Background(), AggSpec{
		Column:     "memory_usage",
		Percentile: &frac,
		Scope:      Scope{"route": "A", "exit_status": "ok"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(conn.queries))
	}
	q := conn.queries[0]
	for _, want := range []string{
		// The aggregate is wrapped in ifNull(<agg>, 0) so an empty / fully-TTL'd
		// population folds to 0 at the SQL layer instead of returning NULL. This
		// is the Frag-level half of the MAJOR-4 silent-suppression fix (#1060);
		// the post-Scan NaN guard is the other half (see TestNormalizeEmptyAgg).
		"ifNull(quantileExact(0.9)(memory_usage), 0)",
		"FROM cerberus_router_corpus",
		"route = 'A'",
		"exit_status = 'ok'",
	} {
		if stringIndex(q, want) < 0 {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}
}

// TestNormalizeEmptyAgg pins the post-Scan guard half of the MAJOR-4 fix
// (#1060): a CH aggregate (avg/quantileExact/stddevPop) over an empty or
// fully-TTL'd population returns NaN, which ifNull does not catch. Left
// unfolded, a NaN watermark makes every `x > NaN` comparison false and
// silently suppresses every finding. The guard folds NaN/±Inf to the JSONL
// backend's 0-contract while leaving finite values (including negatives and
// zero) untouched.
func TestNormalizeEmptyAgg(t *testing.T) {
	posInf := math.Inf(1)
	negInf := math.Inf(-1)
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"NaN folds to zero", math.NaN(), 0},
		{"positive infinity folds to zero", posInf, 0},
		{"negative infinity folds to zero", negInf, 0},
		{"finite positive unchanged", 42.5, 42.5},
		{"zero unchanged", 0, 0},
		{"finite negative unchanged", -7.25, -7.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeEmptyAgg(tc.in); got != tc.want {
				t.Errorf("normalizeEmptyAgg(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestCHSourcePartitionedAggregate(t *testing.T) {
	conn := &recordingConn{respond: []scriptedResponse{
		{match: "GROUP BY", rows: [][]any{{"promql", 100.0}, {"logql", 200.0}}},
	}}
	src := NewCHCorpusSource(conn, 0).(*chCorpusSource)
	frac := 0.5
	v, err := src.Aggregate(context.Background(), AggSpec{
		Column:      "memory_usage",
		Percentile:  &frac,
		PartitionBy: []string{"language"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !v.IsPartitioned() {
		t.Fatalf("expected partitioned value")
	}
	if v.PartitionCol != "language" {
		t.Fatalf("PartitionCol = %q, want language", v.PartitionCol)
	}
	if v.Partition["promql"] != 100 || v.Partition["logql"] != 200 {
		t.Fatalf("partition values wrong: %+v", v.Partition)
	}
}

func TestCHSourceEvalRulePushesGroupAndCount(t *testing.T) {
	conn := &recordingConn{respond: []scriptedResponse{
		{match: "count()", rows: [][]any{{"cerb:sum", "promql", int64(2), 120.0}}},
	}}
	src := NewCHCorpusSource(conn, 0).(*chCorpusSource)
	cond := &EnumCmp{Column: "route", Op: OpEq, Values: []string{"A"}}
	ev, err := parseEvidenceExpr("max(memory_usage)")
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	groups, err := src.EvalRule(context.Background(), RuleQuery{
		Condition: cond,
		GroupBy:   []string{"shape_id", "language"},
		Evidence:  []evidenceExpr{ev},
		Env:       Env{},
	})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	g := groups[0]
	if g.Support != 2 || len(g.GroupKey) != 2 || g.GroupKey[0] != "cerb:sum" {
		t.Fatalf("unexpected group: %+v", g)
	}
	if len(g.Evidence) != 1 || g.Evidence[0] != 120 {
		t.Fatalf("unexpected evidence: %+v", g.Evidence)
	}
	q := conn.queries[0]
	for _, want := range []string{"count()", "GROUP BY", "max(memory_usage)", "route = 'A'"} {
		if stringIndex(q, want) < 0 {
			t.Errorf("eval query missing %q:\n%s", want, q)
		}
	}
}

// TestCHSourceSinceWindow asserts the event_time window predicate is added when
// a since bound is set.
func TestCHSourceSinceWindow(t *testing.T) {
	conn := &recordingConn{}
	src := NewCHCorpusSource(conn, 1000).(*chCorpusSource)
	frac := 0.5
	_, _ = src.Aggregate(context.Background(), AggSpec{Column: "fanout", Percentile: &frac})
	q := conn.queries[0]
	if stringIndex(q, "event_time >= toDateTime(1000)") < 0 {
		t.Errorf("expected since window predicate, got:\n%s", q)
	}
}

// TestCHSourceCountRatioSQL pins the SQL shape of the corpus_count_ratio
// resolver path in the DEFAULT (untagged) lane. The catalogVersion-2 N3 rule is
// the first to use a corpus_count_ratio param (cerberus_reject_ratio), and the
// CH render of countIf(<scope>)/<scope> had no default-lane shape coverage
// before this — only quantile/agg/eval/since were asserted. This test is the
// durable guarantee that the count-ratio SQL stays typed and correct even on a
// PR that the chDB parity job's path filter would skip.
func TestCHSourceCountRatioSQL(t *testing.T) {
	conn := &recordingConn{respond: []scriptedResponse{
		{match: "countIf", rows: [][]any{{2.0, 13.0}}},
	}}
	src := NewCHCorpusSource(conn, 0).(*chCorpusSource)
	v, err := src.Aggregate(context.Background(), AggSpec{
		CountRatio: true,
		NumScope:   Scope{"exit_status": "rejected"},
		DenScope:   Scope{"route": "A"},
	})
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if v.IsPartitioned() {
		t.Fatalf("count ratio must resolve to a scalar, got partitioned")
	}
	want := 2.0 / 13.0
	if v.Scalar < want-1e-9 || v.Scalar > want+1e-9 {
		t.Fatalf("count ratio scalar = %v, want %v", v.Scalar, want)
	}
	if len(conn.queries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(conn.queries))
	}
	q := conn.queries[0]
	for _, want := range []string{
		"countIf(exit_status = 'rejected')",
		"countIf(route = 'A')",
		"FROM cerberus_router_corpus",
	} {
		if stringIndex(q, want) < 0 {
			t.Errorf("count-ratio query missing %q:\n%s", want, q)
		}
	}
}

// TestCHSourceEvalRuleEnumInCondition pins the SQL shape of an `exit_status IN
// (...)` leaf — the condition shape every catalogVersion-2 failure rule uses
// (N1/N2/N4 gate on exit_status in [oom,timeout]; N3 on the three cerberus-side
// statuses). It proves the IN frag and the partitioned param's numeric leaf
// render through the typed builder in the default lane.
func TestCHSourceEvalRuleEnumInCondition(t *testing.T) {
	conn := &recordingConn{respond: []scriptedResponse{
		{match: "count()", rows: [][]any{{"trc:compare", "traceql", "not-sliceable", int64(2), 950.0}}},
	}}
	src := NewCHCorpusSource(conn, 0).(*chCorpusSource)
	// heavy_shape_geometry_failing's shape: exit_status IN (oom,timeout) AND
	// cumulative_d >= <resolved d_high_watermark>.
	cond := &AndCond{Children: []Condition{
		&EnumCmp{Column: "exit_status", Op: OpIn, Values: []string{"oom", "timeout"}},
		&ParamCmp{Column: "cumulative_d", Op: OpGte, Param: "d_wm"},
	}}
	ev, err := parseEvidenceExpr("max(cumulative_d)")
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	groups, err := src.EvalRule(context.Background(), RuleQuery{
		Condition: cond,
		GroupBy:   []string{"shape_id", "language", "decision_reason"},
		Evidence:  []evidenceExpr{ev},
		Env:       Env{"d_wm": {Scalar: 250}},
	})
	if err != nil {
		t.Fatalf("eval: %v", err)
	}
	if len(groups) != 1 || groups[0].Support != 2 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
	q := conn.queries[0]
	for _, want := range []string{
		"exit_status IN ('oom', 'timeout')",
		"cumulative_d >= 250",
		"max(cumulative_d)",
		"GROUP BY",
		"decision_reason",
	} {
		if stringIndex(q, want) < 0 {
			t.Errorf("eval query missing %q:\n%s", want, q)
		}
	}
}
