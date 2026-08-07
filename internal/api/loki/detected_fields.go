package loki

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/axiomhq/hyperloglog"
	"github.com/dustin/go-humanize"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
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

// maxLogPeekLineLimit hard-caps `line_limit` on the metadata peek endpoints
// (/detected_fields, /patterns). The peek SQL is `... ORDER BY Timestamp DESC
// LIMIT line_limit` and the whole result is buffered into a Go slice with no
// streaming, so an unclamped `line_limit` (the param accepts up to 2^31-1)
// lets a single request OOM the process — max_memory_usage bounds ClickHouse,
// not the cerberus heap. This clamp caps the row COUNT the SQL LIMIT returns
// (and thus the buffered slice): 10k newest lines is 10x the default and ample
// for a field/pattern heuristic. It removes the unbounded-row OOM; the
// absolute heap still scales with line SIZE × concurrency, which chclient's
// maxLogPeekBytes backstop bounds hard on the byte axis, alongside the
// client-wide per-drain row budget (drainBudgetExceeded, Config.MaxQuerySamples).
// Mirrors the parseLogLimit/maxLogQueryLimit clamp on the log path.
const maxLogPeekLineLimit = 10_000

// maxDetectedFieldsLimit hard-caps the returned-field count. Each tracked
// field holds a HyperLogLog sketch (~16 KiB), so an unclamped field limit over
// a pathological many-key payload grows the parsedField map without bound.
// 10k fields is far above any real log schema and bounds the sketch memory.
const maxDetectedFieldsLimit = 10_000

// DetectedField is one entry in the /detected_fields response. The
// JSON tags mirror upstream Loki's logproto.DetectedField exactly
// (pkg/logproto/logproto.pb.go): label / type / cardinality are
// omitempty, `parsers` is ALWAYS emitted (null when the field came
// from structured metadata only — upstream nils the slice out before
// marshalling), and `jsonPath` carries the original JSON path
// components when the json parser extracted the field.
type DetectedField struct {
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type,omitempty"`
	Cardinality uint64   `json:"cardinality,omitempty"`
	Parsers     []string `json:"parsers"`
	JSONPath    []string `json:"jsonPath,omitempty"`
}

// DetectedFieldsResponse is the body of a /loki/api/v1/detected_fields
// response. Upstream Loki serializes logproto.DetectedFieldsResponse
// BARE at the top level (pkg/util/marshal/marshal.go,
// WriteDetectedFieldsResponseJSON writes the struct verbatim via
// jsoniter) — there is NO {status, data} envelope on this endpoint.
// Grafana's Logs Drilldown reads `body.fields` directly; wrapping the
// payload renders every service page with "Fields: 0".
//
// `limit` echoes the applied field cap, and — mirroring upstream —
// is only set when at least one field was detected ("otherwise all
// they get is the field limit, which is a bit confusing", per the
// upstream handler).
// `values` carries the per-field value breakdown served by the sibling
// /detected_field/{name}/values route. Upstream reuses this same
// response struct for both endpoints (logproto.DetectedFieldsResponse
// field 3) rather than defining a second one, so cerberus does too.
type DetectedFieldsResponse struct {
	Fields []DetectedField `json:"fields,omitempty"`
	Limit  uint32          `json:"limit,omitempty"`
	Values []string        `json:"values,omitempty"`
}

