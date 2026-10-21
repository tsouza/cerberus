package seed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Loki gate's contract: flush queue empty, no chunks in memory, and
// chunks flushed advanced by at least the pushed stream count since the
// baseline — all three in ONE poll. These tests hold it to each half of
// that sentence.

// lokiGateBudget bounds a gate expected to fail: a few poll intervals, so
// the gate gets more than one look at the stub before the deadline.
const lokiGateBudget = 3 * settleInterval

// lokiGatePassBudget bounds a gate expected to pass. It only has to outlast
// the polls the stub needs; a passing gate returns as soon as it is met.
const lokiGatePassBudget = 20 * settleInterval

// lokiReading is one /metrics answer. A negative value leaves the metric
// out of the exposition entirely.
type lokiReading struct {
	queue, memory, flushed float64
}

// stubLoki answers /metrics from a script of readings, one per poll, and
// repeats the last reading once the script runs out.
type stubLoki struct {
	mu       sync.Mutex
	readings []lokiReading
	polls    int
}

func (s *stubLoki) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		idx := min(s.polls, len(s.readings)-1)
		s.polls++
		rd := s.readings[idx]
		s.mu.Unlock()
		if rd.queue >= 0 {
			fmt.Fprintf(w, "%s %v\n", lokiFlushQueueLengthMetric, rd.queue)
		}
		if rd.memory >= 0 {
			fmt.Fprintf(w, "%s %v\n", lokiMemoryChunksMetric, rd.memory)
		}
		if rd.flushed >= 0 {
			// Split across two label rows: the gate must sum the family.
			fmt.Fprintf(w, "%s{reason=\"forced\"} %v\n", lokiChunksFlushedMetric, rd.flushed-1)
			fmt.Fprintf(w, "%s{reason=\"idle\"} 1\n", lokiChunksFlushedMetric)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *stubLoki) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

func waitLoki(t *testing.T, s *stubLoki, streams int, baseline LokiCounters, budget time.Duration) error {
	t.Helper()
	srv := s.server(t)
	return WaitLokiSettled(context.Background(), srv.URL, streams, baseline, budget)
}

func TestWaitLokiSettledPassesWhenAllThreeHold(t *testing.T) {
	t.Parallel()
	s := &stubLoki{readings: []lokiReading{{queue: 0, memory: 0, flushed: 110}}}
	if err := waitLoki(t, s, 10, LokiCounters{ChunksFlushed: 100}, lokiGatePassBudget); err != nil {
		t.Fatalf("gate should pass at exactly the needed delta: %v", err)
	}
}

func TestWaitLokiSettledWaitsForTheDeltaNotTheAbsoluteCount(t *testing.T) {
	t.Parallel()
	// 109 flushed chunks is far above the 10 streams pushed, but only 9
	// above the baseline: a previous run's chunks are not this push's.
	s := &stubLoki{readings: []lokiReading{{queue: 0, memory: 0, flushed: 109}}}
	err := waitLoki(t, s, 10, LokiCounters{ChunksFlushed: 100}, lokiGateBudget)
	if err == nil {
		t.Fatal("gate passed one chunk short of the pushed stream count")
	}
	if !strings.Contains(err.Error(), "chunks_flushed_delta=9 (need 10)") {
		t.Fatalf("timeout must name the delta and the need, got: %v", err)
	}
}

func TestWaitLokiSettledWaitsForMemoryToEmpty(t *testing.T) {
	t.Parallel()
	s := &stubLoki{readings: []lokiReading{{queue: 0, memory: 2, flushed: 50}}}
	err := waitLoki(t, s, 10, LokiCounters{}, lokiGateBudget)
	if err == nil || !strings.Contains(err.Error(), "memory_chunks=2") {
		t.Fatalf("gate must wait while chunks remain in memory, got: %v", err)
	}
}

func TestWaitLokiSettledWaitsForTheFlushQueue(t *testing.T) {
	t.Parallel()
	s := &stubLoki{readings: []lokiReading{{queue: 3, memory: 0, flushed: 50}}}
	err := waitLoki(t, s, 10, LokiCounters{}, lokiGateBudget)
	if err == nil || !strings.Contains(err.Error(), "flush_queue_length=3") {
		t.Fatalf("gate must wait while the flush queue is non-empty, got: %v", err)
	}
}

