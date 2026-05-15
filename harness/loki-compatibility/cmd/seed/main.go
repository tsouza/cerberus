// Command seed loads the deterministic log fixture used by the LogQL
// compatibility harness.
//
// It does three things:
//
//  1. Applies the upstream OTel ClickHouse Exporter DDL (logs signal) via
//     internal/schema/ddl, so the harness's schema can't drift from what
//     cerberus's auto-create path produces.
//  2. Generates a deterministic log stream (3-5 services × ~600 entries
//     each at 1s spacing, anchored at a fixed timestamp) and fans it out
//     to both targets:
//     - ClickHouse via INSERT INTO otel_logs (the path cerberus reads
//     from when answering LogQL queries).
//     - Loki via HTTP POST /loki/api/v1/push (the path the reference
//     target reads from when answering LogQL queries).
//  3. Polls /loki/api/v1/labels and the CH otel_logs table until both
//     report non-empty, then prints a summary.
//
// The two writes carry the same content modulo each backend's storage
// shape: same anchor timestamp, same service set, same line bodies.
// Differences in query results will surface in PR 2's diff driver as
// genuine LogQL/wire semantics gaps — not as data asymmetry.
//
// Replaces no prior file; this is the inaugural Loki-side seeder.
// Invoked by scripts/run-loki-compatibility.sh against a docker-compose
// stack exposing CH on localhost:28000 and Loki on localhost:23100
// (override via CERBERUS_CH_ADDR / LOKI_URL).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// anchor is the fixture's start timestamp. Every generated log entry
// sits at anchor + i*1s for i in [0, entriesPerService). Keep this in
// lock-step with scripts/run-loki-compatibility.sh's TESTER_END_TIME
// default when that lands in PR 3.
const anchor = "2026-05-11T00:00:00Z"

// services enumerates the synthetic service names mirrored across both
// targets. Each becomes one Loki stream (`{service="…"}`) and one
// ResourceAttributes key (`service.name=…`) on the CH side. The count
// (3-5) matches the per-PR plan in docs/loki-compliance-plan.md.
var services = []string{"checkout", "payments", "search", "shipping"}

// entriesPerService is the per-stream line count. 600 × 1s = 10 minutes —
// the upper bound of the plan's 5-10 minute window. Keeping it at the
// upper bound gives the future diff driver enough range-query depth.
const entriesPerService = 600

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var (
		addr     = flag.String("addr", envOr("CERBERUS_CH_ADDR", "localhost:28000"), "ClickHouse host:port")
		database = flag.String("database", envOr("CERBERUS_CH_DATABASE", "otel"), "ClickHouse database")
		username = flag.String("user", envOr("CERBERUS_CH_USERNAME", "cerberus"), "ClickHouse username")
		password = flag.String("password", envOr("CERBERUS_CH_PASSWORD", "cerberus"), "ClickHouse password")
		lokiURL  = flag.String("loki-url", envOr("LOKI_URL", "http://localhost:23100"), "Reference Loki base URL")
		cerbURL  = flag.String("cerberus-url", envOr("CERBERUS_URL", "http://localhost:29092"), "cerberus LogQL base URL")
		timeout  = flag.Duration("timeout", 2*time.Minute, "overall dial + push + verify timeout")
	)
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	logger.Info("dialing clickhouse", "addr", *addr, "database", *database)
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{*addr},
		Auth: clickhouse.Auth{
			Database: *database,
			Username: *username,
			Password: *password,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := waitCHReady(ctx, conn, logger); err != nil {
		return err
	}

	logger.Info("applying ddl", "signal", "logs")
	cfg := ddl.Config{Database: *database}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Logs}); err != nil {
		return fmt.Errorf("ddl.Apply: %w", err)
	}

	start, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return fmt.Errorf("parse anchor: %w", err)
	}

	streams := buildStreams(start)
	totalEntries := 0
	for _, s := range streams {
		totalEntries += len(s.entries)
	}
	logger.Info("generated fixture",
		"streams", len(streams),
		"entries_per_service", entriesPerService,
		"total_entries", totalEntries,
		"anchor", anchor,
		"span_seconds", entriesPerService,
	)

	logger.Info("inserting into clickhouse otel_logs")
	if err := insertCHLogs(ctx, conn, streams); err != nil {
		return fmt.Errorf("insert clickhouse: %w", err)
	}

	logger.Info("waiting for loki readiness", "url", *lokiURL)
	if err := waitLokiReady(ctx, *lokiURL, logger); err != nil {
		return fmt.Errorf("loki not ready: %w", err)
	}

	logger.Info("pushing into loki", "url", *lokiURL)
	if err := pushLoki(ctx, *lokiURL, streams); err != nil {
		return fmt.Errorf("push loki: %w", err)
	}

	logger.Info("verifying /labels is non-empty on both targets")
	if err := verifyBothNonEmpty(ctx, conn, *lokiURL, *cerbURL, logger); err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	logger.Info("sample log line", "line", streams[0].entries[0].line)
	logger.Info("seed done",
		"streams", len(streams),
		"total_entries", totalEntries,
	)
	return nil
}

