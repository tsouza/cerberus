package solver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFrontierReader is a programmable solver.FrontierReader for the loop
// tests: it returns canned samples (or an error) and counts reads, so a test
// can drive recalibrate and assert what the loop did.
type fakeFrontierReader struct {
	mu      sync.Mutex
	samples []CorpusSample
	err     error
	reads   atomic.Int64
}

func (f *fakeFrontierReader) ReadFrontier(ctx context.Context) ([]CorpusSample, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.samples, f.err
}

func (f *fakeFrontierReader) set(samples []CorpusSample, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.samples, f.err = samples, err
}

// TestSelfTuner_NilReaderOrPlannerReturnsNil: the off-by-default wiring (nil
// reader / planner) must start nothing and be Stop-safe.
func TestSelfTuner_NilReaderOrPlannerReturnsNil(t *testing.T) {
	if st := StartSelfTuner(context.Background(), SelfTuneParams{Planner: nil, Reader: &fakeFrontierReader{}}); st != nil {
		st.Stop()
		t.Fatal("nil planner must return a nil tuner")
	}
	if st := StartSelfTuner(context.Background(), SelfTuneParams{Planner: NewPlanner(calibDefaults()), Reader: nil}); st != nil {
		st.Stop()
		t.Fatal("nil reader must return a nil tuner")
	}
	// Stop on a nil *SelfTuner must not panic.
	var none *SelfTuner
	none.Stop()
}

// TestSelfTuner_ImmediatePassSwapsCalibratedConfig: with a clear-frontier
// corpus the loop's immediate pass must install tightened thresholds into the
// Planner.
func TestSelfTuner_ImmediatePassSwapsCalibratedConfig(t *testing.T) {
	defaults := calibDefaults()
	pl := NewPlanner(defaults)
	reader := &fakeFrontierReader{}
	reader.set(clearFrontierCorpus(), nil)

	st := StartSelfTuner(context.Background(), SelfTuneParams{
		Planner:  pl,
		Reader:   reader,
		Defaults: defaults,
		Interval: time.Hour, // immediate pass only; ticker won't fire in the test
	})
	defer st.Stop()

	// The immediate pass runs in the goroutine; wait for the first read.
	waitFor(t, func() bool { return reader.reads.Load() >= 1 })
	// Then wait for the swap to land (SetConfig happens right after the read).
	waitFor(t, func() bool { return pl.Cfg().MinFanout < defaults.MinFanout })

	got := pl.Cfg()
	if got.MinFanout >= defaults.MinFanout || got.MinAnchorPairs >= defaults.MinAnchorPairs {
		t.Fatalf("expected tightened config installed, got %+v", got)
	}
}

// TestSelfTuner_NoOpReinstallsDefaultsVerbatim: a no-signal corpus must
// leave the Planner on the defaults (the no-op-on-no-signal behavior, end to end
// through the loop).
func TestSelfTuner_NoOpReinstallsDefaultsVerbatim(t *testing.T) {
	defaults := calibDefaults()
	pl := NewPlanner(defaults)
	reader := &fakeFrontierReader{}
	reader.set(noSignalCorpus(), nil)

	st := StartSelfTuner(context.Background(), SelfTuneParams{
		Planner: pl, Reader: reader, Defaults: defaults, Interval: time.Hour,
	})
	defer st.Stop()

	waitFor(t, func() bool { return reader.reads.Load() >= 1 })
	// Give the swap a moment; defaults must remain in force.
	time.Sleep(20 * time.Millisecond)
	if pl.Cfg() != defaults {
		t.Fatalf("no-signal corpus must keep defaults, got %+v", pl.Cfg())
	}
}

