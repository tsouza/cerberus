package loki

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grafana/loki/v3/pkg/logql/syntax"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// Defaults that mirror upstream Loki's documented /tail contract:
//
//   - delay_for: 0s (no extra hold-back beyond the polling cadence)
//   - limit: 100 lines per chunk
//
// See https://grafana.com/docs/loki/latest/reference/loki-http-api/#stream-log-messages.
const (
	defaultTailDelayFor = 0 * time.Second
	defaultTailLimit    = 100
	maxTailDelayFor     = 5 * time.Second

	// tailPollInterval is how often the handler queries ClickHouse for
	// new rows. Upstream Loki streams chunks every ~1s; we match that.
	tailPollInterval = 1 * time.Second

	// tailWriteTimeout caps how long a single WebSocket write may block
	// before we tear the connection down. Catches slow / dead clients
	// without leaking the polling goroutine.
	tailWriteTimeout = 10 * time.Second
)

// tailUpgrader is the default gorilla/websocket upgrader. It permits
// every origin since /tail is consumed by the Loki datasource (Grafana
// running same-origin, or a CLI like logcli where browser-CORS is
// irrelevant) — same posture as upstream Loki.
var tailUpgrader = websocket.Upgrader{
	ReadBufferSize:  4 << 10,
	WriteBufferSize: 4 << 10,
	CheckOrigin:     func(_ *http.Request) bool { return true },
}

// tailResponse is the wire shape of one /tail chunk. Each chunk
// contains zero or more `streams` (grouped by label set) and, for
// future use, dropped-entry metadata (currently always empty; cerberus
// doesn't enforce a tail-side rate limit yet).
//
// Upstream Loki documents the exact JSON encoding at
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#stream-log-messages.
type tailResponse struct {
	Streams        []Stream             `json:"streams"`
	DroppedEntries []droppedEntryStream `json:"dropped_entries"`
}

// droppedEntryStream is the wire shape for dropped-entry diagnostics on
// /tail. Cerberus never drops entries today (the polling SQL is
// LIMITed but we don't surface that as a drop); the slice is always
// empty but the field is present so the JSON shape matches upstream.
type droppedEntryStream struct {
	Labels    map[string]string `json:"labels"`
	Timestamp string            `json:"timestamp"`
}

