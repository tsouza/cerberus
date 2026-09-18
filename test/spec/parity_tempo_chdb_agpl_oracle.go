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
	"errors"
	"fmt"
	"sort"
	"strconv"
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

// attributesColumnAlias is the backtick-quoted alias every canonical
// span-shaped chplan lowering gives its Attributes output column
// (internal/api/tempo/handler.go writes the same wire name) — the same
// WIRE-fact restatement as the span identity keys above, for the same
// reason: whether a fixture's own emitted SQL claims this alias at all is
// what distinguishes a deliberately non-span-shaped aggregate projection
// (MetricsCompare, MetricsAggregate, or any future one) from a
// span-shaped fixture whose Attributes cell is simply malformed (see
// spanIdentitiesOfExpectedRows).
const attributesColumnAlias = "`Attributes`"

// outerSelectClaimsAttributesColumn reports whether sql's OWN (outermost)
// SELECT list aliases a column attributesColumnAlias.
//
// Scoped to the outermost SELECT specifically, not the whole query text,
// because an inner subquery commonly passes an Attributes-named column
// straight through on its way to becoming something else entirely (a
// MetricsCompare/MetricsAggregate output never named Attributes) — chsql's
// own emitted SQL for exactly that shape still has "`Attributes` AS
// `Attributes`" at an inner level, so matching anywhere in sql would read
// every non-canonical projection as canonical. chsql always renders the
// outermost SELECT's own column list before its first top-level `FROM`,
// and none of the column expressions this comparator ever sees embed a
// scalar subquery of their own, so the substring before the first ` FROM
// ` is that column list alone.
func outerSelectClaimsAttributesColumn(sql string) bool {
	outer, _, _ := strings.Cut(sql, " FROM ")
	return strings.Contains(outer, attributesColumnAlias)
}

// errTempoSpanIdentityUnavailable marks a successfully decoded answer shape
// that cannot identify a set of spans for the Tempo comparator. Malformed
// expected-row data deliberately does not wrap this sentinel.
var errTempoSpanIdentityUnavailable = errors.New("tempo span identity unavailable")

// tempoSpanIdentityError carries WHICH way the answer shape fails to
// identify spans — a non-span projection, rows with no identity, or a
// repeated identity — so runTempoParity can refuse under the matching
// refusalClass rather than one catch-all.
type tempoSpanIdentityError struct {
	class refusalClass
	err   error
}

func (e *tempoSpanIdentityError) Error() string {
	return fmt.Sprintf("%v: %v", errTempoSpanIdentityUnavailable, e.err)
}

func (e *tempoSpanIdentityError) Unwrap() error { return e.err }

func (e *tempoSpanIdentityError) Is(target error) bool {
	return target == errTempoSpanIdentityUnavailable
}

func tempoSpanIdentityUnavailable(class refusalClass, err error) error {
	return &tempoSpanIdentityError{class: class, err: err}
}

// errTempoSeedWithoutSpanIdentity marks a traces seed that declares no
// TraceId or no SpanId column. Such a seed's rows all read back as the
// same anonymous span, which used to surface as "duplicate identity" — a
// refusal about a repeat that does not exist, indistinguishable from the
// deliberate duplicate a ReasonDuplicateSpanSeed fixture provokes.
var errTempoSeedWithoutSpanIdentity = errors.New("traces seed declares no span identity")

// tracesTable is the one table a TraceQL fixture's seed creates.
const tracesTable = "otel_traces"

