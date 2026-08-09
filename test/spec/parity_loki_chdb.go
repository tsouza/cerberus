//go:build chdb_agpl_oracle

// Package spec — the Loki half of the parity layer.
//
// This file is the ONLY thing in package spec that reaches the AGPLv3
// Loki engine, and it is compiled only under the synthetic
// `chdb_agpl_oracle` tag that CI sets alongside `chdb` and `agpl_oracle`
// (see .github/workflows/chdb.yml's roundtrip matrix). parity_chdb.go
// reaches it through a nil-by-default function variable, so the plain
// `chdb` build configuration links no AGPL code at all and a
// Loki-enrolled fixture in a lane without the tag fails loudly instead
// of being silently unchecked.
//
// # What the reference can see, and what it cannot
//
// Upstream's in-process querier (logql.NewMockQuerier) processes every
// entry with `labels.EmptyLabels()` for structured metadata, so entry
// StructuredMetadata is DISCARDED before the pipeline ever runs. A
// fixture whose seed populates LogAttributes — cerberus's
// structured-metadata carrier — would therefore have the two engines
// answering over different data, and so would one that populates
// SeverityText, which cerberus folds into the synthesised
// `detected_level` label and into `level` grouping keys. Neither has any
// counterpart the reference can be handed.
//
// [readSeededStreams] refuses such a fixture with a named error rather
// than translating a subset of the row and comparing anyway. A quiet
// subset comparison is the exact hollow green the parity layer exists to
// eliminate: it would run, pass, and prove nothing about the columns it
// dropped.
package spec

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	oracle "github.com/tsouza/cerberus/test/spec/parityoracle/logql"
)

func init() { lokiParityEvaluator = evaluateLokiParity }

// logsTable is the single table a LogQL fixture's seed populates. LogQL
// has one source relation, unlike PromQL's per-type metric tables, so the
// reader names it directly rather than unioning a set.
const logsTable = "otel_logs"

// Column names in the OTel-CH logs layout that this reader understands.
// They are spelled here rather than imported from internal/schema on
// purpose: an oracle that took its column names from the system under
// test would agree with it about the layout by construction, and the
// disjointness gate in test/regression rejects the import outright.
const (
	colTimestamp          = "Timestamp"
	colBody               = "Body"
	colResourceAttributes = "ResourceAttributes"
	colLogAttributes      = "LogAttributes"
	colSeverityText       = "SeverityText"
)

// evaluateLokiParity answers q with the real upstream Loki engine over
// the rows the seed actually landed in chDB.
func evaluateLokiParity(
	t *testing.T, db *sql.DB, c *Case, q parityQuery,
) ([]referenceSample, error) {
	t.Helper()

	streams, err := readSeededStreams(db)
	if err != nil {
		return nil, err
	}
	if len(streams) == 0 {
		return nil, fmt.Errorf(
			"fixture %s: seed produced no readable streams, so the reference engine would "+
				"trivially agree with any answer", c.Name,
		)
	}

	got, err := oracle.Evaluate(t, streams, oracle.Query{
		Expr:  q.Expr,
		Start: q.Start,
		End:   q.End,
		Step:  q.Step,
	})
	if err != nil {
		return nil, err
	}

	out := make([]referenceSample, 0, len(got))
	for _, r := range got {
		out = append(out, referenceSample{Labels: r.Labels, TMillis: r.TMillis, Value: r.Value})
	}
	return out, nil
}

