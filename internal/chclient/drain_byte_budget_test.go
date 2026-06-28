package chclient

import (
	"errors"
	"strings"
	"testing"
)

// TestLogPeekBytesExceeded pins the line-peek byte budget: the row-count
// budget (drainBudgetExceeded) bounds how MANY rows a drain buffers, but the
// /detected_fields and /patterns peeks are SQL-LIMIT capped at a small row
// count while each row's Body is an unbounded CH String. logPeekBytesExceeded
// is the byte-axis backstop — once cumulative buffered bytes cross
// maxLogPeekBytes the drain aborts with *LogPeekBytesError instead of heaping
// the process.
func TestLogPeekBytesExceeded(t *testing.T) {
	t.Parallel()

	// At the cap exactly: still OK (strictly-greater trips).
	if err := logPeekBytesExceeded(maxLogPeekBytes); err != nil {
		t.Fatalf("at the cap (%d): want nil, got %v", maxLogPeekBytes, err)
	}

	// One byte over: aborts with the typed error carrying the cap.
	err := logPeekBytesExceeded(maxLogPeekBytes + 1)
	if !errors.Is(err, ErrLogPeekBytesExceeded) {
		t.Fatalf("over the cap: want ErrLogPeekBytesExceeded, got %v", err)
	}
	var be *LogPeekBytesError
	if !errors.As(err, &be) || be.Limit != maxLogPeekBytes {
		t.Fatalf("want LogPeekBytesError{Limit:%d}, got %v", maxLogPeekBytes, err)
	}
}

// TestLogPeekBytesExceeded_DrainLoop simulates the QueryTimestampedLines /
// QueryDetectedFieldRows accumulation loop over pathologically large lines: a
// row COUNT far under maxLogPeekLineLimit (so the row budget never trips) whose
// per-line SIZE blows past maxLogPeekBytes. The drain must abort partway
// through instead of buffering every oversized line — proving the byte bound,
// not the row bound, is what catches this OOM shape.
func TestLogPeekBytesExceeded_DrainLoop(t *testing.T) {
	t.Parallel()

	const lineBytes = 8 << 20 // 8 MiB per line — well above any sane log line.
	oversized := strings.Repeat("x", lineBytes)

	var buffered int64
	tripped := -1
	const lineCount = 256 // 256 * 8 MiB = 2 GiB if buffered unbounded.
	for i := 0; i < lineCount; i++ {
		buffered += int64(len(oversized))
		if err := logPeekBytesExceeded(buffered); err != nil {
			tripped = i
			break
		}
	}

	if tripped < 0 {
		t.Fatalf("drain buffered all %d oversized lines without aborting", lineCount)
	}
	// 256 MiB cap / 8 MiB per line => trips on the 33rd line (index 32), long
	// before the loop would have buffered 2 GiB.
	wantTrip := maxLogPeekBytes / lineBytes
	if tripped != wantTrip {
		t.Fatalf("aborted at line index %d, want %d (cap %d / line %d)",
			tripped, wantTrip, maxLogPeekBytes, lineBytes)
	}
}

// TestMapBytes pins the attribute-map byte accounting the detected-fields
// drain folds into its byte budget alongside each Body — keys and values both
// count, an empty/nil map contributes nothing.
func TestMapBytes(t *testing.T) {
	t.Parallel()

	if got := mapBytes(nil); got != 0 {
		t.Fatalf("mapBytes(nil) = %d, want 0", got)
	}
	m := map[string]string{"ab": "cde", "f": "g"} // 2+3 + 1+1 = 7
	if got := mapBytes(m); got != 7 {
		t.Fatalf("mapBytes(%v) = %d, want 7", m, got)
	}
}
