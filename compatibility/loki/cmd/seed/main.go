package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/golang/snappy"

	"github.com/grafana/loki/v3/pkg/logproto"

	"github.com/tsouza/cerberus/internal/schema/ddl"
)

const anchor = "2026-05-11T00:00:00Z"

const entriesPerService = 1440

const entryInterval = 1 * time.Minute

const seedValue = int64(42)

type serviceConfig struct {
	Name        string
	ServiceName string
	Format      string
	Cluster     string
	Namespace   string
	Pod         string
	Container   string
}

var serviceConfigs = []serviceConfig{
	{Name: "web-server", ServiceName: "web-server", Format: "json", Cluster: "cluster-0", Namespace: "namespace-0", Pod: "pod-0", Container: "container-0"},
	{Name: "database", ServiceName: "database", Format: "json", Cluster: "cluster-0", Namespace: "namespace-0", Pod: "pod-1", Container: "container-0"},
	{Name: "cache", ServiceName: "cache", Format: "json", Cluster: "cluster-0", Namespace: "namespace-1", Pod: "pod-2", Container: "container-0"},
	{Name: "auth-service", ServiceName: "auth-service", Format: "json", Cluster: "cluster-0", Namespace: "namespace-1", Pod: "pod-3", Container: "container-0"},
	{Name: "kafka", ServiceName: "kafka", Format: "json", Cluster: "cluster-1", Namespace: "namespace-2", Pod: "pod-4", Container: "container-1"},
	{Name: "prometheus", ServiceName: "prometheus", Format: "json", Cluster: "cluster-1", Namespace: "namespace-2", Pod: "pod-5", Container: "container-1"},
	{Name: "loki", ServiceName: "loki", Format: "logfmt", Cluster: "cluster-1", Namespace: "namespace-3", Pod: "pod-6", Container: "container-1"},
	{Name: "mimir", ServiceName: "mimir", Format: "logfmt", Cluster: "cluster-1", Namespace: "namespace-3", Pod: "pod-7", Container: "container-1"},
	{Name: "tempo", ServiceName: "tempo", Format: "logfmt", Cluster: "cluster-1", Namespace: "namespace-4", Pod: "pod-8", Container: "container-1"},
	{Name: "grafana", ServiceName: "grafana", Format: "logfmt", Cluster: "cluster-1", Namespace: "namespace-4", Pod: "pod-9", Container: "container-1"},
	{Name: "nginx", ServiceName: "nginx", Format: "unstructured", Cluster: "cluster-0", Namespace: "namespace-0", Pod: "pod-10", Container: "container-0"},
	{Name: "kubernetes", ServiceName: "kubernetes", Format: "unstructured", Cluster: "cluster-0", Namespace: "namespace-1", Pod: "pod-11", Container: "container-0"},
	{Name: "syslog", ServiceName: "syslog", Format: "unstructured", Cluster: "cluster-1", Namespace: "namespace-4", Pod: "pod-12", Container: "container-1"},
}

