package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// requestIDSeed seeds the PRNG generateRequestID draws from — a value
// distinct from seedValue so the two draw streams stay independent (see
// buildStreams).
const requestIDSeed = int64(1498)

// requestIDAbsenceEvery gates generateRequestID: one entry in every
// requestIDAbsenceEvery carries NO request_id structured-metadata key
// at all, the rest carry a distinct one. detected_level is present on
// every row (it's a synthesized severity discriminant); request_id
// models the far more common OTel/Loki shape — a generic, arbitrary
// correlation id that's only sometimes attached — so leaving a
// deterministic gap keeps the "absent on some entries" half of issue
// #1498's fix honest rather than accidentally seeding it as universal.
const requestIDAbsenceEvery = 3

// packedFormat identifies packed-source's stream format: entries cycle
// through the four payload shapes `| unpack` must discriminate (see
// generatePackedLine). It is not one of the three formats
// LogFormat/QueryRequirements.log_format recognises (json/logfmt/
// unstructured) because none of them describe a Promtail-pack payload.
const packedFormat = "packed"

// metadataProbeFormat identifies metadata-probe's stream format, the
// ONLY seeded stream that carries the generic `request_id`
// structured-metadata key (see generateRequestID / issue #1498). Like
// packedFormat, it is deliberately NOT one of the three formats
// LogFormat/QueryRequirements.log_format recognises (json/logfmt/
// unstructured): the vendored corpus's `${SELECTOR}` resolver picks a
// stream at random from whichever ones satisfy a case's `requires:`
// bounds, and reference Loki folds structured-metadata values directly
// into a log/metric query's output identity (stream labels / ungrouped
// series `metric` map) — so a resolver-selected stream that
// unexpectedly carries a SECOND, high-cardinality structured-metadata
// key would silently reshape the output of every EXISTING corpus case
// that happens to land on it, with no relation to what that case
// actually tests. Keeping request_id off the log_format enum blocks
// the resolver from ever picking metadata-probe; only this file's own
// LITERAL-selector cerberus-queries corpus (which names the stream
// explicitly) ever queries it.
const metadataProbeFormat = "metadata-probe"

// The two synthetic cluster partitions the seeded services split across,
// and the container id each partition's services run under.
const (
	clusterZero   = "cluster-0"
	clusterOne    = "cluster-1"
	containerZero = "container-0"
	containerOne  = "container-1"
)

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
	{Name: "web-server", ServiceName: "web-server", Format: "json", Cluster: clusterZero, Namespace: "namespace-0", Pod: "pod-0", Container: containerZero},
	{Name: "database", ServiceName: "database", Format: "json", Cluster: clusterZero, Namespace: "namespace-0", Pod: "pod-1", Container: containerZero},
	{Name: "cache", ServiceName: "cache", Format: "json", Cluster: clusterZero, Namespace: "namespace-1", Pod: "pod-2", Container: containerZero},
	{Name: "auth-service", ServiceName: "auth-service", Format: "json", Cluster: clusterZero, Namespace: "namespace-1", Pod: "pod-3", Container: containerZero},
	{Name: "kafka", ServiceName: "kafka", Format: "json", Cluster: clusterOne, Namespace: "namespace-2", Pod: "pod-4", Container: containerOne},
	{Name: "prometheus", ServiceName: "prometheus", Format: "json", Cluster: clusterOne, Namespace: "namespace-2", Pod: "pod-5", Container: containerOne},
	{Name: "loki", ServiceName: "loki", Format: "logfmt", Cluster: clusterOne, Namespace: "namespace-3", Pod: "pod-6", Container: containerOne},
	{Name: "mimir", ServiceName: "mimir", Format: "logfmt", Cluster: clusterOne, Namespace: "namespace-3", Pod: "pod-7", Container: containerOne},
	{Name: "tempo", ServiceName: "tempo", Format: "logfmt", Cluster: clusterOne, Namespace: "namespace-4", Pod: "pod-8", Container: containerOne},
	{Name: "grafana", ServiceName: "grafana", Format: "logfmt", Cluster: clusterOne, Namespace: "namespace-4", Pod: "pod-9", Container: containerOne},
	{Name: "nginx", ServiceName: "nginx", Format: "unstructured", Cluster: clusterZero, Namespace: "namespace-0", Pod: "pod-10", Container: containerZero},
	{Name: "kubernetes", ServiceName: "kubernetes", Format: "unstructured", Cluster: clusterZero, Namespace: "namespace-1", Pod: "pod-11", Container: containerZero},
	{Name: "syslog", ServiceName: "syslog", Format: "unstructured", Cluster: clusterOne, Namespace: "namespace-4", Pod: "pod-12", Container: containerOne},
	// packed-source is the ONLY seeded stream carrying Promtail
	// `pack`-shaped payloads (a JSON object with a top-level string
	// "_entry" key) — see generatePackedLine. It exists so the LogQL
	// `| unpack` compatibility corpus (compatibility/loki/cerberus-queries/)
	// has real packed/malformed lines to run differentially against;
	// every other service's JSON lines are plain structured logs that
	// `| unpack` would either no-op on or reject, never extract from.
	{Name: "packed-source", ServiceName: "packed-source", Format: packedFormat, Cluster: clusterOne, Namespace: "namespace-5", Pod: "pod-13", Container: containerOne},
	// metadata-probe is the ONLY seeded stream carrying the generic
	// `request_id` structured-metadata key — see metadataProbeFormat's
	// doc comment for why request_id can't safely ride along on any of
	// the streams above.
	{Name: "metadata-probe", ServiceName: "metadata-probe", Format: metadataProbeFormat, Cluster: clusterOne, Namespace: "namespace-6", Pod: "pod-14", Container: containerOne},
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

	// packedCategories is the small, fixed value set generatePackedLine
	// draws "category" from for its valid-packed payload class. Kept
	// small and fixed so the unpack-then-aggregate compat query
	// (compatibility/loki/cerberus-queries/regression/unpack.yaml)
	// groups into a deterministic, bounded set of series.
	packedCategories = []string{"auth", "billing", "search", "ingest"}
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

