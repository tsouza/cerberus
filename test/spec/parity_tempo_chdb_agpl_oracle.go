//go:build chdb_agpl_oracle

// Package spec — the Tempo half of the parity runner.
//
// The `chdb_agpl_oracle` tag is the synthetic single-term tag CI sets
// ALONGSIDE `chdb` and `agpl_oracle` (never alone), the same pattern
// test/property/logql_test.go uses: this file needs the chDB session from
// runner_chdb.go AND the AGPL reference engine behind
// test/spec/parityoracle/traceql, and every `//go:build` line in this tree
// must be a single term (test/regression/lint_build_tags_test.go pins why).
package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/traceql"
)

// Cerberus projects a span's identity into the reserved attribute keys
// below (internal/api/tempo/handler.go writes the same three). They are
// restated rather than imported because they are a WIRE fact — the shape
// the row already has by the time it reaches a fixture — and reaching into
// the handler for them would give this comparator a dependency on the
// system under test for no gain.
const (
	spanIdentityTraceIDKey = "__cerberus_traceID"
	spanIdentitySpanIDKey  = "__cerberus_spanID"
)

// tracesTable is the one table a TraceQL fixture's seed creates.
const tracesTable = "otel_traces"

// runTempoParity evaluates a TraceQL fixture on the upstream Tempo engine
// and compares the matched spans with cerberus's.
//
// There is no update path here, for the reason [RunParity] documents: the
// reference answer is computed on every run, so GOLDEN_UPDATE=1 has
// nothing to overwrite.
func runTempoParity(t *testing.T, c *Case, p *Parity, rt *RoundTripSections) {
	t.Helper()

	query, ok := c.Section("query.traceql")
	if !ok {
		t.Fatalf("fixture %s: `parity:` with oracle tempo requires a query.traceql section", c.Name)
	}
	rejectNarrowingSections(t, c)

	// Serialize the whole engine span, same contract RunRoundTrip honours.
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	db := OpenChDB(t)
	ApplySeed(t, db, rt.Seed)

	spans, err := readSeededSpans(db)
	if err != nil {
		t.Fatalf("fixture %s: read seeded spans back: %v", c.Name, err)
	}
	if len(spans) == 0 {
		t.Fatalf(
			"fixture %s: seed produced no readable spans, so the reference engine would "+
				"trivially agree with any answer", c.Name,
		)
	}

	got, err := oracle.Evaluate(t, spans, strings.TrimSpace(query))
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}

	want, err := spanIdentitiesOfExpectedRows(rt)
	if err != nil {
		t.Fatalf("fixture %s: %v", c.Name, err)
	}

	compareSpanSets(t, c, p, got, want)
}

// rejectNarrowingSections refuses to run a fixture whose scan cerberus
// narrows in a way the reference engine's pipeline never sees.
//
// `search_window:` bounds which rows cerberus reads and `search_limit:`
// truncates how many it returns; both are storage-layer concerns Tempo
// applies in its FETCH layer, not in the pipeline this oracle evaluates.
// Comparing across that difference would report a disagreement about the
// query where there is only a difference in what each side was asked to
// read. Failing loudly is the point: silently ignoring the section would
// enrol a fixture that then proves nothing.
func rejectNarrowingSections(t *testing.T, c *Case) {
	t.Helper()
	for _, section := range []string{"search_window", "search_limit"} {
		if _, ok := c.Section(section); ok {
			t.Fatalf(
				"fixture %s carries both `parity:` and `%s:`. The reference oracle evaluates the "+
					"spanset pipeline over every seeded span; %s narrows cerberus's scan in the "+
					"storage layer, which upstream applies before the pipeline the oracle runs. "+
					"The two answers would differ about what was READ, not about the query. "+
					"Remove the `parity:` section from this fixture.",
				c.Name, section, section,
			)
		}
	}
}

// --- reading the seeded spans back ------------------------------------

// spanColumn describes how one span field is recovered from whatever
// columns the fixture's seed actually declared.
//
// Reading back rather than re-parsing the `-- seed --` text is deliberate,
// for the same reason the PromQL side does it: it shows the oracle the data
// as it ACTUALLY LANDED — after DEFAULTs, after MATERIALIZED expressions,
// after coercion — instead of as the SQL claims it will land.
//
// Each fixture's seed declares its own narrow table (some carry no SpanId
// at all), so the projection is built from the table's real columns and
// every absent one falls back to a typed literal. Selecting a column the
// seed never created would fail the whole read.
type spanColumn struct {
	// name is the ClickHouse column, and also the key this file uses.
	name string

	// missing is the SQL literal used when the seed omitted the column.
	missing string

	// read renders the projection for a column that IS present. chType is
	// the DESCRIBE-reported type, which matters because the corpus seeds
	// Timestamp as both DateTime64(9) and UInt64.
	read func(chType string) string
}

