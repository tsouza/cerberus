package chclient

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ClickHouse/ch-go"
	"github.com/ClickHouse/ch-go/chpool"
	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
	"go.opentelemetry.io/otel/trace"
)

// columnar.go — the columnar `query_range` matrix decode.
//
// The clickhouse-go/v2 row path pays column.Map.row (reflect.MakeMap + a
// boxed SetMapIndex per entry) on EVERY row of a matrix, then the cursor
// interns the decoded label map and discards all but the first per series.
// The Map sub-columns the win would read (keys/values/offsets) are
// UNEXPORTED in clickhouse-go/v2 and the driver exposes no block-read API, so
// the saving is unreachable through that dependency. ch-go's low-level
// proto.ColMap exports Keys/Values/Offsets and its OnResult(ctx, block)
// streams typed result blocks — so a SECOND dedicated ch-go dial, used ONLY
// for the matrix shape, can build each series' label map once per contiguous
// run instead of once per row.
//
// Everything conn-agnostic is reused, not duplicated: the breaker
// allow()/record() seam, the per-dispatch query_id (EnsureQueryID), the
// per-query settings (max_memory_usage / max_execution_time), the sample
// budget, the execute span, and the progress recorder all wrap this path the
// same way they wrap the row path. A ch-go server *proto.Exception is
// translated into the SAME *clickhouse.Exception shape classifyDriverErr
// already maps, so a memory-limit (code 241) or timeout (code 159) surfaces as
// the IDENTICAL typed error the row path produces.

// matrixColumns is the four-column projection the prom `query_range` matrix
// pivot drains: MetricName, the Attributes Map(String,String), TimeUnix, and
// Value, in that order. The columnar path engages ONLY when a result block's
// shape matches this exactly; any other shape (the five-column Loki log-stream
// projection, the metadata endpoints) falls back to the row path.
var matrixColumns = [...]string{"MetricName", "Attributes", "TimeUnix", "Value"}

// columnarMatrixDecoder owns the dedicated ch-go pool used for the columnar
// matrix decode. It is built (lazily-dialled, mirroring clickhouse.Open's lazy
// semantics) once per Client when CERBERUS_COLUMNAR_MATRIX_DECODE is set, and
// shared by every ForHead view of that Client.
type columnarMatrixDecoder struct {
	cfg Config

	mu   sync.Mutex
	pool *chpool.Pool // nil until first use (lazy dial)
}

// newColumnarMatrixDecoder constructs the decoder around a copy of the
// Client's Config. No socket is opened here — the pool is dialled lazily on
// the first matrix query so a flag-on replica that never serves a matrix query
// (or boots while ClickHouse is unreachable) comes up without a second dial,
// exactly as the row pool does.
func newColumnarMatrixDecoder(cfg Config) *columnarMatrixDecoder {
	return &columnarMatrixDecoder{cfg: cfg}
}

// acquirePool returns the lazily-dialled ch-go pool, building it on first use.
// chpool.New is lazy (it does not dial until a query acquires a connection),
// matching clickhouse.Open: a replica booting while ClickHouse is saturated
// comes up "started but unready" rather than crash-looping on a dial error.
func (d *columnarMatrixDecoder) acquirePool() (*chpool.Pool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pool != nil {
		return d.pool, nil
	}
	pool, err := chpool.New(context.Background(), chPoolOptions(d.cfg))
	if err != nil {
		return nil, err
	}
	d.pool = pool
	return pool, nil
}

// close tears down the ch-go pool if it was ever dialled. Safe to call when
// the pool was never built (flag on, no matrix query served).
func (d *columnarMatrixDecoder) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.pool != nil {
		d.pool.Close()
		d.pool = nil
	}
}

