package solver

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tsouza/cerberus/internal/chclient"
)

// shardCursor concatenates the K per-shard sample streams behind the
// chclient.Cursor interface (docs §"Result composition"). Composition is
// CONCATENATION, not evaluation: anchors are disjoint across slices by
// construction, so cerberus computes nothing — zero arithmetic, zero
// merge-by-key. The composer drains channel 0 (oldest slice) to exhaustion,
// then channel 1, ..., keeping per-series timestamps nearly ascending so
// the handler's insertion sort stays ~O(n).
//
// All per-request state — child cursors, interned labels, the errgroup, the
// gate / admit releases — is born with the request and dies at Close. There
// is no cross-request cache: the no-caching invariant is untouched.
type shardCursor struct {
	k   int
	cfg Config

	// client opens the per-shard cursors (set by the Executor).
	client CursorQuerier

	// gctx is the cause-carrying group context. Err() returns its cause
	// when non-Canceled.
	gctx        context.Context
	cancelCause context.CancelCauseFunc
	timer       *time.Timer
	g           *errgroup.Group

	// chans[i] carries shard i's samples, oldest-first. The composer drains
	// them in index order. Each is closed by its producer.
	chans []chan chclient.Sample

	// childCursors[i] is shard i's open cursor, registered by the producer
	// so Close can tear it down even mid-drain. Guarded by childMu.
	childMu      sync.Mutex
	childCursors []chclient.Cursor

	// releaseGate / releaseAdmit free the global gate slots and the admit
	// top-up. Each is idempotent; Close invokes them exactly once.
	releaseGate  func()
	releaseAdmit func()

	// interned re-interns labels ACROSS shards keyed by the canonical label
	// key, so the same series arriving from K shards holds ONE label-map
	// copy during the drain (not K). It ALSO stamps each distinct series a
	// stable cross-shard [chclient.Sample.SeriesID] — the child rowsCursors
	// each number their series independently from 1, so without this
	// re-numbering the composed stream would hand two genuinely-distinct
	// series the same per-shard ordinal, and a SeriesID-keyed consumer (the
	// prom matrix/vector label memo) would alias them. Per-request state.
	interned map[string]internedSeries
	// internSeq assigns each newly-seen canonical label key its 1-based
	// cross-shard SeriesID; it advances on first sight of a key, so the
	// composed cursor presents ONE consistent SeriesID namespace regardless
	// of which shard a row arrived on.
	internSeq uint32

	// drainIdx is the index of the channel the composer is currently
	// draining (0..k). Only Next touches it (single-consumer), so no lock.
	drainIdx int

	// cur is the sample the last successful Next landed on.
	cur chclient.Sample

	// emitted counts samples handed out via Next, enforced against
	// cfg.MaxOutputRows.
	emitted int64

	// err latches the first terminal error (output-cap or a shard error
	// surfaced via the group). Next returns false once set.
	err error

	closeOnce sync.Once
	closeErr  error
}

// registerChild records a producer's open cursor so Close can close it even
// if the producer is blocked mid-send when the group cancels. If the cursor
// is registered after Close already ran (a late open racing teardown), it
// is closed immediately so no connection leaks.
func (sc *shardCursor) registerChild(idx int, cur chclient.Cursor) {
	sc.childMu.Lock()
	closedAlready := sc.childCursors == nil
	if !closedAlready {
		sc.childCursors[idx] = cur
	}
	sc.childMu.Unlock()
	if closedAlready {
		_ = cur.Close()
	}
}

// Next advances to the next concatenated sample. It drains channel 0 to
// exhaustion, then channel 1, and so on — oldest-first. It returns false at
// end-of-stream or on a terminal error (check Err()). The per-request
// output-row cap (cfg.MaxOutputRows) is enforced here with a distinct typed
// 422 (OutputCapError) — NOT the upstream max-samples message.
func (sc *shardCursor) Next() bool {
	if sc.err != nil {
		return false
	}
	for sc.drainIdx < sc.k {
		select {
		case s, ok := <-sc.chans[sc.drainIdx]:
			if !ok {
				// This shard is exhausted; advance to the next oldest.
				sc.drainIdx++
				continue
			}
			// Enforce the composed output-row cap BEFORE handing the row
			// out. A high-cardinality success must not OOM the shared
			// gateway heap: this is a distinct 422, never the upstream
			// max-samples text.
			if sc.cfg.MaxOutputRows > 0 && sc.emitted >= sc.cfg.MaxOutputRows {
				sc.err = &OutputCapError{Limit: sc.cfg.MaxOutputRows}
				// Cancel the group so producers stop streaming; Close will
				// drain + tear down.
				sc.cancelCause(sc.err)
				return false
			}
			s.Labels, s.SeriesID = sc.reintern(s.Labels)
			sc.cur = s
			sc.emitted++
			return true
		case <-sc.gctx.Done():
			// A shard failed (or the request was cancelled / timed out).
			// Surface the cause as the terminal error; Err() classifies it.
			sc.err = sc.causeErr()
			return false
		}
	}
	// All channels drained without a cancel. But a producer may have
	// returned an error WITHOUT cancelling fast enough to beat the drain
	// (e.g. an open-time error on the last shard while earlier shards
	// streamed clean). Reap the group to surface a first-error-wins result.
	if werr := sc.g.Wait(); werr != nil {
		sc.err = sc.normalizeGroupErr(werr)
		return false
	}
	return false
}