// The seeder runs in two stages around a restart of the reference Loki.
// .github/scripts/run-loki-compatibility.mjs drives the sequence:
//
//  1. `-stage=corpus` writes the deterministic corpus to ClickHouse and
//     to the reference Loki, forces the ingester to flush, and waits
//     until every pushed chunk has left ingester memory
//     (waitLokiFlushDrained).
//  2. `docker compose restart loki`. The graceful stop builds the
//     ingester's TSDB head into an index file and uploads it to the
//     object store; the restarted process comes up with an empty WAL,
//     head and local index directory (all three live on a tmpfs — see
//     loki-config.yaml and docker-compose.yml), so it reads the corpus
//     index from the object store and from nowhere else.
//  3. `-stage=live` writes the now-anchored /patterns fixture to both
//     targets, checks both answer /labels over the corpus window, and
//     writes the fixture handshake for the diff driver.
//
// The restart is what makes the reference's /index/stats exact.
// Single-binary Loki answers index stats by summing per-chunk statistics
// over every index it holds — the active TSDB head, the head rotated out
// at the last 15-minute wall-clock boundary (kept to serve queries until
// queriers download its built file), the built file in the shipper's
// local upload set (kept for IndexCacheValidity + 3×resync_interval
// after upload) and the same file downloaded back from the object store
// — and deduplicates only streams, never chunks
// (pkg/storage/stores/index/stats.Blooms). A corpus flushed into the head
// is therefore counted once until the next rotation and two or three
// times after it, so `entries` depended on whether the diff driver ran
// before or after the boundary. Served from the object store alone, each
// chunk is in exactly one index and the count is the data's.
const (
	stageCorpus = "corpus"
	stageLive   = "live"
)