// chPoolOptions maps the existing Config onto ch-go's pool/client options. The
// auth, address, dial timeout, TLS, and read-timeout knobs map 1:1 off the
// SAME Config the clickhouse-go/v2 pool uses, so the columnar dial talks to the
// same endpoint with the same credentials and the same socket budget. Pool
// sizing mirrors the row pool's MaxOpenConns when configured.
func chPoolOptions(cfg Config) chpool.Options {
	addr := cfg.Addr
	if len(cfg.Addrs) > 0 {
		addr = cfg.Addrs[0]
	}
	opts := ch.Options{
		Address:  addr,
		Database: cfg.Database,
		User:     cfg.Username,
		Password: cfg.Password,
		TLS:      cfg.TLS,
	}
	if cfg.DialTimeout > 0 {
		opts.DialTimeout = cfg.DialTimeout
	}
	// ReadTimeout mirrors the row path's derivation: the explicit knob wins,
	// else the per-query wall-clock budget bounds a stale half-open read.
	switch {
	case cfg.ReadTimeout > 0:
		opts.ReadTimeout = cfg.ReadTimeout
	case cfg.QueryTimeout > 0:
		opts.ReadTimeout = cfg.QueryTimeout
	}
	poolOpts := chpool.Options{ClientOptions: opts}
	if cfg.MaxOpenConns > 0 {
		// Clamp to int32 (chpool.Options.MaxConns is int32); a pool-size config
		// past 2^31 is nonsensical, so cap rather than wrap.
		n := cfg.MaxOpenConns
		if n > math.MaxInt32 {
			n = math.MaxInt32
		}
		poolOpts.MaxConns = int32(n)
	}
	return poolOpts
}

// queryCursorColumnar runs the matrix query through the ch-go pool and returns
// a Cursor over its decoded Samples, reusing the row path's plumbing. The bool
// reports whether the columnar path handled the query: false means the result
// shape was not the matrix projection (the caller falls back to the row path),
// with err nil. A non-nil err is a real failure already classified + recorded
// against the breaker, exactly as the row path's QueryCursor open failure.
func (d columnarDecoder) queryCursorColumnar(c *Client, ctx context.Context, sql string, args ...any) (Cursor, bool, error) {
	// Bind the positional `?` args into the raw SQL body ch-go sends. An
	// unbindable arg type (outside the matrix path's closed string/int/float
	// set) defers to the row path BEFORE any dial or breaker touch — the
	// columnar decode is an optimisation, never a correctness gamble.
	body, ok := bindArgs(sql, args)
	if !ok {
		return nil, false, nil
	}

	pool, err := d.matrix.acquirePool()
	if err != nil {
		c.br.record(ctx, err)
		return nil, false, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}

	ctx = c.queryContext(ctx)
	queryID := queryIDFromContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)

	dec := &columnarCursor{
		budget:         budgetFromContext(ctx),
		maxSamples:     c.maxSamples,
		maxMemoryBytes: c.maxMemory,
		queryTimeout:   c.effectiveQueryTimeout(ctx),
	}
	rec := recorderFromContext(ctx)

	q := ch.Query{
		Body:     body,
		QueryID:  queryID,
		Result:   dec.results.Auto(),
		Settings: chSettings(c.querySettings(ctx)),
		OnResult: dec.onResult,
	}
	if rec != nil {
		q.OnProgress = progressBridge(rec)
	}

	runErr := pool.Do(ctx, q)
	// Record the open-call outcome against the breaker exactly once — the same
	// contract the row path keeps: a shape-mismatch (matrixMismatchErr) or a
	// budget rejection is NOT a CH failure, a transport/server error is.
	if isMatrixMismatch(runErr) {
		span.End()
		return nil, false, nil
	}
	breakerErr := runErr
	if isBudgetErr(runErr) {
		breakerErr = nil // budget rejection is the caller asking for too much, not an outage
	}
	c.br.record(ctx, breakerErr)
	if rec != nil {
		rec.flush()
	}
	if runErr != nil && !isBudgetErr(runErr) {
		span.RecordError(runErr)
		span.End()
		return nil, false, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, translateCHGoErr(runErr)))
	}

	// Stream is fully drained into dec.samples (bounded by the result set the
	// SAME max-samples budget the row path enforces caps). A budget crossing
	// leaves dec.err set so the cursor surfaces the identical
	// *TooManySamplesError on the same Next() boundary the row path would.
	dec.span = span
	return dec, true, nil
}

