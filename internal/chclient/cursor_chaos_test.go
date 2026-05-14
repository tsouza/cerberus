package chclient

import (
	"sync"
	"sync/atomic"
	"testing"
)

// closeCounterRows is a driver.Rows fake whose Close counts invocations.
// Used to assert that under N concurrent rowsCursor.Close() callers, the
// underlying driver.Rows.Close is invoked exactly once — the sync.Once
// contract on rowsCursor.
type closeCounterRows struct {
	fakeRows
	closeCalls atomic.Int32
}

func (r *closeCounterRows) Close() error {
	r.closeCalls.Add(1)
	return r.fakeRows.Close()
}

// TestCursor_ConcurrentClose stresses rowsCursor.Close from many
// goroutines simultaneously. Without sync.Once, the body would race the
// nil-outs of c.rows / c.span / c.rec and (under -race) the race
// detector would fire. With sync.Once, the body runs exactly once and
// every concurrent caller observes the same closeErr return.
//
// Layer 11 (chaos / failure-mode) regression coverage.
func TestCursor_ConcurrentClose(t *testing.T) {
	t.Parallel()

	const goroutines = 64

	rows := &closeCounterRows{}
	cursor := &rowsCursor{rows: rows}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	start := make(chan struct{})
	errs := make([]error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = cursor.Close()
		}(i)
	}
	close(start)
	wg.Wait()

	// Every caller must see the same outcome — here, nil.
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Close: %v", i, err)
		}
	}

	if got := rows.closeCalls.Load(); got != 1 {
		t.Errorf("underlying rows.Close: got %d calls, want 1", got)
	}

	// A subsequent serial Close must still be a no-op (idempotent).
	if err := cursor.Close(); err != nil {
		t.Errorf("post-race Close: %v", err)
	}
	if got := rows.closeCalls.Load(); got != 1 {
		t.Errorf("after post-race Close: got %d calls, want 1", got)
	}
}