func run(logger *slog.Logger) error {
	// `-timeout` worst-case budget, per stage:
	//   corpus: waitCHReady (30s) + waitLokiReady (60s) + insertCHLogs /
	//           pushLoki + flushLoki + waitLokiFlushDrained (90s)
	//   live:   waitCHReady (30s) + waitLokiReady (60s) + the fixture
	//           writes + waitLabelsNonEmpty × 2 (60s) + the /patterns
	//           probes (2 × livePatternsProbeTimeout)
	// 4 minutes leaves headroom for slow CI runners in either stage.
	var (
		stage    = flag.String("stage", "", "seed stage: "+stageCorpus+" (before the reference Loki restart) or "+stageLive+" (after it)")
		addr     = flag.String("addr", envOr("CERBERUS_CH_ADDR", "localhost:28000"), "ClickHouse host:port")
		database = flag.String("database", envOr("CERBERUS_CH_DATABASE", "otel"), "ClickHouse database")
		username = flag.String("user", envOr("CERBERUS_CH_USERNAME", "cerberus"), "ClickHouse username")
		password = flag.String("password", envOr("CERBERUS_CH_PASSWORD", "cerberus"), "ClickHouse password")
		lokiURL  = flag.String("loki-url", envOr("LOKI_URL", "http://localhost:23100"), "Reference Loki base URL")
		cerbURL  = flag.String("cerberus-url", envOr("CERBERUS_URL", "http://localhost:29092"), "cerberus LogQL base URL")
		livePath = flag.String("live-patterns-metadata", envOr("LIVE_PATTERNS_METADATA", ""), "Path for the now-anchored /patterns fixture handshake (required by -stage="+stageLive+")")
		timeout  = flag.Duration("timeout", 4*time.Minute, "overall dial + push + verify timeout")
	)
	flag.Parse()
	switch *stage {
	case stageCorpus:
	case stageLive:
		if *livePath == "" {
			return errors.New("-live-patterns-metadata is required by -stage=" + stageLive)
		}
	default:
		return fmt.Errorf("-stage must be %q or %q, got %q", stageCorpus, stageLive, *stage)
	}

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

	if *stage == stageCorpus {
		return seedCorpus(ctx, conn, *database, *lokiURL, logger)
	}
	return seedLive(ctx, conn, *lokiURL, *cerbURL, *livePath, logger)
}

// seedCorpus is stage 1: the deterministic corpus, written to both
// targets and flushed out of the reference ingester's memory.
func seedCorpus(ctx context.Context, conn driver.Conn, database, lokiURL string, logger *slog.Logger) error {
	logger.Info("applying ddl", "signal", "logs")
	cfg := ddl.Config{Database: database}
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
		"generated corpus",
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

	logger.Info("waiting for loki readiness", "url", lokiURL)
	if err := waitLokiReady(ctx, lokiURL, logger); err != nil {
		return fmt.Errorf("loki not ready: %w", err)
	}

	// Snapshot the ingester's flushed-chunk counter *before* pushing.
	// The drain gate downstream waits for it to move past the baseline
	// by at least one chunk per pushed stream. A pre-push baseline is
	// robust against the harness running against a Loki instance that
	// has handled traffic earlier in the same process lifetime.
	baseline, err := readLokiMetricsBaseline(ctx, lokiURL)
	if err != nil {
		return fmt.Errorf("read loki metrics baseline: %w", err)
	}
	logger.Info("loki metrics baseline captured", "chunks_flushed_total", baseline.chunksFlushed)

	logger.Info("pushing into loki", "url", lokiURL)
	if err := pushLoki(ctx, lokiURL, streams); err != nil {
		return fmt.Errorf("push loki: %w", err)
	}

	logger.Info("flushing loki ingester", "url", lokiURL)
	if err := flushLoki(ctx, lokiURL); err != nil {
		return fmt.Errorf("flush loki: %w", err)
	}
	if err := waitLokiFlushDrained(ctx, lokiURL, len(streams), baseline, logger); err != nil {
		return err
	}

	logger.Info("corpus seeded", "streams", len(streams), "total_entries", totalEntries)
	return nil
}

