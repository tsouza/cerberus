package chclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"go.opentelemetry.io/otel/trace"
)

// ErrTooManySamples is the sentinel matched (via errors.Is) when a
// cursor's iteration crosses the per-query sample budget configured by
// Config.MaxQuerySamples. It mirrors upstream Prometheus's
// promql.ErrTooManySamples contract: the query is aborted instead of
// materialising an unbounded result set in process memory.
//
// The concrete error a Cursor's Err() returns is *TooManySamplesError,
// which wraps this sentinel and carries the configured limit.
//
// Budget exceedance is an iteration error, not a transport failure: it
// surfaces via cursor.Err() AFTER QueryCursor's open call already
// recorded success against the circuit breaker, so it never counts as
// a CH failure (see the breaker note on QueryCursor).
var ErrTooManySamples = errors.New("query sample budget exceeded")

// TooManySamplesError is the concrete error returned by Cursor.Err()
// when iteration crosses the configured per-query sample budget. It
// wraps [ErrTooManySamples] (errors.Is matches) and carries the limit
// so API handlers can render head-idiomatic over-limit messages.
type TooManySamplesError struct {
	// Limit is the configured per-query sample budget that iteration
	// crossed.
	Limit int64
}

func (e *TooManySamplesError) Error() string {
	return fmt.Sprintf("chclient: query sample budget exceeded: result set exceeds %d samples", e.Limit)
}

func (e *TooManySamplesError) Unwrap() error { return ErrTooManySamples }

// Cursor is a forward-only iterator over a Sample result set. Use it to
// stream rows out of ClickHouse without materialising the full slice in
// process memory — the canonical pattern for `query_range` matrix
// responses, where a long-window / fine-step query can produce millions
// of rows.
//
// Lifecycle: call Next() in a loop; while it returns true, Sample()
// yields the current row. When Next() returns false, check Err() — a
// non-nil value means the iterator terminated due to a decode or
// transport error rather than end-of-stream. Close() releases the
// underlying CH resources and MUST be called exactly once, typically via
// `defer cursor.Close()` immediately after a successful QueryCursor.
//
// Inspected reports the number of rows the consumer has pulled off the
// cursor so far (the count of Next() calls that returned true). This is
// the size of the buffer a result-buffering handler accumulates as it
// drains — the quantity that OOMed the gateway twice (Tempo /api/search
// and PromQL /api/v1/query_range). A handler whose memory must be O(output)
// rather than O(input) proves it by asserting Inspected stays bounded by
// the output limit as the input axis (dataset / window / cardinality)
// scales; see test/chdb/boundsdrain. It mirrors the eager path's
// len(Result.Samples) semantics — the rows that crossed from ClickHouse
// into the process — so a streaming handler can report a drain count the
// same way Tempo's SearchMetrics.InspectedTraces does on the eager path.
type Cursor interface {
	Next() bool
	Sample() Sample
	Err() error
	Close() error
	Inspected() int64
}

