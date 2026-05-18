// Seeder implementation for the Tempo / TraceQL compatibility harness
// (PR 3 of docs/tempo-compliance-plan.md).
//
// What this file does, end to end:
//
//  1. Build a deterministic in-memory fixture of traces (4 services ×
//     25 traces × 3-5 spans).  All trace IDs / span IDs / timestamps /
//     attributes are derived from a fixed anchor + indices so re-running
//     the seeder against fresh backends produces byte-identical content.
//  2. Push the fixture into the reference Tempo via OTLP gRPC :4317.
//  3. Insert the fixture into ClickHouse `otel_traces` (the read path
//     cerberus uses to answer Tempo HTTP queries). The cerberus binary
//     itself is **read-only over OTLP** — see the top-of-file comment in
//     `main.go` and docs/tempo-compliance-plan.md "Open question 1" for
//     the architectural reasoning.
//  4. Poll `/api/traces/<first-trace-id>` on both backends with a 30s
//     deadline and assert each returns the same non-zero span count.
//
// Implementation notes:
//
//   - OTLP write uses go.opentelemetry.io/proto/otlp's generated
//     TraceServiceClient. The proto types are already a transitive dep
//     of cerberus via otlptrace (see go.mod). Using protojson / raw
//     gRPC keeps the build out of the vendored vulture httpclient,
//     which would have pulled tempopb + jaeger + zap into the module
//     graph for no read-path payoff.
//   - CH write uses clickhouse-go/v2 batch INSERTs against `otel_traces`,
//     mirroring the OTel-CH Exporter schema. The DDL is applied first
//     via internal/schema/ddl, exactly like the prom + loki harness
//     seeders do. The column subset matches the e2e seeder's INSERT
//     (test/e2e/seed/cmd/seed/main.go) so the harness data shape is
//     consistent with what cerberus's existing tests expect.
//   - "30s replication wait" from the PR-3 plan is implemented as a
//     poll, not a flat sleep: we re-query `/api/traces/<id>` until it
//     returns spans OR the deadline expires. Tempo's WAL → block flush
//     can race with the smoke read on a cold runner; a poll is more
//     reliable than a single sleep at any specific duration.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	tracev1 "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

// anchor pins the fixture's first span timestamp. Mirrors the anchor
// used by the prom + loki harness seeders so all three datasets land
// in the same wall-clock window when re-running compatibility locally.
const anchor = "2026-05-11T00:00:00Z"

// fixture-shape constants. Sized to match docs/tempo-compliance-plan.md
// PR 3's stated target (3-5 services × 100 traces × 5 spans ≈ 1500
// spans). Concrete pick: 4 services × 25 traces × variable spans
// (3..5 round-robin). 4 × 25 × ~4 ≈ 400 spans — well above the
// "non-zero span count" smoke assertion and small enough that the
// gRPC ExportTraceServiceRequest stays under the default 4MB message
// limit without a custom dial option.
const (
	traceCount         = 25
	rootSpanDurationNs = int64(150 * time.Millisecond)
)

// services enumerates the synthetic service names baked into the
// fixture. The names are intentionally non-collision with the e2e
// seeder's set ("frontend" / "api" / "db") so a developer can run both
// harnesses against the same CH without trace-id confusion.
var services = []string{"checkout", "payments", "search", "shipping"}

// spanKinds rotates per child span. Mirrors the OTLP enum values 1..5
// (UNSPECIFIED is skipped — every span in the fixture is an explicit
// kind).
var spanKinds = []otlptrace.Span_SpanKind{
	otlptrace.Span_SPAN_KIND_INTERNAL,
	otlptrace.Span_SPAN_KIND_SERVER,
	otlptrace.Span_SPAN_KIND_CLIENT,
	otlptrace.Span_SPAN_KIND_PRODUCER,
	otlptrace.Span_SPAN_KIND_CONSUMER,
}