// seedLive is stage 3, run against the restarted reference Loki: the
// now-anchored /patterns fixture, written to both targets, then the
// both-targets visibility checks the diff driver relies on.
func seedLive(ctx context.Context, conn driver.Conn, lokiURL, cerbURL, livePath string, logger *slog.Logger) error {
	logger.Info("waiting for loki readiness", "url", lokiURL)
	if err := waitLokiReady(ctx, lokiURL, logger); err != nil {
		return fmt.Errorf("loki not ready: %w", err)
	}

	fixture := buildLivePatternsFixture(time.Now().UTC())
	fixtureStreams := []stream{fixture.stream}
	logger.Info("generated live /patterns fixture", "entries", len(fixture.stream.entries), "selector", fixture.metadata.Selector)

	logger.Info("inserting live fixture into clickhouse otel_logs")
	if err := insertCHLogs(ctx, conn, fixtureStreams); err != nil {
		return fmt.Errorf("insert clickhouse: %w", err)
	}
	logger.Info("pushing live fixture into loki", "url", lokiURL)
	if err := pushLoki(ctx, lokiURL, fixtureStreams); err != nil {
		return fmt.Errorf("push loki: %w", err)
	}

	logger.Info("verifying /labels is non-empty on both targets")
	if err := verifyBothNonEmpty(ctx, conn, lokiURL, cerbURL, logger); err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	logger.Info("waiting for live /patterns fixture on both targets", "selector", fixture.metadata.Selector)
	if err := waitLivePatternsBoth(ctx, lokiURL, cerbURL, fixture.metadata, logger); err != nil {
		return fmt.Errorf("verify live patterns: %w", err)
	}
	if err := writeLivePatternsMetadata(livePath, fixture.metadata); err != nil {
		return fmt.Errorf("write live patterns metadata: %w", err)
	}

	logger.Info("seed done", "live_patterns_metadata", livePath)
	return nil
}

type stream struct {
	config  serviceConfig
	labels  map[string]string
	entries []entry
}

type entry struct {
	ts        time.Time
	level     string
	line      string
	requestID string // "" means absent — see generateRequestID.
}

func buildStreams(start time.Time) []stream {
	rng := rand.New(rand.NewSource(seedValue)) //nolint:gosec
	// requestIDRng is deliberately a SEPARATE PRNG stream from rng, seeded
	// with its own constant, rather than drawing request_id values from
	// rng inline alongside level / line-content generation. Every other
	// generator below (generateJSONLine, generateLogfmtLine, ...) draws
	// its random content from `rng` in a fixed, load-bearing sequence
	// that OTHER corpus cases pin exact values against (client_ip CIDR
	// membership, error-message selection, ...); interleaving even one
	// extra conditional rng.Uint32() draw per entry would shift every
	// later draw in the shared sequence and silently reshape unrelated
	// seeded content. See issue #1498.
	requestIDRng := rand.New(rand.NewSource(requestIDSeed)) //nolint:gosec
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

		for i := range entriesPerService {
			ts := start.Add(time.Duration(i) * entryInterval)
			level := levels[rng.Intn(len(levels))]
			// request_id is scoped to metadata-probe ONLY — see
			// metadataProbeFormat's doc comment for why it can't ride
			// along on any of the other seeded streams.
			var requestID string
			if sc.Format == metadataProbeFormat {
				requestID = generateRequestID(requestIDRng, i)
			}
			var line string
			switch sc.Format {
			case "json":
				line = generateJSONLine(sc.Name, level, ts, rng, i)
			case "logfmt":
				line = generateLogfmtLine(sc.Name, level, ts, rng, i)
			case packedFormat:
				line = generatePackedLine(level, i)
			case metadataProbeFormat:
				// Reuses generateJSONLine's own default arm (svc name
				// "metadata-probe" matches none of its named cases) —
				// realistic JSON content, no bespoke generator needed.
				line = generateJSONLine(sc.Name, level, ts, rng, i)
			default:
				line = generateUnstructuredLine(sc.Name, level, ts, rng, i)
			}
			s.entries = append(s.entries, entry{ts: ts, level: level, line: line, requestID: requestID})
		}
		out = append(out, s)
	}
	return out
}

