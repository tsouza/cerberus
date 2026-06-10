package chclient

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// freshLabelRows is a driver.Rows fake whose Scan decodes a FRESH
// labels map on every row — mirroring clickhouse-go's Map column
// decode, which allocates per row. The interning tests need this so
// any map identity observed downstream is provably produced by the
// cursor's intern cache, not by the fake handing the same map twice.
type freshLabelRows struct {
	rows   []Sample
	idx    int
	closed bool
}

func (r *freshLabelRows) Next() bool {
	if r.idx >= len(r.rows) {
		return false
	}
	r.idx++
	return true
}

func (r *freshLabelRows) Scan(dest ...any) error {
	if len(dest) != 4 {
		return errors.New("freshLabelRows.Scan: want 4 destinations")
	}
	s := r.rows[r.idx-1]
	if p, ok := dest[0].(*string); ok {
		*p = s.MetricName
	}
	if p, ok := dest[1].(*map[string]string); ok {
		// Fresh allocation per row — the per-row overhead the intern
		// cache exists to deduplicate.
		m := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			m[k] = v
		}
		*p = m
	}
	if p, ok := dest[2].(*time.Time); ok {
		*p = s.Timestamp
	}
	if p, ok := dest[3].(*float64); ok {
		*p = s.Value
	}
	return nil
}

func (r *freshLabelRows) ScanStruct(any) error {
	return errors.New("test mock: ScanStruct unused by cursor budget tests")
}
func (r *freshLabelRows) ColumnTypes() []driver.ColumnType { return nil }
func (r *freshLabelRows) Totals(...any) error              { return nil }
func (r *freshLabelRows) Columns() []string                { return nil }
func (r *freshLabelRows) Err() error                       { return nil }
func (r *freshLabelRows) HasData() bool                    { return len(r.rows) > 0 }

func (r *freshLabelRows) Close() error {
	r.closed = true
	return nil
}

// mapID returns a stable identity token for a map value. Two map
// values print the same %p pointer iff they are the same map instance;
// no mutation, no unsafe.
func mapID(m map[string]string) string { return fmt.Sprintf("%p", m) }

// TestRowsCursor_InternsLabelMapsPerSeries — the memory-shape pin for
// the k3d OOM class (run 27269987620): all rows of one series must
// share ONE label-map instance even though the driver decodes a fresh
// map per row, and distinct series must keep distinct maps.
func TestRowsCursor_InternsLabelMapsPerSeries(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	seriesA := map[string]string{"job": "api", "instance": "a"}
	seriesB := map[string]string{"job": "api", "instance": "b"}
	rows := &freshLabelRows{rows: []Sample{
		{MetricName: "up", Labels: seriesA, Timestamp: ts, Value: 1},
		{MetricName: "up", Labels: seriesB, Timestamp: ts, Value: 0},
		{MetricName: "up", Labels: seriesA, Timestamp: ts.Add(time.Minute), Value: 1},
		{MetricName: "up", Labels: seriesB, Timestamp: ts.Add(time.Minute), Value: 1},
		{MetricName: "up", Labels: seriesA, Timestamp: ts.Add(2 * time.Minute), Value: 0},
	}}
	cursor := &rowsCursor{rows: rows}
	defer func() { _ = cursor.Close() }()

	var got []Sample
	for cursor.Next() {
		got = append(got, cursor.Sample())
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("drained %d rows, want 5", len(got))
	}

	// Same series → the IDENTICAL map instance on every row.
	if mapID(got[0].Labels) != mapID(got[2].Labels) || mapID(got[2].Labels) != mapID(got[4].Labels) {
		t.Errorf("series A rows do not share one map instance: %s / %s / %s",
			mapID(got[0].Labels), mapID(got[2].Labels), mapID(got[4].Labels))
	}
	if mapID(got[1].Labels) != mapID(got[3].Labels) {
		t.Errorf("series B rows do not share one map instance: %s / %s",
			mapID(got[1].Labels), mapID(got[3].Labels))
	}
	// Distinct series → distinct maps.
	if mapID(got[0].Labels) == mapID(got[1].Labels) {
		t.Errorf("series A and B alias the same map instance: %s", mapID(got[0].Labels))
	}
	// Content survives interning intact.
	for i, want := range []map[string]string{seriesA, seriesB, seriesA, seriesB, seriesA} {
		for k, v := range want {
			if got[i].Labels[k] != v {
				t.Errorf("row %d label %q: got %q, want %q", i, k, got[i].Labels[k], v)
			}
		}
		if len(got[i].Labels) != len(want) {
			t.Errorf("row %d: got %d labels, want %d", i, len(got[i].Labels), len(want))
		}
	}
}