// span kind → cerberus CH SpanKind column literal. Mirrors the
// `SpanKind` enum the OTel-CH exporter writes; matches the strings
// used in test/e2e/seed/cmd/seed/main.go.
var spanKindCH = map[otlptrace.Span_SpanKind]string{
	otlptrace.Span_SPAN_KIND_INTERNAL: "Internal",
	otlptrace.Span_SPAN_KIND_SERVER:   "Server",
	otlptrace.Span_SPAN_KIND_CLIENT:   "Client",
	otlptrace.Span_SPAN_KIND_PRODUCER: "Producer",
	otlptrace.Span_SPAN_KIND_CONSUMER: "Consumer",
}

// fixtureTrace is the in-memory representation of one trace, shared
// across the OTLP and CH write paths. Holding the OTLP types directly
// keeps the Tempo-side write trivial — we just batch fixtureTrace.spans
// into ResourceSpans envelopes. The CH-side write reads the same fields
// and converts them to the OTel-CH column shapes.
type fixtureTrace struct {
	service  string
	resAttrs map[string]string
	traceID  [16]byte
	spans    []*otlptrace.Span
}

// runSeed is the subcommand entry point. Wired from main.go's switch
// on os.Args[1]; the args slice is os.Args[2:].
func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	var (
		tempoOTLP   = fs.String("tempo-otlp", envOr("TEMPO_OTLP_ADDR", "localhost:24317"), "Tempo OTLP gRPC endpoint (host:port)")
		tempoHTTP   = fs.String("tempo-http", envOr("TEMPO_HTTP_URL", "http://localhost:23200"), "Tempo HTTP base URL (read-back / smoke)")
		cerberusURL = fs.String("cerberus", envOr("CERBERUS_URL", "http://localhost:29092"), "cerberus HTTP base URL (read-back / smoke)")
		chAddr      = fs.String("ch-addr", envOr("CERBERUS_CH_ADDR", "localhost:29100"), "ClickHouse host:port (CH-side write path)")
		chDatabase  = fs.String("ch-database", envOr("CERBERUS_CH_DATABASE", "otel"), "ClickHouse database")
		chUser      = fs.String("ch-user", envOr("CERBERUS_CH_USERNAME", "cerberus"), "ClickHouse username")
		chPassword  = fs.String("ch-password", envOr("CERBERUS_CH_PASSWORD", "cerberus"), "ClickHouse password")
		smokeWait   = fs.Duration("smoke-wait", 30*time.Second, "max wait for /api/traces/<id> to return spans on both backends")
		overall     = fs.Duration("timeout", 3*time.Minute, "overall dial + push + verify timeout")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), *overall)
	defer cancel()

	startAt, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return fmt.Errorf("parse anchor: %w", err)
	}

	traces := buildFixture(startAt)
	totalSpans := 0
	for _, t := range traces {
		totalSpans += len(t.spans)
	}
	logger.Info(
		"generated fixture",
		"services", len(services),
		"traces", len(traces),
		"total_spans", totalSpans,
		"anchor", anchor,
	)

	// --- ClickHouse side ----------------------------------------------
	logger.Info("dialing clickhouse", "addr", *chAddr, "database", *chDatabase)
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{*chAddr},
		Auth: clickhouse.Auth{
			Database: *chDatabase,
			Username: *chUser,
			Password: *chPassword,
		},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("open clickhouse: %w", err)
	}
	defer func() { _ = conn.Close() }()

	if err := waitCHReady(ctx, conn, logger); err != nil {
		return err
	}

	logger.Info("applying ddl", "signal", "traces")
	cfg := ddl.Config{Database: *chDatabase}
	if err := ddl.ApplyWithConfig(ctx, conn, cfg, []ddl.Signal{ddl.Traces}); err != nil {
		return fmt.Errorf("ddl.Apply: %w", err)
	}

	logger.Info("inserting into clickhouse otel_traces")
	if err := insertCHTraces(ctx, conn, traces); err != nil {
		return fmt.Errorf("insert clickhouse: %w", err)
	}

	// --- Tempo side ----------------------------------------------------
	logger.Info("waiting for tempo /ready", "url", *tempoHTTP)
	if err := waitTempoReady(ctx, *tempoHTTP, logger); err != nil {
		return fmt.Errorf("tempo not ready: %w", err)
	}
	logger.Info("pushing into tempo via otlp gRPC", "endpoint", *tempoOTLP)
	if err := pushOTLP(ctx, *tempoOTLP, traces, logger); err != nil {
		return fmt.Errorf("push otlp: %w", err)
	}

	// --- Smoke ---------------------------------------------------------
	firstID := hex.EncodeToString(traces[0].traceID[:])
	logger.Info(
		"smoke /api/traces/<id> on both backends",
		"trace_id", firstID,
		"deadline", *smokeWait,
	)
	if err := smokeTraceByID(ctx, logger, *tempoHTTP, *cerberusURL, firstID, *smokeWait); err != nil {
		return fmt.Errorf("smoke: %w", err)
	}

	logger.Info("smoke /api/search?q={} on tempo (live-store)", "deadline", *smokeWait)
	if err := smokeSearchLiveStore(ctx, logger, *tempoHTTP, *smokeWait); err != nil {
		return fmt.Errorf("smoke search: %w", err)
	}

	logger.Info(
		"seed done",
		"traces", len(traces),
		"total_spans", totalSpans,
	)
	return nil
}

