package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/golang/snappy"
	"github.com/grafana/loki/v3/pkg/logproto"
)

// seedEnvKeys are the environment variables parseConfig falls back to.
// Every parseConfig case blanks them so the host environment cannot leak
// into a case; envOr treats an empty value as unset.
var seedEnvKeys = []string{
	"CERBERUS_CH_ADDR",
	"CERBERUS_CH_DATABASE",
	"CERBERUS_CH_USERNAME",
	"CERBERUS_CH_PASSWORD",
	"LOKI_URL",
	"CERBERUS_URL",
	"LIVE_PATTERNS_METADATA",
}

// TestParseConfig pins the stage contract of the command line: -stage is
// mandatory and closed over the two stages, the live stage alone requires
// the fixture handshake path, and a flag wins over its environment
// fallback. No t.Parallel(): the cases set environment variables.
func TestParseConfig(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     map[string]string
		want    seedConfig
		wantErr string
	}{
		{
			name: "corpus_needs_no_handshake_path",
			args: []string{"-stage=corpus"},
			want: seedConfig{
				stage: stageCorpus, addr: "localhost:28000", database: "otel",
				username: "cerberus", password: "cerberus",
				lokiURL: "http://localhost:23100", cerbURL: "http://localhost:29092",
				timeout: 4 * time.Minute,
			},
		},
		{
			name: "live_with_handshake_path",
			args: []string{"-stage", "live", "-live-patterns-metadata", "/tmp/x.json", "-timeout", "90s"},
			want: seedConfig{
				stage: stageLive, addr: "localhost:28000", database: "otel",
				username: "cerberus", password: "cerberus",
				lokiURL: "http://localhost:23100", cerbURL: "http://localhost:29092",
				livePath: "/tmp/x.json", timeout: 90 * time.Second,
			},
		},
		{
			name: "live_handshake_path_from_env_and_flag_beats_env",
			args: []string{"-stage=live", "-loki-url=http://flag:1"},
			env: map[string]string{
				"LIVE_PATTERNS_METADATA": "/env/live.json",
				"LOKI_URL":               "http://env:2",
				"CERBERUS_CH_DATABASE":   "envdb",
			},
			want: seedConfig{
				stage: stageLive, addr: "localhost:28000", database: "envdb",
				username: "cerberus", password: "cerberus",
				lokiURL: "http://flag:1", cerbURL: "http://localhost:29092",
				livePath: "/env/live.json", timeout: 4 * time.Minute,
			},
		},
		{
			name:    "live_without_handshake_path",
			args:    []string{"-stage=live"},
			wantErr: "-live-patterns-metadata is required by -stage=live",
		},
		{
			name:    "missing_stage",
			args:    nil,
			wantErr: `-stage must be "corpus" or "live", got ""`,
		},
		{
			name:    "unknown_stage",
			args:    []string{"-stage=both", "-live-patterns-metadata=/tmp/x.json"},
			wantErr: `-stage must be "corpus" or "live", got "both"`,
		},
		{
			name:    "unknown_flag",
			args:    []string{"-stage=corpus", "-shipper-uploads=1"},
			wantErr: "flag provided but not defined: -shipper-uploads",
		},
		{
			name:    "malformed_timeout",
			args:    []string{"-stage=corpus", "-timeout=soon"},
			wantErr: `invalid value "soon" for flag -timeout`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range seedEnvKeys {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := parseConfig(tc.args, io.Discard)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("parseConfig(%q) = %+v, want error containing %q", tc.args, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseConfig(%q) error = %q, want it to contain %q", tc.args, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfig(%q): %v", tc.args, err)
			}
			if got != tc.want {
				t.Fatalf("parseConfig(%q) =\n  %+v\nwant\n  %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestLokiDrainSnapshotDrained pins the drain-gate decision over one
// poll: every criterion is necessary, the delta is measured against the
// baseline rather than zero, and exactly expectedStreams flushed chunks
// is enough.
func TestLokiDrainSnapshotDrained(t *testing.T) {
	t.Parallel()

	const streams = 13
	baseline := lokiMetricsSnapshot{chunksFlushed: 100}
	cases := []struct {
		name string
		snap lokiDrainSnapshot
		want bool
	}{
		{"all_three_hold", lokiDrainSnapshot{0, 0, 100 + streams}, true},
		{"delta_above_needed", lokiDrainSnapshot{0, 0, 100 + streams + 7}, true},
		{"delta_one_short", lokiDrainSnapshot{0, 0, 100 + streams - 1}, false},
		{"absolute_count_without_delta", lokiDrainSnapshot{0, 0, 100}, false},
		{"flush_queue_not_empty", lokiDrainSnapshot{1, 0, 100 + streams}, false},
		{"memory_chunks_remaining", lokiDrainSnapshot{0, 1, 100 + streams}, false},
		{"nothing_holds", lokiDrainSnapshot{4, 9, 0}, false},
	}
	for _, tc := range cases {
		if got := tc.snap.drained(baseline, streams); got != tc.want {
			t.Errorf("%s: drained(%+v, baseline=%v, streams=%d) = %v, want %v",
				tc.name, tc.snap, baseline.chunksFlushed, streams, got, tc.want)
		}
	}
}

// fakeConn is a driver.Conn that records what a stage writes to
// ClickHouse. Only the methods the stages call are implemented; any other
// call panics on the nil embedded interface, which is the point — a
// stage that starts querying ClickHouse some other way fails loudly here.
type fakeConn struct {
	driver.Conn

	execErr    error
	prepareErr error
	sendErr    error
	countRows  uint64

	mu    sync.Mutex
	execs []string
	rows  []string // ServiceName of every appended row
	sent  int
}

func (c *fakeConn) Exec(_ context.Context, query string, _ ...any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.execs = append(c.execs, query)
	return c.execErr
}

func (c *fakeConn) PrepareBatch(_ context.Context, query string, _ ...driver.PrepareBatchOption) (driver.Batch, error) {
	if c.prepareErr != nil {
		return nil, c.prepareErr
	}
	if !strings.Contains(query, "INSERT INTO otel_logs") {
		return nil, fmt.Errorf("unexpected batch query %q", query)
	}
	return &fakeBatch{conn: c}, nil
}

func (c *fakeConn) QueryRow(_ context.Context, query string, _ ...any) driver.Row {
	return fakeRow{query: query, count: c.countRows}
}

// insertCHLogs appends ServiceName as the seventh column.
const serviceNameColumn = 6

type fakeBatch struct {
	driver.Batch
	conn *fakeConn
}

func (b *fakeBatch) Append(v ...any) error {
	svc, ok := v[serviceNameColumn].(string)
	if !ok {
		return fmt.Errorf("column %d is %T, want the ServiceName string", serviceNameColumn, v[serviceNameColumn])
	}
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	b.conn.rows = append(b.conn.rows, svc)
	return nil
}

func (b *fakeBatch) Send() error {
	b.conn.mu.Lock()
	defer b.conn.mu.Unlock()
	if b.conn.sendErr != nil {
		return b.conn.sendErr
	}
	b.conn.sent++
	return nil
}

type fakeRow struct {
	driver.Row
	query string
	count uint64
}

func (r fakeRow) Scan(dest ...any) error {
	if r.query != "SELECT count() FROM otel_logs" || len(dest) != 1 {
		return fmt.Errorf("unexpected row query %q", r.query)
	}
	p, ok := dest[0].(*uint64)
	if !ok {
		return fmt.Errorf("scan target is %T, want *uint64", dest[0])
	}
	*p = r.count
	return nil
}

// fakeLoki is a reference-Loki stand-in. Its ingester holds every pushed
// stream as one in-memory chunk. Like the real ingester, /flush only
// enqueues them: the first /metrics poll after a flush still reports them
// queued and in memory, and the flush completes behind that poll. A stage
// that returns without polling the drain gate past a flush therefore
// leaves chunks in memory, which the corpus test checks.
type fakeLoki struct {
	pushStatus  int
	flushStatus int
	labels      []string

	mu            sync.Mutex
	pushes        [][]string // service_name label of every stream, per push
	flushes       int
	queued        int
	memoryChunks  int
	chunksFlushed int
}

func (f *fakeLoki) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /loki/api/v1/push", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "snappy" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			http.Error(w, "want snappy protobuf", http.StatusUnsupportedMediaType)
			return
		}
		compressed, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw, err := snappy.Decode(nil, compressed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req logproto.PushRequest
		if err := req.Unmarshal(raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if f.pushStatus != 0 {
			http.Error(w, "push rejected", f.pushStatus)
			return
		}
		services := make([]string, 0, len(req.Streams))
		for _, s := range req.Streams {
			services = append(services, serviceNameLabel(s.Labels))
		}
		f.mu.Lock()
		f.pushes = append(f.pushes, services)
		f.memoryChunks += len(req.Streams)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /flush", func(w http.ResponseWriter, _ *http.Request) {
		if f.flushStatus != 0 {
			http.Error(w, "flush broke", f.flushStatus)
			return
		}
		f.mu.Lock()
		f.flushes++
		f.queued = f.memoryChunks
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, "loki_ingester_flush_queue_length %d\n", f.queued)
		fmt.Fprintf(w, "loki_ingester_memory_chunks %d\n", f.memoryChunks)
		fmt.Fprintf(w, "loki_ingester_chunks_flushed_total{reason=\"forced\"} %d\n", f.chunksFlushed)
		// The flush workers finish the queued chunks behind this poll.
		f.chunksFlushed += f.queued
		f.memoryChunks -= f.queued
		f.queued = 0
	})
	serveLabelsAndPatterns(mux, f.labels)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serveLabelsAndPatterns adds the two read endpoints the live stage
// probes on both targets.
func serveLabelsAndPatterns(mux *http.ServeMux, labels []string) {
	mux.HandleFunc("GET /loki/api/v1/labels", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": labels})
	})
	mux.HandleFunc("GET /loki/api/v1/patterns", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":[{"pattern":"<_>"}]}`)
	})
}