// rowsCursor wraps a driver.Rows and decodes each row positionally into a
// Sample. The driver's Rows is itself an iterator over the wire stream,
// so allocations per row stay bounded — only the current Sample is kept
// in memory.
type rowsCursor struct {
	rows driver.Rows
	cur  Sample
	err  error
	// maxSamples is the per-query sample budget (0 = unlimited). When
	// iteration crosses it, Next returns false and Err yields a
	// *TooManySamplesError wrapping ErrTooManySamples.
	maxSamples int64
	// budget is the request-scoped SHARED sample budget pulled from ctx at
	// QueryCursor time (nil on the single-statement route-A path). When
	// present it takes precedence over maxSamples so a routed fan-out of K
	// cursors enforces ONE per-request max-samples limit (the 422 parity).
	// A crossing yields the IDENTICAL *TooManySamplesError (errors.Is
	// ErrTooManySamples) the per-cursor limit produces.
	budget *SampleBudget
	// seen counts rows successfully decoded so far, compared against
	// maxSamples after each scan.
	seen int64
	// maxMemoryBytes is the Client's configured per-query ClickHouse
	// memory cap (Config.MaxQueryMemoryBytes), carried so a mid-stream
	// MEMORY_LIMIT_EXCEEDED (code 241) surfacing via rows.Err() — the
	// exact shape of k3d run 27277793810 — is wrapped into a
	// *MemoryLimitError naming the cap. Informational only; the cap
	// itself is enforced server-side via the max_memory_usage setting.
	maxMemoryBytes int64
	// queryTimeout is the effective per-query wall-clock cap the cursor
	// ran under (the Client default min'd with any per-request
	// WithQueryTimeout override), carried so a mid-stream
	// TIMEOUT_EXCEEDED (code 159) surfacing via rows.Err() is wrapped
	// into a *QueryTimeoutError naming the budget. Informational only;
	// the cap itself is enforced server-side via the max_execution_time
	// setting. 0 = no cap was configured.
	queryTimeout time.Duration
	// interned caches decoded label maps keyed by their canonical
	// serialised form so all rows of one series share a single map
	// instance. A long-window matrix query returns thousands of rows
	// per series, each carrying an identical label set; without
	// interning every row retains its own map (header + N string
	// pairs), which is exactly the per-row overhead that OOMKilled the
	// k3d e2e pods (run 27269987620). Consumers MUST treat
	// Sample.Labels as read-only — see the contract on [Sample].
	interned map[string]internedSeries
	// internSeq is the running counter that assigns each newly-seen series
	// its 1-based [Sample.SeriesID]; it advances on first sight of a
	// canonical label key so the ordinal is stable for every later row of
	// that series within this cursor.
	internSeq uint32
	// span is the `execute` pipeline-stage span opened by QueryCursor.
	// Held by the cursor (rather than closed when QueryCursor returns)
	// so that row decode + CH wire transit are billed to the execute
	// stage — the iteration loop is part of the round-trip's cost.
	span trace.Span
	// rec is the progress recorder latched at QueryCursor time. The
	// cursor flushes it on Close so the rows/bytes_read histograms
	// observe the per-query total exactly once — irrespective of how
	// long the caller takes to drain rows.
	rec *progressRecorder
	// closeOnce serialises Close() so the span / rec / rows nil-outs
	// happen exactly once, even under concurrent Close calls (e.g. a
	// caller's `defer cursor.Close()` racing a context-cancellation
	// path that also calls Close).
	closeOnce sync.Once
	closeErr  error
	// metadataProbed latches the one-time column-shape probe (see
	// [rowsCursor.scanRow]). hasMetadata records whether the projection
	// carries a fifth `Metadata` column — the Loki log-stream path — so
	// the scan binds five destinations (…, &metadata) instead of four.
	// Every metric query and the prom / tempo heads project four columns,
	// leaving hasMetadata false and the scan byte-identical to before.
	metadataProbed bool
	hasMetadata    bool
}

// metadataColumn is the projection alias the Loki log-stream path appends
// as a fifth column to carry per-row structured metadata (the OTel-CH
// LogAttributes map) into [Sample.Metadata]. The shared cursor probes the
// result-set columns for this name once and adapts its positional scan.
const metadataColumn = "Metadata"