// generateRequestID returns a generic structured-metadata value for the
// entry at index idx — a synthetic per-record correlation id, the
// unconstrained-value-domain twin of the small, closed-set
// `detected_level` — or "" when this entry should carry no request_id
// key at all (see requestIDAbsenceEvery). Unlike detected_level
// (derived from severity), the value here is unbounded and
// content-free: it exists purely to exercise generic structured
// metadata — selectors, `| label_filter`, and metric-query `by(...)`
// grouping over an arbitrary key — the gap issue #1498 identified
// (compatibility/loki/cmd/seed/main.go previously stamped
// detected_level only, so every non-detected_level structured-metadata
// path went undiffed against reference Loki).
func generateRequestID(rng *rand.Rand, idx int) string {
	if idx%requestIDAbsenceEvery == 0 {
		return ""
	}
	return fmt.Sprintf("req-%08x", rng.Uint32())
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

// packedPayloadShapeCount is the number of distinct payload shapes
// generatePackedLine cycles through, matching the four cases `|
// unpack` must discriminate per internal/logql/lower.go's
// unpackMergeLabels: a well-formed packed object, a well-formed
// object with no "_entry" key, a non-object JSON value, and malformed
// JSON.
const packedPayloadShapeCount = 4

const (
	packedShapeValid = iota
	packedShapeNoEntry
	packedShapeNonObject
	packedShapeMalformed
)

// generatePackedLine cycles a seeded stream's entries through the four
// payload shapes `| unpack` must discriminate between (see
// internal/logql/lower.go's unpackMergeLabels):
//
//   - packedShapeValid: a well-formed Promtail-pack object — a
//     top-level string "_entry" key (the line `| unpack` replaces the
//     original line with) plus extra string-valued keys that become
//     labels. "category" is drawn from the small fixed
//     packedCategories set so the unpack-then-aggregate compat query
//     groups deterministically.
//   - packedShapeNoEntry: a well-formed JSON object with NO "_entry"
//     key. Loki's UnpackParser (and cerberus's SQL-side extraction)
//     treat this as a complete no-op: original line, no extracted
//     labels, no error.
//   - packedShapeNonObject: valid JSON that is not an object (a JSON
//     array here). The `_entry`-gate rejects anything not starting
//     with '{' as `__error__="JSONParserErr"`.
//   - packedShapeMalformed: a truncated/malformed JSON object — starts
//     with '{' but never parses, also stamping
//     `__error__="JSONParserErr"`.
func generatePackedLine(level string, idx int) string {
	switch idx % packedPayloadShapeCount {
	case packedShapeValid:
		category := packedCategories[idx%len(packedCategories)]
		return fmt.Sprintf(`{"_entry":"packed log entry %d","category":"%s","priority":"%s"}`, idx, category, strings.ToLower(level))
	case packedShapeNoEntry:
		return fmt.Sprintf(`{"level":"%s","msg":"structured but not packed"}`, strings.ToLower(level))
	case packedShapeNonObject:
		return fmt.Sprintf(`["not","an","object",%d]`, idx)
	default: // packedShapeMalformed
		return fmt.Sprintf(`{"_entry":"truncated payload %d`, idx)
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
			// mirror what pushLoki sends as structured metadata: the
			// /detected_fields differential compares the two backends'
			// structured-metadata-derived fields, so any CH-only key
			// here would surface as a permanent parity diff. Stream
			// labels live in resourceAttrs above, not here.
			//
			// Two keys are stamped: detected_level (synthesized,
			// present on every row, small closed value set) and
			// request_id (generic, unconstrained value domain,
			// non-empty only on metadata-probe's rows per
			// generateRequestID / e.requestID's scoping — see
			// metadataProbeFormat) — see issue #1498, which found the
			// seeder previously carried detected_level only, leaving
			// every generic structured-metadata path undiffed against
			// reference Loki.
			logAttrs := map[string]string{
				"detected_level": strings.ToLower(level),
			}
			if e.requestID != "" {
				logAttrs["request_id"] = e.requestID
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
				sm = append(sm, logproto.LabelAdapter{Name: "detected_level", Value: lvl})
			}
			// request_id is the generic-structured-metadata twin of
			// detected_level (see generateRequestID / issue #1498):
			// unconstrained value domain, absent on some entries. Must
			// stay in lockstep with insertCHLogs' logAttrs so the
			// /detected_fields differential compares like-for-like.
			if e.requestID != "" {
				sm = append(sm, logproto.LabelAdapter{Name: "request_id", Value: e.requestID})
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
// HTTP listener as `/loki/api/v1/*`). The handler enqueues every
// in-memory chunk for an immediate flush to the configured object store
// (filesystem in the harness) and returns 204 No Content; the flush
// workers then write the chunks and record them in the TSDB head.
// waitLokiFlushDrained is what waits for that work to finish.
//
// Without this call, the seed's hard-coded `2026-05-11T00:00:00Z`
// anchor ages past the default `chunk_idle_period` (30m) +
// `max_chunk_age` (2h) thresholds the ingester uses to decide when to
// move a chunk to the store, and the chunks would linger in memory. The
// ingester does not flush on shutdown either (its WAL is what normally
// carries unflushed chunks across a restart, and the harness's WAL is on
// a tmpfs), so a chunk still in memory when the corpus stage hands over
// to the reference Loki restart would be lost.
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

// verifyBothNonEmpty checks that both targets answer the corpus: the
// ClickHouse table has rows, and `/labels` over the corpus window is
// non-empty on the reference Loki and on cerberus. Run after the
// reference restart, the Loki half is positive evidence that the
// restarted process reads the corpus index from the object store.
func verifyBothNonEmpty(ctx context.Context, conn driver.Conn, lokiURL, cerbURL string, logger *slog.Logger) error {
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

// lokiMetricsSnapshot is a baseline reading of the ingester counter the
// drain gate watches: the chunks the ingester has flushed. The gate
// waits for it to move past the baseline by the delta implied by the
// just-pushed batch.
type lokiMetricsSnapshot struct {
	chunksFlushed float64
}

// readLokiMetricsBaseline pulls the current value of the counter the
// drain gate keys on. Used to anchor the post-flush delta so the gate
// works whether the reference Loki is freshly booted or being re-used
// across seed runs.
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
		chunksFlushed: sumMatching(metrics, "loki_ingester_chunks_flushed_total"),
	}
}

// Drain-gate cadence. Declared as `var` (not `const`) so unit tests
// can shrink the budget — production keeps the 90s/500ms/5s shape the
// function doc-comment pins. Do not mutate at runtime outside of tests.
var (
	drainTimeout    = 90 * time.Second
	drainInterval   = 500 * time.Millisecond
	drainProgressAt = 5 * time.Second
)

// waitLokiFlushDrained polls the reference Loki's `/metrics` endpoint
// until the ingester signals that the just-pushed batch has been
// flushed out of memory into the object store.
//
// Drain criteria — all three must hold concurrently in a single poll
// (no latches, no thresholds, no straggler tolerance):
//
//   - `loki_ingester_flush_queue_length == 0` — the ingester's flush
//     queue has drained.
//   - `loki_ingester_memory_chunks == 0` — no chunks remain held in
//     memory. Together with the queue-length check this rules out both
//     "queued but not started" and "started but not finished" flushes.
//   - `loki_ingester_chunks_flushed_total` has incremented by at least
//     `expectedStreams` since the pre-push baseline. Every pushed
//     stream produces at least one chunk; the counter going up by
//     ≥expectedStreams is positive evidence that *our* push made it
//     through the flush path (rather than a stale baseline reading
//     happening to satisfy the queue/memory checks).
//
// The gate deliberately says nothing about the index. The flushed
// chunks are recorded in the ingester's TSDB head, and the index the
// diff driver reads is built from that head and uploaded by the
// graceful stop of the reference restart that follows this stage (see
// stageCorpus). verifyBothNonEmpty, in the live stage, is the check
// that the restarted process serves it.
//
// On deadline expiry the error carries every counter's current value
// alongside the baseline so the failure mode is recoverable from a
// single log line. A missing metric (e.g. an upstream Loki rename) is
// reported as a structured error rather than silently treated as zero.
func waitLokiFlushDrained(ctx context.Context, baseURL string, expectedStreams int, baseline lokiMetricsSnapshot, logger *slog.Logger) error {
	logger.Info(
		"waiting for the reference loki ingester to drain",
		"url", baseURL,
		"expected_streams", expectedStreams,
		"baseline_chunks_flushed", baseline.chunksFlushed,
		"timeout", drainTimeout,
	)

	begin := time.Now()
	deadline := begin.Add(drainTimeout)
	lastProgress := begin
	var lastSnapshot lokiDrainSnapshot
	var lastErr error

	for {
		snap, err := fetchLokiDrainSnapshot(ctx, baseURL)
		if err != nil {
			lastErr = err
		} else {
			lastErr = nil
			lastSnapshot = snap

			flushedDelta := snap.chunksFlushed - baseline.chunksFlushed

			if snap.flushQueueLength == 0 &&
				snap.memoryChunks == 0 &&
				flushedDelta >= float64(expectedStreams) {
				logger.Info(
					"loki ingester drained",
					"flush_queue_length", snap.flushQueueLength,
					"memory_chunks", snap.memoryChunks,
					"chunks_flushed_delta", flushedDelta,
					"chunks_flushed_needed", expectedStreams,
					"elapsed", time.Since(begin).Round(time.Millisecond),
				)
				return nil
			}
		}

		if time.Since(lastProgress) >= drainProgressAt {
			logger.Info(
				"still waiting for the loki ingester to drain",
				"flush_queue_length", lastSnapshot.flushQueueLength,
				"memory_chunks", lastSnapshot.memoryChunks,
				"chunks_flushed_delta", lastSnapshot.chunksFlushed-baseline.chunksFlushed,
				"chunks_flushed_needed", expectedStreams,
				"last_err", lastErr,
				"remaining", time.Until(deadline).Round(time.Second),
			)
			lastProgress = time.Now()
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("loki ingester drain: last error: %w (after %s, baseline_chunks_flushed=%v)",
					lastErr, drainTimeout, baseline.chunksFlushed)
			}
			return fmt.Errorf(
				"loki ingester drain: timed out after %s (flush_queue_length=%v memory_chunks=%v chunks_flushed=%v→%v needed_delta=%d)",
				drainTimeout,
				lastSnapshot.flushQueueLength,
				lastSnapshot.memoryChunks,
				baseline.chunksFlushed, lastSnapshot.chunksFlushed,
				expectedStreams,
			)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(drainInterval):
		}
	}
}

// lokiDrainSnapshot is the per-poll reading the drain loop diffs
// against the baseline. Kept as plain float64 because Prometheus text
// exposition itself uses float64 for every numeric value (including
// counters).
type lokiDrainSnapshot struct {
	flushQueueLength float64
	memoryChunks     float64
	chunksFlushed    float64
}

// fetchLokiDrainSnapshot pulls `/metrics` and resolves the three
// drain-gate signals. The required-metrics check is strict: a missing
// metric is returned as an error rather than silently coerced to zero,
// so an upstream Loki rename surfaces immediately instead of looking
// like an instantly-drained gate.
func fetchLokiDrainSnapshot(ctx context.Context, baseURL string) (lokiDrainSnapshot, error) {
	metrics, err := fetchLokiMetrics(ctx, baseURL)
	if err != nil {
		return lokiDrainSnapshot{}, err
	}
	required := []string{
		"loki_ingester_flush_queue_length",
		"loki_ingester_memory_chunks",
		"loki_ingester_chunks_flushed_total",
	}
	for _, name := range required {
		if !hasMatching(metrics, name) {
			return lokiDrainSnapshot{}, fmt.Errorf("loki /metrics missing required gauge/counter %q (upstream rename? regenerate gate against grafana/loki version in use)", name)
		}
	}
	return lokiDrainSnapshot{
		flushQueueLength: sumMatching(metrics, "loki_ingester_flush_queue_length"),
		memoryChunks:     sumMatching(metrics, "loki_ingester_memory_chunks"),
		chunksFlushed:    sumMatching(metrics, "loki_ingester_chunks_flushed_total"),
	}, nil
}

// fetchLokiMetrics fetches the raw Prometheus text-format `/metrics`
// payload and parses every sample line into a name→value map. The
// parser is intentionally line-oriented (no full Prometheus expfmt
// dependency): the drain gate only needs to look up a handful of
// metric families by name.
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
	for raw := range strings.SplitSeq(string(body), "\n") {
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
// semantic for the drain gate.
func parseFloatOrNaN(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// hasMatching reports whether `metrics` contains either the exact key
// `name` (an unlabelled scalar) or any key of the shape `name{...}` (a
// labelled metric family). Used to detect the presence of a metric
// without caring about its label set.
func hasMatching(metrics map[string]float64, name string) bool {
	for k := range metrics {
		if matchesFamily(k, name) {
			return true
		}
	}
	return false
}

// sumMatching collapses a metric family to a single scalar by summing
// every sample of `name`: the unlabelled scalar or every `name{...}`
// row of the labelled family.
func sumMatching(metrics map[string]float64, name string) float64 {
	var total float64
	for k, v := range metrics {
		if matchesFamily(k, name) {
			total += v
		}
	}
	return total
}

// matchesFamily is the rule shared by hasMatching and sumMatching: the
// key is the unlabelled metric `name` itself or a `name{...}` sample of
// its labelled family. The brace keeps `foo_total` from matching
// `foo_total_bucket`.
func matchesFamily(key, name string) bool {
	return key == name || strings.HasPrefix(key, name+"{")
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