func fakeCerberus(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	serveLabelsAndPatterns(mux, []string{"service_name"})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serviceNameLabel extracts service_name from a pushed `{k="v", ...}`
// label string.
func serviceNameLabel(labels string) string {
	const key = `service_name="`
	_, rest, ok := strings.Cut(labels, key)
	if !ok {
		return ""
	}
	value, _, _ := strings.Cut(rest, `"`)
	return value
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func corpusEntryCount(t *testing.T) (streams, entries int) {
	t.Helper()
	start, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		t.Fatalf("parse anchor: %v", err)
	}
	corpus := buildStreams(start)
	for _, s := range corpus {
		entries += len(s.entries)
	}
	return len(corpus), entries
}

func stageContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestRunStageCorpus pins what the corpus stage does and does not do:
// it writes the deterministic corpus — and only the corpus — to both
// targets, flushes the reference ingester, waits for it to drain, and
// leaves the now-anchored /patterns fixture and its handshake to the live
// stage.
func TestRunStageCorpus(t *testing.T) {
	t.Parallel()

	loki := &fakeLoki{labels: []string{"service_name"}}
	lokiSrv := loki.server(t)
	conn := &fakeConn{}
	livePath := filepath.Join(t.TempDir(), "live.json")
	cfg := seedConfig{stage: stageCorpus, database: "otel", lokiURL: lokiSrv.URL, livePath: livePath}

	if err := runStage(stageContext(t), cfg, conn, discardLogger()); err != nil {
		t.Fatalf("corpus stage: %v", err)
	}

	wantStreams, wantEntries := corpusEntryCount(t)
	if len(conn.execs) == 0 {
		t.Errorf("corpus stage applied no DDL")
	}
	if len(conn.rows) != wantEntries || conn.sent != 1 {
		t.Errorf("clickhouse got %d rows in %d sends, want the %d corpus entries in 1", len(conn.rows), conn.sent, wantEntries)
	}
	for _, svc := range conn.rows {
		if svc == livePatternsService {
			t.Fatalf("corpus stage wrote the live /patterns fixture to clickhouse")
		}
	}
	if len(loki.pushes) != 1 || len(loki.pushes[0]) != wantStreams {
		t.Fatalf("loki pushes = %v, want one push of the %d corpus streams", loki.pushes, wantStreams)
	}
	for _, svc := range loki.pushes[0] {
		if svc == livePatternsService {
			t.Fatalf("corpus stage pushed the live /patterns fixture to loki")
		}
	}
	if loki.flushes != 1 {
		t.Errorf("loki /flush called %d times, want 1", loki.flushes)
	}
	if loki.memoryChunks != 0 {
		t.Errorf("corpus stage returned with %d chunks still in ingester memory", loki.memoryChunks)
	}
	if _, err := os.Stat(livePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("corpus stage touched the live handshake path (stat err=%v)", err)
	}
}

// TestRunStageLive pins the live stage: it writes the now-anchored
// fixture — and only the fixture — to both targets, never flushes the
// restarted reference, and writes a valid handshake for the diff driver.
func TestRunStageLive(t *testing.T) {
	t.Parallel()

	loki := &fakeLoki{labels: []string{"service_name"}}
	lokiSrv := loki.server(t)
	cerb := fakeCerberus(t)
	conn := &fakeConn{countRows: 1}
	livePath := filepath.Join(t.TempDir(), "handshake", "live.json")
	cfg := seedConfig{stage: stageLive, database: "otel", lokiURL: lokiSrv.URL, cerbURL: cerb.URL, livePath: livePath}

	if err := runStage(stageContext(t), cfg, conn, discardLogger()); err != nil {
		t.Fatalf("live stage: %v", err)
	}

	wantEntries := len(livePatternsLevels) * livePatternsEntriesPerLevel
	if len(conn.execs) != 0 {
		t.Errorf("live stage applied DDL %q; the schema belongs to the corpus stage", conn.execs)
	}
	if len(conn.rows) != wantEntries || conn.sent != 1 {
		t.Errorf("clickhouse got %d rows in %d sends, want the %d fixture entries in 1", len(conn.rows), conn.sent, wantEntries)
	}
	for _, svc := range conn.rows {
		if svc != livePatternsService {
			t.Fatalf("live stage wrote a %q row to clickhouse; only the fixture belongs here", svc)
		}
	}
	if len(loki.pushes) != 1 || len(loki.pushes[0]) != 1 || loki.pushes[0][0] != livePatternsService {
		t.Fatalf("loki pushes = %v, want one push of the single fixture stream", loki.pushes)
	}
	if loki.flushes != 0 {
		t.Errorf("live stage flushed the reference ingester %d times, want 0", loki.flushes)
	}

	raw, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatalf("live stage wrote no handshake: %v", err)
	}
	var got livePatternsMetadata
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	if err := validateLivePatternsMetadata(got); err != nil {
		t.Fatalf("handshake is invalid: %v", err)
	}
	if want := `{service_name="` + livePatternsService + `"}`; got.Selector != want {
		t.Errorf("handshake selector = %q, want %q", got.Selector, want)
	}
}

