package loki

import (
	"errors"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/axiomhq/hyperloglog"
	"github.com/dustin/go-humanize"
	loglib "github.com/grafana/loki/v3/pkg/logql/log"
	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
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

// maxLogPeekLineLimit hard-caps `line_limit` on the metadata peek endpoints
// (/detected_fields, /patterns). The peek SQL is `... ORDER BY Timestamp DESC
// LIMIT line_limit` and the whole result is buffered into a Go slice with no
// streaming, so an unclamped `line_limit` (the param accepts up to 2^31-1)
// lets a single request OOM the process — max_memory_usage bounds ClickHouse,
// not the cerberus heap. This clamp caps the row COUNT the SQL LIMIT returns
// (and thus the buffered slice): 10k newest lines is 10x the default and ample
// for a field/pattern heuristic. It removes the unbounded-row OOM; the
// absolute heap still scales with line SIZE × concurrency, which the deferred
// uniform per-drain maxSamples backstop (the "complete the net" follow-up)
// bounds hard. Mirrors the parseLogLimit/maxLogQueryLimit clamp on the log path.
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
type DetectedFieldsResponse struct {
	Fields []DetectedField `json:"fields,omitempty"`
	Limit  uint32          `json:"limit,omitempty"`
}

// handleDetectedFields implements GET /loki/api/v1/detected_fields. The
// upstream Loki feature peeks at the first N matching rows (newest
// first) and reports every field it can derive from each record.
// Cerberus mirrors the upstream frontend implementation
// (pkg/querier/queryrange/detected_fields.go) on top of the OTel-CH
// schema:
//
//   - SQL fetches (Body, LogAttributes, ResourceAttributes), newest
//     first, capped at line_limit.
//   - LogAttributes is the structured-metadata source: every key
//     becomes a field with a nil parser list (Loki's OTLP ingestion
//     maps log-record attributes to structured metadata, so the
//     OTel-CH LogAttributes map is the same data on the CH side).
//   - Body is parsed with Loki's own json parser first, falling back
//     to logfmt — the per-field `parsers` list records which one hit,
//     and json-extracted fields carry their original `jsonPath`.
//   - Types follow upstream determineType (int → float → boolean →
//     duration → bytes → string), re-detected per record so the last
//     record processed wins — exactly the upstream loop.
//   - Cardinality is a hyperloglog estimate over the observed values
//     (same sketch library upstream uses, so estimates match a
//     reference Loki fed the same records).
//
// Cerberus serves only the fields variant; the sibling
// /detected_field/{name}/values endpoint is not mounted.
//
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#detected-fields
func (h *Handler) handleDetectedFields(w http.ResponseWriter, r *http.Request) {
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	lineLimit, err := parsePositiveInt31(r.FormValue("line_limit"), defaultDetectedFieldsLineLimit, maxLogPeekLineLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	limit, err := parsePositiveInt31(r.FormValue("limit"), defaultDetectedFieldsLimit, maxDetectedFieldsLimit)
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
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki detected_fields", "logql", q, "sql", sqlStr, "args", args)

	rows, err := h.Client.QueryDetectedFieldRows(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki detected_fields CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, classifyMetadataErr(err))
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
		).
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

		// Body parsing: seed the labels builder with the stream labels
		// so parsed keys that shadow a stream label rename to
		// `<key>_extracted`, matching upstream.
		streamLbls := labels.FromMap(format.NormalizeLabelMap(row.Resource))
		entryLbls := loglib.NewBaseLabelsBuilder().ForLabels(streamLbls, labels.StableHash(streamLbls))
		parsedLabels, parsers := parseLine(row.Line, entryLbls)
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
			// If we parsed with JSON, record the original JSON path.
			if slices.Contains(parsers, "json") {
				df.jsonPath = entryLbls.GetJSONPath(k)
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

// parseLine runs Loki's own line parsers over a log body: json first
// (capturing JSON paths), logfmt as the fallback — the exact cascade
// upstream's parseEntry uses. Returns the extracted (key → value) map
// (logqlmodel error labels dropped) plus the single-parser list that
// produced it; (nil, nil) when neither parser accepts the line.
func parseLine(line string, lbls *loglib.LabelsBuilder) (map[string]string, []string) {
	parser := "json"
	jsonParser := loglib.NewJSONParser(true)
	_, jsonSuccess := jsonParser.Process(0, []byte(line), lbls)
	if !jsonSuccess || lbls.HasErr() {
		lbls.Reset()

		logfmtParser := loglib.NewLogfmtParser(false, false)
		parser = "logfmt"
		_, logfmtSuccess := logfmtParser.Process(0, []byte(line), lbls)
		if !logfmtSuccess || lbls.HasErr() {
			return nil, nil
		}
	}

	out := map[string]string{}
	lbls.LabelsResult().Parsed().Range(func(l labels.Label) {
		if l.Name == logqlmodel.ErrorLabel || l.Name == logqlmodel.ErrorDetailsLabel ||
			l.Name == logqlmodel.PreserveErrorLabel {
			return
		}
		out[l.Name] = l.Value
	})
	if len(out) == 0 {
		return nil, nil
	}
	return out, []string{parser}
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