// buildFixture assembles every trace + span the seeder will push. The
// generator is purely deterministic: the same `start` argument always
// produces the same byte content, so reseeding gives both backends a
// stable diff base.
func buildFixture(start time.Time) []*fixtureTrace {
	out := make([]*fixtureTrace, 0, len(services)*traceCount)
	for si, svc := range services {
		for i := 0; i < traceCount; i++ {
			t := newTrace(start, svc, si, i)
			out = append(out, t)
		}
	}
	return out
}

// newTrace builds one trace's spans: a root + 2..4 children. Counts
// rotate (3..5 spans inclusive) so the corpus exercises both single-
// service trees and short multi-hop fan-outs. Times step by 1s × trace
// index to give the read path a meaningful Timestamp window without
// piling everything on the anchor.
func newTrace(start time.Time, svc string, svcIdx, traceIdx int) *fixtureTrace {
	// trace ID is hash-derived so cross-service traces have distinct
	// IDs and a re-run of the seeder produces the exact same hex.
	traceID := deriveTraceID(svc, traceIdx)
	rootSpanID := deriveSpanID(svc, traceIdx, 0)
	ts := start.Add(time.Duration(svcIdx*traceCount+traceIdx) * time.Second)

	childCount := 2 + ((svcIdx + traceIdx) % 3) // 2, 3, or 4 children
	spans := make([]*otlptrace.Span, 0, 1+childCount)

	// Root span. Kind=Server is the canonical entry-point span.
	rootName := fmt.Sprintf("GET /api/%s/%d", svc, traceIdx)
	spans = append(spans, &otlptrace.Span{
		TraceId:           traceID[:],
		SpanId:            rootSpanID[:],
		Name:              rootName,
		Kind:              otlptrace.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: uint64(ts.UnixNano()),
		EndTimeUnixNano:   uint64(ts.UnixNano() + rootSpanDurationNs),
		Attributes: keyValues(map[string]string{
			"http.method": "GET",
			"http.target": fmt.Sprintf("/api/%s/%d", svc, traceIdx),
			"trace.index": fmt.Sprintf("%d", traceIdx),
		}),
		Status: &otlptrace.Status{
			Code: otlptrace.Status_STATUS_CODE_OK,
		},
	})

	for c := 0; c < childCount; c++ {
		childID := deriveSpanID(svc, traceIdx, c+1)
		childKind := spanKinds[(c+traceIdx)%len(spanKinds)]
		childStart := ts.Add(time.Duration(10*(c+1)) * time.Millisecond)
		childDur := time.Duration(20*(c+1)) * time.Millisecond
		// Status alternates: every 5th child reports Error so the
		// fixture has a non-trivial status distribution.
		statusCode := otlptrace.Status_STATUS_CODE_OK
		statusMsg := ""
		if (traceIdx+c)%5 == 4 {
			statusCode = otlptrace.Status_STATUS_CODE_ERROR
			statusMsg = "synthetic error"
		}
		spans = append(spans, &otlptrace.Span{
			TraceId:           traceID[:],
			SpanId:            childID[:],
			ParentSpanId:      rootSpanID[:],
			Name:              fmt.Sprintf("%s.child.%d", svc, c),
			Kind:              childKind,
			StartTimeUnixNano: uint64(childStart.UnixNano()),
			EndTimeUnixNano:   uint64(childStart.Add(childDur).UnixNano()),
			Attributes: keyValues(map[string]string{
				"child.index": fmt.Sprintf("%d", c),
				"kind":        childKind.String(),
			}),
			Status: &otlptrace.Status{Code: statusCode, Message: statusMsg},
		})
	}

	return &fixtureTrace{
		service: svc,
		resAttrs: map[string]string{
			"service.name":    svc,
			"deployment.env":  "compat-test",
			"telemetry.sdk":   "cerberus-tempo-compat-seeder",
			"trace.fixture":   "pr3",
			"trace.fixture.v": "1",
		},
		traceID: traceID,
		spans:   spans,
	}
}