// TestRowsCursor_InternDoesNotAliasDifferentValues — two label sets
// with the same keys but different values must NOT collapse onto one
// map (the canonical key includes values).
func TestRowsCursor_InternDoesNotAliasDifferentValues(t *testing.T) {
	t.Parallel()

	ts := time.Unix(0, 0)
	rows := &freshLabelRows{rows: []Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
		{MetricName: "up", Labels: map[string]string{"job": "db"}, Timestamp: ts, Value: 1},
	}}
	cursor := &rowsCursor{rows: rows}
	defer func() { _ = cursor.Close() }()

	var got []Sample
	for cursor.Next() {
		got = append(got, cursor.Sample())
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if mapID(got[0].Labels) == mapID(got[1].Labels) {
		t.Fatal("distinct label values were interned onto one map instance")
	}
	if got[0].Labels["job"] != "api" || got[1].Labels["job"] != "db" {
		t.Fatalf("label values corrupted: %v / %v", got[0].Labels, got[1].Labels)
	}
}

// TestRowsCursor_SampleBudgetAborts — crossing MaxQuerySamples stops
// iteration and surfaces the ErrTooManySamples sentinel (wrapped in a
// *TooManySamplesError carrying the limit).
func TestRowsCursor_SampleBudgetAborts(t *testing.T) {
	t.Parallel()

	rows := newGenRows(10)
	cursor := &rowsCursor{rows: rows, maxSamples: 3}
	defer func() { _ = cursor.Close() }()

	count := 0
	for cursor.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("drained %d rows before abort, want exactly the 3-sample budget", count)
	}
	err := cursor.Err()
	if !errors.Is(err, ErrTooManySamples) {
		t.Fatalf("Err: got %v, want errors.Is(_, ErrTooManySamples)", err)
	}
	var tooMany *TooManySamplesError
	if !errors.As(err, &tooMany) {
		t.Fatalf("Err: got %T, want *TooManySamplesError", err)
	}
	if tooMany.Limit != 3 {
		t.Fatalf("Limit: got %d, want 3", tooMany.Limit)
	}
	// Iteration stays terminated.
	if cursor.Next() {
		t.Fatal("Next must stay false after the budget abort")
	}
}

// TestRowsCursor_SampleBudgetExactLimitPasses — a result set exactly
// at the budget drains fully with no error (the budget rejects "more
// than", not "equal to").
func TestRowsCursor_SampleBudgetExactLimitPasses(t *testing.T) {
	t.Parallel()

	rows := newGenRows(3)
	cursor := &rowsCursor{rows: rows, maxSamples: 3}
	defer func() { _ = cursor.Close() }()

	count := 0
	for cursor.Next() {
		count++
	}
	if count != 3 {
		t.Fatalf("drained %d rows, want 3", count)
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v, want nil at exactly the budget", err)
	}
}

// TestRowsCursor_SampleBudgetZeroDisables — MaxQuerySamples = 0 means
// unlimited (the documented opt-out).
func TestRowsCursor_SampleBudgetZeroDisables(t *testing.T) {
	t.Parallel()

	rows := newGenRows(50)
	cursor := &rowsCursor{rows: rows, maxSamples: 0}
	defer func() { _ = cursor.Close() }()

	count := 0
	for cursor.Next() {
		count++
	}
	if count != 50 {
		t.Fatalf("drained %d rows, want all 50 with the budget disabled", count)
	}
	if err := cursor.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
}

// budgetConn is a driver.Conn fake whose Query returns a generator of
// rowsPerQuery rows. Reuses chaosConn for the interface surface.
type budgetConn struct {
	chaosConn
	rowsPerQuery int
}

func (c *budgetConn) Query(context.Context, string, ...any) (driver.Rows, error) {
	return newGenRows(c.rowsPerQuery), nil
}

// TestClientQuery_SampleBudget_DoesNotTripBreaker — budget exceedance
// is a drain-time error: the breaker records the (successful) cursor
// open and never sees the sentinel, so repeated over-budget queries
// MUST NOT open the circuit. Pins the "a client asking for too much
// data is not a ClickHouse outage" contract from PR #738's
// open-call-only breaker recording.
func TestClientQuery_SampleBudget_DoesNotTripBreaker(t *testing.T) {
	t.Parallel()

	client := newWithConn(&budgetConn{rowsPerQuery: 10})
	client.maxSamples = 2

	// Well past breakerThreshold (5) consecutive budget rejections.
	for i := 0; i < 3*breakerThreshold; i++ {
		_, err := client.Query(context.Background(), "SELECT budgeted")
		if !errors.Is(err, ErrTooManySamples) {
			t.Fatalf("call %d: got %v, want ErrTooManySamples", i, err)
		}
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatalf("call %d: breaker opened on budget rejections", i)
		}
	}
	if !client.br.allow() {
		t.Fatal("breaker is OPEN after budget-only rejections; budget errors must not count as CH failures")
	}
}