// runTempoParity evaluates a TraceQL fixture on the upstream Tempo engine
// and compares the matched spans with cerberus's.
//
// There is no update path here, for the reason [RunParity] documents: the
// reference answer is computed on every run, so GOLDEN_UPDATE=1 has
// nothing to overwrite.
func runTempoParity(t *testing.T, c *Case, p *Parity, rt *RoundTripSections) error {
	t.Helper()

	query, ok := c.Section("query.traceql")
	if !ok {
		return fmt.Errorf("fixture %s: `parity:` with oracle tempo requires a query.traceql section", c.Name)
	}
	if err := rejectNarrowingSections(c); err != nil {
		return parityRefusal(refusalNarrowingSection, err)
	}

	// Cerberus's answer shape is a structural fact about the fixture, read
	// before the seed or the reference engine is ever consulted: a
	// projection that identifies no span (a per-trace aggregate, a metrics
	// pipeline) has no comparison to make whatever the seed holds or the
	// engine would say, so that refusal must not be pre-empted by a seed
	// fact or an engine verdict on a query it happens not to parse.
	want, err := spanIdentitiesOfExpectedRows(rt, projectionIdentifiesSpans(c))
	if err != nil {
		// These are comparator-shape refusals, not corrupt fixture data: the
		// expected row decoded successfully, but it cannot identify a set of
		// spans for the Tempo oracle to compare. Keep JSON/type decode errors
		// unclassified so a broken harness cannot validate an exemption.
		var identity *tempoSpanIdentityError
		if errors.As(err, &identity) {
			return parityRefusal(identity.class, fmt.Errorf("fixture %s: %w", c.Name, err))
		}
		return fmt.Errorf("fixture %s: %w", c.Name, err)
	}

	// Serialize the whole engine span, same contract RunRoundTrip honours.
	chdbEngineMu.Lock()
	defer chdbEngineMu.Unlock()

	db := OpenChDB(t)

	spans, err := readSeededSpans(db)
	if err != nil {
		if errors.Is(err, errTempoSeedWithoutSpanIdentity) {
			return parityRefusal(refusalSeedWithoutSpanIdentity, fmt.Errorf("fixture %s: %w", c.Name, err))
		}
		return fmt.Errorf("fixture %s: read seeded spans back: %w", c.Name, err)
	}
	if len(spans) == 0 {
		return parityRefusal(refusalEmptySpanSeed, fmt.Errorf(
			"fixture %s: seed produced no readable spans, so the reference engine would "+
				"trivially agree with any answer", c.Name,
		))
	}
	if hasDuplicateSpanIdentity(spans) {
		return parityRefusal(refusalDuplicateSpanIdentity, errors.New(
			"seed contains more than one span with the same trace and span identity, so neither engine has a stable row to compare",
		))
	}

	got, err := oracle.Evaluate(t, spans, strings.TrimSpace(query))
	if err != nil {
		// The oracle names which of its own boundaries it hit. A reference
		// engine that rejects the query text is the engine's verdict
		// (ReasonReferenceIntrinsicUnsupported); one that compiled it but
		// failed on the spans this oracle built is the oracle's own data
		// preparation falling short (ReasonOracleUntypedAttributes); a span
		// its flat model cannot represent is the fetch-layer gap
		// (ReasonReferenceFetchLayer). Anything else — a span it was never
		// fed, a trace it cannot number — stays an unclassified harness
		// error so a broken oracle cannot validate an exemption.
		switch {
		case errors.Is(err, oracle.ErrReferenceRejectedQuery):
			return parityRefusal(refusalReferenceRejectedQuery, fmt.Errorf("fixture %s: %w", c.Name, err))
		case errors.Is(err, oracle.ErrReferenceEvaluation):
			return parityRefusal(refusalReferenceEvaluation, fmt.Errorf("fixture %s: %w", c.Name, err))
		case errors.Is(err, oracle.ErrUnrepresentableSpan):
			return parityRefusal(refusalUnrepresentableSpan, fmt.Errorf("fixture %s: %w", c.Name, err))
		}
		return fmt.Errorf("fixture %s: %w", c.Name, err)
	}

	return compareSpanSets(t, c, p, got, want)
}