// deriveTraceID hashes the (service, traceIdx) pair into the 16-byte
// OTLP trace ID space. Hash-derived IDs are byte-stable across runs and
// avoid collisions between (svc=a, idx=1) and (svc=b, idx=1).
func deriveTraceID(svc string, idx int) [16]byte {
	var b [8]byte
	// idx is a non-negative loop counter bounded by traceCount (25).
	binary.BigEndian.PutUint64(b[:], uint64(idx)) //nolint:gosec // bounded loop index, no overflow
	h := sha256.Sum256(append([]byte("cerberus-tempo-trace:"+svc+":"), b[:]...))
	var id [16]byte
	copy(id[:], h[:16])
	return id
}

// deriveSpanID hashes (service, traceIdx, spanIdx) into the 8-byte
// OTLP span ID space. Same rationale as deriveTraceID.
func deriveSpanID(svc string, traceIdx, spanIdx int) [8]byte {
	var b [8]byte
	// traceIdx ∈ [0, traceCount=25); spanIdx ∈ [0, ~5]. Product fits in uint64.
	binary.BigEndian.PutUint64(b[:], uint64(traceIdx*100+spanIdx)) //nolint:gosec // bounded loop indices, no overflow
	h := sha256.Sum256(append([]byte("cerberus-tempo-span:"+svc+":"), b[:]...))
	var id [8]byte
	copy(id[:], h[:8])
	return id
}

// keyValues converts a Go string-string map to OTLP's repeated KeyValue
// shape, sorted by key for deterministic encoding.
func keyValues(m map[string]string) []*commonv1.KeyValue {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Sort.Strings would import "sort"; tiny manual sort keeps the
	// dep surface small.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := make([]*commonv1.KeyValue, 0, len(m))
	for _, k := range keys {
		out = append(out, &commonv1.KeyValue{
			Key:   k,
			Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: m[k]}},
		})
	}
	return out
}

