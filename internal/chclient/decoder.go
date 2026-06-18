package chclient

import (
	"context"
	"fmt"
)

// decoder.go — the cursor-decode strategy, resolved ONCE at Client
// construction.
//
// QueryCursor's per-query dispatch holds NO branch: the Client carries a
// single, always-non-nil cursorDecoder, wired at boot. The default is the
// concrete rowDecoder (the clickhouse-go/v2 row path, byte-unchanged). When the
// columnar matrix decode is enabled the Client swaps in a columnarDecoder that
// embeds the rowDecoder as its fallback. Query-time is then a plain method call
// — `c.cursorDecoder.decode(c, ...)` — with every optimisation already landed
// at boot. The dispatch site never sees the decision, nor a nil/presence check
// standing in for it.
//
// The enable decision has two wiring points, both at boot: construction
// (newCursorDecoder, when Config.ColumnarMatrixDecode is set — the path the
// parity test drives) and UseColumnarMatrixDecode (the production path: cmd
// installs it after the chopt EnabledSet resolves columnar_result_decode, since
// that resolve needs the version probe and therefore the already-built client).
// Either way the strategy is resolved ONCE, before any handler serves.
//
// The strategy is client-agnostic: decode takes the *Client to act on as an
// argument rather than capturing one, so a ForHead shallow copy reuses the SAME
// strategy value yet every breaker / budget / span read resolves off the view
// the call dispatched from (each ForHead view keeps its own per-head breaker).

// cursorDecoder opens a forward-only Cursor over the result of a query. It is
// the strategy QueryCursor dispatches to after the breaker admits the call.
// The breaker allow() gate, and the eventual cursor lifecycle, stay in
// QueryCursor / Close; the decoder owns the open path (and, for the columnar
// strategy, the per-block shape decision and fallback) entirely.
type cursorDecoder interface {
	// decode opens a Cursor over sql bound with the positional args against
	// the supplied client (the ForHead view the call dispatched from). It is
	// reached only after the breaker admitted the call; it is responsible for
	// recording the open-call outcome against that client's breaker exactly as
	// the row path's QueryCursor did.
	decode(c *Client, ctx context.Context, sql string, args ...any) (Cursor, error)

	// close releases any decode-strategy-owned resources at Client.Close. The
	// row strategy owns none (no-op); the columnar strategy tears down its
	// dedicated ch-go pool if it was ever dialled.
	close()
}

// newCursorDecoder resolves the decode strategy from a Config: the columnar
// strategy (embedding the row path as its fallback) when
// Config.ColumnarMatrixDecode is set, the bare row path otherwise. It is the
// single wiring point shared by Client construction and the boot-time
// UseColumnarMatrixDecode install, so the two paths can never drift on how the
// columnar strategy is built.
func newCursorDecoder(cfg Config) cursorDecoder {
	if cfg.ColumnarMatrixDecode {
		return columnarDecoder{
			matrix:   newColumnarMatrixDecoder(cfg),
			fallback: rowDecoder{},
		}
	}
	return rowDecoder{}
}

// UseColumnarMatrixDecode installs the columnar matrix decode strategy at boot,
// AFTER construction but BEFORE any handler serves a query. It exists because
// the production decision source -- the resolved chopt EnabledSet -- is only
// known after cmd/cerberus probes the server version, which needs this very
// client; New therefore comes up on the row path and cmd swaps the strategy in
// here once columnar_result_decode is confirmed enabled. The decode strategy is
// still resolved exactly ONCE per process (just slightly later than New) and is
// never toggled at query time; the dispatch site in QueryCursor stays
// branch-free. on=false is a no-op (the row default already stands); a redundant
// on=true re-wire tears down any previously-installed columnar pool first so the
// call leaks no ch-go socket if invoked twice.
//
// cfg is the SAME chclient.Config the client was built from: the columnar
// strategy owns a second ch-go dial mapped 1:1 off it (addr / auth / TLS / dial
// timeout), so cmd passes cfg.ClickHouse straight through. It MUST be called
// before the client begins serving (cmd does, between the optimization resolve
// and handler.Mount); it is not safe to call concurrently with in-flight
// QueryCursor calls.
func (c *Client) UseColumnarMatrixDecode(on bool, cfg Config) {
	if !on {
		return
	}
	c.cursorDecoder.close()
	cfg.ColumnarMatrixDecode = true
	c.cursorDecoder = newCursorDecoder(cfg)
}

// rowDecoder is the default, always-present decode strategy: the
// clickhouse-go/v2 row path. It is the Client's cursorDecoder when the
// columnar flag is off, and the embedded fallback the columnarDecoder
// delegates to for any non-matrix / unbindable shape. It is stateless — the
// client to act on arrives as a decode argument.
type rowDecoder struct{}

// decode opens the row-path Cursor: it stamps the query context + execute
// span, opens the driver rows, records the open outcome against the breaker,
// and wraps the rows in a *rowsCursor carrying the same budget / memory /
// timeout knobs. This is the body that lived inline in QueryCursor before the
// strategy was extracted — byte-for-byte the flag-off behaviour.
func (rowDecoder) decode(c *Client, ctx context.Context, sql string, args ...any) (Cursor, error) {
	ctx = c.queryContext(ctx)
	ctx, span := startExecuteSpan(ctx, sql, c.addr)
	rows, err := c.queryOpen(ctx, sql, args...)
	c.br.record(ctx, err)
	if err != nil {
		span.RecordError(err)
		span.End()
		return nil, fmt.Errorf("chclient: query: %w", c.classifyDriverErr(ctx, err))
	}
	return &rowsCursor{
		rows:           rows,
		span:           span,
		rec:            recorderFromContext(ctx),
		maxSamples:     c.maxSamples,
		maxMemoryBytes: c.maxMemory,
		queryTimeout:   c.effectiveQueryTimeout(ctx),
		budget:         budgetFromContext(ctx),
	}, nil
}

// close is a no-op: the row path owns no decode-strategy resources (the
// clickhouse-go/v2 pool is owned by the Client and closed via c.conn.Close).
func (rowDecoder) close() {}

// columnarDecoder is the decode strategy wired at boot when the columnar matrix
// decode is enabled. It routes the four-column `query_range`
// matrix shape through a dedicated ch-go dial (each series' label map built
// once per run instead of once per row) and embeds a rowDecoder as the
// fallback for every non-matrix / unbindable shape — the shape decision lives
// INSIDE this strategy (queryCursorColumnar's bindArgs + the block-shape
// assertion), never at the QueryCursor dispatch site.
type columnarDecoder struct {
	matrix   *columnarMatrixDecoder
	fallback rowDecoder
}

// decode tries the columnar matrix path first. queryCursorColumnar reports via
// its bool whether the shape was the matrix projection (and the args were
// bindable): a false with nil err means "not the matrix shape" — delegate to
// the embedded row path, byte-unchanged from the flag-off behaviour. A non-nil
// err is a real, already-classified-and-recorded failure and surfaces as-is.
func (d columnarDecoder) decode(c *Client, ctx context.Context, sql string, args ...any) (Cursor, error) {
	cur, ok, err := d.queryCursorColumnar(c, ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	if ok {
		return cur, nil
	}
	return d.fallback.decode(c, ctx, sql, args...)
}

// close tears down the dedicated ch-go pool if it was ever dialled.
func (d columnarDecoder) close() { d.matrix.close() }
