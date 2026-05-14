package loki

import (
	"errors"
	"net/http"
	"time"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// IndexStats is the body of a /loki/api/v1/index/stats response, matching
// the upstream Loki schema documented at
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#query-log-statistics.
//
// Cerberus is sample-storage (rows in an OTel-CH logs table), not
// chunk-storage like upstream Loki, so Chunks is always reported as 0.
// The PR body documents this gap; Grafana ignores the field for the
// "approximate matches" hint it uses Streams + Bytes for.
type IndexStats struct {
	Streams uint64 `json:"streams"`
	Chunks  uint64 `json:"chunks"`
	Entries uint64 `json:"entries"`
	Bytes   uint64 `json:"bytes"`
}

// handleIndexStats implements GET /loki/api/v1/index/stats. It accepts a
// LogQL selector + start/end and returns aggregate counts for the
// matched rows. Pipeline stages (line filters, label filters, parsers,
// template stages) are ignored — Loki's contract is "selector only" for
// this endpoint and that's what Grafana sends.
func (h *Handler) handleIndexStats(w http.ResponseWriter, r *http.Request) {
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

	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	sqlStr, args, err := buildIndexStatsSQL(h.Schema, matchers, start, end)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki index_stats", "logql", q, "sql", sqlStr, "args", args)

	row, err := h.Client.QueryIndexStats(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki index_stats CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway})
		return
	}

	writeJSON(w, http.StatusOK, IndexStats{
		Streams: row.Streams,
		// Cerberus has no chunk model — see IndexStats docstring.
		Chunks:  0,
		Entries: row.Entries,
		Bytes:   row.Bytes,
	})
}

// buildIndexStatsSQL builds the per-selector aggregate-count SELECT via
// the chsql.Builder. The shape is:
//
//	SELECT
//	  uniqExact(`ResourceAttributes`)         AS streams,
//	  count()                                  AS entries,
//	  sum(length(`Body`))                     AS bytes
//	FROM `otel_logs`
//	WHERE <matcher predicate>
//	  AND `Timestamp` >= toDateTime64(<start>, 9)
//	  AND `Timestamp` <= toDateTime64(<end>, 9)
//
// All identifiers and time-range bounds flow through Builder helpers so
// no SQL string concatenation happens at this level (the CLAUDE.md
// "no raw SQL" rule for new code).
func buildIndexStatsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time) (string, []any, error) {
	pred := logql.SelectorPredicate(matchers, s)
	sb := chsql.NewQuery().
		Select(
			aggFrag("uniqExact", s.ResourceAttributesColumn),
			countStar(),
			bytesAggFrag(s.BodyColumn),
		).
		From(chsql.Col(s.LogsTable))

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

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// aggFrag returns a Frag that emits "<fn>(`<col>`)" via the typed
// Call constructor so the SQL stream stays inside the chsql surface
// (no raw clause-keyword cosplay). fn is a CH function name and is
// emitted verbatim by Call (same trust contract as Cast's type name).
func aggFrag(fn, col string) chsql.Frag {
	return chsql.Call(fn, chsql.Col(col))
}

// countStar returns a Frag that emits "count()" via the typed Call
// constructor with no arguments — matches CH's nullary aggregate shape.
func countStar() chsql.Frag {
	return chsql.Call("count")
}

// bytesAggFrag emits "sum(length(`<body>`))" — the bytes-volume
// approximation. OTel-CH stores Body as a String column; length() counts
// bytes (not codepoints), matching Loki's chunk-byte semantics within
// rounding noise. If a deployment compresses Body and wants exact bytes
// the schema override can swap this column.
func bytesAggFrag(bodyCol string) chsql.Frag {
	return chsql.Call("sum", chsql.Call("length", chsql.Col(bodyCol)))
}

// timeBoundFrag emits "`<col>` <op> toDateTime64('YYYY-MM-DD HH:MM:SS.fffffffff', 9)".
// Only ">=" and "<=" are used by callers; anything else panics so a
// typo lands at boot rather than as a silent SQL syntax error.
func timeBoundFrag(col, op string, t time.Time) chsql.Frag {
	left := chsql.Col(col)
	right := func(b *chsql.Builder) { b.DateTime64Lit(t) }
	switch op {
	case ">=":
		return chsql.Gte(left, right)
	case "<=":
		return chsql.Lte(left, right)
	default:
		panic("loki.timeBoundFrag: unsupported op " + op)
	}
}

// exprFrag adapts a chplan.Expr into a Frag by deferring rendering to
// Builder.Expr. Returns an error if the expression contains a node
// Builder.Expr doesn't recognise.
func exprFrag(x chplan.Expr) (chsql.Frag, error) {
	// Capture a dry-run error early so handlers can surface it before
	// the SQL ever lands in front of CH.
	if err := (&chsql.Builder{}).Expr(x); err != nil {
		return nil, err
	}
	return func(b *chsql.Builder) {
		_ = b.Expr(x)
	}, nil
}

// selectorMatchers parses a LogQL expression and returns its
// stream-selector matchers. Both raw log-stream and metric-form queries
// are accepted; pipeline stages and aggregation wrappers are stripped.
func selectorMatchers(q string) ([]*labels.Matcher, error) {
	expr, err := syntax.ParseExpr(q)
	if err != nil {
		return nil, err
	}
	switch e := expr.(type) {
	case syntax.LogSelectorExpr:
		return e.Matchers(), nil
	case syntax.SampleExpr:
		sel, err := e.Selector()
		if err != nil {
			return nil, err
		}
		return sel.Matchers(), nil
	}
	return nil, errors.New("query does not expose a log selector")
}

// parseStartEnd reads optional start/end timestamps. Either may be
// absent — `start` defaults to 1h-ago, `end` to now, matching upstream
// Loki defaults. Returns a 400-shaped error on a malformed value (the
// caller wraps that into a bad_data response).
func parseStartEnd(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	startStr := r.URL.Query().Get("start")
	endStr := r.URL.Query().Get("end")

	start, err := format.ParseTimeLoki(startStr, now.Add(-time.Hour))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err := format.ParseTimeLoki(endStr, now)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !end.After(start) && !end.Equal(start) {
		return time.Time{}, time.Time{}, errors.New("'end' must not be before 'start'")
	}
	return start, end, nil
}