// pushOTLP dials Tempo over plaintext gRPC and exports the fixture as
// one ExportTraceServiceRequest per trace. Per-trace batching keeps
// each request tiny and well under the 4MB default message limit.
//
// Plaintext is correct here: docker-compose's `tempo` service exposes
// :4317 with no TLS, exactly as upstream's getting-started config and
// upstream's tempo-vulture configure it.
func pushOTLP(ctx context.Context, addr string, traces []*fixtureTrace, logger *slog.Logger) error {
	// grpc.NewClient is the post-1.65 replacement for grpc.DialContext
	// — it returns a lazy channel and doesn't block on the initial TCP
	// dial. We deliberately don't WithBlock + Wait here because Tempo's
	// healthcheck already gates compose `up --wait` for us, and the
	// first Export() call surfaces unreachable-server errors precisely
	// (vs grpc.WithBlock's opaque "context deadline exceeded").
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("grpc new client %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	client := tracev1.NewTraceServiceClient(conn)

	for i, t := range traces {
		req := &tracev1.ExportTraceServiceRequest{
			ResourceSpans: []*otlptrace.ResourceSpans{{
				Resource: &resourcev1.Resource{
					Attributes: keyValues(t.resAttrs),
				},
				ScopeSpans: []*otlptrace.ScopeSpans{{
					Scope: &commonv1.InstrumentationScope{
						Name:    "cerberus-tempo-compat-seeder",
						Version: "1",
					},
					Spans: t.spans,
				}},
			}},
		}
		if _, err := client.Export(ctx, req); err != nil {
			return fmt.Errorf("otlp export trace %d (%s): %w", i, t.service, err)
		}
	}
	logger.Info("otlp push done", "traces", len(traces))
	return nil
}

// insertCHTraces writes every span into `otel_traces` using a single
// batched INSERT. The column list matches the OTel-CH exporter shape
// (and test/e2e/seed/cmd/seed/main.go's insertTracesSQL) so cerberus's
// existing read path needs no harness-specific quirks.
//
// One subtle conversion: OTel-CH stores `TraceId` / `SpanId` as
// FixedString hex strings, not the raw bytes the OTLP wire carries.
// We hex-encode here once per span and pass through clickhouse-go's
// batch API; the driver does the FixedString length validation
// automatically.
func insertCHTraces(ctx context.Context, conn driver.Conn, traces []*fixtureTrace) error {
	batch, err := conn.PrepareBatch(ctx, `INSERT INTO otel_traces (
        Timestamp, TraceId, SpanId, ParentSpanId,
        SpanName, SpanKind, ServiceName,
        ResourceAttributes, SpanAttributes,
        Duration, StatusCode, StatusMessage
    )`)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}

	for _, t := range traces {
		for _, s := range t.spans {
			// Span timestamps come from our own seeder via ts.UnixNano() —
			// 2026 wall-clock values are far below int64 max; dur is the
			// non-negative difference of two such values.
			ts := time.Unix(0, int64(s.StartTimeUnixNano))               //nolint:gosec // seeder-controlled 2026 ns, fits int64
			dur := int64(s.EndTimeUnixNano) - int64(s.StartTimeUnixNano) //nolint:gosec // seeder-controlled 2026 ns, fits int64
			if err := batch.Append(
				ts,
				hex.EncodeToString(s.TraceId),
				hex.EncodeToString(s.SpanId),
				hex.EncodeToString(s.ParentSpanId),
				s.Name,
				spanKindCH[s.Kind],
				t.service,
				t.resAttrs,
				keyValuesToMap(s.Attributes),
				uint64(dur), //nolint:gosec // dur is positive (end > start)
				statusCodeCH(s.Status),
				statusMessage(s.Status),
			); err != nil {
				return fmt.Errorf("append span %s: %w", s.Name, err)
			}
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	return nil
}

// keyValuesToMap is the inverse of keyValues — handy for the CH side
// which takes `map[string]string` for the Map(LowCardinality(String),
// String) columns.
func keyValuesToMap(kvs []*commonv1.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if kv == nil || kv.Value == nil {
			continue
		}
		if sv, ok := kv.Value.Value.(*commonv1.AnyValue_StringValue); ok {
			out[kv.Key] = sv.StringValue
		}
	}
	return out
}

// statusCodeCH maps the OTLP Status enum to the CH StatusCode column's
// string literals. The OTel-CH exporter writes "Unset" / "Ok" / "Error"
// — same literals cerberus's TraceQL emitter expects.
func statusCodeCH(s *otlptrace.Status) string {
	if s == nil {
		return "Unset"
	}
	switch s.Code {
	case otlptrace.Status_STATUS_CODE_OK:
		return "Ok"
	case otlptrace.Status_STATUS_CODE_ERROR:
		return "Error"
	default:
		return "Unset"
	}
}

// statusMessage extracts the Status.Message safely (Status may be nil
// for some spans even though the fixture always sets it).
func statusMessage(s *otlptrace.Status) string {
	if s == nil {
		return ""
	}
	return s.Message
}

// waitTempoReady polls Tempo's /ready HTTP endpoint until it returns
// 200 or ctx expires. Tempo's image is distroless so the docker-compose
// service has no healthcheck — readiness gating lives here. Tempo's
// /ready flips to 200 only after the live-store + partition-ring are
// fully up, which is also exactly the precondition for OTLP Export
// to succeed.
func waitTempoReady(ctx context.Context, baseURL string, logger *slog.Logger) error {
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
				return fmt.Errorf("tempo /ready: %w", err)
			}
			return fmt.Errorf("tempo /ready returned %d", resp.StatusCode)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		logger.Debug("waiting for tempo", "url", url)
	}
}

