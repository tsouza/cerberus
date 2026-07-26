// Command seed loads the deterministic all-signal fixture into the Tier-1
// migration substrate.
//
// It builds ONE in-memory fixture from an archetype's data declaration and
// writes it twice: directly into ClickHouse (the side cerberus reads) and into
// the reference Prometheus, Loki and Tempo (the side an operator is migrating
// off). Both sides therefore hold the same logical telemetry over the same
// window by construction rather than by luck — which is the only condition
// under which a parity diff means anything.
//
// It does NOT create schema. The collector's clickhouseexporter
// (create_schema: true) is the sole schema authority in the Tier-1 stack; a
// cerberus-side DDL path here would mint cerberus-named tables and mask a real
// drift between the exporter's names and cerberus's read-side defaults. The
// seeder instead emits one OTLP warm-up record per signal to the collector and
// then WAITS for the exporter's tables to appear.
//
// Invocation:
//
//	go run ./test/e2e/migration/cmd/seed \
//	  --fixture  test/e2e/migration/archetypes/three-signal/seed/fixture.json \
//	  --manifest <out>/manifest.json
//
// Endpoints are env-driven, matching the other seeders in the tree:
// CH_ADDR, CH_DATABASE, CH_USERNAME, CH_PASSWORD, OTLP_ADDR, PROM_ADDR,
// LOKI_ADDR, TEMPO_OTLP_ADDR, TEMPO_HTTP_ADDR. Each defaults to the published
// port map in tiers/tier1-dual/docker-compose.dual.yml.
//
// Running the command more than once against the same live stack — once per
// archetype the @tier1 scenario set needs — seeds every archetype into ONE
// compose-stack lifecycle. Each invocation writes its own --manifest, so
// nothing here is overwritten by the next; --window-offset shifts one
// archetype's fixture window into the past relative to another's so two
// archetypes whose declarations happen to share a label value (e.g. a trace
// service name) still occupy disjoint windows, which is what keeps a
// window-scoped reference search (Tempo's per-service /api/search) from
// returning both archetypes' data for what looks like one archetype's query.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Endpoint defaults mirror the tier-1 compose published ports.
const (
	defaultCHAddr        = "127.0.0.1:27000"
	defaultCHDatabase    = "otel"
	defaultCHUsername    = "cerberus"
	defaultCHPassword    = "cerberus"
	defaultOTLPAddr      = "127.0.0.1:27317"
	defaultPromAddr      = "http://127.0.0.1:27090"
	defaultLokiAddr      = "http://127.0.0.1:27100"
	defaultTempoOTLPAddr = "127.0.0.1:27201"
	defaultTempoHTTPAddr = "http://127.0.0.1:27200"
)

