package testsql

import "strings"

// logsTableName is schema.DefaultOTelLogs().LogsTable, mirrored here so this
// build-tag-free seed helper stays independent of internal/schema.
const logsTableName = "otel_logs"

// logsBackfilledColumns are the default OTel-CH columns the production
// LogQL log-stream wire projection reads regardless of the fixture's own raw
// pre-wrap SQL. Spec seeds deliberately declare only the columns their raw
// lowering touches, so strict-scan's post-ProjectSamples reconstruction needs
// these defaults added before it can exercise the real cursor shape.
var logsBackfilledColumns = []backfilledColumn{
	{name: "Timestamp", ddl: "Timestamp DateTime64(9) DEFAULT toDateTime64(0, 9)"},
	{name: "Body", ddl: "Body String DEFAULT ''"},
	{name: "SeverityText", ddl: "SeverityText LowCardinality(String) DEFAULT ''"},
	{name: "ResourceAttributes", ddl: "ResourceAttributes Map(String, String) DEFAULT map()"},
	{name: "LogAttributes", ddl: "LogAttributes Map(String, String) DEFAULT map()"},
}

// BackfillLogsColumns adds the production columns needed by LogQL's
// post-ProjectSamples log-row projection and preserves positional INSERTs by
// rewriting them with their original explicit column list.
func BackfillLogsColumns(stmts []string) []string {
	cols := map[string][]string{}
	out := make([]string, 0, len(stmts))
	for _, stmt := range stmts {
		if table, colNames, body, ok := parseLogsCreate(stmt); ok {
			cols[table] = colNames
			out = append(out, body)
			continue
		}
		if rewritten, ok := rewriteBackfilledInsert(stmt, cols); ok {
			out = append(out, rewritten)
			continue
		}
		out = append(out, stmt)
	}
	return out
}

func parseLogsCreate(stmt string) (table string, colNames []string, rewritten string, ok bool) {
	trimmed := stripLeadingNoise(stmt)
	prefix := stmt[:len(stmt)-len(trimmed)]
	rest, ok := createTableTail(trimmed)
	if !ok {
		return "", nil, "", false
	}
	name := strings.ToLower(strings.TrimSpace(firstToken(rest)))
	if name != logsTableName {
		return "", nil, "", false
	}
	open := strings.IndexByte(trimmed, '(')
	if open < 0 {
		return "", nil, "", false
	}
	closeParen := matchParen(trimmed, open)
	if closeParen < 0 {
		return "", nil, "", false
	}
	defs := splitTopLevelCommas(trimmed[open+1 : closeParen])
	declared := make(map[string]bool, len(defs))
	names := make([]string, 0, len(defs))
	normalizedDefs := make([]string, len(defs))
	changed := false
	for i, d := range defs {
		normalized := normalizeLogsColumnDef(d)
		if normalized != d {
			changed = true
		}
		normalizedDefs[i] = normalized
		column := firstToken(strings.TrimSpace(normalized))
		if column != "" {
			declared[column] = true
			names = append(names, column)
		}
	}
	missing := make([]string, 0, len(logsBackfilledColumns))
	for _, column := range logsBackfilledColumns {
		if !declared[column.name] {
			missing = append(missing, " "+column.ddl)
		}
	}
	if len(missing) == 0 && !changed {
		return "", nil, "", false
	}
	newDefs := append(append(make([]string, 0, len(normalizedDefs)+len(missing)), normalizedDefs...), missing...)
	rewritten = prefix + trimmed[:open+1] + strings.Join(newDefs, ",") + trimmed[closeParen:]
	return name, names, rewritten, true
}

// normalizeLogsColumnDef replaces fixture-only MATERIALIZED definitions on
// columns the production log-row wrap reads with DEFAULT. ClickHouse excludes
// MATERIALIZED columns from SELECT *, while production's real otel_logs schema
// exposes these columns normally. DEFAULT preserves the fixture's derived
// value but keeps it visible to the reconstructed outer projection.
func normalizeLogsColumnDef(def string) string {
	switch firstToken(strings.TrimSpace(def)) {
	case "Timestamp", "Body", "SeverityText", "ResourceAttributes", "LogAttributes":
		return strings.Replace(def, " MATERIALIZED ", " DEFAULT ", 1)
	default:
		return def
	}
}