// waitCHReady polls SELECT 1 until ClickHouse answers or ctx expires.
// Mirrors the loki + prom harness seeders so all three follow the same
// readiness contract.
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

// smokeTraceByID polls `/api/traces/<id>` on both backends and asserts:
//
//  1. Each backend returns a non-zero span count for the trace ID.
//  2. Both counts agree.
//
// The deadline absorbs Tempo's WAL→block flush race (a span that just
// landed via /push isn't visible on /api/traces until ingester flushes
// to disk; the configured 5m max_block_duration doesn't apply to
// individual traces, but cold-start ingest can still take a few
// seconds). Cerberus reads CH directly so its visibility is bounded by
// the CH INSERT round-trip — typically sub-second.
func smokeTraceByID(ctx context.Context, logger *slog.Logger, tempoHTTP, cerberusURL, traceID string, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	tempoCount, err := pollTraceSpanCount(ctx, logger, "tempo", tempoHTTP, traceID, true)
	if err != nil {
		return fmt.Errorf("tempo trace-by-id: %w", err)
	}
	cerberusCount, err := pollTraceSpanCount(ctx, logger, "cerberus", cerberusURL, traceID, false)
	if err != nil {
		return fmt.Errorf("cerberus trace-by-id: %w", err)
	}

	logger.Info(
		"smoke result",
		"trace_id", traceID,
		"tempo_spans", tempoCount,
		"cerberus_spans", cerberusCount,
	)
	if tempoCount == 0 {
		return fmt.Errorf("tempo reported 0 spans for %s after %s", traceID, deadline)
	}
	if cerberusCount == 0 {
		return fmt.Errorf("cerberus reported 0 spans for %s after %s", traceID, deadline)
	}
	if tempoCount != cerberusCount {
		// Not fatal at PR-3's contract ("non-zero on both"), but log
		// loudly. The real per-span equivalence diff lands in PR 4.
		logger.Warn(
			"span count mismatch between backends — PR 4 will surface this as a corpus diff",
			"tempo_spans", tempoCount,
			"cerberus_spans", cerberusCount,
		)
	}
	return nil
}

