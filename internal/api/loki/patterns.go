package loki

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/drain"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
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
// Level mirrors upstream's per-cluster `detected_level` discriminant:
// cerberus buckets pattern mining per row's normalized SeverityText
// (see [minePatterns]), so each cluster carries the level of the lines
// that trained it.
type Pattern struct {
	Pattern string     `json:"pattern"`
	Level   string     `json:"level"`
	Samples [][2]int64 `json:"samples"`
}

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
//	   {"pattern":"GET /api/<_> 200","level":"info","samples":[[ts,n], ...]},
//	   ...
//	]}
//
// Cerberus is stateless — every request rebuilds the drain from scratch
// (the CLAUDE.md "No caching" rule reinforces this; drain state is a
// per-request artefact). Determinism reduces to "feed lines in the same
// order every time"; the SQL emits `ORDER BY Timestamp DESC LIMIT N` so
// chDB returns rows in deterministic order.
//
// Cerberus trains one drain instance per detected level (see
// [minePatterns]), so each cluster carries a real `level` value —
// Grafana's pattern panel filters by level using this field.
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
	h.Logger.Debug("cerberus loki patterns", "logql", telemetry.SanitizeForLog(q), "sql", sqlStr, "args", args)

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
//	SELECT `Timestamp`, `Body`, `SeverityText`
//	FROM `otel_logs`
//	WHERE <matchers> AND <time bounds>
//	ORDER BY `Timestamp` DESC
//	LIMIT <lineLimit>
//
// Mirrors buildDetectedFieldsSQL but projects three columns — drain
// needs the body and a real timestamp to bucket per-cluster samples, and
// [minePatterns] buckets mining itself by the row's raw severity.
func buildPatternsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time, lineLimit int) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(
			chsql.Col(s.TimestampColumn),
			chsql.Col(s.BodyColumn),
			chsql.Col(s.SeverityColumn),
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

// minePatterns buckets the peek window by each row's normalized
// detected level ([logql.NormalizeDetectedLevel] of the row's raw
// SeverityText), trains one drain instance per bucket, and projects the
// resulting clusters onto the upstream `[]Pattern` wire shape — so each
// cluster carries the real `level` of the lines that trained it, mirroring
// upstream's per-cluster `detected_level` discriminant. Returns an empty
// (non-nil) slice when no lines hit any cluster — the JSON envelope
// encodes that as `data:[]`, matching upstream Loki.
//
// The miner is the in-house clean-room Drain implementation
// (internal/drain), which tokenises on whitespace and treats
// digit-bearing tokens as variables — generic enough for arbitrary log
// lines without a per-format tokeniser gate. Each level bucket gets its
// own drain.Miner instance (confirmed independent — a Miner owns its own
// token tree and cluster slice, no shared state), so lines never mix
// templates across levels.
func minePatterns(lines []chclient.TimestampedLine) []Pattern {
	miners := make(map[string]*drain.Miner)
	levels := make([]string, 0)
	for _, line := range lines {
		level := logql.NormalizeDetectedLevel(line.Severity)
		m, ok := miners[level]
		if !ok {
			m = drain.New(drain.DefaultConfig())
			miners[level] = m
			levels = append(levels, level)
		}
		m.Train(line.Body, line.Timestamp.UnixNano())
	}

	out := make([]Pattern, 0)
	for _, level := range levels {
		for _, c := range miners[level].Clusters() {
			if c == nil {
				continue
			}
			s := c.String()
			if s == "" {
				continue
			}
			out = append(out, Pattern{
				Pattern: s,
				Level:   level,
				Samples: projectSamples(c.Samples()),
			})
		}
	}
	// Stable response order — drain's Clusters() return order follows
	// LRU cache traversal, which is not deterministic across runs.
	// Sorting by (pattern, level) lets Grafana / tests pin on the wire
	// shape without flake; the level tiebreak matters now that the same
	// pattern string can recur across two different level buckets.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pattern != out[j].Pattern {
			return out[i].Pattern < out[j].Pattern
		}
		return out[i].Level < out[j].Level
	})
	return out
}

// projectSamples converts the in-house drain cluster samples
// (TimestampUnixSec, Count) onto the upstream wire shape
// `[][unix_seconds, count]`. Samples already arrive ascending by
// timestamp (drain.Cluster.Samples sorts), and the resolution is whole
// seconds, matching upstream's `WriteQueryPatternsResponseJSON`, which
// emits `sample.Timestamp.Unix()`.
func projectSamples(samples []drain.Sample) [][2]int64 {
	out := make([][2]int64, 0, len(samples))
	for _, s := range samples {
		out = append(out, [2]int64{s.TimestampUnixSec, s.Count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
