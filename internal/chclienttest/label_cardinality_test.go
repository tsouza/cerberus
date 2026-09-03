//go:build chdb

package chclienttest

import (
	"context"
	"errors"
	"testing"
)

// labelCardinalitySeedDDL stands in for the loki label-cardinality catalog
// the /detected_labels read path queries (cerberus issue #2770): one row per
// distinct label key, carrying that key's cardinality as an unsigned count.
//
// The seeded counts are deliberately unequal and NOT in the same order as
// the label keys sort, so a decoder that paired the two columns by position
// across rows — or that returned rows in insertion rather than query order —
// produces a visibly wrong pairing rather than a plausible one.
const labelCardinalitySeedDDL = `CREATE TABLE label_cardinality_probe (
    LabelKey String,
    Cardinality UInt64
) ENGINE = Memory;
INSERT INTO label_cardinality_probe VALUES ('service', 12), ('level', 3), ('pod', 4096);`

const labelCardinalityQuery = `SELECT LabelKey, Cardinality FROM label_cardinality_probe ORDER BY LabelKey`

// TestQueryLabelCardinalities is the chDB-backed decode test for the
// Querier method the loki /detected_labels handler tests run against
// (cerberus issue #2991). It was added with the catalog read path and had
// no caller in any test, so the (String, UInt64) decode below had never
// executed.
//
// That gap matters more than an ordinary uncovered method: this Client is
// the SUBSTRATE the api/loki handler assertions stand on. If it decoded the
// two columns wrongly, every handler test built on it would agree with the
// handler about a wrong answer and still pass.
func TestQueryLabelCardinalities(t *testing.T) {
	c := NewChDB(t)
	ctx := context.Background()
	c.Seed(t, labelCardinalitySeedDDL)

	got, err := c.QueryLabelCardinalities(ctx, labelCardinalityQuery)
	if err != nil {
		t.Fatalf("QueryLabelCardinalities: %v", err)
	}

	// ORDER BY LabelKey: level, pod, service.
	want := []struct {
		key         string
		cardinality uint64
	}{
		{"level", 3},
		{"pod", 4096},
		{"service", 12},
	}
	if len(got) != len(want) {
		t.Fatalf("QueryLabelCardinalities returned %d rows (%+v); want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].LabelKey != w.key {
			t.Errorf("row %d: LabelKey = %q; want %q", i, got[i].LabelKey, w.key)
		}
		if got[i].Cardinality != w.cardinality {
			t.Errorf("row %d (%q): Cardinality = %d; want %d", i, w.key, got[i].Cardinality, w.cardinality)
		}
	}
}

// TestQueryLabelCardinalities_EmptyResult pins the empty-catalog answer.
// /detected_labels asks this for a stream selector that may match nothing,
// so "no rows" is an ordinary outcome and must come back as an empty result
// with a nil error — not as an error the handler would render as a 500, and
// not as a single zero-valued row.
func TestQueryLabelCardinalities_EmptyResult(t *testing.T) {
	c := NewChDB(t)
	ctx := context.Background()
	c.Seed(t, labelCardinalitySeedDDL)

	got, err := c.QueryLabelCardinalities(ctx,
		`SELECT LabelKey, Cardinality FROM label_cardinality_probe WHERE LabelKey = 'absent' ORDER BY LabelKey`)
	if err != nil {
		t.Fatalf("QueryLabelCardinalities on an empty result: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("QueryLabelCardinalities on an empty result = %+v; want no rows", got)
	}
}

// TestQueryLabelCardinalities_QueryErrorSurfaces pins that a ClickHouse
// exception reaches the caller as an error rather than as a silently empty
// catalog. An empty catalog and a failed catalog query are different
// answers: the first legitimately means "no labels", while the second means
// the handler must not report that.
func TestQueryLabelCardinalities_QueryErrorSurfaces(t *testing.T) {
	c := NewChDB(t)
	ctx := context.Background()
	c.Seed(t, labelCardinalitySeedDDL)

	got, err := c.QueryLabelCardinalities(ctx, `SELECT LabelKey, Cardinality FROM no_such_catalog_table`)
	if err == nil {
		t.Fatalf("QueryLabelCardinalities against a missing table = %+v, nil error; want an error", got)
	}
	if got != nil {
		t.Errorf("QueryLabelCardinalities returned %+v alongside an error; want nil rows", got)
	}
}

// TestQueryLabelCardinalities_InjectedClientError pins the error-only
// Client's short circuit. NewChDBWithError builds a Querier that fails every
// call, which is how a handler test drives its ClickHouse-unreachable path;
// the injected error must be returned as-is so the test can match on it,
// rather than being wrapped into the query-failure vocabulary and made
// indistinguishable from a real ClickHouse exception.
func TestQueryLabelCardinalities_InjectedClientError(t *testing.T) {
	injected := errors.New("chclienttest: clickhouse unreachable")
	c := NewChDBWithError(t, injected)

	got, err := c.QueryLabelCardinalities(context.Background(), labelCardinalityQuery)
	if !errors.Is(err, injected) {
		t.Errorf("QueryLabelCardinalities on an error-only client = %v; want the injected %v", err, injected)
	}
	if got != nil {
		t.Errorf("QueryLabelCardinalities returned %+v alongside the injected error; want nil rows", got)
	}
}