func hasDuplicateSpanIdentity(spans []oracle.Span) bool {
	seen := make(map[string]struct{}, len(spans))
	for _, span := range spans {
		key := span.TraceID + "\x00" + span.SpanID
		if _, ok := seen[key]; ok {
			return true
		}
		seen[key] = struct{}{}
	}
	return false
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
func rejectNarrowingSections(c *Case) error {
	for _, section := range []string{"search_window", "search_limit"} {
		if _, ok := c.Section(section); ok {
			return fmt.Errorf(
				"fixture %s carries both `parity:` and `%s:`. The reference oracle evaluates the "+
					"spanset pipeline over every seeded span; %s narrows cerberus's scan in the "+
					"storage layer, which upstream applies before the pipeline the oracle runs. "+
					"The two answers would differ about what was READ, not about the query. "+
					"Remove the `parity:` section from this fixture",
				c.Name, section, section,
			)
		}
	}
	return nil
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

// arrayColumn reads one of OTel-CH's nested `Events.*` / `Links.*` array
// columns. Like attrsColumn it goes through toJSONString, because that is
// the one projection whose result shape does not depend on the element
// type the seed happened to declare.
func arrayColumn(name string) spanColumn {
	return spanColumn{
		name:    name,
		missing: "'[]'",
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
	stringColumn("ScopeName"),
	stringColumn("ScopeVersion"),
	arrayColumn("Events.Name"),
	arrayColumn("Events.Attributes"),
	arrayColumn("Links.TraceId"),
	arrayColumn("Links.SpanId"),
	arrayColumn("Links.Attributes"),
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The round-trip runner backfills a seed's missing TraceId / SpanId
	// columns as `DEFAULT ''` (internal/testsql's BackfillTracesColumns) so
	// the canonical wrap can read them, which is why this is checked on
	// the rows that came back rather than on DESCRIBE: a seed that never
	// declared the column reads back as spans with an EMPTY identity.
	for _, span := range spans {
		if span.TraceID == "" || span.SpanID == "" {
			return nil, fmt.Errorf(
				"%w: a seeded row carries TraceId %q / SpanId %q — the seed declares no such column "+
					"(or leaves it empty), so its rows read back as the same anonymous span and the "+
					"comparison over WHICH SPANS matched has no identity to compare",
				errTempoSeedWithoutSpanIdentity, span.TraceID, span.SpanID,
			)
		}
	}
	return spans, nil
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
			nested                 nestedJSON
		)
		if err := rows.Scan(
			&s.TraceID, &s.SpanID, &s.ParentSpanID,
			&s.Name, &s.Kind, &s.StatusCode, &s.StatusMessage, &s.ServiceName,
			&durationNanos, &startNs,
			&resourceJSON, &spanJSON,
			&s.ScopeName, &s.ScopeVersion,
			&nested.eventNames, &nested.eventAttrs,
			&nested.linkTraceIDs, &nested.linkSpanIDs, &nested.linkAttrs,
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
		if s.Events, s.Links, err = nested.decode(); err != nil {
			return nil, fmt.Errorf("span %s nested records: %w", s.SpanID, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// nestedJSON is the five `Events.*` / `Links.*` columns as
// [readSeededSpans] projected them: each a JSON array, one element per
// child record. OTel-CH models events and links as PARALLEL arrays rather
// than as an array of structs, so the name at index i and the attribute
// map at index i belong to the same event.
type nestedJSON struct {
	eventNames   string
	eventAttrs   string
	linkTraceIDs string
	linkSpanIDs  string
	linkAttrs    string
}

// decode zips the parallel arrays back into records.
//
// The arrays are allowed to be of DIFFERENT lengths, and that is not
// laxity: a fixture seeds only the columns its own query reads, so a
// `{ event:name = "x" }` fixture declares `Events.Name` and no
// `Events.Attributes` at all. The record count is therefore the longest
// array, and a record a shorter array does not reach takes that field's
// zero value — the same reading the absent column itself would have
// given.
func (n nestedJSON) decode() ([]oracle.Event, []oracle.Link, error) {
	eventNames, err := decodeStringArrayJSON(n.eventNames)
	if err != nil {
		return nil, nil, fmt.Errorf("Events.Name: %w", err)
	}
	eventAttrs, err := decodeAttrsArrayJSON(n.eventAttrs)
	if err != nil {
		return nil, nil, fmt.Errorf("Events.Attributes: %w", err)
	}
	linkTraceIDs, err := decodeStringArrayJSON(n.linkTraceIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("Links.TraceId: %w", err)
	}
	linkSpanIDs, err := decodeStringArrayJSON(n.linkSpanIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("Links.SpanId: %w", err)
	}
	linkAttrs, err := decodeAttrsArrayJSON(n.linkAttrs)
	if err != nil {
		return nil, nil, fmt.Errorf("Links.Attributes: %w", err)
	}

	events := make([]oracle.Event, 0, max(len(eventNames), len(eventAttrs)))
	for i := range max(len(eventNames), len(eventAttrs)) {
		var e oracle.Event
		if i < len(eventNames) {
			e.Name = eventNames[i]
		}
		if i < len(eventAttrs) {
			e.Attrs = eventAttrs[i]
		}
		events = append(events, e)
	}

	linkCount := max(len(linkTraceIDs), max(len(linkSpanIDs), len(linkAttrs)))
	links := make([]oracle.Link, 0, linkCount)
	for i := range linkCount {
		var l oracle.Link
		if i < len(linkTraceIDs) {
			l.TraceID = linkTraceIDs[i]
		}
		if i < len(linkSpanIDs) {
			l.SpanID = linkSpanIDs[i]
		}
		if i < len(linkAttrs) {
			l.Attrs = linkAttrs[i]
		}
		links = append(links, l)
	}
	return events, links, nil
}

func decodeStringArrayJSON(raw string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("decode %q: %w", trimmed, err)
	}
	return out, nil
}

func decodeAttrsArrayJSON(raw string) ([]map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	var maps []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &maps); err != nil {
		return nil, fmt.Errorf("decode %q: %w", trimmed, err)
	}
	out := make([]map[string]string, 0, len(maps))
	for _, m := range maps {
		attrs, err := decodeAttrsJSON(string(m))
		if err != nil {
			return nil, err
		}
		out = append(out, attrs)
	}
	return out, nil
}

// decodeAttrsJSON reads one attribute map.
//
// A seed is free to declare Map(String, Int64) rather than OTel-CH's own
// Map(String, String) — several corpus fixtures do, to model a schema
// override — so a value arrives as a JSON number rather than a JSON
// string. It is rendered with its own literal text, which is the reading
// the package doc already commits to: "every attribute enters the engine
// as a string, which is the honest reading of what the column holds".
//
// Rendering rather than erroring is deliberate. A typed literal on the
// query side still will not match such an attribute, which is exactly
// what ReasonOracleUntypedAttributes describes and what those fixtures
// declare — but that is a comparison the oracle makes, visibly, rather
// than a read it refuses.
func decodeAttrsJSON(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "{}" {
		return nil, nil
	}
	scalars := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(trimmed), &scalars); err != nil {
		return nil, fmt.Errorf("decode %q: %w", trimmed, err)
	}
	attrs := make(map[string]string, len(scalars))
	for k, v := range scalars {
		text := string(v)
		var str string
		if err := json.Unmarshal(v, &str); err == nil {
			text = str
		}
		attrs[k] = text
	}
	return attrs, nil
}

// --- comparison -------------------------------------------------------

// spanRowAttrsIdx / spanRowArity are the position and width of the
// canonical span shape (SpanName, Attributes, Timestamp, Duration) inside
// an `expected_rows:` row. Distinct from parity_chdb.go's sampleColumns
// (which locates the Sample family's MetricName/Attributes/TimeUnix/Value
// shape dynamically, since that family's projection width varies by
// metric kind): a TraceQL fixture's row is always this fixed four-column
// span shape, so no dynamic location is needed here.
const (
	spanRowAttrsIdx = 1
	spanRowArity    = 4
)

// spanIdentitiesOfExpectedRows reads cerberus's answer out of
// `expected_rows:` as the set of spans it matched.
//
// It errors rather than skipping on a row it cannot identify. A TraceQL
// fixture whose projection carries no span identity — an aggregate, or a
// seed with no SpanId column — cannot be parity-checked at this layer, and
// silently comparing nothing would be the hollow green this mechanism
// exists to prevent.
func spanIdentitiesOfExpectedRows(rt *RoundTripSections, identifiesSpans bool) ([]oracle.Result, error) {
	// A zero-row answer never reaches the per-row shape check below, so a
	// non-span projection whose seed happens to match nothing would read
	// as an empty span set, agree with the reference's empty set, and pass
	// a comparison its projection cannot support — the exact trap
	// ReasonNoComparableOracle's own doc warns of. Whether the projection
	// identifies spans is a fact about the executed query, not its rows
	// (projectionIdentifiesSpans), so it is decided first.
	if len(rt.ExpectedRows) == 0 && !identifiesSpans {
		return nil, tempoSpanIdentityUnavailable(refusalNonSpanProjection, errors.New(
			"expected_rows is empty and the executed projection never writes the "+
				spanIdentitySpanIDKey+" key, so it is not the canonical span shape and carries no "+
				"span identity to compare — its empty answer must not be read as an empty span set",
		))
	}
	out := make([]oracle.Result, 0, len(rt.ExpectedRows))
	seen := make(map[oracle.Result]bool, len(rt.ExpectedRows))

	for i, row := range rt.ExpectedRows {
		if len(row) != spanRowArity {
			return nil, tempoSpanIdentityUnavailable(refusalNonSpanProjection, fmt.Errorf(
				"expected_rows[%d] has %d column(s), not the canonical span shape "+
					"(SpanName, Attributes, Timestamp, Duration); this fixture's projection "+
					"carries no span identity and cannot be parity-checked", i, len(row),
			))
		}
		// A four-column row is not necessarily the canonical span shape:
		// MetricsCompare's aggregate projection (is_selection, attr, val,
		// Value) and MetricsAggregate's by/without projection (one column
		// per grouping key, then Value) both also have arity 4, and their
		// second column is whatever the user's second grouping expression
		// is — never an Attributes object. A non-object second column is
		// ambiguous on its own, though — it is equally what a genuinely
		// malformed span-shaped fixture's Attributes cell looks like — so
		// the column's decoded type alone cannot tell the two apart, and
		// neither can naming one specific emitter's own column, because a
		// third non-canonical shape would just repeat this bug. What does
		// generalize is outerSelectClaimsAttributesColumn: every canonical
		// span projection's OWN (outermost) SELECT aliases this position
		// literally `Attributes` — internal/api/tempo/handler.go and every
		// span-shaped chplan lowering agree on that name — so its absence
		// from the fixture's own emitted SQL is positive, structural
		// evidence the projection never claimed to carry Attributes here
		// at all. Its presence (or unknown SQL, as in a caller that never
		// populated rt.SQL) means a row that IS trying to be the canonical
		// shape has a broken Attributes cell, which stays an unclassified
		// error — like a row whose second column genuinely IS an object
		// but fails to decode (a malformed attribute value), which falls
		// through to rowAttrs below — so a broken decoder, or a caller
		// that can't prove the projection is legitimately different, can
		// never manufacture liveness evidence for a stale exemption.
		if _, ok := row[spanRowAttrsIdx].(map[string]any); !ok {
			if rt.SQL != "" && !outerSelectClaimsAttributesColumn(rt.SQL) {
				return nil, tempoSpanIdentityUnavailable(refusalNonSpanProjection, fmt.Errorf(
					"expected_rows[%d] column %d (where the canonical span shape carries Attributes) "+
						"is %T, not an object; this fixture's projection is not the canonical span shape "+
						"(SpanName, Attributes, Timestamp, Duration) and carries no span identity to compare",
					i, spanRowAttrsIdx, row[spanRowAttrsIdx],
				))
			}
			return nil, fmt.Errorf(
				"expected_rows[%d] column %d (where the canonical span shape carries Attributes) "+
					"is %T, not an object", i, spanRowAttrsIdx, row[spanRowAttrsIdx],
			)
		}
		attrs, err := rowAttrs(row[spanRowAttrsIdx], i)
		if err != nil {
			return nil, err
		}
		traceID, hasTrace := attrs[spanIdentityTraceIDKey]
		spanID, hasSpan := attrs[spanIdentitySpanIDKey]
		if !hasTrace || !hasSpan {
			// A canonical span row always carries BOTH keys (the wrap
			// writes them from the seeded columns, empty or not). A row
			// missing one is a per-trace aggregate projection — `| by(...)`,
			// `| count() > N` — whose Attributes are the trace's, not a
			// span's: a non-span shape, never an identity-less seed.
			return nil, tempoSpanIdentityUnavailable(refusalNonSpanProjection, fmt.Errorf(
				"expected_rows[%d] carries no %s/%s pair; this projection aggregates per trace and "+
					"identifies no span, so there is no set of matched spans to compare",
				i, spanIdentityTraceIDKey, spanIdentitySpanIDKey,
			))
		}
		if traceID == "" || spanID == "" {
			return nil, tempoSpanIdentityUnavailable(refusalSeedWithoutSpanIdentity, fmt.Errorf(
				"expected_rows[%d] carries an empty %s/%s identity. The comparison is over WHICH SPANS "+
					"matched, so a fixture whose seed declares no TraceId/SpanId columns (or leaves them "+
					"empty) cannot be enrolled — every row would be the same anonymous span",
				i, spanIdentityTraceIDKey, spanIdentitySpanIDKey,
			))
		}

		r := oracle.Result{TraceID: traceID, SpanID: spanID}
		if seen[r] {
			return nil, tempoSpanIdentityUnavailable(refusalDuplicateSpanIdentity, fmt.Errorf(
				"expected_rows[%d] repeats span %s/%s. Span identity is the comparison key, so a "+
					"projection that emits one span twice cannot be matched against a set of "+
					"matched spans", i, traceID, spanID,
			))
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

// projectionIdentifiesSpans reports whether the query the round trip
// executed writes a per-span identity into every answer row. The canonical
// span wrap (internal/api/tempo's ProjectSamples) binds the
// spanIdentitySpanIDKey map key as a query argument; a per-trace aggregate
// or metrics projection never does. The fixture's `args_optimized` section
// is the argument list of exactly the SQL RunRoundTripSQL executed (and
// spec.Match has already verified it against the live lowering before
// RunParity runs), which is what makes this a structural fact independent
// of how many rows the seed happened to match.
func projectionIdentifiesSpans(c *Case) bool {
	for _, section := range []string{"args_optimized", "args"} {
		if body, ok := c.Section(section); ok {
			return strings.Contains(body, strconv.Quote(spanIdentitySpanIDKey))
		}
	}
	return false
}

// compareSpanSets is the assertion itself: the reference engine and
// cerberus must have matched exactly the same spans.
func compareSpanSets(t *testing.T, c *Case, p *Parity, got, want []oracle.Result) error {
	t.Helper()

	if len(got) != len(want) {
		return parityDisagreement(fmt.Errorf(
			"fixture %s: reference engine matched %d span(s), cerberus %d.\n"+
				"  reference: %v\n  cerberus:  %v\n"+
				"This is a real disagreement about the answer, not a golden to regenerate — "+
				"there is no update path for this check",
			c.Name, len(got), len(want), got, want,
		))
	}

	var mismatches []string
	for i := range got {
		if got[i] != want[i] {
			mismatches = append(mismatches, fmt.Sprintf(
				"fixture %s span %d: the two engines matched different spans\n"+
					"  reference: %v\n  cerberus:  %v\n"+
					"  full reference set: %v\n  full cerberus set:  %v",
				c.Name, i, got[i], want[i], got, want,
			))
		}
	}

	if !p.ComparesInFull() {
		t.Logf("fixture %s compared with scope %q", c.Name, p.Scope)
	}
	if len(mismatches) > 0 {
		return parityDisagreement(fmt.Errorf("%s", strings.Join(mismatches, "\n")))
	}
	return nil
}
