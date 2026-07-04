package autotune

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/routerrules"
	"github.com/tsouza/cerberus/internal/solver"
)

// fakeFloorSource is a fixed OOMFloorSource for loop tests.
type fakeFloorSource struct{ f routerrules.OOMFloor }

func (s fakeFloorSource) OOMFloor(_ context.Context) (routerrules.OOMFloor, error) {
	return s.f, nil
}

func newPlanner() *solver.Planner {
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	return &solver.Planner{Cfg: cfg}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newLoop(p *solver.Planner, floor routerrules.OOMFloor) *Loop {
	newTuner := func() *routerrules.Autotuner {
		return routerrules.NewAutotuner(fakeFloorSource{f: floor})
	}
	return New(p, newTuner, time.Hour, quietLogger(), NewReporter(Status{}))
}

// TestLoop_Tick_Applies drives one tick with an OOM floor below the default
// fan-out and asserts the Planner's live thresholds dropped.
func TestLoop_Tick_Applies(t *testing.T) {
	p := newPlanner()
	if f0, p0 := p.CurrentThresholds(); f0 != 16 || p0 != 4000 {
		t.Fatalf("unexpected default thresholds: fanout=%d pairs=%d", f0, p0)
	}

	l := newLoop(p, routerrules.OOMFloor{HasSignal: true, MinFanout: 9, MinAnchors: 241})
	l.tick(context.Background())

	if gotF, gotP := p.CurrentThresholds(); gotF != 9 || gotP != 2169 {
		t.Errorf("after tick: fanout=%d pairs=%d, want 9 / 2169", gotF, gotP)
	}
}

// TestLoop_Tick_ReportsStatus asserts each tick publishes its outcome to the
// Reporter for /info/autotune: live thresholds, tick count, and the last fit.
func TestLoop_Tick_ReportsStatus(t *testing.T) {
	p := newPlanner()
	rep := NewReporter(Status{Enabled: true, Active: true, Reason: ReasonStatusActive})
	newTuner := func() *routerrules.Autotuner {
		return routerrules.NewAutotuner(fakeFloorSource{f: routerrules.OOMFloor{HasSignal: true, MinFanout: 9, MinAnchors: 241}})
	}
	l := New(p, newTuner, time.Hour, quietLogger(), rep)

	l.tick(context.Background())

	st := rep.Snapshot()
	if st.Reason != ReasonStatusActive {
		t.Errorf("static Reason clobbered: %q", st.Reason)
	}
	if st.Ticks != 1 {
		t.Errorf("Ticks = %d, want 1", st.Ticks)
	}
	if st.Live.MinFanout != 9 || st.Live.MinAnchorPairs != 2169 {
		t.Errorf("Live = %+v, want {9 2169}", st.Live)
	}
	if st.LastFit == nil {
		t.Fatal("LastFit is nil after a tick")
	}
	if !st.LastFit.Changed || !st.LastFit.HasOOMSignal || st.LastFit.OOMMinFanout != 9 {
		t.Errorf("LastFit = %+v, want changed+signal at OOM fanout 9", st.LastFit)
	}
	if st.LastFit.At.IsZero() {
		t.Error("LastFit.At not stamped")
	}
}

// TestLoop_Tick_ColdStartNoOp asserts an empty (no-signal) floor leaves the
// configured thresholds untouched — the default-on safety guarantee.
func TestLoop_Tick_ColdStartNoOp(t *testing.T) {
	p := newPlanner()
	l := newLoop(p, routerrules.OOMFloor{HasSignal: false})
	l.tick(context.Background())

	if gotF, gotP := p.CurrentThresholds(); gotF != 16 || gotP != 4000 {
		t.Errorf("cold start changed thresholds: fanout=%d pairs=%d, want 16 / 4000", gotF, gotP)
	}
}

// TestLoop_Run_StopsOnCancel asserts Run returns promptly on ctx cancel and
// leaves no goroutine behind.
func TestLoop_Run_StopsOnCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := newPlanner()
	newTuner := func() *routerrules.Autotuner {
		return routerrules.NewAutotuner(fakeFloorSource{f: routerrules.OOMFloor{HasSignal: false}})
	}
	l := New(p, newTuner, time.Millisecond, quietLogger(), NewReporter(Status{}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

// TestLoop_Tick_RaceFree exercises the atomic threshold swap concurrently with
// reads (the Plan hot path reads through the same overlay). Run under -race.
func TestLoop_Tick_RaceFree(t *testing.T) {
	p := newPlanner()
	l := newLoop(p, routerrules.OOMFloor{HasSignal: true, MinFanout: 9, MinAnchors: 241})

	var wg sync.WaitGroup
	const iters = 1000
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			l.tick(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = p.CurrentThresholds()
		}
	}()
	wg.Wait()
}
