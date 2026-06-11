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
	"os"
	"sort"
	"strconv"
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

	// Snapshot the ingester's flush + shipper-upload counters *before*
	// pushing. The settle gate downstream waits for these counters to
	// move past the baseline by the expected deltas (one flushed chunk
	// per pushed stream; ≥1 TSDB shipper upload after the flush). A
	// pre-push baseline is robust against the harness running against a
	// Loki instance that has handled traffic earlier in the same
	// process lifetime (re-run, restart-without-volume-wipe, etc.).
	baseline, err := readLokiMetricsBaseline(ctx, *lokiURL)
	if err != nil {
		return fmt.Errorf("read loki metrics baseline: %w", err)
	}
	logger.Info(
		"loki metrics baseline captured",
		"chunks_flushed_total", baseline.chunksFlushed,
		"shipper_uploads_total", baseline.shipperUploads,
	)

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
	// subsequent settle wait only has to absorb the asynchronous TSDB
	// shipper upload that follows the in-memory→object-store flush.
	logger.Info("flushing loki ingester to tsdb", "url", *lokiURL)
	if err := flushLoki(ctx, *lokiURL); err != nil {
		return fmt.Errorf("flush loki: %w", err)
	}

	logger.Info("verifying /labels is non-empty on both targets")
	if err := verifyBothNonEmpty(ctx, conn, *lokiURL, *cerbURL, streams, baseline, logger); err != nil {
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
		// Every web-server line carries a `client_ip` IPv4, mirroring the
		// loki-bench faker's web-server generator (the dataset metadata
		// the diff driver loads promises this field). A delimited,
		// well-formed dotted-quad makes the ip() line filter (IP-in-line
		// scan) and the `| json | client_ip = ip(...)` label filter both
		// exercise real data on reference Loki and cerberus alike.
		clientIP := webServerClientIP(rng)
		line := fmt.Sprintf(`{"level":"%s","ts":"%s","msg":"HTTP request","method":"%s","path":"%s","status":%d,"duration_ms":%d,"client_ip":"%s"}`, lvl, tsStr, method, path, status, durationMs, clientIP)
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

// webServerClientIP returns a deterministic dotted-quad IPv4 drawn from
// a fixed two-octet pool. The pool deliberately straddles the
// 10.0.0.0/8 boundary (10.x and 172.x) so the burndown ip() label-
// filter cases over `client_ip = ip("10.0.0.0/8")` carve a real,
// non-trivial subset that BOTH backends must agree on, while the
// match-all `0.0.0.0/0` CIDR keeps every line.
func webServerClientIP(rng *rand.Rand) string {
	first := webServerClientIPFirstOctets[rng.Intn(len(webServerClientIPFirstOctets))]
	return fmt.Sprintf("%s.%d.%d", first, rng.Intn(256), rng.Intn(256))
}

// webServerClientIPFirstOctets is the fixed leading-two-octet pool for
// web-server client_ip values; half fall inside 10.0.0.0/8.
var webServerClientIPFirstOctets = []string{"10.0", "10.128", "172.16", "172.31"}

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
			// LogAttributes carries per-record attributes — the OTel-CH
			// analogue of Loki's structured metadata. The key set MUST
			// mirror what pushLoki sends as structured metadata
			// (detected_level only): the /detected_fields differential
			// compares the two backends' structured-metadata-derived
			// fields, so any CH-only key here would surface as a
			// permanent parity diff. Stream labels live in
			// resourceAttrs above, not here.
			logAttrs := map[string]string{
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

func verifyBothNonEmpty(ctx context.Context, conn driver.Conn, lokiURL, cerbURL string, streams []stream, baseline lokiMetricsSnapshot, logger *slog.Logger) error {
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
	start, end := verifyLabelsWindow(anchorTS)

	// Wait for the reference Loki ingester to drain its flush queue and
	// for the TSDB shipper to publish at least one fresh index table.
	// Cerberus reads ClickHouse directly so its visibility is bounded by
	// the INSERT round-trip (sub-second); only the reference Loki target
	// needs this gate.
	if err := waitLokiIndexSettle(ctx, lokiURL, len(streams), baseline, logger); err != nil {
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

// lokiMetricsSnapshot is a baseline reading of the two ingester /
// shipper counters the settle gate watches: chunks the ingester has
// flushed and table uploads the TSDB shipper has completed. The gate
// waits for these counters to move past the baseline by the deltas
// implied by the just-pushed batch.
type lokiMetricsSnapshot struct {
	chunksFlushed  float64
	shipperUploads float64
}

// readLokiMetricsBaseline pulls the current values of the two counters
// the settle gate keys on. Used to anchor the post-flush deltas so the
// gate works whether the reference Loki is freshly booted or being
// re-used across seed runs.
func readLokiMetricsBaseline(ctx context.Context, baseURL string) (lokiMetricsSnapshot, error) {
	metrics, err := fetchLokiMetrics(ctx, baseURL)
	if err != nil {
		return lokiMetricsSnapshot{}, err
	}
	return extractSnapshot(metrics), nil
}

// extractSnapshot is the pure-function decoder pulled out so the unit
// tests can drive the snapshot logic directly with synthetic metrics
// payloads.
func extractSnapshot(metrics map[string]float64) lokiMetricsSnapshot {
	return lokiMetricsSnapshot{
		chunksFlushed:  sumMatching(metrics, "loki_ingester_chunks_flushed_total"),
		shipperUploads: sumMatching(metrics, `loki_tsdb_shipper_tables_upload_operation_total{status="success"`),
	}
}

// Settle-gate cadence. Declared as `var` (not `const`) so unit tests
// can shrink the budget — production keeps the 90s/500ms/5s shape the
// function doc-comment pins. Do not mutate at runtime outside of tests.
var (
	settleTimeout    = 90 * time.Second
	settleInterval   = 500 * time.Millisecond
	settleProgressAt = 5 * time.Second
)

// waitLokiIndexSettle polls the reference Loki's `/metrics` endpoint
// until the ingester and TSDB shipper signal that the just-pushed batch
// has been fully flushed and indexed.
//
// The earlier implementation polled `/loki/api/v1/labels` and
// `/loki/api/v1/series` and guessed "are we done?" from cardinality.
// That heuristic accumulated four PRs of patches across #66 → #561 →
// #576 → #608: timeout bumps, sticky latches, and a 90 % cardinality
// floor to mask a single straggling stream. None of them got at the
// real signal — they were all proxies for "has the ingester finished
// flushing?" Loki itself answers that directly through its Prometheus
// `/metrics` endpoint, which is what this implementation now polls.
//
// Settle criteria — all four must hold concurrently in a single poll
// (no latches, no thresholds, no straggler tolerance):
//
//   - `loki_ingester_flush_queue_length == 0` — the ingester's flush
//     queue has drained. The flush handler enqueues work synchronously
//     under `sweepUsers(immediate=true)`; when the queue is empty the
//     ingester has nothing more to push to the chunk store.
//   - `loki_ingester_memory_chunks == 0` — no chunks remain held in
//     memory waiting on a flush. Together with the queue-length check
//     this rules out both "queued but not started" and "started but
//     not finished" flushes.
//   - `loki_ingester_chunks_flushed_total` has incremented by at least
//     `expectedStreams` since the pre-push baseline. Every pushed
//     stream produces at least one chunk; the counter going up by
//     ≥expectedStreams is positive evidence that *our* push made it
//     through the flush path (rather than a stale baseline reading
//     happening to satisfy the queue/memory checks).
//   - `loki_tsdb_shipper_tables_upload_operation_total{status="success"}`
//     has incremented by at least 1 since the baseline. The TSDB
//     shipper uploads fresh index tables asynchronously after the
//     in-memory→object-store flush; without waiting for at least one
//     successful upload, `/query_range` can still race the index
//     publication and see an empty TSDB.
//
// On deadline expiry the error carries every counter's current value
// alongside the baseline so the failure mode is recoverable from a
// single log line. A missing metric (e.g. an upstream Loki rename) is
// reported as a structured error rather than silently treated as zero.
func waitLokiIndexSettle(ctx context.Context, baseURL string, expectedStreams int, baseline lokiMetricsSnapshot, logger *slog.Logger) error {
	logger.Info(
		"waiting for reference loki index to settle",
		"url", baseURL,
		"expected_streams", expectedStreams,
		"baseline_chunks_flushed", baseline.chunksFlushed,
		"baseline_shipper_uploads", baseline.shipperUploads,
		"timeout", settleTimeout,
	)

	begin := time.Now()
	deadline := begin.Add(settleTimeout)
	lastProgress := begin
	var lastSnapshot lokiSettleSnapshot
	var lastErr error

	for {
		snap, err := fetchLokiSettleSnapshot(ctx, baseURL)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			lastSnapshot = snap

			flushedDelta := snap.chunksFlushed - baseline.chunksFlushed
			uploadsDelta := snap.shipperUploads - baseline.shipperUploads

			if snap.flushQueueLength == 0 &&
				snap.memoryChunks == 0 &&
				flushedDelta >= float64(expectedStreams) &&
				uploadsDelta >= 1 {
				logger.Info(
					"loki index settled",
					"flush_queue_length", snap.flushQueueLength,
					"memory_chunks", snap.memoryChunks,
					"chunks_flushed_delta", flushedDelta,
					"chunks_flushed_needed", expectedStreams,
					"shipper_uploads_delta", uploadsDelta,
					"elapsed", time.Since(begin).Round(time.Millisecond),
				)
				return nil
			}
		}

		if time.Since(lastProgress) >= settleProgressAt {
			logger.Info(
				"still waiting for loki index settle",
				"flush_queue_length", lastSnapshot.flushQueueLength,
				"memory_chunks", lastSnapshot.memoryChunks,
				"chunks_flushed_delta", lastSnapshot.chunksFlushed-baseline.chunksFlushed,
				"chunks_flushed_needed", expectedStreams,
				"shipper_uploads_delta", lastSnapshot.shipperUploads-baseline.shipperUploads,
				"last_err", lastErr,
				"remaining", time.Until(deadline).Round(time.Second),
			)
			lastProgress = time.Now()
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("loki index settle: last error: %w (after %s, baseline_chunks_flushed=%v baseline_shipper_uploads=%v)",
					lastErr, settleTimeout, baseline.chunksFlushed, baseline.shipperUploads)
			}
			return fmt.Errorf(
				"loki index settle: timed out after %s (flush_queue_length=%v memory_chunks=%v chunks_flushed=%v→%v needed_delta=%d shipper_uploads=%v→%v needed_delta=1)",
				settleTimeout,
				lastSnapshot.flushQueueLength,
				lastSnapshot.memoryChunks,
				baseline.chunksFlushed, lastSnapshot.chunksFlushed,
				expectedStreams,
				baseline.shipperUploads, lastSnapshot.shipperUploads,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// lokiSettleSnapshot is the per-poll reading the settle loop diffs
// against the baseline. Kept as plain float64 because Prometheus text
// exposition itself uses float64 for every numeric value (including
// counters).
type lokiSettleSnapshot struct {
	flushQueueLength float64
	memoryChunks     float64
	chunksFlushed    float64
	shipperUploads   float64
}

// fetchLokiSettleSnapshot pulls `/metrics` and resolves the four
// settle-gate signals. The required-metrics check is strict: a missing
// metric is returned as an error rather than silently coerced to zero,
// so an upstream Loki rename surfaces immediately instead of looking
// like an instantly-settled gate.
func fetchLokiSettleSnapshot(ctx context.Context, baseURL string) (lokiSettleSnapshot, error) {
	metrics, err := fetchLokiMetrics(ctx, baseURL)
	if err != nil {
		return lokiSettleSnapshot{}, err
	}
	required := []string{
		"loki_ingester_flush_queue_length",
		"loki_ingester_memory_chunks",
		"loki_ingester_chunks_flushed_total",
	}
	for _, name := range required {
		if !hasMatching(metrics, name) {
			return lokiSettleSnapshot{}, fmt.Errorf("loki /metrics missing required gauge/counter %q (upstream rename? regenerate gate against grafana/loki version in use)", name)
		}
	}
	// The shipper-upload counter only appears after the first upload
	// has been attempted; on a freshly-booted Loki it is absent until
	// the first table flush. We treat missing-but-zero as zero so the
	// pre-push baseline read doesn't fail on a cold start.
	return lokiSettleSnapshot{
		flushQueueLength: sumMatching(metrics, "loki_ingester_flush_queue_length"),
		memoryChunks:     sumMatching(metrics, "loki_ingester_memory_chunks"),
		chunksFlushed:    sumMatching(metrics, "loki_ingester_chunks_flushed_total"),
		shipperUploads:   sumMatching(metrics, `loki_tsdb_shipper_tables_upload_operation_total{status="success"`),
	}, nil
}

// fetchLokiMetrics fetches the raw Prometheus text-format `/metrics`
// payload and parses every sample line into a name→value map. The
// parser is intentionally line-oriented (no full Prometheus expfmt
// dependency): the settle gate only needs to look up a handful of
// metric names by prefix-match.
func fetchLokiMetrics(ctx context.Context, baseURL string) (map[string]float64, error) {
	u := strings.TrimRight(baseURL, "/") + "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("loki /metrics status %d: %s", resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read /metrics body: %w", err)
	}
	return parsePromMetrics(body), nil
}

// parsePromMetrics decodes the Prometheus text exposition into a
// map keyed by the full sample identifier — the metric name plus any
// label set (e.g. `loki_ingester_chunks_flushed_total{reason="forced"}`).
// Comment / blank lines are skipped. A line that fails to parse is
// dropped silently so a future Loki version adding an exemplar or
// histogram extension doesn't break the gate.
func parsePromMetrics(body []byte) map[string]float64 {
	out := make(map[string]float64, 256)
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		// Sample line: `<metric>{labels...} <value> [timestamp]`. We
		// split off the last whitespace-separated field as the value
		// (so an embedded space inside a label value doesn't confuse
		// the parse).
		idx := strings.LastIndexByte(line, ' ')
		if idx < 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		valStr := strings.TrimSpace(line[idx+1:])
		// Prometheus emits NaN as the literal `NaN`; ParseFloat
		// handles it natively. Counters / gauges we care about are
		// always finite, but tolerating NaN keeps the parse robust.
		v, err := parseFloatOrNaN(valStr)
		if err != nil {
			continue
		}
		out[name] = v
	}
	return out
}

// parseFloatOrNaN wraps strconv.ParseFloat so the caller doesn't need
// the import directly. NaN values are returned as-is; downstream code
// treats them as "metric present but unusable" which is the right
// semantic for the settle gate.
func parseFloatOrNaN(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// hasMatching reports whether `metrics` contains either the exact key
// `prefix` (an unlabelled scalar) or any key of the shape
// `prefix{...}` (a labelled metric family). Used to detect the presence
// of a metric without caring about its label set.
func hasMatching(metrics map[string]float64, prefix string) bool {
	for k := range metrics {
		if matchesPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// sumMatching collapses a metric family to a single scalar by summing
// every sample matched by `prefix`. The match is exact for an
// unlabelled scalar or `prefix{...}` for a labelled family — but the
// caller may also pass a partial label-set match like
// `loki_tsdb_shipper_tables_upload_operation_total{status="success"`
// (no closing brace), in which case every key starting with that
// literal is summed. That partial form lets the gate target a single
// `status` value without enumerating the full label set.
func sumMatching(metrics map[string]float64, prefix string) float64 {
	var total float64
	for k, v := range metrics {
		if matchesPrefix(k, prefix) {
			total += v
		}
	}
	return total
}

// matchesPrefix encodes the family/partial match rule shared by
// hasMatching and sumMatching. Two shapes are supported:
//
//   - An unlabelled metric-name match like
//     `loki_ingester_flush_queue_length` matches the exact key (an
//     unlabelled scalar) or any `<prefix>{...}` (the labelled
//     family).
//   - A name+label-selector match like
//     `loki_tsdb_shipper_tables_upload_operation_total{status="success"`
//     matches any key whose metric name equals the literal up to the
//     `{` AND whose label set contains the literal after the `{`
//     anywhere inside the brace pair. We can't use HasPrefix on the
//     label portion because Prometheus serialises labels in
//     alphabetical order — `component` sorts before `status`, so the
//     `status="success"` literal lands mid-string in the rendered
//     key.
func matchesPrefix(key, prefix string) bool {
	if key == prefix {
		return true
	}
	braceIdx := strings.IndexRune(prefix, '{')
	if braceIdx < 0 {
		return strings.HasPrefix(key, prefix+"{")
	}
	name := prefix[:braceIdx]
	selector := prefix[braceIdx+1:]
	// Key must be `<name>{<labels>}` and the requested selector must
	// appear inside the brace pair. Strict prefix on the name guards
	// against `foo_total` matching `foo_total_bucket`.
	if !strings.HasPrefix(key, name+"{") {
		return false
	}
	end := strings.IndexRune(key, '}')
	if end < 0 {
		return false
	}
	return strings.Contains(key[len(name)+1:end], selector)
}

// verifyLabelsWindow returns the /labels query window for the
// post-seed verification probe. The fixture is deterministic — every
// pushed line lives in [anchor, anchor+24h] — so the probe brackets
// that span with a day of slack on each side and never references the
// wall clock.
//
// The previous implementation used `end = time.Now() + 24h`, which
// made the window's span grow as real time drifted away from the
// fixed anchor. Once the span crossed Loki's default
// `max_query_length` (30d1h — `limits_config` in loki-config.yaml
// doesn't override it), the reference Loki rejected the probe with
// status 400 and the nightly went red (2026-06-08, with zero commits
// on main since 2026-05-23). Any probe window in this harness must be
// anchor-relative, not wall-clock-relative.
func verifyLabelsWindow(anchorTS time.Time) (start, end time.Time) {
	return anchorTS.Add(-24 * time.Hour), anchorTS.Add(48 * time.Hour)
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