// Next advances the cursor to the next row. Returns false when the
// stream is exhausted or when a decode error occurred; in the error case
// Err() returns the cause.
func (c *rowsCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if !c.rows.Next() {
		if err := c.rows.Err(); err != nil {
			// A mid-stream CH resource abort — memory-limit (code 241)
			// or wall-clock timeout (code 159) — is a per-query resource
			// rejection, not a transport failure: wrap it so handlers map
			// it onto the resource-exhausted / timeout wire shape instead
			// of a 502 (k3d run 27277793810 surfaced the memory case
			// mid-stream; a long-window matrix can hit max_execution_time
			// the same way).
			c.err = fmt.Errorf("chclient: rows.Err: %w", wrapQueryTimeout(wrapMemoryLimit(err, c.maxMemoryBytes), c.queryTimeout))
		}
		return false
	}
	var s Sample
	var labels map[string]string
	// metadataJSON receives the fifth `Metadata` column when present: the
	// log-stream projection renders the filtered LogAttributes map via
	// `toJSONString(...)`, so it scans as a plain `String` (a JSON object)
	// rather than a raw `Map(String, String)`. A native Map column scans
	// on prod ClickHouse but NOT under the chDB probe lane (chdb-go's
	// Parquet driver can't cast a Map — see chdb_probe_test.go); the JSON
	// string scans cleanly on both, and is json.Unmarshal'd back below.
	var metadataJSON string
	// Probe the result-set shape once: the Loki log-stream projection
	// appends a fifth `Metadata` column, every other path projects four.
	// driver.Rows.Columns() is stable across the stream, so latch the
	// decision on the first row and bind the scan accordingly.
	if !c.metadataProbed {
		cols := c.rows.Columns()
		c.hasMetadata = len(cols) > 0 && cols[len(cols)-1] == metadataColumn
		c.metadataProbed = true
	}
	if c.hasMetadata {
		if err := c.rows.Scan(&s.MetricName, &labels, &s.Timestamp, &s.Value, &metadataJSON); err != nil {
			c.err = fmt.Errorf("chclient: scan: %w", err)
			return false
		}
	} else if err := c.rows.Scan(&s.MetricName, &labels, &s.Timestamp, &s.Value); err != nil {
		c.err = fmt.Errorf("chclient: scan: %w", err)
		return false
	}
	c.seen++
	// A per-request shared budget (set by QueryCursor from the ctx) takes
	// precedence over the per-cursor maxSamples: charging one sample
	// against it lets a fan-out request's concurrent shard cursors
	// collectively trip the 422 at the request total. When no budget is
	// attached, fall back to the per-cursor limit. Both paths produce the
	// IDENTICAL *TooManySamplesError so the upstream 422 message and
	// behaviour are the same regardless of which limit fired — the
	// budget's Limit() is the configured max a single-statement query
	// would report.
	if c.budget != nil {
		if !c.budget.consume(1) {
			c.err = &TooManySamplesError{Limit: c.budget.Limit()}
			return false
		}
	} else if c.maxSamples > 0 && c.seen > c.maxSamples {
		c.err = &TooManySamplesError{Limit: c.maxSamples}
		return false
	}
	s.Labels, s.SeriesID = c.internLabels(labels)
	// Per-row structured metadata is genuinely distinct per log line
	// (durations, byte counts, query ids), so it is NOT interned — unlike
	// the series-identity Labels map. The fifth column arrives as a JSON
	// object string (`toJSONString` of the filtered LogAttributes map);
	// decode it back to a map. An empty / `{}` payload leaves Metadata
	// nil, so [StreamValue.MarshalJSON] falls back to the two-element
	// `[ts, line]` tuple. Left nil entirely on the four-column path.
	if metadata := decodeMetadataJSON(metadataJSON); len(metadata) > 0 {
		s.Metadata = metadata
	}
	c.cur = s
	return true
}

// decodeMetadataJSON parses the `toJSONString(<Map>)` payload of the
// Loki log-stream `Metadata` column back into a `map[string]string`. An
// empty string, a `{}` object, or a JSON null all decode to an empty
// map, so a row whose filtered LogAttributes were empty surfaces no
// structured metadata. A decode error is swallowed to a nil map rather
// than failing the whole stream — a malformed metadata object should
// never take down an otherwise valid log query; the handler simply emits
// the two-element `[ts, line]` tuple for that entry.
func decodeMetadataJSON(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// internedSeries pairs an interned label map with the 1-based ordinal the
// cursor assigned it on first sight, so [rowsCursor.internLabels] can hand
// both back: the shared map (memory dedup) and a stable per-series identity
// (consumer-side per-series memoisation, reflect-free).
type internedSeries struct {
	labels map[string]string
	id     uint32
}

// internLabels returns the canonical shared map instance for the
// decoded label set plus its stable 1-based series ordinal: the first
// occurrence of a label set is cached under its canonical key, and every
// later row with the same set returns that exact map and the same ordinal.
// The freshly decoded duplicate becomes short-lived garbage; what matters
// is that the RETAINED set (the samples a handler buffers while pivoting a
// matrix response) holds one map per series instead of one map per row.
//
// The ordinal lets consumers memoise per-series work without a
// reflect-based map-pointer probe — see [Sample.SeriesID]. A nil label set
// returns id 0 (the "not interned" sentinel).
func (c *rowsCursor) internLabels(labels map[string]string) (map[string]string, uint32) {
	if labels == nil {
		return nil, 0
	}
	key := canonicalLabelKey(labels)
	if cached, ok := c.interned[key]; ok {
		return cached.labels, cached.id
	}
	if c.interned == nil {
		c.interned = make(map[string]internedSeries)
	}
	c.internSeq++
	c.interned[key] = internedSeries{labels: labels, id: c.internSeq}
	return labels, c.internSeq
}

// canonicalLabelKey is a deterministic string form of a label set:
// keys sorted ASCII-ascending, pairs joined as "k=v\x00" so two
// distinct label sets cannot alias. Mirrors the canonical-key shape
// the API layer uses for series grouping (internal/api/format
// .CanonicalKey) — duplicated locally because chclient must not
// import api packages.
func canonicalLabelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
		b = append(b, 0)
	}
	return string(b)
}