func TestWaitLokiSettledRequiresAllThreeInOnePoll(t *testing.T) {
	t.Parallel()
	// Each condition holds on some poll, never all on the same one until
	// the last: a gate that latched "queue was empty once" would pass on
	// the second poll.
	s := &stubLoki{readings: []lokiReading{
		{queue: 0, memory: 0, flushed: 3},
		{queue: 4, memory: 1, flushed: 12},
		{queue: 0, memory: 0, flushed: 12},
	}}
	if err := waitLoki(t, s, 10, LokiCounters{}, lokiGatePassBudget); err != nil {
		t.Fatalf("gate should pass once all three hold together: %v", err)
	}
	if got := s.pollCount(); got != 3 {
		t.Fatalf("gate returned after %d polls, want 3: it passed on a poll where not all three held", got)
	}
}

func TestWaitLokiSettledFailsOnAMissingQuiescenceMetric(t *testing.T) {
	t.Parallel()
	// The memory gauge vanished (an upstream rename). Coerced to zero it
	// would satisfy the gate; it must fail instead.
	s := &stubLoki{readings: []lokiReading{{queue: 0, memory: -1, flushed: 50}}}
	err := waitLoki(t, s, 10, LokiCounters{}, lokiGateBudget)
	if err == nil {
		t.Fatal("gate passed with the memory-chunks gauge missing")
	}
	if !strings.Contains(err.Error(), lokiMemoryChunksMetric) || !strings.Contains(err.Error(), "upstream has renamed it") {
		t.Fatalf("error must name the missing gauge as a rename, got: %v", err)
	}
}

func TestWaitLokiSettledNamesANeverObservedProgressMetric(t *testing.T) {
	t.Parallel()
	s := &stubLoki{readings: []lokiReading{{queue: 0, memory: 0, flushed: -1}}}
	err := waitLoki(t, s, 1, LokiCounters{}, lokiGateBudget)
	if err == nil {
		t.Fatal("gate passed with no chunks ever flushed")
	}
	if !strings.Contains(err.Error(), "never observed: "+lokiChunksFlushedMetric) {
		t.Fatalf("timeout must name the never-observed counter, got: %v", err)
	}
}

func TestWaitLokiSettledReportsAnUnreachableBackend(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	err := WaitLokiSettled(context.Background(), srv.URL, 1, LokiCounters{}, lokiGateBudget)
	if err == nil || !strings.Contains(err.Error(), "returned 502") {
		t.Fatalf("timeout must carry the last scrape failure, got: %v", err)
	}
}

func TestWaitLokiSettledHonoursCancellation(t *testing.T) {
	t.Parallel()
	s := &stubLoki{readings: []lokiReading{{queue: 1, memory: 1, flushed: 0}}}
	srv := s.server(t)
	ctx, cancel := context.WithTimeout(context.Background(), settleInterval/2)
	t.Cleanup(cancel)
	err := WaitLokiSettled(ctx, srv.URL, 1, LokiCounters{}, time.Hour)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("gate must stop on context expiry, got: %v", err)
	}
}

func TestReadLokiCountersSumsTheFamilyAndReadsAbsenceAsZero(t *testing.T) {
	t.Parallel()
	present := &stubLoki{readings: []lokiReading{{queue: 0, memory: 0, flushed: 42}}}
	srv := present.server(t)
	got, err := ReadLokiCounters(context.Background(), srv.URL)
	if err != nil || got.ChunksFlushed != 42 {
		t.Fatalf("ReadLokiCounters = %+v, %v; want 42 summed across the reason rows", got, err)
	}

	absent := &stubLoki{readings: []lokiReading{{queue: 0, memory: 0, flushed: -1}}}
	srv = absent.server(t)
	got, err = ReadLokiCounters(context.Background(), srv.URL)
	if err != nil || got.ChunksFlushed != 0 {
		t.Fatalf("ReadLokiCounters on a never-flushed Loki = %+v, %v; want a zero baseline", got, err)
	}
}