// handleDetectedFields implements GET /loki/api/v1/detected_fields. The
// upstream Loki feature peeks at the first N matching rows (newest
// first) and reports every field it can derive from each record.
// Cerberus mirrors the upstream frontend implementation
// (pkg/querier/queryrange/detected_fields.go) on top of the OTel-CH
// schema:
//
//   - SQL fetches (Body, LogAttributes, ResourceAttributes) newest
//     first, capped at line_limit, PLUS the `| logfmt` and `| json`
//     parser-stage extractions evaluated by ClickHouse itself.
//   - LogAttributes is the structured-metadata source: every key
//     becomes a field with a nil parser list (Loki's OTLP ingestion
//     maps log-record attributes to structured metadata, so the
//     OTel-CH LogAttributes map is the same data on the CH side).
//   - The body-parsed fields come from those CH-side extractions, json
//     first with a logfmt fallback — the per-field `parsers` list
//     records which one hit, and json-extracted fields carry their
//     original `jsonPath`. Because they are the query path's OWN
//     expressions, every advertised field is one a LogQL query can
//     read back (see [rowParsedFields]).
//   - Types follow upstream determineType (int → float → boolean →
//     duration → bytes → string), re-detected per record so the last
//     record processed wins — exactly the upstream loop.
//   - Cardinality is a hyperloglog estimate over the observed values
//     (same sketch library upstream uses, so estimates match a
//     reference Loki fed the same records).
//
// [Handler.handleDetectedFieldValues] serves the sibling
// /detected_field/{name}/values route off the same inventory.
//
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#detected-fields
func (h *Handler) handleDetectedFields(w http.ResponseWriter, r *http.Request) {
	rows, limit, ok := h.detectedFieldsPeek(w, r, "detected_fields")
	if !ok {
		return
	}

	fields := detectFields(rows, limit)

	resp := DetectedFieldsResponse{Fields: fields}
	// Mirror upstream: the limit is echoed only when fields exist.
	// parsePositiveInt31 already bounds limit, but the guard is
	// restated on the SAME variable so both gosec G115 and CodeQL
	// go/incorrect-integer-conversion can prove the uint32 conversion
	// locally (neither follows the bound across the helper call).
	if len(fields) > 0 && limit > 0 && limit <= math.MaxInt32 {
		resp.Limit = uint32(limit)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDetectedFieldValues implements GET/POST
// /loki/api/v1/detected_field/{name}/values — the sibling route
// Grafana's Logs Drilldown calls when a user opens one of the fields
// [Handler.handleDetectedFields] advertised, to populate that field's
// value breakdown.
//
// It answers from the SAME peek rows and the SAME field derivation as
// the fields route, so the two can never disagree about which fields
// exist or what a field is called: a name that /detected_fields reports
// resolves here, and a name it does not report yields an empty value
// list rather than a 404 (upstream returns an empty body too).
//
// Upstream reuses logproto.DetectedFieldsResponse for this route,
// populating only `values` (pkg/querier/queryrange/detected_fields.go,
// parseDetectedFieldValues). Byte-suffixed values from a parser stage
// are canonicalised through humanize the way upstream does — see
// [humanizeByteValue] — because the Loki differential harness diffs this
// endpoint and a spelling difference there is a real parity failure.
//
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#detected-field-values
func (h *Handler) handleDetectedFieldValues(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing field name in path"))
		return
	}

	rows, limit, ok := h.detectedFieldsPeek(w, r, "detected_field_values")
	if !ok {
		return
	}

	values := detectFieldValues(rows, name, limit)

	resp := DetectedFieldsResponse{Values: values}
	// Same restated bound as the fields route — see there.
	if len(values) > 0 && limit > 0 && limit <= math.MaxInt32 {
		resp.Limit = uint32(limit)
	}
	writeJSON(w, http.StatusOK, resp)
}

// detectedFieldsPeek parses the parameters both detected-field routes
// take (query / start / end / line_limit / limit), runs the peek SQL and
// returns its rows together with the applied limit. It writes the error
// response itself; ok reports whether the caller may proceed.
//
// Both routes share this one path deliberately: the values route must
// answer for exactly the inventory the fields route advertises, and a
// second query shape would be a second declaration of the same fact —
// the drift that produced #1888.
func (h *Handler) detectedFieldsPeek(w http.ResponseWriter, r *http.Request, route string) (rows []chclient.DetectedFieldRow, limit int, ok bool) {
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return nil, 0, false
	}
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return nil, 0, false
	}

	lineLimit, err := parsePositiveInt31(r.FormValue("line_limit"), defaultDetectedFieldsLineLimit, maxLogPeekLineLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return nil, 0, false
	}
	limit, err = parsePositiveInt31(r.FormValue("limit"), defaultDetectedFieldsLimit, maxDetectedFieldsLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return nil, 0, false
	}

	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return nil, 0, false
	}

	sqlStr, args, err := buildDetectedFieldsSQL(h.Schema, matchers, start, end, lineLimit)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return nil, 0, false
	}
	h.Logger.Debug("cerberus loki "+route, "logql", telemetry.SanitizeForLog(q), "sql", sqlStr, "args", telemetry.SanitizeArgsForLog(args))

	rows, err = h.Client.QueryDetectedFieldRows(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki "+route+" CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, classifyMetadataErr(err))
		return nil, 0, false
	}
	return rows, limit, true
}