// TestRunStageRejectsUnknownStage pins the dispatch's closed set: a stage
// name it does not know touches neither target.
func TestRunStageRejectsUnknownStage(t *testing.T) {
	t.Parallel()

	loki := &fakeLoki{}
	lokiSrv := loki.server(t)
	conn := &fakeConn{}
	err := runStage(stageContext(t), seedConfig{stage: "both", lokiURL: lokiSrv.URL}, conn, discardLogger())
	if err == nil || !strings.Contains(err.Error(), `unknown stage "both"`) {
		t.Fatalf("runStage(both) error = %v, want unknown stage", err)
	}
	if len(conn.execs) != 0 || len(conn.rows) != 0 || len(loki.pushes) != 0 {
		t.Fatalf("unknown stage wrote to a target: execs=%d rows=%d pushes=%d", len(conn.execs), len(conn.rows), len(loki.pushes))
	}
}

// TestSeedCorpusErrors pins that every failing step of the corpus stage
// stops it with an error naming that step, before the steps after it run.
func TestSeedCorpusErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	cases := []struct {
		name        string
		loki        *fakeLoki
		conn        *fakeConn
		brokenLoki  bool
		wantErr     string
		wantFlushes int
	}{
		{name: "ddl", loki: &fakeLoki{}, conn: &fakeConn{execErr: boom}, wantErr: "ddl.Apply: ddl: create database otel: boom"},
		{name: "prepare_batch", loki: &fakeLoki{}, conn: &fakeConn{prepareErr: boom}, wantErr: "insert clickhouse: prepare batch: boom"},
		{name: "send_batch", loki: &fakeLoki{}, conn: &fakeConn{sendErr: boom}, wantErr: "insert clickhouse: send batch: boom"},
		{name: "push_rejected", loki: &fakeLoki{pushStatus: http.StatusBadRequest}, conn: &fakeConn{}, wantErr: "push loki: loki returned 400: push rejected"},
		{name: "flush_failed", loki: &fakeLoki{flushStatus: http.StatusInternalServerError}, conn: &fakeConn{}, wantErr: "flush loki: loki /flush returned 500: flush broke"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := tc.loki.server(t)
			err := seedCorpus(stageContext(t), tc.conn, "otel", srv.URL, discardLogger())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("seedCorpus error = %v, want it to contain %q", err, tc.wantErr)
			}
			if tc.loki.flushes != tc.wantFlushes {
				t.Fatalf("/flush called %d times after a failed earlier step", tc.loki.flushes)
			}
		})
	}

	t.Run("metrics_baseline_unreadable", func(t *testing.T) {
		t.Parallel()
		mux := http.NewServeMux()
		mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {})
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no metrics", http.StatusServiceUnavailable)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		err := seedCorpus(stageContext(t), &fakeConn{}, "otel", srv.URL, discardLogger())
		if err == nil || !strings.Contains(err.Error(), "read loki metrics baseline: loki /metrics status 503") {
			t.Fatalf("seedCorpus error = %v, want the baseline read failure", err)
		}
	})
}