// causeErr maps the group's cancel cause onto the terminal error Err()
// returns. The solver-timeout sentinel and real shard errors pass through;
// a plain context.Canceled (client gone) passes through too so the handler
// maps it breaker-neutrally.
func (sc *shardCursor) causeErr() error {
	cause := context.Cause(sc.gctx)
	if cause == nil {
		return sc.gctx.Err()
	}
	if errors.Is(cause, errSolverTimeout) {
		return &SolverTimeoutError{Timeout: sc.cfg.Timeout.String()}
	}
	return cause
}

// normalizeGroupErr maps an errgroup.Wait() result onto the terminal error.
// It preserves the deterministic first error and only translates the
// timeout sentinel into the typed SolverTimeoutError.
func (sc *shardCursor) normalizeGroupErr(werr error) error {
	if errors.Is(werr, errSolverTimeout) {
		return &SolverTimeoutError{Timeout: sc.cfg.Timeout.String()}
	}
	return werr
}

// internedSeries pairs the canonical shared label-map instance with the
// 1-based cross-shard ordinal the composer assigned it on first sight, so
// [shardCursor.reintern] can hand both back: the shared map (memory dedup)
// and a composed-stream-stable series identity (so a SeriesID-keyed
// consumer can memoise per-series work without aliasing two shards' rows).
type internedSeries struct {
	labels map[string]string
	id     uint32
}

// reintern returns the canonical shared map instance for a label set across
// ALL shards (so the same series from K shards holds one map during the
// drain) plus a stable cross-shard SeriesID. The child rowsCursors restart
// their per-shard numbering at 1, so the composer re-assigns the ordinal
// here against its OWN first-seen order — turning the concatenated stream
// into one consistent SeriesID namespace. A nil label set returns id 0
// (the "not interned" sentinel; a SeriesID-keyed memo recomputes for it).
// Labels stay read-only.
func (sc *shardCursor) reintern(labels map[string]string) (map[string]string, uint32) {
	if labels == nil {
		return nil, 0
	}
	key := canonicalLabelKey(labels)
	if cached, ok := sc.interned[key]; ok {
		return cached.labels, cached.id
	}
	sc.internSeq++
	sc.interned[key] = internedSeries{labels: labels, id: sc.internSeq}
	return labels, sc.internSeq
}

// Sample returns the row the most recent successful Next landed on.
func (sc *shardCursor) Sample() chclient.Sample { return sc.cur }

// Inspected returns the number of composed rows handed out via Next — the
// emitted counter the output-row cap is enforced against. It is the routed
// path's drain count, the same per-request quantity the single-cursor row
// path reports via seen.
func (sc *shardCursor) Inspected() int64 { return sc.emitted }

// Err returns the terminal error that stopped iteration, or nil at a clean
// end-of-stream. First-error-wins, cause-threaded: a sibling's induced
// cancel never masks the deterministic shard error.
func (sc *shardCursor) Err() error {
	if sc.err != nil {
		return sc.err
	}
	// No terminal error latched in Next. If a producer errored after the
	// composer already drained every channel, the cause is on gctx.
	if cause := context.Cause(sc.gctx); cause != nil && !errors.Is(cause, context.Canceled) {
		if errors.Is(cause, errSolverTimeout) {
			return &SolverTimeoutError{Timeout: sc.cfg.Timeout.String()}
		}
		return cause
	}
	return nil
}

// Close tears the routed request down exactly once (docs §"Lifecycle"):
// cancel gctx → wait all producers → close every child cursor → release the
// gate slots + admit top-up → return the first non-nil child close error.
// Safe to call from multiple goroutines; the teardown runs under sync.Once.
func (sc *shardCursor) Close() error {
	sc.closeOnce.Do(func() {
		// Cancel with a benign cause so any still-running producer unblocks
		// and exits. If a real cause already latched (shard error / output
		// cap / timeout), CancelCause keeps the FIRST cause — this call is a
		// no-op on the cause, only ensuring cancellation.
		sc.cancelCause(context.Canceled)

		// Wait for every producer to return. They all select on gctx.Done()
		// while sending, so this terminates promptly — the goleak guarantee.
		_ = sc.g.Wait()

		if sc.timer != nil {
			sc.timer.Stop()
		}

		// Close every child cursor. Take ownership of the slice under the
		// lock and nil it so a late registerChild closes its own cursor.
		sc.childMu.Lock()
		children := sc.childCursors
		sc.childCursors = nil
		sc.childMu.Unlock()
		for _, cur := range children {
			if cur == nil {
				continue
			}
			if cerr := cur.Close(); cerr != nil && sc.closeErr == nil {
				sc.closeErr = cerr
			}
		}

		// Release the gate slots and the admit top-up — exactly once each.
		if sc.releaseGate != nil {
			sc.releaseGate()
		}
		if sc.releaseAdmit != nil {
			sc.releaseAdmit()
		}
	})
	return sc.closeErr
}

// canonicalLabelKey is the cross-shard intern key: keys sorted
// ASCII-ascending, pairs joined "k=v\x00" so two distinct label sets cannot
// alias. Mirrors internal/api/format.CanonicalKey and chclient's per-cursor
// key — duplicated locally because internal/solver must not import the api
// packages (and re-implementing avoids an import-cycle surface).
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