// readSeededStreams reads the seeded rows back OUT of chDB and groups
// them into reference-engine streams.
//
// Reading back rather than re-parsing the `seed:` text is deliberate: it
// shows the oracle the data as it ACTUALLY LANDED — after DEFAULTs, after
// coercion — instead of as the SQL claims it will land. Otherwise a
// disagreement about what the seed means would be invisible to exactly
// the check meant to find disagreements.
func readSeededStreams(db *sql.DB) ([]oracle.Stream, error) {
	present, err := logsTableColumns(db)
	if err != nil {
		return nil, err
	}
	for _, required := range []string{colTimestamp, colBody, colResourceAttributes} {
		if !present[required] {
			return nil, fmt.Errorf(
				"fixture seeds %s without a %s column, so its rows cannot be expressed as Loki "+
					"streams (label set, timestamp, line). This fixture cannot be parity-checked "+
					"against the Loki engine", logsTable, required,
			)
		}
	}

	// The Map column round-trips through toJSONString because chdb-go's
	// native Map scan panics inside parquet-go (see roundtrip.go).
	projection := []string{
		"toUnixTimestamp64Milli(`" + colTimestamp + "`)",
		"`" + colBody + "`",
		"toJSONString(`" + colResourceAttributes + "`)",
	}
	opaque := []string{}
	for _, col := range []string{colLogAttributes, colSeverityText} {
		if !present[col] {
			continue
		}
		opaque = append(opaque, col)
		projection = append(projection, "toString(`"+col+"`)")
	}

	//nolint:gosec // every fragment comes from the constants above, not from fixture text.
	query := "SELECT " + strings.Join(projection, ", ") +
		" FROM `" + logsTable + "` ORDER BY `" + colTimestamp + "`"
	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("read seeded rows back: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byKey := map[string]*oracle.Stream{}
	for rows.Next() {
		var tsMillis int64
		var body, attrsJSON string
		opaqueVals := make([]string, len(opaque))
		dest := []any{&tsMillis, &body, &attrsJSON}
		for i := range opaqueVals {
			dest = append(dest, &opaqueVals[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan seeded row: %w", err)
		}
		for i, col := range opaque {
			if err := rejectOpaqueColumn(col, opaqueVals[i]); err != nil {
				return nil, err
			}
		}

		lbls, err := labelsFromResourceAttributes(attrsJSON)
		if err != nil {
			return nil, err
		}
		key := labelKey(lbls)
		s, ok := byKey[key]
		if !ok {
			s = &oracle.Stream{Labels: lbls}
			byKey[key] = s
		}
		s.Entries = append(s.Entries, oracle.Entry{TMillis: tsMillis, Line: body})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read seeded rows back: %w", err)
	}

	out := make([]oracle.Stream, 0, len(byKey))
	for _, s := range byKey {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		return labelKey(out[i].Labels) < labelKey(out[j].Labels)
	})
	return out, nil
}

// emptyMapJSON is what `toJSONString` renders an empty CH Map as. A
// column holding it carries no information, so it is not an obstacle to
// parity.
const emptyMapJSON = "{}"

// rejectOpaqueColumn fails a fixture whose seed populated a column the
// reference engine cannot be shown.
//
// This is not a scope knob and there is no way to wave it through: the
// data is genuinely unreachable from upstream's in-process querier, so a
// comparison over the remaining columns would be a comparison of two
// different questions.
func rejectOpaqueColumn(column, value string) error {
	if value == "" || value == emptyMapJSON {
		return nil
	}
	return fmt.Errorf(
		"seeded rows carry a non-empty %s (%q), which the reference engine cannot observe: "+
			"upstream's in-process querier processes every entry with an EMPTY structured-metadata "+
			"label set, and cerberus additionally folds %s into the synthesised `detected_level` "+
			"label. Comparing the two answers would compare two different questions, so this "+
			"fixture cannot be enrolled against the Loki oracle",
		column, value, colSeverityText,
	)
}

// labelsFromResourceAttributes builds the reference-engine stream label
// set for one seeded row.
//
// ResourceAttributes is the whole of it. The OTel-CH top-level columns
// (SeverityText, TraceId, ServiceName and friends) are deliberately NOT
// synthesised into labels here: deciding which column becomes which label
// is precisely the schema mapping cerberus's lowering makes, and
// reproducing that decision inside the oracle would make the oracle agree
// with the lowering by construction on exactly the axis a schema bug
// would move.
func labelsFromResourceAttributes(attrsJSON string) (map[string]string, error) {
	out := map[string]string{}
	trimmed := strings.TrimSpace(attrsJSON)
	if trimmed == "" || trimmed == emptyMapJSON {
		return out, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("decode %s %q: %w", colResourceAttributes, attrsJSON, err)
	}
	return out, nil
}

// logsTableColumns reports which columns the fixture's seed DDL actually
// declared. Seeds declare only the columns their own query needs, so the
// reader adapts to the table in front of it rather than assuming the full
// OTel-CH layout.
func logsTableColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(
		"SELECT name FROM system.columns WHERE database = currentDatabase() AND table = ?",
		logsTable,
	)
	if err != nil {
		return nil, fmt.Errorf("read %s column list: %w", logsTable, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan %s column name: %w", logsTable, err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s column list: %w", logsTable, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"seed created no %s table, so there is nothing for the reference engine to read",
			logsTable,
		)
	}
	return out, nil
}