// columnarCursor is the Cursor returned by the columnar matrix path. It holds
// the fully-decoded Samples for the result set and replays them via Next() /
// Sample(), matching the rowsCursor surface. Memory stays bounded by the SAME
// max-samples budget the row path enforces: a result set that would exceed it
// stops decode early with the identical *TooManySamplesError.
type columnarCursor struct {
	results chproto.Results

	samples []Sample
	idx     int
	err     error

	// interner reuses the EXACT canonical-key interning the row path uses, so
	// SeriesID assignment order and the shared-map dedup are byte-identical.
	interner rowsCursor

	budget         *SampleBudget
	maxSamples     int64
	maxMemoryBytes int64
	queryTimeout   time.Duration
	seen           int64

	span      trace.Span
	closeOnce sync.Once
	closeErr  error
}

// onResult decodes one streamed result block columnarly. It validates the
// block is the four-column matrix shape (else returns matrixMismatchErr so the
// caller falls back to the row path), then walks the Attributes Map's offsets
// to build each contiguous series' label map ONCE rather than once per row.
func (d *columnarCursor) onResult(_ context.Context, block chproto.Block) error {
	if block.Rows == 0 {
		return nil
	}
	cols, ok := d.bindColumns()
	if !ok {
		return errMatrixMismatch
	}
	return d.decodeBlock(cols, block.Rows)
}

// matrixCols is the typed view of the four matrix columns after Auto inference.
type matrixCols struct {
	name  *chproto.ColStr
	attrs *chproto.ColMap[string, string]
	ts    timeColumn
	val   *chproto.ColFloat64
}

// timeColumn is the read surface shared by ColDateTime and ColDateTime64 — the
// TimeUnix column is one or the other depending on the table's column type, and
// both expose Row(i) time.Time.
type timeColumn interface {
	Row(i int) time.Time
}

// bindColumns type-asserts the Auto-inferred result columns into the matrix
// shape. A mismatch (wrong count, wrong names, wrong types — e.g. the
// five-column Loki projection) reports false so the caller falls back.
func (d *columnarCursor) bindColumns() (matrixCols, bool) {
	if len(d.results) != len(matrixColumns) {
		return matrixCols{}, false
	}
	for i, want := range matrixColumns {
		if d.results[i].Name != want {
			return matrixCols{}, false
		}
	}
	name, ok := d.results[0].Data.(*chproto.ColStr)
	if !ok {
		return matrixCols{}, false
	}
	attrs, ok := d.results[1].Data.(*chproto.ColMap[string, string])
	if !ok {
		return matrixCols{}, false
	}
	ts, ok := d.results[2].Data.(timeColumn)
	if !ok {
		return matrixCols{}, false
	}
	val, ok := d.results[3].Data.(*chproto.ColFloat64)
	if !ok {
		return matrixCols{}, false
	}
	return matrixCols{name: name, attrs: attrs, ts: ts, val: val}, true
}

// decodeBlock turns one bound block into Samples. The Attributes Map exposes
// Offsets/Keys/Values as typed slices: Offsets[i] is the exclusive end index
// into Keys/Values for row i. A label map is rebuilt only when a row's
// key/value span DIFFERS from the previous row's — for the long-window matrix
// shape (a series' rows are contiguous, identical label set), this collapses
// to one map build per series run. Interning then dedups across blocks exactly
// as the row path does.
func (d *columnarCursor) decodeBlock(cols matrixCols, rows int) error {
	offsets := cols.attrs.Offsets
	var (
		prevStart   = -1
		prevEnd     = -1
		curLabels   map[string]string
		curInterned map[string]string
		curID       uint32
		haveSeries  bool
	)
	for r := 0; r < rows; r++ {
		if !d.charge() {
			d.err = &TooManySamplesError{Limit: d.budgetLimit()}
			return errBudgetExceeded
		}
		// Map offsets index into the Keys/Values slices, so they are bounded
		// by the block's total key count and fit in int on a 64-bit platform —
		// this is ch-go's own ColMap.Row contract (it converts identically).
		end := int(offsets[r]) //nolint:gosec // map offset bounded by block key count
		start := 0
		if r > 0 {
			start = int(offsets[r-1]) //nolint:gosec // map offset bounded by block key count
		}
		if !haveSeries || start != prevStart || end != prevEnd ||
			!sameSpan(cols.attrs, start, end, curLabels) {
			curLabels = buildLabelMap(cols.attrs, start, end)
			curInterned, curID = d.interner.internLabels(curLabels)
			prevStart, prevEnd = start, end
			haveSeries = true
		}
		d.samples = append(d.samples, Sample{
			MetricName: cols.name.Row(r),
			Labels:     curInterned,
			SeriesID:   curID,
			Timestamp:  cols.ts.Row(r),
			Value:      cols.val.Row(r),
		})
	}
	return nil
}