// handleTail implements GET /loki/api/v1/tail. The HTTP request is
// upgraded to a WebSocket; the handler then polls ClickHouse on a
// 1-second cadence and pushes a tailResponse chunk for each batch of
// new log entries.
//
// Query parameters honoured (matches upstream Loki):
//
//   - query (required): LogQL stream selector (log-line queries only;
//     metric-form queries are rejected — Loki tail returns log lines,
//     not vectors).
//   - start (optional): Unix-ns / Unix-seconds / RFC3339; defaults to
//     "now". Used as the lower bound on the very first poll.
//   - delay_for (optional): seconds to hold back the upper bound on
//     each poll, smoothing out late arrivals. Default 0, max 5s
//     (matches upstream Loki's cap).
//   - limit (optional): max entries per chunk (default 100).
//
// 4xx errors are returned BEFORE the WebSocket upgrade so misuse looks
// like an ordinary HTTP bad-request rather than a confusing websocket-
// handshake-then-close failure.
func (h *Handler) handleTail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}

	start, err := format.ParseTimeLoki(r.URL.Query().Get("start"), time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	delayFor, err := parseTailDelayFor(r.URL.Query().Get("delay_for"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	limit, err := parseTailLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	expr, err := syntax.ParseExpr(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	if isMetricQuery(expr) {
		writeError(w, http.StatusBadRequest, ErrBadData,
			errors.New("/tail accepts log-line queries only; metric-form queries (rate, count_over_time, ...) are not supported"))
		return
	}
	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	conn, err := tailUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written the HTTP error response; just log.
		h.Logger.Warn("cerberus loki tail upgrade failed", "err", err)
		return
	}
	defer func() { _ = conn.Close() }()

	tx, err := postProcessExtract(expr)
	if err != nil {
		// postProcessExtract failure on a log-stream query is unexpected
		// (the pipeline parsed fine above); send an error frame and exit.
		_ = conn.WriteJSON(Response{
			Status:    "error",
			ErrorType: ErrBadData,
			Error:     err.Error(),
		})
		return
	}

	h.runTailLoop(r.Context(), conn, tailLoopCfg{
		matchers: matchers,
		schema:   h.Schema,
		cursor:   start,
		delayFor: delayFor,
		limit:    limit,
		tx:       tx,
	})
}

// tailLoopCfg bundles the per-connection state for runTailLoop. Kept
// as a value type so the loop is straightforward to unit-test by
// substituting the Querier in h.
type tailLoopCfg struct {
	matchers []*labels.Matcher
	schema   schema.Logs
	cursor   time.Time
	delayFor time.Duration
	limit    int
	tx       lineTransform
}

// runTailLoop is the polling driver. It ticks every tailPollInterval,
// queries CH for rows in `[cursor, now - delay_for]`, sends a chunk
// over the WebSocket, advances `cursor`, and repeats until the request
// context cancels or the WebSocket write fails.
//
// Goroutine hygiene: the only goroutine spawned is the read-pump
// listener that closes the connection when the client disconnects.
// runTailLoop itself is synchronous on the calling handler goroutine.
func (h *Handler) runTailLoop(ctx context.Context, conn *websocket.Conn, cfg tailLoopCfg) {
	// Spawn a read pump so client-initiated close frames cancel ctx.
	// Without this the loop only learns about disconnects via a failed
	// Write — which can take a full tailPollInterval to surface.
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()

	cursor := cfg.cursor
	for {
		select {
		case <-loopCtx.Done():
			return
		case <-ticker.C:
		}

		end := time.Now().UTC().Add(-cfg.delayFor)
		if !end.After(cursor) {
			continue
		}

		sqlStr, args, err := buildTailSQL(cfg.schema, cfg.matchers, cursor, end, cfg.limit)
		if err != nil {
			h.Logger.Error("cerberus loki tail buildSQL failed", "err", err)
			return
		}

		samples, err := h.Client.Query(loopCtx, sqlStr, args...)
		if err != nil {
			h.Logger.Error("cerberus loki tail CH query failed", "err", err)
			return
		}

		streams := toStreamsWithTransform(samples, cfg.tx)
		// Advance the cursor past the latest row we just sent so the
		// next poll picks up only newer data. If the batch was empty we
		// still advance to `end` to avoid re-querying the same window.
		nextCursor := end
		for _, s := range samples {
			if s.Timestamp.After(nextCursor) {
				nextCursor = s.Timestamp
			}
		}
		// Tick forward by 1ns so the inclusive `>=` lower bound doesn't
		// duplicate the just-sent latest row on the next poll.
		cursor = nextCursor.Add(time.Nanosecond)

		// Skip the wire write on empty chunks — both upstream Loki and
		// the JS websocket client tolerate that, and Grafana's logs
		// volume panel parses empty streams arrays into a no-op.
		if len(streams) == 0 {
			continue
		}

		if err := writeTailChunk(conn, streams); err != nil {
			// Write failure usually means client disconnected; bail
			// cleanly so the deferred Close runs.
			h.Logger.Debug("cerberus loki tail write failed", "err", err)
			return
		}
	}
}

// writeTailChunk encodes streams + an empty dropped_entries slice and
// writes the JSON frame with a bounded write deadline. Caller is
// responsible for tearing the connection down on error.
func writeTailChunk(conn *websocket.Conn, streams []Stream) error {
	_ = conn.SetWriteDeadline(time.Now().Add(tailWriteTimeout))
	return conn.WriteJSON(tailResponse{
		Streams:        streams,
		DroppedEntries: []droppedEntryStream{},
	})
}

// buildTailSQL constructs the per-tick polling SELECT. The shape is:
//
//	SELECT `Body` AS MetricName, `ResourceAttributes` AS Attributes,
//	       `Timestamp` AS TimeUnix, toFloat64(0) AS Value
//	FROM `otel_logs`
//	WHERE <matchers> AND `Timestamp` >= <cursor> AND `Timestamp` <= <end>
//	ORDER BY `Timestamp` ASC
//	LIMIT <n>
//
// MetricName carries the log line (same hijack as the other log-stream
// handlers — chclient.Sample.Value is float64 so the line rides in
// the String-typed MetricName slot). All identifiers and time-range
// bounds flow through chsql.QueryBuilder — no fmt.Sprintf-on-SQL.
//
// Rows are sorted ascending so the runTailLoop cursor-advance logic
// picks the genuinely latest sample. Without ORDER BY the LIMIT could
// truncate to an arbitrary subset and the cursor would skip over rows
// that arrive on the next poll.
func buildTailSQL(s schema.Logs, matchers []*labels.Matcher, cursor, end time.Time, limit int) (string, []any, error) {
	pred := logql.SelectorPredicate(matchers, s)
	sb := chsql.NewQuery().
		Select(
			chsql.As(chsql.Col(s.BodyColumn), "MetricName"),
			chsql.As(chsql.Col(s.ResourceAttributesColumn), "Attributes"),
			chsql.As(chsql.Col(s.TimestampColumn), "TimeUnix"),
			chsql.As(toFloat64Zero(), "Value"),
		).
		From(chsql.Col(s.LogsTable))

	if pred != nil {
		whereFrag, err := exprFrag(pred)
		if err != nil {
			return "", nil, err
		}
		sb.Where(whereFrag)
	}
	sb.Where(timeBoundFrag(s.TimestampColumn, ">=", cursor))
	sb.Where(timeBoundFrag(s.TimestampColumn, "<=", end))
	sb.OrderBy(chsql.Col(s.TimestampColumn), false).
		Limit(int64(limit))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// toFloat64Zero is the chsql.Frag for `toFloat64(0)` — used as the
// placeholder Value column so the chclient.Sample scanner reads a
// stable Float64 instead of CH's UInt8 default for a bare literal `0`.
// Composed via the typed Call constructor wrapping a Lit(0) argument;
// the 0 binds as a positional `?` and CH coerces it inside toFloat64.
func toFloat64Zero() chsql.Frag {
	return chsql.Call("toFloat64", chsql.Lit(0))
}

// parseTailDelayFor reads the optional `delay_for` query param.
// Loki documents it as integer seconds, capped at 5s. Missing → 0s.
func parseTailDelayFor(raw string) (time.Duration, error) {
	if raw == "" {
		return defaultTailDelayFor, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("'delay_for' must be a non-negative integer (seconds)")
	}
	d := time.Duration(n) * time.Second
	if d > maxTailDelayFor {
		return 0, errors.New("'delay_for' must be <= 5 seconds")
	}
	return d, nil
}

// parseTailLimit reads the optional `limit` param. Missing → 100.
// Negative or zero values fall back to the default rather than the
// 400 the /index/volume handler uses — the upstream Loki tail
// endpoint silently coerces bad values to the default.
func parseTailLimit(raw string) (int, error) {
	if raw == "" {
		return defaultTailLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("'limit' must be a positive integer")
	}
	if n <= 0 {
		return defaultTailLimit, nil
	}
	return n, nil
}