func quotedIdent(name string) string { return "`" + name + "`" }

func stringColumn(name string) spanColumn {
	return spanColumn{
		name:    name,
		missing: "''",
		read:    func(string) string { return "toString(" + quotedIdent(name) + ")" },
	}
}

func attrsColumn(name string) spanColumn {
	return spanColumn{
		name:    name,
		missing: "'{}'",
		read:    func(string) string { return "toJSONString(" + quotedIdent(name) + ")" },
	}
}

// spanColumns is the projection, in the order scanSpanRows reads it.
var spanColumns = []spanColumn{
	stringColumn("TraceId"),
	stringColumn("SpanId"),
	stringColumn("ParentSpanId"),
	stringColumn("SpanName"),
	stringColumn("SpanKind"),
	stringColumn("StatusCode"),
	stringColumn("StatusMessage"),
	stringColumn("ServiceName"),
	{
		name:    "Duration",
		missing: "0",
		read:    func(string) string { return "toInt64(" + quotedIdent("Duration") + ")" },
	},
	{
		name:    "Timestamp",
		missing: "0",
		read: func(chType string) string {
			// The corpus seeds Timestamp both as DateTime64(9) and as a
			// bare integer nanosecond count. Reading a DateTime64 with
			// toInt64 would yield a tick count in the column's own scale,
			// which is not what the other seeds mean by the same column.
			if strings.HasPrefix(chType, "DateTime64") {
				return "toUnixTimestamp64Nano(" + quotedIdent("Timestamp") + ")"
			}
			return "toInt64(" + quotedIdent("Timestamp") + ")"
		},
	},
	attrsColumn("ResourceAttributes"),
	attrsColumn("SpanAttributes"),
}

// readSeededSpans reads the seeded rows back OUT of chDB and turns them
// into reference-engine spans.
func readSeededSpans(db *sql.DB) ([]oracle.Span, error) {
	types, err := describeTable(db, tracesTable)
	if err != nil {
		return nil, err
	}

	projection := make([]string, 0, len(spanColumns))
	for _, col := range spanColumns {
		chType, present := types[col.name]
		if !present {
			projection = append(projection, col.missing)
			continue
		}
		projection = append(projection, col.read(chType))
	}

	//nolint:gosec // Every fragment comes from spanColumns, never from fixture text.
	q := "SELECT " + strings.Join(projection, ", ") + " FROM " + quotedIdent(tracesTable)
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", tracesTable, err)
	}
	defer func() { _ = rows.Close() }()

	spans, err := scanSpanRows(rows)
	if err != nil {
		return nil, err
	}
	return spans, rows.Err()
}

// describeTable returns the table's column names mapped to their declared
// ClickHouse types, including MATERIALIZED and ALIAS columns, which a
// `SELECT *` would silently omit.
func describeTable(db *sql.DB, table string) (map[string]string, error) {
	//nolint:gosec // table is the package constant above, not fixture text.
	rows, err := db.Query("DESCRIBE TABLE " + quotedIdent(table))
	if err != nil {
		return nil, fmt.Errorf("describe %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("describe %s columns: %w", table, err)
	}

	out := map[string]string{}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(sql.RawBytes)
		}
		if err := rows.Scan(cells...); err != nil {
			return nil, fmt.Errorf("scan describe %s: %w", table, err)
		}
		// DESCRIBE's first two columns are name and type.
		const describeNameIdx, describeTypeIdx = 0, 1
		if len(cells) <= describeTypeIdx {
			return nil, fmt.Errorf("describe %s returned %d column(s), want at least 2", table, len(cells))
		}
		name := string(*cells[describeNameIdx].(*sql.RawBytes))
		chType := string(*cells[describeTypeIdx].(*sql.RawBytes))
		out[name] = chType
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describe %s: %w", table, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("describe %s returned no columns", table)
	}
	return out, nil
}