// TestSelfTuner_FailOpenOnReaderError: a reader error must keep the current
// Config and never crash; subsequent good reads must still take effect.
func TestSelfTuner_FailOpenOnReaderError(t *testing.T) {
	defaults := calibDefaults()
	pl := NewPlanner(defaults)
	reader := &fakeFrontierReader{}
	reader.set(nil, errors.New("CH busy"))

	st := StartSelfTuner(context.Background(), SelfTuneParams{
		Planner:  pl,
		Reader:   reader,
		Defaults: defaults,
		Interval: 5 * time.Millisecond, // tick quickly so the recovery read fires
	})
	defer st.Stop()

	waitFor(t, func() bool { return reader.reads.Load() >= 1 })
	// Config unchanged after the failing read.
	if pl.Cfg() != defaults {
		t.Fatalf("reader error must keep current config, got %+v", pl.Cfg())
	}

	// Now feed a clear frontier; a later tick must pick it up (fail-open
	// recovered, not stuck).
	reader.set(clearFrontierCorpus(), nil)
	waitFor(t, func() bool { return pl.Cfg().MinFanout < defaults.MinFanout })
}

// TestSelfTuner_AtomicSwapUnderConcurrency hammers Plan from many goroutines
// while the loop swaps Configs, asserting (under -race) no data race and that
// every observed Config is one of the two legal whole values (never torn).
func TestSelfTuner_AtomicSwapUnderConcurrency(t *testing.T) {
	defaults := calibDefaults()
	calibrated, _ := Calibrate(clearFrontierCorpus(), defaults)
	if calibrated == defaults {
		t.Fatal("test precondition: calibrated must differ from defaults")
	}

	pl := NewPlanner(defaults)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Writer: flip between the two legal whole Configs as fast as possible.
	wg.Add(1)
	go func() {
		defer wg.Done()
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
				if flip {
					pl.SetConfig(defaults)
				} else {
					pl.SetConfig(calibrated)
				}
				flip = !flip
			}
		}
	}()

	// Readers: snapshot the Config and assert it is whole (one of the two).
	const readers = 8
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c := pl.Cfg()
					okDefaults := c == defaults
					okCalibrated := c == calibrated
					if !okDefaults && !okCalibrated {
						t.Errorf("torn Config observed: %+v", c)
						return
					}
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSelfTuner_CleanShutdownCancelsInFlight: Stop must cancel the loop ctx and
// the in-flight read's ctx, and join the goroutine (no leak — the package
// goleak TestMain enforces this too).
func TestSelfTuner_CleanShutdownCancelsInFlight(t *testing.T) {
	defaults := calibDefaults()
	pl := NewPlanner(defaults)

	ctxObserved := make(chan context.Context, 1)
	blocked := &blockingReader{ctxObserved: ctxObserved, release: make(chan struct{})}

	st := StartSelfTuner(context.Background(), SelfTuneParams{
		Planner: pl, Reader: blocked, Defaults: defaults, Interval: time.Hour,
	})

	// Wait until the immediate-pass read is in flight (ctx captured).
	var readCtx context.Context
	select {
	case readCtx = <-ctxObserved:
	case <-time.After(2 * time.Second):
		st.Stop()
		t.Fatal("reader never started")
	}
	if readCtx.Err() != nil {
		t.Fatal("read ctx should be live before Stop")
	}

	// Stop in a goroutine: it must cancel the in-flight read's ctx, which
	// unblocks the reader and lets the goroutine exit and Stop return.
	done := make(chan struct{})
	go func() { st.Stop(); close(done) }()

	waitFor(t, func() bool { return readCtx.Err() != nil })
	close(blocked.release) // belt-and-braces: let the reader return

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return; goroutine not joined")
	}

	// Stop is idempotent.
	st.Stop()
}

// blockingReader blocks inside ReadFrontier until its ctx is cancelled (or
// release is closed), exposing the ctx so the shutdown test can assert
// cancellation propagated.
type blockingReader struct {
	ctxObserved chan context.Context
	release     chan struct{}
	once        sync.Once
}

func (b *blockingReader) ReadFrontier(ctx context.Context) ([]CorpusSample, error) {
	b.once.Do(func() { b.ctxObserved <- ctx })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return nil, nil
	}
}

// waitFor polls cond until true or a short deadline, failing the test on
// timeout. Used instead of fixed sleeps so the loop tests stay fast and stable.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