// Sample returns the row that the most recent Next() call landed on.
// Calling Sample before Next, or after Next has returned false, yields
// the zero value.
func (c *rowsCursor) Sample() Sample { return c.cur }

// Err returns any non-EOF error that terminated iteration. It is safe to
// call after Close.
func (c *rowsCursor) Err() error { return c.err }

// Inspected returns the number of rows decoded off the wire so far: seen is
// incremented once per row scanned, before the per-request budget / per-cursor
// maxSamples check. On a clean drain that equals the count of Next() calls that
// returned true — the per-request drain count a streaming handler buffers (the
// matrix the prom /query_range pivot accumulates), the streaming-path analogue
// of the eager path's len(Result.Samples). On a budget / maxSamples trip the
// tripping row is included though its Next() returned false, so Inspected is
// one higher than the buffered count; that case always sets Err(), and every
// reader (matrixFromCursor, Client.Query) reports Inspected only after a
// nil-error full drain, so the off-by-one is never observed.
func (c *rowsCursor) Inspected() int64 { return c.seen }

// Close releases the underlying driver.Rows. Safe to call multiple
// times AND from multiple goroutines concurrently; the underlying
// teardown runs exactly once via sync.Once, and subsequent calls return
// the same error the first call returned.
//
// The first call also flushes the progress recorder so the per-query
// rows/bytes histograms see the totals exactly once. Subsequent calls
// are no-ops.
func (c *rowsCursor) Close() error {
	c.closeOnce.Do(func() {
		if c.span != nil {
			if c.err != nil {
				c.span.RecordError(c.err)
			}
			c.span.End()
			c.span = nil
		}
		if c.rec != nil {
			c.rec.flush()
			c.rec = nil
		}
		if c.rows == nil {
			return
		}
		err := c.rows.Close()
		c.rows = nil
		if err != nil {
			c.closeErr = fmt.Errorf("chclient: rows.Close: %w", err)
		}
	})
	return c.closeErr
}

// QueryCursor runs sql with positional args and returns a forward-only
// Cursor over the result set. The SQL must project (MetricName,
// Attributes, TimeUnix, Value) in that order — Scan binds positionally,
// matching Client.Query.
//
// Compared to Query, QueryCursor keeps only one Sample resident in
// process memory at a time, which is the only way to keep RAM bounded
// for long-window `query_range` requests. Callers MUST Close the cursor
// to return its connection to the pool.
//
// When the Client was configured with a positive MaxQuerySamples, the
// cursor enforces it as a per-query sample budget: crossing it stops
// iteration and Err() returns a *TooManySamplesError (errors.Is
// ErrTooManySamples). Mirrors upstream Prometheus's
// --query.max-samples abort-the-query contract.
//
// When ctx carries a per-request *SampleBudget (attached via
// WithSampleBudget), that shared budget takes precedence over the
// per-cursor MaxQuerySamples: every cursor the request opens charges
// against ONE counter so a fan-out query trips the 422 at the request
// total rather than per cursor. The *TooManySamplesError produced is
// identical either way.
//
// Guarded by the circuit breaker (see [Client] doc). The breaker
// observes the open-call outcome only — once the cursor is returned,
// iteration errors propagate via cursor.Err() but are NOT re-recorded
// against the breaker. A single failed query is one failure, not N
// where N is the number of rows the caller drained before hitting the
// transport drop. The same property keeps sample-budget rejections
// (ErrTooManySamples) out of the breaker's failure count entirely: a
// client asking for too much data is not a ClickHouse outage.
func (c *Client) QueryCursor(ctx context.Context, sql string, args ...any) (Cursor, error) {
	if !c.br.allow() {
		return nil, fmt.Errorf("chclient: query: %w", ErrCircuitOpen)
	}
	// Dispatch to the cursor-decode strategy resolved at boot. It is ALWAYS
	// non-nil: the row path by default, the columnar strategy (which embeds the
	// row path as its fallback) when the columnar matrix decode was enabled at
	// boot (the chopt columnar_result_decode feature). No branch here — the
	// strategy already encodes the choice, and the columnar strategy makes the
	// per-block matrix-shape / bindable-args decision internally before
	// delegating to its row fallback.
	return c.cursorDecoder.decode(c, ctx, sql, args...)
}