// Budgets. Every one is a deadline on a poll loop that FAILS the seed when it
// expires — none of them falls through to "continue anyway".
const (
	// runBudget bounds the whole seed.
	runBudget = 10 * time.Minute
	// schemaWait covers a cold boot where the collector has not yet run its
	// create-schema DDL when the warm-up record arrives.
	schemaWait = 3 * time.Minute
	// referenceReadyWait covers the reference backends' cold start.
	referenceReadyWait = 90 * time.Second
	// settleWait bounds each reference backend's ingest-to-queryable gate.
	settleWait = 90 * time.Second
	// tempoIngestionSlack mirrors reference Tempo's own
	// wal.ingestion_time_range_slack (compatibility/tempo/tempo-config.yaml,
	// restated identically in tiers/tier1-dual for the same live-store
	// module): a span whose timestamp falls outside [now-slack, now+slack] at
	// ingest time is clamped out of its block's metadata, and /api/search then
	// returns nothing for it. --window-offset must leave the whole fixture
	// window inside that slack.
	tempoIngestionSlack = time.Hour
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	var (
		fixturePath  = flag.String("fixture", "", "path to the archetype's seed/fixture.json data declaration")
		manifestPath = flag.String("manifest", "", "path the seeder writes its manifest to")
		chAddr       = flag.String("ch-addr", envOr("CH_ADDR", defaultCHAddr), "ClickHouse host:port")
		chDatabase   = flag.String("ch-database", envOr("CH_DATABASE", defaultCHDatabase), "ClickHouse database")
		chUsername   = flag.String("ch-username", envOr("CH_USERNAME", defaultCHUsername), "ClickHouse username")
		chPassword   = flag.String("ch-password", envOr("CH_PASSWORD", defaultCHPassword), "ClickHouse password")
		otlpAddr     = flag.String("otlp-addr", envOr("OTLP_ADDR", defaultOTLPAddr), "collector OTLP/gRPC host:port")
		promAddr     = flag.String("prom-addr", envOr("PROM_ADDR", defaultPromAddr), "reference Prometheus base URL")
		lokiAddr     = flag.String("loki-addr", envOr("LOKI_ADDR", defaultLokiAddr), "reference Loki base URL")
		tempoOTLP    = flag.String("tempo-otlp-addr", envOr("TEMPO_OTLP_ADDR", defaultTempoOTLPAddr), "reference Tempo OTLP/gRPC host:port")
		tempoHTTP    = flag.String("tempo-http-addr", envOr("TEMPO_HTTP_ADDR", defaultTempoHTTPAddr), "reference Tempo base URL")
		windowOffset = flag.Duration("window-offset", 0, "shift this archetype's fixture window this far into "+
			"the past from now, so a second archetype seeded into the same live stack occupies a disjoint window")
	)
	flag.Parse()

	if *fixturePath == "" {
		return fmt.Errorf("--fixture is required: the seeder builds its dataset from an archetype's data declaration")
	}
	if *manifestPath == "" {
		return fmt.Errorf("--manifest is required: the parity lane reads its query window from the manifest, " +
			"never from a live one")
	}
	if *windowOffset < 0 {
		return fmt.Errorf("--window-offset is %s, want a non-negative duration: it shifts the window into the past, never the future", *windowOffset)
	}
	if *windowOffset+seed.SeedWindow >= tempoIngestionSlack {
		return fmt.Errorf("--window-offset %s pushes the fixture window's start %s before now, at or past "+
			"reference tempo's %s ingestion slack; spans that far back would be clamped out of their block "+
			"and never become searchable", *windowOffset, *windowOffset+seed.SeedWindow, tempoIngestionSlack)
	}

	ctx, cancel := context.WithTimeout(context.Background(), runBudget)
	defer cancel()

	declaration, err := seed.LoadDeclaration(*fixturePath)
	if err != nil {
		return err
	}

	conn, err := seed.DialCH(ctx, seed.CHConfig{
		Addr:     *chAddr,
		Database: *chDatabase,
		Username: *chUsername,
		Password: *chPassword,
	})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	// 1. Provoke the schema, then prove it exists. The warm-up record makes
	//    table creation unconditional; WaitForTables is the assertion.
	logger.Info("emitting the schema warm-up record", "otlp_addr", *otlpAddr)
	if err := seed.WarmUpSchema(ctx, *otlpAddr, time.Now().UTC()); err != nil {
		return err
	}
	logger.Info("waiting for the collector to provision the OTel schema", "database", *chDatabase)
	if err := seed.WaitForTables(ctx, conn, *chDatabase, seed.ExporterTables, schemaWait); err != nil {
		return err
	}

	// 2. Build the ONE fixture both sides are written from. --window-offset
	//    shifts the anchor into the past so a second archetype seeded into
	//    the same live stack lands in a window disjoint from the first's —
	//    see the package doc comment.
	window := seed.NewWindow(time.Now().Add(-*windowOffset))
	fixture, err := seed.Build(declaration, window)
	if err != nil {
		return err
	}
	manifest := seed.NewManifest(declaration, fixture)
	logger.Info(
		"built fixture",
		"seed_start", manifest.SeedStart,
		"verify_start", manifest.VerifyStart,
		"verify_end", manifest.VerifyEnd,
		"step", manifest.Step,
		"streams", manifest.Streams,
		"log_records", manifest.LogRecords,
		"prom_series", manifest.PromSeries,
		"traces", manifest.Traces,
		"spans", manifest.Spans,
	)

	// 3. Wait for every reference backend before touching either side, so a
	//    half-written reference is never left behind by a late boot.
	for _, ref := range []struct{ name, url string }{
		{"prometheus", *promAddr + "/-/ready"},
		{"loki", *lokiAddr + "/ready"},
		{"tempo", *tempoHTTP + "/ready"},
	} {
		logger.Info("waiting for reference backend", "backend", ref.name, "url", ref.url)
		if err := seed.WaitHTTPOK(ctx, ref.url, referenceReadyWait); err != nil {
			return err
		}
	}

	// 4. Anchor the Loki settle gate BEFORE the push, so it waits for deltas
	//    this run produced rather than for an absolute threshold a previous
	//    run already satisfied.
	lokiBaseline, err := seed.ReadLokiCounters(ctx, *lokiAddr)
	if err != nil {
		return err
	}

	// 5. Write the ClickHouse side.
	logger.Info("writing the fixture into clickhouse", "addr", *chAddr, "database", *chDatabase)
	if err := seed.InsertFixture(ctx, conn, fixture); err != nil {
		return err
	}

	// 6. Write the reference side — the same fixture, re-shaped into each
	//    backend's wire vocabulary and nothing more.
	//    The incumbent's own pre-ingest history goes FIRST, and the order is
	//    load-bearing twice over. Its samples are older than every fixture
	//    sample on the same series, and reference Prometheus rejects an
	//    out-of-order sample outright (its out-of-order window is zero by
	//    default), so writing it after the fixture would fail the seed. And it
	//    is written HERE and nowhere else: ClickHouse's earliest row has to stay
	//    at SeedStart for the ingest-start boundary to exist at all (see
	//    seed.PreIngestWindow).
	logger.Info("remote-writing the incumbent's pre-ingest history into reference prometheus",
		"url", *promAddr, "from", manifest.PreIngestStart, "series", manifest.PreIngestSeries,
		"samples_per_series", manifest.PreIngestSamplesPerSeries)
	if err := seed.WriteSeries(ctx, *promAddr, fixture.PromPreIngestSeries()); err != nil {
		return err
	}
	logger.Info("remote-writing the fixture into reference prometheus", "url", *promAddr)
	if err := seed.WriteSeries(ctx, *promAddr, fixture.PromSeries()); err != nil {
		return err
	}
	logger.Info("pushing the fixture into reference loki", "url", *lokiAddr)
	if err := seed.PushStreams(ctx, *lokiAddr, fixture.LokiStreams()); err != nil {
		return err
	}
	logger.Info("pushing the fixture into reference tempo", "addr", *tempoOTLP)
	if err := seed.PushTraces(ctx, *tempoOTLP, fixture.Traces); err != nil {
		return err
	}

	// 7. Close the ingestion skew on the reference side. cerberus reads
	//    ClickHouse directly, so its visibility is already bounded by the
	//    INSERT round-trip and needs no gate.
	logger.Info("flushing the reference loki ingester", "url", *lokiAddr)
	if err := seed.FlushLoki(ctx, *lokiAddr); err != nil {
		return err
	}
	logger.Info("waiting for the reference loki index to settle", "streams", manifest.Streams)
	if err := seed.WaitLokiSettled(ctx, *lokiAddr, manifest.Streams, lokiBaseline, settleWait); err != nil {
		return err
	}
	logger.Info("waiting for reference tempo to hold the whole fixture",
		"url", *tempoHTTP, "traces", manifest.Traces)
	if err := seed.WaitTempoSettled(ctx, *tempoHTTP, fixture, settleWait); err != nil {
		return err
	}
	logger.Info("waiting for reference prometheus to hold the whole fixture",
		"metric", manifest.CounterMetric, "series", manifest.Series.Sum)
	if err := seed.WaitPrometheusSettled(
		ctx, *promAddr, manifest.CounterMetric, manifest.Series.Sum, manifest.AnchorEnd, settleWait,
	); err != nil {
		return err
	}

	// 8. Publish the manifest last: it is only true once every gate above has
	//    passed.
	if err := seed.WriteManifest(*manifestPath, manifest); err != nil {
		return err
	}
	logger.Info("seed done", "manifest", *manifestPath)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
