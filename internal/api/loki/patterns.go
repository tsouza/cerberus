package loki

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/grafana/loki/v3/pkg/logproto"
	drainpkg "github.com/grafana/loki/v3/pkg/pattern/drain"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// defaultPatternsLineLimit caps the number of log rows the drain
// template miner peeks at when no `line_limit` query parameter is
// supplied. Mirrors detected_fields' default — drain's
// `MaxAllowedLineLength` already bounds per-line cost; this caps the
// number of lines.
const defaultPatternsLineLimit = 1000

// Pattern is one detected log-line template plus its time-bucketed
// sample counts. The upstream Loki contract (verified against
// `pkg/util/marshal/marshal.go:WriteQueryPatternsResponseJSON`) emits
// each sample as a `[unix_seconds, count]` 2-tuple — the timestamp is
// `sample.Timestamp.Unix()`, which strips the millisecond component.
//
// Level mirrors upstream's per-cluster `detected_level` discriminant.
// Cerberus emits `""` for every cluster (one drain instance for all
// severities, no per-level bucketing).
type Pattern struct {
	Pattern string     `json:"pattern"`
	Level   string     `json:"level"`
	Samples [][2]int64 `json:"samples"`
}

// nilLimits satisfies drain.Limits with no per-tenant JSON tokenisation
// hints. Cerberus is single-tenant; the upstream pattern ingester uses
// this hook to tokenise specific JSON fields differently, but the
// format passed to drain.New ignores it for FormatUnknown (the generic
// punctuation tokeniser).
type nilLimits struct{}

func (nilLimits) PatternIngesterTokenizableJSONFields(string) []string { return nil }

// handlePatterns implements GET /loki/api/v1/patterns.
//
// Upstream Loki 3.x exposes a pattern-discovery subsystem (drain3-style
// log template extraction) on this endpoint. Cerberus mirrors that flow
// with a per-request drain instance trained over a peek window (default
// 1000 most-recent lines from the matched stream selector). The
// resulting clusters are projected onto the upstream
// `WriteQueryPatternsResponseJSON` wire shape:
//
//	{"status":"success","data":[
//	   {"pattern":"GET /api/<_> 200","level":"","samples":[[ts,n], ...]},
//	   ...
//	]}
//
// Cerberus is stateless — every request rebuilds the drain from scratch
// (the CLAUDE.md "No caching" rule reinforces this; drain state is a
// per-request artefact). Determinism reduces to "feed lines in the same
// order every time"; the SQL emits `ORDER BY Timestamp DESC LIMIT N` so
// chDB returns rows in deterministic order.
//
// Cerberus trains a single drain instance for all severities and emits
// `level:""` for every cluster; Grafana's pattern panel renders both
// with-level and without-level payloads.
func (h *Handler) handlePatterns(w http.ResponseWriter, r *http.Request) {
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
	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	lineLimit, err := parsePositiveInt31(r.FormValue("line_limit"), defaultPatternsLineLimit, maxLogPeekLineLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	sqlStr, args, err := buildPatternsSQL(h.Schema, matchers, start, end, lineLimit)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki patterns", "logql", q, "sql", sqlStr, "args", args)

	lines, err := h.Client.QueryTimestampedLines(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki patterns CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, classifyMetadataErr(err))
		return
	}

	patterns := minePatterns(lines)

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   patterns,
	})
}

// buildPatternsSQL renders:
//
//	SELECT `Timestamp`, `Body`
//	FROM `otel_logs`
//	WHERE <matchers> AND <time bounds>
//	ORDER BY `Timestamp` DESC
//	LIMIT <lineLimit>
//
// Mirrors buildDetectedFieldsSQL but projects two columns — drain needs
// both the body and a real timestamp to bucket per-cluster samples.
func buildPatternsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time, lineLimit int) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(
			chsql.Col(s.TimestampColumn),
			chsql.Col(s.BodyColumn),
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

// minePatterns trains a single drain instance over the peek window and
// projects the resulting clusters onto the upstream `[]Pattern` wire
// shape. Returns an empty (non-nil) slice when no lines hit any cluster
// — the JSON envelope encodes that as `data:[]`, matching upstream Loki.
//
// drain.New(FormatUnknown) uses the punctuation tokeniser, which is the
// most generic of the three (JSON / logfmt / unknown) and works on
// arbitrary log lines. Cerberus does not run DetectLogFormat on the
// first line because that gates the tokeniser for all subsequent lines
// — a single non-JSON / non-logfmt row in a mostly-JSON stream would
// then break tokenisation for the rest. The punctuation tokeniser is
// equally happy with all three shapes.
func minePatterns(lines []chclient.TimestampedLine) []Pattern {
	d := drainpkg.New("", drainpkg.DefaultConfig(), nilLimits{}, drainpkg.FormatUnknown, nil)
	for _, line := range lines {
		d.Train(line.Body, line.Timestamp.UnixNano())
	}

	clusters := d.Clusters()
	out := make([]Pattern, 0, len(clusters))
	for _, c := range clusters {
		if c == nil {
			continue
		}
		s := c.String()
		if s == "" {
			continue
		}
		out = append(out, Pattern{
			Pattern: s,
			Level:   "",
			Samples: projectSamples(c.Samples()),
		})
	}
	// Stable response order — drain's Clusters() return order follows
	// LRU cache traversal, which is not deterministic across runs.
	// Sorting by pattern string lets Grafana / tests pin on the wire
	// shape without flake.
	sort.Slice(out, func(i, j int) bool { return out[i].Pattern < out[j].Pattern })
	return out
}

// projectSamples converts drain's per-cluster []*logproto.PatternSample
// (Timestamp model.Time = millisecond, Value int64 = count) onto the
// upstream wire shape `[][unix_seconds, count]`. Samples are sorted by
// timestamp ascending so the response is stable across runs.
//
// model.Time is milliseconds-since-epoch typed as int64; its .Unix()
// returns seconds. Upstream's WriteQueryPatternsResponseJSON calls
// `sample.Timestamp.Unix()`, which strips the millisecond component —
// cerberus matches that exactly.
func projectSamples(samples []*logproto.PatternSample) [][2]int64 {
	out := make([][2]int64, 0, len(samples))
	for _, s := range samples {
		if s == nil {
			continue
		}
		out = append(out, [2]int64{s.Timestamp.Unix(), s.Value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