// detectFieldValues collects the distinct observed values of one field
// across the peek window, capped at limit. It reads the field from the
// same two sources [detectFields] does, in the same precedence order:
// structured metadata first, then the body-parsed labels of whichever
// parser stage claimed the row.
//
// Empty values are skipped — a CH Map read of an absent key yields "",
// so an empty string here is indistinguishable from "field not present
// on this row", and offering it as a value would build a filter that
// matches nothing. The result is sorted for determinism (upstream
// iterates a Go map and returns whatever order it gets).
//
// A byte-suffixed PARSED value is canonicalised by [humanizeByteValue],
// which is what upstream's parseDetectedFieldValues does; structured
// metadata is never rewritten, matching upstream's split.
func detectFieldValues(rows []chclient.DetectedFieldRow, name string, limit int) []string {
	// No size hint: limit is request-supplied, and pre-allocating against a
	// caller-chosen bound hands the client the allocation size. The map
	// grows to whatever the peek window actually yields, which is bounded
	// by the rows read, not by the parameter.
	seen := make(map[string]struct{})
	out := []string{}

	// add reports whether collection should continue.
	add := func(v string) bool {
		if v == "" {
			return true
		}
		if _, dup := seen[v]; dup {
			return true
		}
		if len(out) >= limit {
			return false
		}
		seen[v] = struct{}{}
		out = append(out, v)
		return true
	}

	for _, row := range rows {
		if v, present := format.NormalizeLabelMap(row.Attributes)[name]; present {
			if !add(v) {
				break
			}
		}
		parsedLabels, _ := rowParsedFields(row)
		if v, present := parsedLabels[name]; present {
			if !add(humanizeByteValue(v)) {
				break
			}
		}
	}

	sort.Strings(out)
	return out
}

// byteValueUnits is upstream's allowedBytesUnits verbatim
// (pkg/querier/queryrange/detected_fields.go). humanize.ParseBytes is
// permissive enough to read "200" as 200 bytes, so a bare number would
// otherwise be rewritten into a byte quantity; the suffix test is what
// keeps a plain integer a plain integer.
var byteValueUnits = []string{"b", "kib", "kb", "mib", "mb", "gib", "gb", "tib", "tb", "pib", "pb", "eib", "eb"}

// humanizeByteValue canonicalises a byte-suffixed value to the spelling
// a LogQL byte comparison takes ("1024B" → "1.0kB"), matching upstream's
// parseDetectedFieldValues. The space humanize.Bytes inserts is removed
// because LogQL's byte literal grammar has no space in it.
//
// Anything without a byte unit, or with one humanize cannot parse, is
// returned untouched.
func humanizeByteValue(v string) string {
	lower := strings.ToLower(v)
	hasUnit := false
	for _, u := range byteValueUnits {
		if strings.HasSuffix(lower, u) {
			hasUnit = true
			break
		}
	}
	if !hasUnit {
		return v
	}
	n, err := humanize.ParseBytes(v)
	if err != nil {
		return v
	}
	return strings.Replace(humanize.Bytes(n), " ", "", 1)
}