// stream is one logical {service=...} stream that fans out to both Loki
// (as one /push streams entry) and ClickHouse (as N rows in otel_logs).
type stream struct {
	service string
	entries []entry
}

type entry struct {
	ts   time.Time
	line string
}

// buildStreams generates the deterministic fixture. The output is
// stable across runs: same anchor, same line ordering, same level
// rotation — so re-running the seeder against a fresh CH + Loki gives
// byte-identical content.
func buildStreams(start time.Time) []stream {
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	templates := []string{
		"%s service=%s msg=request_received path=/api/v1/items rid=%06d",
		"%s service=%s msg=cache_hit key=tenant-%d duration_ms=%d",
		"%s service=%s msg=db_query rows=%d duration_ms=%d",
		"%s service=%s msg=request_completed status=200 duration_ms=%d rid=%06d",
	}
	out := make([]stream, 0, len(services))
	for si, svc := range services {
		s := stream{service: svc, entries: make([]entry, 0, entriesPerService)}
		for i := 0; i < entriesPerService; i++ {
			ts := start.Add(time.Duration(i) * time.Second)
			level := levels[i%len(levels)]
			// rotate template index across services so coverage of
			// each line shape is spread evenly — service A starts at
			// template 0, service B at template 1, etc.
			tmpl := templates[(i+si)%len(templates)]
			var line string
			switch (i + si) % len(templates) {
			case 0:
				line = fmt.Sprintf(tmpl, level, svc, i)
			case 1:
				line = fmt.Sprintf(tmpl, level, svc, i%17, 5+(i%23))
			case 2:
				line = fmt.Sprintf(tmpl, level, svc, i%101, 1+(i%19))
			case 3:
				line = fmt.Sprintf(tmpl, level, svc, 2+(i%13), i)
			}
			s.entries = append(s.entries, entry{ts: ts, line: line})
		}
		out = append(out, s)
	}
	return out
}

// waitCHReady polls SELECT 1 until ClickHouse answers or ctx expires.
// The compose healthcheck already gates this, but the seeder may be
// invoked from run-loki-compatibility.sh against a freshly started
// container — the extra poll absorbs the ~1s tail where ping passes but
// Exec doesn't.
func waitCHReady(ctx context.Context, conn driver.Conn, logger *slog.Logger) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := conn.Exec(ctx, "SELECT 1")
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clickhouse not ready: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		logger.Debug("waiting for clickhouse", "err", err)
	}
}

// waitLokiReady polls Loki /ready until it returns 200. The compose
// healthcheck already gates this — the extra poll exists for cases
// where the seeder is invoked directly (e.g. `go run ...` against a
// hand-started compose stack).
func waitLokiReady(ctx context.Context, baseURL string, logger *slog.Logger) error {
	deadline := time.Now().Add(60 * time.Second)
	url := strings.TrimRight(baseURL, "/") + "/ready"
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("loki /ready: %w", err)
			}
			return fmt.Errorf("loki /ready returned %d", resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		logger.Debug("waiting for loki", "url", url)
	}
}

