package loki

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// defaultDetectedFieldsLineLimit caps the number of log rows the
// detected-fields heuristic peeks at when no `line_limit` is supplied.
// Loki's own default is 1000.
const defaultDetectedFieldsLineLimit = 1000

// defaultDetectedFieldsLimit caps the number of fields returned. The
// upstream default is 1000 — typical log payloads top out far below
// this, the limit just defends Grafana's autocomplete from a misbehaving
// payload exposing thousands of unique keys.
const defaultDetectedFieldsLimit = 1000

// DetectedField is one entry in the /detected_fields response. Mirrors
// the upstream Loki shape Grafana expects.
type DetectedField struct {
	Label       string `json:"label"`
	Type        string `json:"type"`
	Cardinality uint64 `json:"cardinality"`
}

// DetectedFieldsData is the body of a /loki/api/v1/detected_fields
// response. `fields` holds the heuristic results; `limit` and
// `line_limit` echo what the handler actually applied.
type DetectedFieldsData struct {
	Fields    []DetectedField `json:"fields"`
	Limit     int             `json:"limit"`
	LineLimit int             `json:"line_limit"`
}

// handleDetectedFields implements GET /loki/api/v1/detected_fields. The
// upstream Loki feature peeks at the first N matching rows and returns
// the JSON / logfmt keys it can extract from each Body. Cerberus's
// implementation matches the contract:
//
//   - SQL fetches Body (most-recent-first, capped at line_limit).
//   - Per-row JSON and logfmt parsing happens in Go (post-process).
//   - Each unique field name is reported with the dominant scalar type
//     (string / int / float / bool / duration / bytes) and cardinality.
//
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#detected-fields
func (h *Handler) handleDetectedFields(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	lineLimit, err := parsePositiveInt(r.URL.Query().Get("line_limit"), defaultDetectedFieldsLineLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	limit, err := parsePositiveInt(r.URL.Query().Get("limit"), defaultDetectedFieldsLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	sqlStr, args, err := buildDetectedFieldsSQL(h.Schema, matchers, start, end, lineLimit)
	if err != nil {
		h.respondError(w, &apiError{kind: ErrInternal, err: err, status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki detected_fields", "logql", q, "sql", sqlStr, "args", args)

	lines, err := h.Client.QueryStrings(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Warn("cerberus loki detected_fields CH query failed", "err", err.Error(), "sql", sqlStr)
		h.respondError(w, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway})
		return
	}

	fields := detectFields(lines, limit)

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data: DetectedFieldsData{
			Fields:    fields,
			Limit:     limit,
			LineLimit: lineLimit,
		},
	})
}

// buildDetectedFieldsSQL renders:
//
//	SELECT `Body` AS line
//	FROM `otel_logs`
//	WHERE <matchers> AND <time bounds>
//	ORDER BY `Timestamp` DESC
//	LIMIT <lineLimit>
//
// The peek window is small (1000 rows by default) — CH executes this as
// a top-N scan on the primary key, comparable to /index/stats.
func buildDetectedFieldsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time, lineLimit int) (string, []any, error) {
	sb := chsql.NewSelect().
		Select(aliased(chsql.Col(s.BodyColumn), "line")).
		From(chsql.Col(s.LogsTable))

	pred := logql.SelectorPredicate(matchers, s)
	if pred != nil {
		whereFrag, err := exprFrag(pred)
		if err != nil {
			return "", nil, err
		}
		sb.Where(whereFrag)
	}
	if !start.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, ">=", start))
	}
	if !end.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, "<=", end))
	}
	sb.OrderBy(chsql.Col(s.TimestampColumn), true).
		Limit(lineLimit)

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// detectFields runs the heuristic over the peek window. For each line:
//
//  1. attempt JSON object parse — every top-level scalar value becomes
//     a field whose type is inferred from the JSON kind.
//  2. attempt logfmt parse (key=value pairs) — each value is sniffed
//     for int / float / bool / duration / bytes / string.
//
// Field types collapse via `mergeType`: identical → preserved;
// otherwise downgraded to "string". Returns the field list sorted by
// label name, truncated at limit.
func detectFields(lines []string, limit int) []DetectedField {
	type acc struct {
		typ    string
		values map[string]struct{}
	}
	fields := map[string]*acc{}

	addValue := func(name, typ, raw string) {
		a, ok := fields[name]
		if !ok {
			a = &acc{typ: typ, values: map[string]struct{}{}}
			fields[name] = a
		} else {
			a.typ = mergeType(a.typ, typ)
		}
		// Cap the per-field value set to defend against blow-up on
		// high-cardinality fields (request_id, etc.) — we only need the
		// cardinality COUNT, not the actual values.
		if len(a.values) < 1024 {
			a.values[raw] = struct{}{}
		}
	}

	for _, line := range lines {
		// JSON object detection.
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{") {
			obj := map[string]json.RawMessage{}
			if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
				for k, raw := range obj {
					typ, val := classifyJSON(raw)
					addValue(k, typ, val)
				}
				// JSON wins — don't double-parse as logfmt.
				continue
			}
		}
		// Logfmt: reuse Loki's own parser so the result matches what
		// `| logfmt` would yield in a pipeline.
		extracted, ok := parseLogfmt(line)
		if !ok {
			continue
		}
		for k, v := range extracted {
			addValue(k, classifyScalar(v), v)
		}
	}

	out := make([]DetectedField, 0, len(fields))
	for k, a := range fields {
		out = append(out, DetectedField{
			Label:       k,
			Type:        a.typ,
			Cardinality: uint64(len(a.values)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// classifyJSON returns (type-tag, stringified-value) for a JSON token.
// Object / array values are stringified as their raw JSON; scalar
// strings unwrap the quoted form so cardinality dedup works on the
// payload, not the quoted form.
func classifyJSON(raw json.RawMessage) (string, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "string", ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return classifyScalar(s), s
		}
		return "string", trimmed
	case 't', 'f':
		return "boolean", trimmed
	case 'n':
		return "string", trimmed
	case '{', '[':
		return "string", trimmed
	}
	// numeric: int vs float.
	if _, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		return "int", trimmed
	}
	if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return "float", trimmed
	}
	return "string", trimmed
}

