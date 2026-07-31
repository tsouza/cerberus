package regression

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/optcorpus"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// e2eReconcileInterval drives the reconciler's tick fast enough that the test
// does not wait on a real observability cadence. The sink signals completion, so
// the interval bounds latency, never correctness.
const e2eReconcileInterval = 2 * time.Millisecond

// e2eSinkWait bounds how long the test waits for the reconciled row before
// declaring the pipeline broken.
const e2eSinkWait = 5 * time.Second

// e2eSearchWindow is the bounded TraceQL search window the offline explain Lang
// needs so lowering emits a BOUNDED spans scan.
const e2eSearchWindow = time.Hour

// e2eSearchStep sizes the TraceQL metrics matrix; it must be > 0.
const e2eSearchStep = time.Minute

// echoQueryLogSource is a QueryLogSource that reports every id it is asked
// about as a clean finish with a fixed cost. It stands in for system.query_log
// without needing to know the query_id the engine minted: the reconciler asks
// for exactly the ids it observed, so echoing them back joins whatever the
// dispatch seam actually recorded.
type echoQueryLogSource struct{}

// e2eReadRows is the cost the echo source reports, so the test can tell a real
// join apart from a zero-valued row.
const e2eReadRows = 7

func (echoQueryLogSource) FinishedByQueryID(_ context.Context, ids []string) ([]optcorpus.SourceRow, error) {
	rows := make([]optcorpus.SourceRow, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, optcorpus.SourceRow{
			QueryID:    id,
			ReadRows:   e2eReadRows,
			ExitStatus: optcorpus.ExitOK,
		})
	}
	return rows, nil
}

// signallingSink captures written rows and publishes the first non-empty batch
// on a channel, so the test synchronises on the pipeline's own output instead of
// sleeping for a tick.
type signallingSink struct {
	rows chan []optcorpus.Row
}

func (s *signallingSink) Write(rows []optcorpus.Row) error {
	if len(rows) == 0 {
		return nil
	}
	select {
	case s.rows <- rows:
	default: // A later batch has nothing to add; the first one is the assertion.
	}
	return nil
}

func (s *signallingSink) Close() error { return nil }

// e2eQuerier is the engine's Querier: it returns no samples, because this test
// asserts on what the CORPUS recorded about the dispatch, not on query results.
type e2eQuerier struct{}

func (e2eQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return nil, nil
}

// e2eTracedContext returns a context carrying a span, without which
// chclient.EnsureQueryID mints no query_id and the corpus dispatch seam
// short-circuits for lack of a join key.
func e2eTracedContext(ctx context.Context) context.Context {
	tid := trace.TraceID{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30}
	sid := trace.SpanID{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid})
	return trace.ContextWithSpanContext(ctx, sc)
}

// TestCorpusNonPromQLReasonReachesPersistedRow drives a REAL non-PromQL query
// (TraceQL) through the real engine into a real optcorpus.Reconciler and asserts
// the persisted Row carries decision_reason="non-promql" with no route and no
// solver geometry.
//
// It exists because the two unit layers each proved half of this and the whole
// was still broken. internal/engine's solver-wiring test proved the dispatch
// SEAM emitted the reason; internal/optcorpus's reconciler test proved a Record
// carrying one reconciles. Neither could see that joinRouteFeatures returned
// early on Present=false and dropped the reason on the floor between them, so
// the feature shipped green at both layers and recorded nothing. This is the
// only test that spans the seam, and it is deliberately in test/regression
// rather than in either package: neither may import the other in production
// code, so the join can only be exercised from outside both.
//
// It is a genuine end-to-end assertion — real Lang, real optimizer, real
// reconciler, real join — with only the ClickHouse boundary faked (the querier
// and the query_log source).
func TestCorpusNonPromQLReasonReachesPersistedRow(t *testing.T) {
	t.Parallel()

	sink := &signallingSink{rows: make(chan []optcorpus.Row, 1)}
	rec := optcorpus.New(echoQueryLogSource{}, sink, optcorpus.Options{
		RingCapacity: 8,
		Interval:     e2eReconcileInterval,
	})

	runCtx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec.Run(runCtx)
	}()

	eng := &engine.Engine{
		Optimizer:     optimizer.Default(),
		Client:        e2eQuerier{},
		QueryObserver: rec,
	}

	// A real TraceQL head: the offline-explain Lang wraps the same lowering the
	// /api/search handler drives, so the language the engine reports is the
	// production TraceQL name rather than a test double's.
	lang := tempo.NewExplainLang(schema.DefaultOTelTraces(), e2eSearchStep)
	end := time.Now()
	ctx := tempo.ExplainContext(
		e2eTracedContext(context.Background()), end.Add(-e2eSearchWindow), end, 0,
	)

	if _, err := eng.Query(ctx, lang, `{ name = "GET /" }`); err != nil {
		t.Fatalf("TraceQL Query: %v", err)
	}

	var rows []optcorpus.Row
	select {
	case rows = <-sink.rows:
	case <-time.After(e2eSinkWait):
		t.Fatal("no corpus row reconciled; the dispatch never reached the sink")
	}
	stop()
	<-done

	if len(rows) != 1 {
		t.Fatalf("reconciled rows = %d; want 1", len(rows))
	}
	got := rows[0]

	if got.DecisionReason != engine.CorpusReasonNonPromQL {
		t.Errorf("persisted decision_reason = %q; want %q — the reason did not survive the join",
			got.DecisionReason, engine.CorpusReasonNonPromQL)
	}
	// The boundary the reason must not cross: a non-PromQL row never enters
	// Solver.Classify, so it has no route and no geometry to report.
	if got.Route != "" || got.KShards != 0 || got.NAnchors != 0 || got.Fanout != 0 ||
		got.CumulativeD != 0 || got.OuterRange != 0 || got.Step != 0 {
		t.Errorf("non-PromQL row carries routing output: %+v", got)
	}
	// The row is a real join, not a fabricated shell: proving the reason rides
	// the same row the query_log cost landed on.
	if got.ReadRows != e2eReadRows {
		t.Errorf("joined ReadRows = %d; want %d", got.ReadRows, e2eReadRows)
	}
	if got.Language == "" {
		t.Error("persisted language is empty; the row cannot be attributed to a head")
	}
}