// insertCHLogs writes every (service, entry) pair into otel_logs using
// a single batched INSERT. The label schema mirrors the OTel-CH Exporter
// shape: service.name on ResourceAttributes, level on LogAttributes,
// SeverityText pulled from the line's leading token. TraceId / SpanId
// stay empty — the fixture is synthetic without trace correlation.
//
// Column list / order matches sqltemplates/logs_insert.sql verbatim
// (TimestampTime is `DEFAULT toDateTime(Timestamp)` so we don't write
// it explicitly; EventName isn't in the upstream insert).
func insertCHLogs(ctx context.Context, conn driver.Conn, streams []stream) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO otel_logs (
        Timestamp, TraceId, SpanId, TraceFlags,
        SeverityText, SeverityNumber, ServiceName, Body,
        ResourceSchemaUrl, ResourceAttributes,
        ScopeSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes,
        LogAttributes
    )`)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}

	for _, s := range streams {
		for _, e := range s.entries {
			level := strings.SplitN(e.line, " ", 2)[0]
			if err := batch.Append(
				e.ts,
				"",
				"",
				uint8(0),
				level,
				severityNumber(level),
				s.service,
				e.line,
				"",
				map[string]string{"service.name": s.service},
				"",
				"cerberus-loki-compat-seeder",
				"1",
				map[string]string{},
				map[string]string{"level": level, "service": s.service},
			); err != nil {
				return fmt.Errorf("append: %w", err)
			}
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	return nil
}

// severityNumber maps the level token to the OTel-CH SeverityNumber
// scale (https://opentelemetry.io/docs/specs/otel/logs/data-model/).
// Return type is uint8 so it matches the otel_logs DDL column type
// without an extra conversion at the call site.
func severityNumber(level string) uint8 {
	switch strings.ToUpper(level) {
	case "TRACE":
		return 1
	case "DEBUG":
		return 5
	case "INFO":
		return 9
	case "WARN", "WARNING":
		return 13
	case "ERROR":
		return 17
	case "FATAL":
		return 21
	}
	return 0
}

// pushLoki POSTs the streams to /loki/api/v1/push as one JSON body. The
// payload shape matches the Loki ingest contract:
//
//	{"streams":[{"stream":{"service":"checkout","level":"info"},
//	              "values":[["<ns_ts>","<line>"], ...]}]}
//
// `values` is ordered oldest-first; Loki accepts either direction but
// preserves the strictly-increasing ordering the fixture builds.
func pushLoki(ctx context.Context, baseURL string, streams []stream) error {
	type pushStream struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	}
	type pushBody struct {
		Streams []pushStream `json:"streams"`
	}

	body := pushBody{Streams: make([]pushStream, 0, len(streams))}
	for _, s := range streams {
		ps := pushStream{
			Stream: map[string]string{"service": s.service},
			Values: make([][2]string, 0, len(s.entries)),
		}
		for _, e := range s.entries {
			ps.Values = append(ps.Values, [2]string{
				fmt.Sprintf("%d", e.ts.UnixNano()),
				e.line,
			})
		}
		body.Streams = append(body.Streams, ps)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/loki/api/v1/push"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cerberus-loki-compat-seeder/1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("loki returned %d: %s", resp.StatusCode, string(bodyBytes))
	}
	return nil
}

// verifyBothNonEmpty asserts the PR 1 smoke contract: /loki/api/v1/labels
// returns non-empty on BOTH the reference Loki and the cerberus side, and
// the CH otel_logs table is non-empty. Each check has its own short poll
// loop — Loki's index is asynchronous (a /push returning 200 doesn't
// guarantee /labels sees the new stream immediately), and cerberus may
// still be warming its CH connection pool right after compose start.
func verifyBothNonEmpty(ctx context.Context, conn driver.Conn, lokiURL, cerbURL string, logger *slog.Logger) error {
	// CH side — direct COUNT against otel_logs. Fast and authoritative.
	var chCount uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM otel_logs").Scan(&chCount); err != nil {
		return fmt.Errorf("ch count: %w", err)
	}
	if chCount == 0 {
		return fmt.Errorf("clickhouse otel_logs is empty after seed")
	}
	logger.Info("clickhouse otel_logs row count", "rows", chCount)

	// Build a [start, end] window that brackets the anchor with enough
	// slack to absorb each backend's quirks:
	//   - Cerberus scans otel_logs by `Timestamp BETWEEN start AND end`,
	//     so the window MUST cover the anchor (2026-05-11T00:00:00Z →
	//     anchor + 10min). Without it cerberus's /labels is empty even
	//     though CH holds the rows.
	//   - Reference Loki's index is built async by ingest time. When the
	//     fixture's anchor is in the past relative to system time (as
	//     happens during CI runs that don't pin the clock), the
	//     freshly-pushed chunks aren't yet visible under a narrow
	//     anchor-aligned window — so we widen to [anchor - 1d, now + 1d]
	//     to absorb any (system clock vs anchor) skew. PR 3's diff
	//     driver will need to thread per-query time windows correctly;
	//     the smoke just needs both endpoints to report non-empty
	//     /labels.
	anchorTS, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return fmt.Errorf("parse anchor: %w", err)
	}
	start := anchorTS.Add(-24 * time.Hour)
	end := time.Now().Add(24 * time.Hour)
	if end.Before(anchorTS.Add(time.Hour)) {
		end = anchorTS.Add(time.Hour)
	}
	for _, target := range []struct {
		label, base string
	}{
		{"loki", lokiURL},
		{"cerberus", cerbURL},
	} {
		if err := waitLabelsNonEmpty(ctx, target.label, target.base, start, end, logger); err != nil {
			return err
		}
	}
	return nil
}

// waitLabelsNonEmpty polls baseURL + /loki/api/v1/labels for up to 30s
// and returns once the response carries at least one label name. The
// [start, end] window is passed as `?start=<ns>&end=<ns>` so cerberus's
// time-windowed label scan covers the seeded fixture.
func waitLabelsNonEmpty(ctx context.Context, label, baseURL string, start, end time.Time, logger *slog.Logger) error {
	deadline := time.Now().Add(30 * time.Second)
	url := fmt.Sprintf("%s/loki/api/v1/labels?start=%d&end=%d",
		strings.TrimRight(baseURL, "/"), start.UnixNano(), end.UnixNano())
	for {
		labels, err := fetchLokiLabels(ctx, url)
		if err == nil && len(labels) > 0 {
			sort.Strings(labels)
			logger.Info("/labels non-empty", "target", label, "url", url, "labels", labels)
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("%s /labels: %w", label, err)
			}
			return fmt.Errorf("%s /labels returned 0 labels after 30s", label)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// fetchLokiLabels parses the Loki /labels response shape
//
//	{"status":"success","data":["__name__","service",...]}
func fetchLokiLabels(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var out struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