// charge consumes one row against the active budget, returning false when the
// budget is exhausted. It mirrors rowsCursor.Next's precedence: a per-request
// shared *SampleBudget wins over the per-cursor maxSamples, and both produce
// the IDENTICAL over-limit boundary.
func (d *columnarCursor) charge() bool {
	d.seen++
	if d.budget != nil {
		return d.budget.consume(1)
	}
	if d.maxSamples > 0 && d.seen > d.maxSamples {
		return false
	}
	return true
}

// budgetLimit returns the configured limit the over-budget error reports,
// matching rowsCursor: the shared budget's limit when present, else the
// per-cursor max.
func (d *columnarCursor) budgetLimit() int64 {
	if d.budget != nil {
		return d.budget.Limit()
	}
	return d.maxSamples
}

// buildLabelMap materialises the label map for a Map row's [start,end) span
// from the exported Keys/Values typed slices — the columnar win the row path
// cannot reach. A nil span (no labels) returns nil so interning assigns the
// zero SeriesID, matching the row path's nil-map handling.
func buildLabelMap(m *chproto.ColMap[string, string], start, end int) map[string]string {
	if end <= start {
		// An empty Map row decodes to a non-nil empty map under
		// clickhouse-go's Scan, so match that shape for byte-exact parity.
		return map[string]string{}
	}
	out := make(map[string]string, end-start)
	for i := start; i < end; i++ {
		out[m.Keys.Row(i)] = m.Values.Row(i)
	}
	return out
}

// sameSpan reports whether the Map span [start,end) yields exactly the label
// set already built as want — used to confirm a run boundary cheaply before
// rebuilding the map. It compares keys+values directly off the typed slices.
func sameSpan(m *chproto.ColMap[string, string], start, end int, want map[string]string) bool {
	if end-start != len(want) {
		return false
	}
	for i := start; i < end; i++ {
		v, ok := want[m.Keys.Row(i)]
		if !ok || v != m.Values.Row(i) {
			return false
		}
	}
	return true
}

// Next advances to the next decoded Sample.
func (d *columnarCursor) Next() bool {
	if d.err != nil {
		return false
	}
	if d.idx >= len(d.samples) {
		return false
	}
	d.idx++
	return true
}

// Sample returns the Sample the most recent Next() landed on.
func (d *columnarCursor) Sample() Sample {
	if d.idx == 0 || d.idx > len(d.samples) {
		return Sample{}
	}
	return d.samples[d.idx-1]
}

// Err returns the budget-exceeded error, if any. Transport/server errors are
// surfaced from QueryCursor's open call, not here, matching the row path.
func (d *columnarCursor) Err() error { return d.err }

// Close ends the execute span exactly once. The ch-go pool itself is owned by
// the Client (closed on Client.Close), not the cursor — the per-query
// connection was already released when pool.Do returned.
func (d *columnarCursor) Close() error {
	d.closeOnce.Do(func() {
		if d.span != nil {
			if d.err != nil {
				d.span.RecordError(d.err)
			}
			d.span.End()
			d.span = nil
		}
	})
	return d.closeErr
}

// translateCHGoErr maps a ch-go server *proto.Exception onto the
// *clickhouse.Exception shape classifyDriverErr already classifies, so a
// memory-limit (code 241) or timeout (code 159) from the columnar dial
// surfaces as the IDENTICAL typed error (*MemoryLimitError / *QueryTimeoutError)
// the row path produces. Any other error passes through untouched.
func translateCHGoErr(err error) error {
	ex, ok := ch.AsException(err)
	if !ok {
		return err
	}
	return &clickhouse.Exception{
		Code:    int32(ex.Code), //nolint:gosec // CH error codes are small positive ints (proto.Error)
		Name:    ex.Name,
		Message: ex.Message,
	}
}
