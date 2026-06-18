package optcorpus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// JSONLSink is the v1 durable sink: it appends each reconciled Row as one JSON
// object per line (JSON Lines) to a file. The file is opened append-only so a
// cerberus restart continues the same corpus; the Row JSON shape is stable so
// a later ClickHouse-table sink is a column-for-column swap. Writes are
// serialized by a mutex so the single Run loop and any future concurrent
// writer cannot interleave partial lines.
type JSONLSink struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// NewJSONLSink opens (creating if absent, appending if present) the JSONL
// corpus file at path. An empty path is a misconfiguration the caller should
// have guarded (CERBERUS_CH_OPT_CORPUS_SINK_PATH empty disables the sink); it
// is rejected here with an error rather than silently writing nowhere.
func NewJSONLSink(path string) (*JSONLSink, error) {
	if path == "" {
		return nil, fmt.Errorf("optcorpus: empty JSONL sink path")
	}
	// The path is an operator-supplied config value
	// (CERBERUS_CH_OPT_CORPUS_SINK_PATH): writing the corpus to an
	// operator-chosen location is the intended behaviour, not an injection.
	//nolint:gosec // G304: path is a trusted operator config value, by design.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("optcorpus: open sink %q: %w", path, err)
	}
	return &JSONLSink{f: f, w: bufio.NewWriter(f)}, nil
}

// Write appends each row as one JSON line and flushes so the corpus is durable
// at interval granularity (a crash loses at most the in-flight buffer, not
// already-flushed lines). An empty slice is a no-op.
func (s *JSONLSink) Write(rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	enc := json.NewEncoder(s.w)
	for i := range rows {
		if err := enc.Encode(rows[i]); err != nil {
			return fmt.Errorf("optcorpus: encode row: %w", err)
		}
	}
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("optcorpus: flush sink: %w", err)
	}
	return nil
}

// Close flushes any buffered bytes and closes the underlying file.
func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w != nil {
		if err := s.w.Flush(); err != nil {
			_ = s.f.Close()
			return fmt.Errorf("optcorpus: flush on close: %w", err)
		}
	}
	if s.f != nil {
		return s.f.Close()
	}
	return nil
}