// classifyScalar sniffs a string value and picks the best-fit type tag.
// The vocabulary matches Loki's documented set: string / int / float /
// boolean / duration / bytes.
func classifyScalar(v string) string {
	if v == "" {
		return "string"
	}
	if v == "true" || v == "false" {
		return "boolean"
	}
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return "int"
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return "float"
	}
	if _, err := time.ParseDuration(v); err == nil {
		return "duration"
	}
	if isBytesLiteral(v) {
		return "bytes"
	}
	return "string"
}

// isBytesLiteral detects payload-size suffixes like "10KB", "1.5MiB".
// Conservatively gated on a small whitelist — anything fancier and the
// caller falls back to "string".
func isBytesLiteral(v string) bool {
	suffixes := []string{"B", "KB", "MB", "GB", "TB", "KiB", "MiB", "GiB", "TiB"}
	for _, suf := range suffixes {
		if !strings.HasSuffix(v, suf) {
			continue
		}
		num := strings.TrimSuffix(v, suf)
		if num == "" {
			continue
		}
		if _, err := strconv.ParseFloat(num, 64); err == nil {
			return true
		}
	}
	return false
}

// mergeType collapses two type tags observed for the same field across
// rows. Identical → preserved; mismatched → "string" (the universal
// downgrade). Anything else: pick the loser deterministically.
func mergeType(a, b string) string {
	if a == b {
		return a
	}
	return "string"
}

// parseLogfmt runs Loki's logfmt parser over a line. Returns the
// extracted (key, value) map plus true if the parser accepted the line.
// The parser's hint argument lets it skip records it can't classify;
// we want every key, so the hint is unset.
func parseLogfmt(line string) (map[string]string, bool) {
	parser := loglib.NewLogfmtParser(false, false)
	lbs := loglib.NewBaseLabelsBuilder().ForLabels(labels.EmptyLabels(), 0)
	_, ok := parser.Process(0, []byte(line), lbs)
	if !ok {
		return nil, false
	}
	out := map[string]string{}
	lbs.LabelsResult().Labels().Range(func(l labels.Label) {
		out[l.Name] = l.Value
	})
	return out, len(out) > 0
}

// parsePositiveInt parses an optional integer query parameter. Empty
// input returns the default; non-numeric or non-positive input is
// rejected with a 400.
func parsePositiveInt(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("parameter must be a positive integer")
	}
	return n, nil
}