// TestSeedLiveErrors pins the live stage's failure paths.
func TestSeedLiveErrors(t *testing.T) {
	t.Parallel()

	t.Run("clickhouse_empty", func(t *testing.T) {
		t.Parallel()
		loki := &fakeLoki{labels: []string{"service_name"}}
		srv := loki.server(t)
		cerb := fakeCerberus(t)
		path := filepath.Join(t.TempDir(), "live.json")
		err := seedLive(stageContext(t), &fakeConn{countRows: 0}, srv.URL, cerb.URL, path, discardLogger())
		if err == nil || !strings.Contains(err.Error(), "verify: clickhouse otel_logs is empty after seed") {
			t.Fatalf("seedLive error = %v, want the empty-table verification failure", err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("a failed verification still wrote the handshake (stat err=%v)", statErr)
		}
	})

	t.Run("reference_labels_empty", func(t *testing.T) {
		t.Parallel()
		// The restarted reference does not serve the corpus index: its
		// /labels stays empty. The stage must not hand a handshake to
		// the diff driver.
		loki := &fakeLoki{labels: []string{}}
		srv := loki.server(t)
		cerb := fakeCerberus(t)
		path := filepath.Join(t.TempDir(), "live.json")
		ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
		t.Cleanup(cancel)
		err := seedLive(ctx, &fakeConn{countRows: 1}, srv.URL, cerb.URL, path, discardLogger())
		if err == nil || !strings.Contains(err.Error(), "verify: ") {
			t.Fatalf("seedLive error = %v, want a verification failure", err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("an unverified reference still got a handshake (stat err=%v)", statErr)
		}
	})

	t.Run("handshake_unwritable", func(t *testing.T) {
		t.Parallel()
		loki := &fakeLoki{labels: []string{"service_name"}}
		srv := loki.server(t)
		cerb := fakeCerberus(t)
		blocker := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := seedLive(stageContext(t), &fakeConn{countRows: 1}, srv.URL, cerb.URL, filepath.Join(blocker, "live.json"), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "write live patterns metadata: mkdir") {
			t.Fatalf("seedLive error = %v, want the handshake write failure", err)
		}
	})

	t.Run("loki_never_ready", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		conn := &fakeConn{}
		err := seedLive(ctx, conn, "http://127.0.0.1:1", "http://127.0.0.1:1", filepath.Join(t.TempDir(), "x.json"), discardLogger())
		if err == nil || !strings.Contains(err.Error(), "loki not ready: context canceled") {
			t.Fatalf("seedLive error = %v, want loki not ready", err)
		}
		if len(conn.rows) != 0 {
			t.Fatalf("live stage wrote %d rows before the reference was ready", len(conn.rows))
		}
	})
}