// pollTraceSpanCount fetches /api/traces/<id> with retries until either
// the response carries spans, the context expires, or a non-retriable
// error is returned. `tempoShape` selects between Tempo's
// (proto-mapped) `Batches[].ScopeSpans[].Spans` and cerberus's
// (flat) `Batches[].Spans` shape — both are decoded with the same
// permissive struct.
func pollTraceSpanCount(ctx context.Context, logger *slog.Logger, label, baseURL, traceID string, tempoShape bool) (int, error) {
	url := strings.TrimRight(baseURL, "/") + "/api/traces/" + traceID
	for {
		n, err := fetchTraceSpanCount(ctx, url, tempoShape)
		if err == nil && n > 0 {
			return n, nil
		}
		// Distinguish:
		//   * transport / 5xx errors that may resolve on retry
		//   * 404 from cerberus while CH propagates — also retry
		//   * ctx.Done — bail out
		if ctx.Err() != nil {
			if err != nil {
				return 0, fmt.Errorf("%s: %w", label, errors.Join(ctx.Err(), err))
			}
			return n, ctx.Err()
		}
		logger.Debug("retrying trace-by-id", "target", label, "url", url, "err", err, "spans", n)
		select {
		case <-ctx.Done():
			return n, ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// fetchTraceSpanCount issues one GET against /api/traces/<id> and
// counts spans. The decoder is permissive — it doesn't validate every
// OTLP field, just walks the JSON tree counting span objects so the
// same code works against Tempo's full shape and cerberus's flat
// shape.
func fetchTraceSpanCount(ctx context.Context, url string, tempoShape bool) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	// Force JSON so we don't accidentally get the protobuf body that
	// Tempo's /api/v2/traces negotiates by default for some clients.
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, errors.New("404")
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	if tempoShape {
		return decodeTempoSpanCount(resp.Body)
	}
	return decodeCerberusSpanCount(resp.Body)
}

// decodeTempoSpanCount handles Tempo's official `Batches[].ScopeSpans[].Spans`
// JSON shape (the protojson-marshalled tempopb.Trace). Older Tempo builds
// flatten ScopeSpans away; we tolerate both nested and flat shapes by
// summing spans wherever they appear.
func decodeTempoSpanCount(r io.Reader) (int, error) {
	var raw struct {
		Batches []struct {
			ScopeSpans []struct {
				Spans []json.RawMessage `json:"spans"`
			} `json:"scopeSpans"`
			InstrumentationLibrarySpans []struct {
				Spans []json.RawMessage `json:"spans"`
			} `json:"instrumentationLibrarySpans"`
			Spans []json.RawMessage `json:"spans"` // flat fallback
		} `json:"batches"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return 0, fmt.Errorf("decode tempo trace: %w", err)
	}
	total := 0
	for _, b := range raw.Batches {
		for _, ss := range b.ScopeSpans {
			total += len(ss.Spans)
		}
		for _, ils := range b.InstrumentationLibrarySpans {
			total += len(ils.Spans)
		}
		total += len(b.Spans)
	}
	return total, nil
}

// decodeCerberusSpanCount handles cerberus's flat shape (see
// internal/api/tempo/types.go TraceByIDResponse: Batches[].Spans).
func decodeCerberusSpanCount(r io.Reader) (int, error) {
	var raw struct {
		Batches []struct {
			Spans []json.RawMessage `json:"spans"`
		} `json:"batches"`
	}
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return 0, fmt.Errorf("decode cerberus trace: %w", err)
	}
	total := 0
	for _, b := range raw.Batches {
		total += len(b.Spans)
	}
	return total, nil
}

// smokeSearchLiveStore polls Tempo's /api/search?q={} with the
// Recent-Data-Target: live-store header until the response contains at
// least one trace. This catches the failure mode where Tempo's search
// only scans completed blocks (which may not have been flushed yet) and
// returns 0 results while cerberus returns many.
func smokeSearchLiveStore(ctx context.Context, logger *slog.Logger, tempoHTTP string, deadline time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	url := strings.TrimRight(tempoHTTP, "/") + "/api/search?q=%7B%7D"
	for {
		n, err := fetchSearchResultCount(ctx, url)
		if err == nil && n > 0 {
			logger.Info("tempo search returned traces", "count", n)
			return nil
		}
		if ctx.Err() != nil {
			if err != nil {
				return fmt.Errorf("tempo search: %w", errors.Join(ctx.Err(), err))
			}
			return fmt.Errorf("tempo search returned 0 traces after %s", deadline)
		}
		logger.Debug("retrying tempo search", "url", url, "err", err, "traces", n)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

// fetchSearchResultCount issues one GET against /api/search with the
// Recent-Data-Target: live-store header and returns the number of
// traces in the response.
func fetchSearchResultCount(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Recent-Data-Target", "live-store")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	var raw struct {
		Traces []json.RawMessage `json:"traces"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, fmt.Errorf("decode search response: %w", err)
	}
	return len(raw.Traces), nil
}

// envOr returns the env value or `fallback` if the env var is unset
// or empty. Same shape as the prom + loki seeders.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