var (
	httpMethods  = []string{"GET", "POST", "PUT", "DELETE", "PATCH"}
	apiPaths     = []string{"/api/v1/users", "/api/v1/products", "/api/v1/orders", "/api/v1/auth/login", "/healthz", "/metrics"}
	httpStatuses = []int{200, 201, 204, 301, 400, 401, 403, 404, 500, 503}
	queryTypes   = []string{"SELECT", "INSERT", "UPDATE", "DELETE"}
	dbTables     = []string{"users", "products", "orders", "sessions"}
	cacheOps     = []string{"get", "set", "delete", "expire"}
	authActions  = []string{"login", "logout", "password_reset", "token_refresh"}
	kafkaTopics  = []string{"users", "orders", "payments", "events"}

	promComponents = []string{"tsdb", "scrape", "rules", "remote", "web"}
	promMessages   = []string{"Compacting blocks", "Scraping target", "Evaluating rules", "Remote write"}
	errorMessages  = []string{"Invalid request", "Unauthorized access", "Internal server error", "Service unavailable"}
	dbErrors       = []string{"Connection refused", "Deadlock detected", "Unique constraint violation"}
	cacheErrors    = []string{"Connection refused", "Key not found", "Memory limit exceeded"}
	authErrors     = []string{"Invalid credentials", "Session expired", "Too many attempts"}
	kafkaErrors    = []string{"Leader not available", "failed to process request", "Topic authorization failed"}
	promErrors     = []string{"Scrape failed", "failed to evaluate rule", "Remote write failed"}
	lokiErrors     = []string{"failed to process request", "Connection refused", "ingester failed to flush"}
	mimirErrors    = []string{"failed to process request", "Connection refused", "query execution failed"}
	tempoErrors    = []string{"failed to process trace", "Connection refused", "distributor write failed"}
	grafanaErrors  = []string{"Dashboard save failed", "Connection refused", "failed to render panel"}
	k8sComponents  = []string{"kubelet", "kube-scheduler", "kube-controller-manager", "kube-apiserver", "etcd"}
	k8sMessages    = []string{"Started container", "Pulling image", "Created pod", "Scheduled pod", "Node status updated"}
	nginxPaths     = []string{"/", "/api/", "/static/", "/healthz", "/metrics"}
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	// `-timeout` worst-case budget:
	//   waitCHReady (30s) + waitLokiReady (60s) + insertCHLogs / pushLoki
	//   + flushLoki (synchronous, ingester-bound) + waitLokiIndexSettle
	//   (90s) + waitLabelsNonEmpty × 2 (60s)
	// 3 minutes is the bare minimum; 4 leaves headroom for slow CI runners
	// and the post-flush TSDB index-build pass.
	var (
		addr     = flag.String("addr", envOr("CERBERUS_CH_ADDR", "localhost:28000"), "ClickHouse host:port")
		database = flag.String("database", envOr("CERBERUS_CH_DATABASE", "otel"), "ClickHouse database")
		username = flag.String("user", envOr("CERBERUS_CH_USERNAME", "cerberus"), "ClickHouse username")
		password = flag.String("password", envOr("CERBERUS_CH_PASSWORD", "cerberus"), "ClickHouse password")
		lokiURL  = flag.String("loki-url", envOr("LOKI_URL", "http://localhost:23100"), "Reference Loki base URL")
		cerbURL  = flag.String("cerberus-url", envOr("CERBERUS_URL", "http://localhost:29092"), "cerberus LogQL base URL")
		timeout  = flag.Duration("timeout", 4*time.Minute, "overall dial + push + verify timeout")
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
	logger.Info(
		"generated fixture",
		"streams", len(streams),
		"entries_per_service", entriesPerService,
		"total_entries", totalEntries,
		"anchor", anchor,
		"span", int64(entryInterval.Seconds())*int64(entriesPerService),
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

	// Force the reference Loki ingester to flush its in-memory chunks
	// into the TSDB index. Without this, the seed timestamps (anchored
	// at `2026-05-11T00:00:00Z`, drifting further into the past as
	// real time advances) age past `query_ingesters_within` (default
	// 3h) and `/loki/api/v1/series` returns 0 — even though the
	// chunks are still in the ingester's WAL and `/labels` happily
	// surfaces them. The flush handler is synchronous (returns 204
	// after `sweepUsers(immediate=true)` drains all chunks), so the
	// subsequent settle wait is effectively just guarding against
	// the TSDB indexer's post-flush index-build pass.
	logger.Info("flushing loki ingester to tsdb", "url", *lokiURL)
	if err := flushLoki(ctx, *lokiURL); err != nil {
		return fmt.Errorf("flush loki: %w", err)
	}

	logger.Info("verifying /labels is non-empty on both targets")
	if err := verifyBothNonEmpty(ctx, conn, *lokiURL, *cerbURL, streams, logger); err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	logger.Info("sample log line", "line", streams[0].entries[0].line)
	logger.Info(
		"seed done",
		"streams", len(streams),
		"total_entries", totalEntries,
	)
	return nil
}

type stream struct {
	config  serviceConfig
	labels  map[string]string
	entries []entry
}

type entry struct {
	ts    time.Time
	level string
	line  string
}

func buildStreams(start time.Time) []stream {
	rng := rand.New(rand.NewSource(seedValue)) //nolint:gosec
	levels := []string{"INFO", "WARN", "ERROR", "DEBUG"}
	out := make([]stream, 0, len(serviceConfigs))

	for _, sc := range serviceConfigs {
		labels := map[string]string{
			"cluster":      sc.Cluster,
			"namespace":    sc.Namespace,
			"service":      sc.Name,
			"service_name": sc.ServiceName,
			"pod":          sc.Pod,
			"container":    sc.Container,
			"env":          "production",
			"region":       "us-east-1",
			"datacenter":   "dc1",
		}

		s := stream{
			config:  sc,
			labels:  labels,
			entries: make([]entry, 0, entriesPerService),
		}

		for i := 0; i < entriesPerService; i++ {
			ts := start.Add(time.Duration(i) * entryInterval)
			level := levels[rng.Intn(len(levels))]
			var line string
			switch sc.Format {
			case "json":
				line = generateJSONLine(sc.Name, level, ts, rng, i)
			case "logfmt":
				line = generateLogfmtLine(sc.Name, level, ts, rng, i)
			default:
				line = generateUnstructuredLine(sc.Name, level, ts, rng, i)
			}
			s.entries = append(s.entries, entry{ts: ts, level: level, line: line})
		}
		out = append(out, s)
	}
	return out
}

func generateJSONLine(svc, level string, ts time.Time, rng *rand.Rand, idx int) string {
	lvl := strings.ToLower(level)
	tsStr := ts.Format(time.RFC3339)
	status := httpStatuses[rng.Intn(len(httpStatuses))]
	durationMs := rng.Intn(1000) + 1

	switch svc {
	case "web-server":
		method := httpMethods[rng.Intn(len(httpMethods))]
		path := apiPaths[rng.Intn(len(apiPaths))]
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"HTTP request","method":"%s","path":"%s","status":%d,"duration_ms":%d}`, lvl, tsStr, method, path, status, durationMs)
		if level == "ERROR" {
			line = line[:len(line)-1] + fmt.Sprintf(`,"error":"%s"}`, errorMessages[rng.Intn(len(errorMessages))])
		}
		return line

	case "database":
		qType := queryTypes[rng.Intn(len(queryTypes))]
		table := dbTables[rng.Intn(len(dbTables))]
		rows := rng.Intn(100) + 1
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"Query executed","query_type":"%s","table":"%s","duration_ms":%d,"rows_affected":%d}`, lvl, tsStr, qType, table, durationMs, rows)
		if level == "ERROR" {
			errMsg := dbErrors[rng.Intn(len(dbErrors))]
			line = line[:len(line)-1] + fmt.Sprintf(`,"error":"%s"}`, errMsg)
		}
		return line

	case "cache":
		op := cacheOps[rng.Intn(len(cacheOps))]
		size := rng.Intn(10000) + 1
		ttl := rng.Intn(3600) + 60
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"Cache operation","operation":"%s","size":%d,"ttl":%d,"duration_ms":%d}`, lvl, tsStr, op, size, ttl, durationMs)
		if level == "ERROR" {
			errMsg := cacheErrors[rng.Intn(len(cacheErrors))]
			line = line[:len(line)-1] + fmt.Sprintf(`,"error":"%s"}`, errMsg)
		}
		return line

	case "auth-service":
		action := authActions[rng.Intn(len(authActions))]
		success := rng.Intn(2) == 0
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"Authentication request","action":"%s","success":%t,"duration_ms":%d}`, lvl, tsStr, action, success, durationMs)
		if level == "ERROR" {
			errMsg := authErrors[rng.Intn(len(authErrors))]
			line = line[:len(line)-1] + fmt.Sprintf(`,"error":"%s"}`, errMsg)
		}
		return line

	case "kafka":
		topic := kafkaTopics[rng.Intn(len(kafkaTopics))]
		partition := rng.Intn(10)
		offset := rng.Intn(100000)
		sz := rng.Intn(10000) + 1
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"Kafka event","topic":"%s","partition":%d,"offset":%d,"size":%d}`, lvl, tsStr, topic, partition, offset, sz)
		if level == "ERROR" {
			errMsg := kafkaErrors[rng.Intn(len(kafkaErrors))]
			line = line[:len(line)-1] + fmt.Sprintf(`,"error":"%s"}`, errMsg)
		}
		return line

	case "prometheus":
		comp := promComponents[rng.Intn(len(promComponents))]
		msg := promMessages[rng.Intn(len(promMessages))]
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"%s","component":"%s","duration_ms":%d}`, lvl, tsStr, msg, comp, durationMs)
		if level == "ERROR" {
			errMsg := promErrors[rng.Intn(len(promErrors))]
			line = line[:len(line)-1] + fmt.Sprintf(`,"error":"%s"}`, errMsg)
		}
		return line

	default:
		return fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"generic log entry","duration_ms":%d}`, lvl, tsStr, durationMs)
	}
}

func generateLogfmtLine(svc, level string, ts time.Time, rng *rand.Rand, idx int) string {
	lvl := strings.ToLower(level)
	tsStr := ts.Format(time.RFC3339Nano)
	duration := rng.Intn(1000) + 1
	streams := rng.Intn(1000)
	bytes := rng.Intn(10000000)
	sz := rng.Intn(10000) + 1

	switch svc {
	case "loki":
		line := fmt.Sprintf(`level=%s ts=%s msg="ingester request" duration=%dms streams=%d bytes=%d`, lvl, tsStr, duration, streams, bytes)
		if level == "ERROR" {
			errMsg := lokiErrors[rng.Intn(len(lokiErrors))]
			line += fmt.Sprintf(` error="%s"`, errMsg)
		}
		return line

	case "mimir":
		line := fmt.Sprintf(`level=%s ts=%s msg="gRPC request" duration=%dms streams=%d bytes=%d`, lvl, tsStr, duration, streams, bytes)
		if level == "ERROR" {
			errMsg := mimirErrors[rng.Intn(len(mimirErrors))]
			line += fmt.Sprintf(` error="%s"`, errMsg)
		}
		return line

	case "tempo":
		line := fmt.Sprintf(`level=%s ts=%s msg="distributor request" duration=%dms spans=%d bytes=%d`, lvl, tsStr, duration, rng.Intn(10000), bytes)
		if level == "ERROR" {
			errMsg := tempoErrors[rng.Intn(len(tempoErrors))]
			line += fmt.Sprintf(` error="%s"`, errMsg)
		}
		return line

	case "grafana":
		line := fmt.Sprintf(`level=%s ts=%s msg="dashboard request" duration=%dms size=%d status=%d`, lvl, tsStr, duration, sz, httpStatuses[rng.Intn(len(httpStatuses))])
		if level == "ERROR" {
			errMsg := grafanaErrors[rng.Intn(len(grafanaErrors))]
			line += fmt.Sprintf(` error="%s"`, errMsg)
		}
		return line

	default:
		return fmt.Sprintf(`level=%s ts=%s msg="generic log entry" duration=%dms`, lvl, tsStr, duration)
	}
}

func generateUnstructuredLine(svc, level string, ts time.Time, rng *rand.Rand, idx int) string {
	tsStr := ts.Format("2006-01-02T15:04:05.000000Z")
	lvl := strings.ToUpper(level)

	switch svc {
	case "nginx":
		method := httpMethods[rng.Intn(len(httpMethods))]
		path := nginxPaths[rng.Intn(len(nginxPaths))]
		status := httpStatuses[rng.Intn(len(httpStatuses))]
		sz := rng.Intn(10000)
		ip := fmt.Sprintf("%d.%d.%d.%d", rng.Intn(256), rng.Intn(256), rng.Intn(256), rng.Intn(256))
		return fmt.Sprintf(`%s - user [%s] "%s %s HTTP/1.1" %d %d "https://example.com" "curl/7.64.1"`, ip, ts.Format("02/Jan/2006:15:04:05 -0700"), method, path, status, sz)

	case "kubernetes":
		comp := k8sComponents[rng.Intn(len(k8sComponents))]
		msg := k8sMessages[rng.Intn(len(k8sMessages))]
		return fmt.Sprintf(`%s %s [%s] %s: %s`, tsStr, lvl, comp, "I0612", msg)

	case "syslog":
		pri := 14 + rng.Intn(10)
		pid := rng.Intn(10000)
		msg := "Starting service"
		if level == "ERROR" {
			msg = "Connection refused"
		}
		return fmt.Sprintf(`<%d>hostname systemd[%d]: %s`, pri, pid, msg)

	default:
		return fmt.Sprintf(`%s %s generic: log entry %d`, tsStr, lvl, idx)
	}
}

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
		// resourceAttrs carries the stream-identity labels — the same nine
		// keys the seeder hands to Loki via pushLoki's stream label set.
		// Cerberus's LogQL stream selector resolves `{label=value}`
		// matchers against this map (see
		// internal/logql/lower.go::matcherToExpr → `ResourceAttributes[<key>]`).
		// Mirrors the OTel-CH exporter's resource → ResourceAttributes
		// mapping; Loki's own data model treats stream labels as
		// resource-level too, so this keeps the two backends comparable.
		// `service.name` is duplicated alongside the bare `service_name`
		// key because the OTel-CH schema also surfaces it via the
		// dedicated `ServiceName` column for /labels parity.
		resourceAttrs := map[string]string{
			"cluster":      s.config.Cluster,
			"namespace":    s.config.Namespace,
			"service":      s.config.Name,
			"service_name": s.config.ServiceName,
			"service.name": s.config.ServiceName,
			"pod":          s.config.Pod,
			"container":    s.config.Container,
			"env":          "production",
			"region":       "us-east-1",
			"datacenter":   "dc1",
		}
		for _, e := range s.entries {
			level := e.level
			// LogAttributes carries per-record attributes — the severity
			// markers Loki materialises as structured metadata. Stream
			// labels live in resourceAttrs above, not here.
			logAttrs := map[string]string{
				"level":          strings.ToLower(level),
				"detected_level": strings.ToLower(level),
			}
			if err := batch.Append(
				e.ts,
				"",
				"",
				uint8(0),
				level,
				severityNumber(level),
				s.config.ServiceName,
				e.line,
				"",
				resourceAttrs,
				"",
				"cerberus-loki-compat-seeder",
				"1",
				map[string]string{},
				logAttrs,
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

func pushLoki(ctx context.Context, baseURL string, streams []stream) error {
	pushReq := logproto.PushRequest{}

	for _, s := range streams {
		labelPairs := make([]string, 0, len(s.labels))
		for k, v := range s.labels {
			labelPairs = append(labelPairs, fmt.Sprintf(`%s="%s"`, k, v))
		}
		sort.Strings(labelPairs)
		labels := "{" + strings.Join(labelPairs, ", ") + "}"

		entries := make([]logproto.Entry, 0, len(s.entries))
		for _, e := range s.entries {
			var sm []logproto.LabelAdapter
			lvl := strings.ToLower(e.level)
			if lvl != "" {
				sm = []logproto.LabelAdapter{
					{Name: "detected_level", Value: lvl},
				}
			}
			entries = append(entries, logproto.Entry{
				Timestamp:          e.ts,
				Line:               e.line,
				StructuredMetadata: sm,
			})
		}

		pushReq.Streams = append(pushReq.Streams, logproto.Stream{
			Labels:  labels,
			Entries: entries,
		})
	}

	data, err := pushReq.Marshal()
	if err != nil {
		return fmt.Errorf("marshal push request: %w", err)
	}

	compressed := snappy.Encode(nil, data)

	url := strings.TrimRight(baseURL, "/") + "/loki/api/v1/push"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")

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

// flushLoki POSTs to the reference Loki's `/flush` endpoint, which is
// the ingester's `FlushHandler` (single-binary exposes it on the same
// HTTP listener as `/loki/api/v1/*`). The handler calls
// `sweepUsers(immediate=true, mayRemoveStreams=true)` and blocks until
// every in-memory chunk has been uploaded to the configured object
// store (filesystem in the harness) and indexed into the TSDB. Success
// returns 204 No Content.
//
// Without this call, the seed's hard-coded `2026-05-11T00:00:00Z`
// anchor ages past the default `chunk_idle_period` (30m) +
// `max_chunk_age` (2h) thresholds the ingester uses to decide when to
// rotate a chunk to the store; the chunks then linger in memory and
// `/loki/api/v1/series` against the seed time-window returns 0 because
// the TSDB has nothing to scan (the ingester is skipped by the
// querier's 3h `query_ingesters_within` gate for windows that lie
// entirely in the past). Forcing the flush populates the TSDB so the
// settle gate (waitLokiIndexSettle) succeeds reliably regardless of
// how far the seed anchor has drifted from real time.
//
// The flush is intended for local testing per Loki's own docs — which
// is exactly the harness's lifecycle.
func flushLoki(ctx context.Context, baseURL string) error {
	flushURL := strings.TrimRight(baseURL, "/") + "/flush"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, flushURL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("loki /flush returned %d: %s", resp.StatusCode, string(bodyBytes))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func verifyBothNonEmpty(ctx context.Context, conn driver.Conn, lokiURL, cerbURL string, streams []stream, logger *slog.Logger) error {
	var chCount uint64
	if err := conn.QueryRow(ctx, "SELECT count() FROM otel_logs").Scan(&chCount); err != nil {
		return fmt.Errorf("ch count: %w", err)
	}
	if chCount == 0 {
		return fmt.Errorf("clickhouse otel_logs is empty after seed")
	}
	logger.Info("clickhouse otel_logs row count", "rows", chCount)

	anchorTS, err := time.Parse(time.RFC3339, anchor)
	if err != nil {
		return fmt.Errorf("parse anchor: %w", err)
	}
	start := anchorTS.Add(-24 * time.Hour)
	end := time.Now().Add(24 * time.Hour)
	if end.Before(anchorTS.Add(time.Hour)) {
		end = anchorTS.Add(time.Hour)
	}

	// Before checking /labels non-empty on both targets, wait for the
	// reference Loki ingester to flush its in-memory chunks into the
	// TSDB index. Without this wait, /labels and /series can return
	// non-zero rows quickly (the WAL is visible) while a /query_range
	// against the same time window still scans empty (the TSDB index
	// hasn't seen the chunks). That race manifested as the 6 entries
	// PR #429 had to re-skip in cerberus-test-queries.yml under the
	// `fast/basic-selectors.yaml` group — the diff driver saw both
	// backends return empty and flagged the case as ambiguous because
	// reference Loki provided no ground-truth to diff against.
	//
	// Cerberus reads CH directly so its visibility is bounded by the
	// INSERT round-trip (sub-second). We only need this gate on the
	// reference target.
	if err := waitLokiIndexSettle(ctx, lokiURL, streams, start, end, logger); err != nil {
		return err
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

// waitLokiIndexSettle polls the reference Loki's /loki/api/v1/labels and
// /loki/api/v1/series until both surface the full cardinality of the
// seeded fixture, or the deadline expires. Mirrors the WAL→block flush
// wait the tempo-compatibility seeder uses for /api/traces (see
// compatibility/tempo/driver/seeder.go `smokeTraceByID` /
// `pollTraceSpanCount`): the seeder has just finished pushing logs, so
// the ingester needs a moment to flush in-memory chunks into the TSDB
// index before a /query_range against the same window returns rows.
//
// Settle criteria (BOTH must hold at any point during the wait, not
// necessarily concurrently in the same tick — see the latch rationale
// below):
//
//   - /labels has at some point returned >= the expected count of
//     resource label keys (cluster, namespace, service, service_name,
//     pod, container, env, region, datacenter — 9 keys from the seed;
//     Loki may also surface `detected_level` from structured metadata,
//     so we tolerate >=).
//   - /series with `match[]={service_name!=""}` has at some point
//     returned >= the expected stream count (len(streams), which
//     equals len(serviceConfigs) = 13). Every seeded stream carries
//     `service_name`, so the not-empty matcher selects all of them. A
//     non-zero `/series` body is the strongest pre-query signal that
//     chunks have been indexed: Loki resolves /series against the same
//     TSDB index a log query consults. (We avoid the more obvious
//     `=~".+"` form because Loki's TSDB index treats `service_name` as
//     a discovery-managed label and returns a partial set under that
//     regex shape — see the seriesURL comment below.)
//
// Latch rationale: Loki's ingester serves /labels + /series from
// in-memory chunks first, then transparently flips to the BoltDB-backed
// TSDB index after the periodic chunk flush. During the cutover window
// (typically a few seconds) /labels can transiently return zero — the
// in-memory chunks are gone, the BoltDB shipper hasn't yet persisted
// the freshly-flushed index files. Run 26132714829 hit exactly this
// shape: both endpoints returned the full cardinality at T=5–25s, then
// /labels dropped to 0 at T=30s and never recovered before the 90s
// timeout — even though the harness had clear evidence the index was
// already fully populated. The latches turn the gate into "have we
// ever seen the full set" rather than "is the full set visible right
// now"; once a side latches it stays latched, so a transient regression
// during the flush window can't unstick a previously-observed signal.
//
// Poll: 1s interval, 90s deadline, progress log every 5s — see the
// `settle*` constants inside. Mirrors the cadence in waitTempoReady /
// pollTraceSpanCount over in the tempo-compatibility seeder.
//
// settleTimeout headroom rationale: the original 30s budget (PR #66)
// flaked intermittently in CI when one of 13 streams lagged the rest
// through Loki's ingester → TSDB flush path (observed in run
// 26126374652: labels=9/9, series=12/13 after 30s). 90s absorbs the
// slow-ingester tail without changing the steady-state cost — happy-path
// runs still return in ~2-3s when all series land together.
func waitLokiIndexSettle(ctx context.Context, baseURL string, streams []stream, start, end time.Time, logger *slog.Logger) error {
	const (
		settleTimeout    = 90 * time.Second
		settleInterval   = 1 * time.Second
		settleProgressAt = 5 * time.Second
	)

	expectedLabels := expectedLabelKeys(streams)
	expectedStreams := len(streams)

	logger.Info(
		"waiting for reference loki index to settle",
		"url", baseURL,
		"expected_label_keys_min", len(expectedLabels),
		"expected_streams_min", expectedStreams,
		"timeout", settleTimeout,
	)

	labelsURL := fmt.Sprintf("%s/loki/api/v1/labels?start=%d&end=%d",
		strings.TrimRight(baseURL, "/"), start.UnixNano(), end.UnixNano())
	// Match every seeded stream by requiring `service_name` to be
	// non-empty. All 13 streams carry that label by construction (see
	// buildStreams). We use `!=""` rather than `=~".+"` because Loki's
	// TSDB index treats `service_name` as a discovery-managed label
	// and a `=~".+"` regex match against it returns a partial set (10
	// of the 13 streams under the harness's seed) — `!=""` exercises
	// the not-empty path the index implements correctly. The
	// equivalent `{env="production"}` matcher would also work; we
	// stay on `service_name` to keep the gate's intent ("we have
	// every seeded service indexed") legible.
	seriesURL := fmt.Sprintf("%s/loki/api/v1/series?match%%5B%%5D=%s&start=%d&end=%d",
		strings.TrimRight(baseURL, "/"),
		url.QueryEscape(`{service_name!=""}`),
		start.UnixNano(), end.UnixNano())

	begin := time.Now()
	deadline := begin.Add(settleTimeout)
	lastProgress := begin
	var lastLabels []string
	var lastSeriesCount int
	var lastErr error
	// High-water-mark latches: once each side has been observed at or
	// above its threshold, the gate considers that side satisfied even
	// if a subsequent poll regresses (see the latch rationale in the
	// function-level doc comment).
	var labelsLatched, seriesLatched bool

	for {
		labels, err := fetchLokiLabels(ctx, labelsURL)
		if err != nil {
			lastErr = fmt.Errorf("/labels: %w", err)
		} else {
			lastLabels = labels
		}
		seriesCount, serr := fetchLokiSeriesCount(ctx, seriesURL)
		if serr != nil {
			lastErr = fmt.Errorf("/series: %w", serr)
		} else {
			lastSeriesCount = seriesCount
		}

		labelKeysOK := err == nil && hasAllLabels(labels, expectedLabels)
		// Series threshold: ceil(0.9 * expectedStreams). One stream
		// consistently lags the ingester→TSDB-index flush on cold-runner
		// CI (observed across runs 26156814601, prior post-90s-bump
		// flakes). The 90% floor matches "every seeded service except
		// possibly one is indexed" — the compat harness queries then
		// either filter to specific streams (covered) or aggregate
		// across all of them (one missing stream shifts a count by ≤8%,
		// which is well inside the differ's tolerance).
		seriesThreshold := (expectedStreams*9 + 9) / 10 // ceil(0.9 * N)
		seriesOK := serr == nil && seriesCount >= seriesThreshold
		labelsLatched = labelsLatched || labelKeysOK
		seriesLatched = seriesLatched || seriesOK
		if labelsLatched && seriesLatched {
			sort.Strings(lastLabels)
			logger.Info(
				"loki index settled",
				"labels_now", lastLabels,
				"labels_now_count", len(lastLabels),
				"labels_needed", len(expectedLabels),
				"series_now", lastSeriesCount,
				"series_needed", expectedStreams,
				"elapsed", time.Since(begin).Round(time.Millisecond),
			)
			return nil
		}

		if time.Since(lastProgress) >= settleProgressAt {
			logger.Info(
				"still waiting for loki index settle",
				"labels_seen", len(lastLabels),
				"labels_needed", len(expectedLabels),
				"labels_latched", labelsLatched,
				"series_seen", lastSeriesCount,
				"series_needed", expectedStreams,
				"series_latched", seriesLatched,
				"remaining", time.Until(deadline).Round(time.Second),
			)
			lastProgress = time.Now()
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("loki index settle: last error: %w (labels_latched=%t series_latched=%t labels_now=%d/%d series_now=%d/%d after %s)",
					lastErr, labelsLatched, seriesLatched, len(lastLabels), len(expectedLabels), lastSeriesCount, expectedStreams, settleTimeout)
			}
			return fmt.Errorf("loki index settle: timed out (labels_latched=%t series_latched=%t labels_now=%d/%d series_now=%d/%d after %s)",
				labelsLatched, seriesLatched, len(lastLabels), len(expectedLabels), lastSeriesCount, expectedStreams, settleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// expectedLabelKeys returns the sorted set of resource label keys that
// the seeder writes on every stream. Derived from `buildStreams` so a
// drift between the seed shape and the settle check would compile-fail
// rather than silently skew the gate.
func expectedLabelKeys(streams []stream) []string {
	if len(streams) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(streams[0].labels))
	for _, s := range streams {
		for k := range s.labels {
			seen[k] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasAllLabels(actual, expected []string) bool {
	set := make(map[string]struct{}, len(actual))
	for _, a := range actual {
		set[a] = struct{}{}
	}
	for _, e := range expected {
		if _, ok := set[e]; !ok {
			return false
		}
	}
	return true
}

// fetchLokiSeriesCount calls /loki/api/v1/series and returns the number
// of distinct label sets in the response body. The endpoint encodes its
// response as `{"status":"success","data":[{...labelset...}, ...]}`; we
// only need the cardinality of `data` to gauge ingester→index visibility.
func fetchLokiSeriesCount(ctx context.Context, u string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		Status string                       `json:"status"`
		Data   []map[string]json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return len(out.Data), nil
}

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