func scanSpanRows(rows *sql.Rows) ([]oracle.Span, error) {
	var out []oracle.Span
	for rows.Next() {
		var (
			s                      oracle.Span
			durationNanos, startNs int64
			resourceJSON, spanJSON string
		)
		if err := rows.Scan(
			&s.TraceID, &s.SpanID, &s.ParentSpanID,
			&s.Name, &s.Kind, &s.StatusCode, &s.StatusMessage, &s.ServiceName,
			&durationNanos, &startNs,
			&resourceJSON, &spanJSON,
		); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", tracesTable, err)
		}

		// A negative Duration or Timestamp is not a value either side can
		// mean anything by; clamping silently would invent data, so read
		// them as the unsigned counts the engine's fields are.
		if durationNanos < 0 || startNs < 0 {
			return nil, fmt.Errorf(
				"span %s carries a negative Duration (%d) or Timestamp (%d); the reference "+
					"engine models both as unsigned nanosecond counts",
				s.SpanID, durationNanos, startNs,
			)
		}
		s.DurationNanos = uint64(durationNanos)
		s.StartUnixNano = uint64(startNs)

		var err error
		if s.ResourceAttrs, err = decodeAttrsJSON(resourceJSON); err != nil {
			return nil, fmt.Errorf("span %s ResourceAttributes: %w", s.SpanID, err)
		}
		if s.SpanAttrs, err = decodeAttrsJSON(spanJSON); err != nil {
			return nil, fmt.Errorf("span %s SpanAttributes: %w", s.SpanID, err)
		}
		out = append(out, s)
	}
	return out, nil
}

func decodeAttrsJSON(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil, nil
	}
	attrs := map[string]string{}
	if err := json.Unmarshal([]byte(trimmed), &attrs); err != nil {
		return nil, fmt.Errorf("decode %q: %w", trimmed, err)
	}
	return attrs, nil
}

// --- comparison -------------------------------------------------------

// spanIdentitiesOfExpectedRows reads cerberus's answer out of
// `expected_rows:` as the set of spans it matched.
//
// It errors rather than skipping on a row it cannot identify. A TraceQL
// fixture whose projection carries no span identity — an aggregate, or a
// seed with no SpanId column — cannot be parity-checked at this layer, and
// silently comparing nothing would be the hollow green this mechanism
// exists to prevent.
func spanIdentitiesOfExpectedRows(rt *RoundTripSections) ([]oracle.Result, error) {
	out := make([]oracle.Result, 0, len(rt.ExpectedRows))
	seen := make(map[oracle.Result]bool, len(rt.ExpectedRows))

	for i, row := range rt.ExpectedRows {
		if len(row) != expectedRowArity {
			return nil, fmt.Errorf(
				"expected_rows[%d] has %d column(s), not the canonical span shape "+
					"(SpanName, Attributes, Timestamp, Duration); this fixture's projection "+
					"carries no span identity and cannot be parity-checked", i, len(row),
			)
		}
		attrs, err := rowAttrs(row[expectedRowAttrsIdx], i)
		if err != nil {
			return nil, err
		}
		traceID, hasTrace := attrs[spanIdentityTraceIDKey]
		spanID, hasSpan := attrs[spanIdentitySpanIDKey]
		if !hasTrace || !hasSpan || traceID == "" || spanID == "" {
			return nil, fmt.Errorf(
				"expected_rows[%d] carries no %s/%s identity. The comparison is over WHICH SPANS "+
					"matched, so a fixture whose seed declares no TraceId/SpanId columns cannot be "+
					"enrolled — every row would be the same anonymous span",
				i, spanIdentityTraceIDKey, spanIdentitySpanIDKey,
			)
		}

		r := oracle.Result{TraceID: traceID, SpanID: spanID}
		if seen[r] {
			return nil, fmt.Errorf(
				"expected_rows[%d] repeats span %s/%s. Span identity is the comparison key, so a "+
					"projection that emits one span twice cannot be matched against a set of "+
					"matched spans", i, traceID, spanID,
			)
		}
		seen[r] = true
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].TraceID != out[j].TraceID {
			return out[i].TraceID < out[j].TraceID
		}
		return out[i].SpanID < out[j].SpanID
	})
	return out, nil
}

// compareSpanSets is the assertion itself: the reference engine and
// cerberus must have matched exactly the same spans.
func compareSpanSets(t *testing.T, c *Case, p *Parity, got, want []oracle.Result) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf(
			"fixture %s: reference engine matched %d span(s), cerberus %d.\n"+
				"  reference: %v\n  cerberus:  %v\n"+
				"This is a real disagreement about the answer, not a golden to regenerate — "+
				"there is no update path for this check.",
			c.Name, len(got), len(want), got, want,
		)
	}

	for i := range got {
		if got[i] != want[i] {
			t.Errorf(
				"fixture %s span %d: the two engines matched different spans\n"+
					"  reference: %v\n  cerberus:  %v\n"+
					"  full reference set: %v\n  full cerberus set:  %v",
				c.Name, i, got[i], want[i], got, want,
			)
		}
	}

	if !p.ComparesInFull() {
		t.Logf("fixture %s compared with scope %q", c.Name, p.Scope)
	}
}