// buildDetectedFieldsSQL renders:
//
//	SELECT `Body` AS `line`, `LogAttributes` AS `log_attributes`,
//	       `ResourceAttributes` AS `stream_labels`
//	FROM `otel_logs`
//	WHERE <matchers> AND <time bounds>
//	ORDER BY `Timestamp` DESC
//	LIMIT <lineLimit>
//
// The projection aliases are deliberately DISTINCT from the source
// column names: the selector predicate references the raw
// `ResourceAttributes` map in WHERE, and a same-name alias would
// shadow the column once a test harness (chclienttest) rewrites the
// projection to toJSONString(...) — CH resolves WHERE identifiers
// against SELECT aliases first.
//
// The peek window is small (1000 rows by default) — CH executes this as
// a top-N scan on the primary key, comparable to /index/stats.
func buildDetectedFieldsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time, lineLimit int) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(
			chsql.As(chsql.Col(s.BodyColumn), "line"),
			chsql.As(chsql.Col(s.AttributesColumn), "log_attributes"),
			chsql.As(chsql.Col(s.ResourceAttributesColumn), "stream_labels"),
			// The two parser-stage extractions, rendered from the SAME
			// chplan expressions the LogQL lowering emits for `| logfmt`
			// and `| json` — so the advertised body-parsed field set is
			// by construction the set a query can read back (#1888).
			// Frag is `func(*chsql.Builder)` and Builder.Expr records its
			// first error on the Builder, which QueryBuilder.Build
			// surfaces; that is the documented way to embed a chplan
			// expression in a clause slot.
			chsql.As(func(b *chsql.Builder) { _ = b.Expr(logql.LogfmtParsedLabels(s)) }, "logfmt_fields"),
			chsql.As(func(b *chsql.Builder) { _ = b.Expr(logql.JSONParsedLabels(s)) }, "json_fields"),
		).
		From(chsql.Col(s.LogsTable))

	if err := applySelectorAndWindow(sb, s, matchers, start, end); err != nil {
		return "", nil, err
	}
	sb.OrderBy(chsql.Col(s.TimestampColumn), true).
		Limit(int64(lineLimit))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// parsedField accumulates the per-field state across the peek window.
// Mirrors upstream's parsedFields struct: a hyperloglog sketch for
// cardinality, the most recent type detection, the set of parsers
// that produced the field, and the JSON path when applicable.
type parsedField struct {
	sketch   *hyperloglog.Sketch
	typ      string
	parsers  []string
	jsonPath []string
}

func newParsedField(parsers []string) *parsedField {
	return &parsedField{
		sketch:  hyperloglog.New(),
		typ:     "string",
		parsers: parsers,
	}
}

// detectFields runs the upstream detected-fields loop over the peek
// window (mirrors pkg/querier/queryrange/detected_fields.go,
// parseDetectedFields). For each row:
//
//  1. every LogAttributes entry (the structured-metadata analogue)
//     becomes a field with an empty parser list,
//  2. the Body is parsed — Loki's json parser first, logfmt fallback —
//     and each extracted key becomes a field tagged with the parser
//     that produced it (collisions with stream labels surface as
//     `<key>_extracted`, exactly as the upstream labels builder does).
//
// The field type is re-detected once per row from the first observed
// value, so the last row processed wins — upstream semantics, NOT a
// merge-to-string collapse. `limit` caps the number of DISTINCT fields
// tracked (rows keep contributing values to already-tracked fields).
//
// Keys iterate in sorted order so the (map-ordered upstream) loop is
// deterministic in cerberus; the output is sorted by label.
func detectFields(rows []chclient.DetectedFieldRow, limit int) []DetectedField {
	fields := map[string]*parsedField{}
	fieldCount := 0
	emptyParsers := []string{}

	track := func(name string, parsers []string) *parsedField {
		df, ok := fields[name]
		if !ok && fieldCount < limit {
			df = newParsedField(parsers)
			fields[name] = df
			fieldCount++
		}
		return df
	}

	for _, row := range rows {
		// Structured metadata: the normalised LogAttributes map. Keys
		// are normalised through the same OTel→Prom grammar the rest
		// of the Loki surface applies, so a field reported here is
		// queryable as written.
		structuredMetadata := format.NormalizeLabelMap(row.Attributes)
		for _, k := range sortedKeys(structuredMetadata) {
			df := track(k, emptyParsers)
			if df == nil {
				continue
			}
			v := structuredMetadata[k]
			df.typ = determineFieldType(v)
			df.sketch.Insert([]byte(v))
		}

		// Body-parsed fields: whatever the query path's own parser
		// stages extracted for this row, collision rename included. No
		// Go-side re-parse of row.Line happens here — see
		// [rowParsedFields].
		parsedLabels, parsers := rowParsedFields(row)
		for _, k := range sortedKeys(parsedLabels) {
			df := track(k, parsers)
			if df == nil {
				continue
			}
			for _, parser := range parsers {
				if !slices.Contains(df.parsers, parser) {
					df.parsers = append(df.parsers, parser)
				}
			}
			// `| json` reads top-level keys only (the lowering is
			// JSONExtractKeysAndValues), so a field's JSON path is its
			// own key with any collision suffix stripped — the suffix is
			// cerberus's rename, not part of the document.
			if slices.Contains(parsers, parserJSON) {
				df.jsonPath = []string{strings.TrimSuffix(k, duplicateSuffix)}
			}
			v := parsedLabels[k]
			df.typ = determineFieldType(v)
			df.sketch.Insert([]byte(v))
		}
	}

	out := make([]DetectedField, 0, len(fields))
	for k, df := range fields {
		p := df.parsers
		if len(p) == 0 {
			p = nil
		}
		out = append(out, DetectedField{
			Label:       k,
			Type:        df.typ,
			Cardinality: df.sketch.Estimate(),
			Parsers:     p,
			JSONPath:    df.jsonPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

// parserJSON / parserLogfmt are the `parsers` tags upstream Loki puts on
// a detected field, naming the LogQL parser stage that produces it.
const (
	parserJSON   = "json"
	parserLogfmt = "logfmt"
)

// rowParsedFields returns the body-parsed labels for one peek row plus
// the parser stage that produced them.
//
// The maps come straight from ClickHouse: the peek SQL evaluates the
// very chplan expressions the LogQL lowering emits for `| logfmt` and
// `| json` (logql.LogfmtParsedLabels / logql.JSONParsedLabels), so a
// field advertised here is by construction a field a query can read
// back. Deriving it a second time in Go is what made /detected_fields
// advertise logfmt keys the query path could never produce (#1888):
// cerberus lowers `| logfmt` to CH's extractKeyValuePairs, whose key
// grammar skips characters a Loki-shaped Go decoder rewrites to `_`, so
// the two disagree on the key NAMES — `(method='GET')` is `method` to
// the query path and `_method` to the Go decoder.
//
// The json-then-logfmt cascade mirrors upstream's parseEntry: a body
// that parses as a JSON object contributes its JSON fields and is not
// also run through logfmt.
func rowParsedFields(row chclient.DetectedFieldRow) (map[string]string, []string) {
	if len(row.JSONFields) > 0 {
		return row.JSONFields, []string{parserJSON}
	}
	if len(row.LogfmtFields) > 0 {
		return row.LogfmtFields, []string{parserLogfmt}
	}
	return nil, nil
}

// determineFieldType sniffs a value and picks the upstream type tag.
// The cascade order is upstream's determineType verbatim: int → float
// → boolean → duration → bytes → string. Note boolean uses
// strconv.ParseBool but sits AFTER the numeric probes, so "1" is an
// int, not a boolean; bytes uses humanize.ParseBytes ("10MB",
// "1.5GiB", ...), the same parser upstream calls.
func determineFieldType(value string) string {
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return "int"
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return "float"
	}
	if _, err := strconv.ParseBool(value); err == nil {
		return "boolean"
	}
	if _, err := time.ParseDuration(value); err == nil {
		return "duration"
	}
	if _, err := humanize.ParseBytes(value); err == nil {
		return "bytes"
	}
	return "string"
}

// sortedKeys returns the map's keys in sorted order — the determinism
// shim for the upstream loops that iterate Go maps directly.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parsePositiveInt31 parses an optional integer query parameter and clamps it
// to max. Empty input returns the default; non-numeric, non-positive, or
// out-of-range (>2^31-1) input is rejected with a 400; a value above max is
// silently clamped DOWN to max (mirroring parseLogLimit/maxLogQueryLimit on
// the log path — a request that asks for too much gets the most we'll serve,
// not an error). ParseUint with bitSize 31 bounds the parsed value to
// MaxInt32, which fits int on every architecture AND uint32 on the wire (the
// echoed `limit` is logproto.DetectedFieldsResponse.limit), so every
// downstream conversion is provably in range. Callers pass a finite max so the
// peek SQL's LIMIT — and the Go slice that buffers its whole result — stays
// bounded (see maxLogPeekLineLimit / maxDetectedFieldsLimit).
func parsePositiveInt31(raw string, def, max int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(raw, 10, 31)
	if err != nil || n == 0 {
		return 0, errors.New("parameter must be a positive integer no larger than 2147483647")
	}
	v := int(n)
	if v > max {
		v = max
	}
	return v, nil
}
