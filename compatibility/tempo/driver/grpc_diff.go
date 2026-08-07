// gRPC/h2c StreamingQuerier transport for the Tempo / TraceQL
// compatibility differ (#1453).
//
// compatibility/tempo/driver/diff.go drives the corpus over HTTP only —
// cerberus's gRPC/h2c StreamingQuerier listener (internal/api/tempo/grpc,
// e2e-covered by #1665 / test/e2e/playwright/tempo_grpc_streaming.spec.ts)
// had zero differential-correctness coverage against reference Tempo,
// even though it is the transport Grafana's Tempo datasource actually
// opens when "Streaming" is enabled. This file adds the gRPC arm: the
// SAME TXTAR corpus, run through cerberus's and reference Tempo's
// tempopb.StreamingQuerierClient instead of net/http, diffed with the
// UNCHANGED comparator in differ.go / differ_metrics.go.
//
// Wire-contract findings that shaped this file (see the PR description
// for the full trace):
//
//   - Tempo's own StreamingQuerierServer is registered on the
//     query-frontend module (grafana/tempo cmd/tempo/app/modules.go,
//     initQueryFrontend) on Server.GRPC() — a genuine second TCP
//     listener (dskit server, default :9095), NOT multiplexed onto the
//     HTTP port the way cerberus's h2c dual-stack listener is. The
//     query-frontend module is part of Tempo's SingleBinary module set,
//     so the reference Tempo image this harness already runs DOES serve
//     StreamingQuerier — docker-compose.yml publishes that port
//     alongside the existing HTTP + OTLP ports.
//   - With multitenancy disabled (this harness's tempo-config.yaml sets
//     no `multitenancy_enabled`), Tempo installs a fake-auth gRPC stream
//     interceptor that injects a tenant ID automatically
//     (cmd/tempo/app/fake_auth.go) — no per-request auth metadata is
//     needed, matching the HTTP side's equally-auth-free posture.
//   - tempopb.StreamingQuerierService exposes exactly 7 RPCs: Search,
//     SearchTags[V2], SearchTagValues[V2], MetricsQueryRange,
//     MetricsQueryInstant. There is no trace-by-id RPC and no
//     search/recent RPC — TraceByID is HTTP/proto-only on both
//     reference Tempo and cerberus. The `traces` / `traces_v2` corpus
//     cases are therefore genuinely inapplicable to this transport (not
//     narrowed for convenience); grpcSupportsEndpoint documents and
//     enforces the boundary, and the report lists what was skipped and
//     why instead of silently dropping cases.
//   - Search streams multiple TraceSearchMetadata frames (searchFrameSize
//     = 20 on cerberus's side); the tag / tag-values / metrics RPCs are
//     documented single-frame streams on cerberus and, per Tempo's own
//     "we register the streaming querier service" wiring, behave the
//     same on the reference side. Every fetch helper below therefore
//     drains its stream to EOF and accumulates before converting —
//     response accumulation IS required (the streaming envelope is not
//     wire-identical to the HTTP body until flattened), but once
//     accumulated + converted to the differ's JSON-shaped structs
//     (grpc_convert.go), the comparator itself needs no changes at all.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"sort"
	"time"

	"github.com/grafana/tempo/pkg/tempopb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tsouza/cerberus/compatibility/internal/score"
)

// grpcReportTitle / grpcStatusLabel parameterise the shared
// writeReport/renderReport (diff.go) for the gRPC transport's report.
const (
	grpcReportTitle = "Tempo / TraceQL compatibility — gRPC/h2c StreamingQuerier diff report"
	grpcStatusLabel = "gRPC status"
)

// grpcHeadName is the parity-ratchet head identity for this transport —
// distinct from diff.go's "tempo" so the two transports gate on
// independent baselines (compatibility/parity-baseline.json's
// heads.tempo-grpc) and a regression on one never masks or is masked by
// the other.
const grpcHeadName = "tempo-grpc"

// grpcSupportsEndpoint reports whether a corpus endpoint kind has a
// tempopb.StreamingQuerier RPC counterpart. See the file-level comment
// for the wire-contract citation: the service exposes exactly 7 RPCs and
// has no trace-by-id or search/recent equivalent.
func grpcSupportsEndpoint(ep string) bool {
	switch ep {
	case "search", "tags_v1", "tags_v2", "tag_values_v1", "tag_values_v2", "metrics_range", "metrics_instant":
		return true
	default:
		return false
	}
}

// unixSeconds32 converts a time.Time to the uint32 Unix-second wire form
// SearchRequest / SearchTagsRequest / SearchTagValuesRequest all use for
// Start/End (tempopb's search-side RPCs predate the nanosecond uint64
// convention the metrics RPCs use — see fetchGRPCMetricsRange). Centralised
// so the corpus's compatibility-window timestamps (always well below the
// year-2106 uint32 wraparound) get one documented conversion instead of a
// gosec suppression at every call site.
func unixSeconds32(t time.Time) uint32 {
	sec := t.Unix()
	if sec < 0 || sec > math.MaxUint32 {
		// Unreachable in practice — the differ's anchor is always a
		// recent-past timestamp (see currentAnchor) — but a saturating
		// clamp is a safer failure mode than a silent wraparound for a
		// window bound.
		if sec < 0 {
			return 0
		}
		return math.MaxUint32
	}
	return uint32(sec)
}

// runDiffGRPC is the diff-grpc subcommand entry point. Mirrors runDiff's
// flag surface + anchor/window computation (diff.go) so the two
// transports run against the same effective seed window in a
// side-by-side invocation, but dials tempopb.StreamingQuerierClient
// connections instead of building an *http.Client.
func runDiffGRPC(args []string) error {
	fs := flag.NewFlagSet("diff-grpc", flag.ContinueOnError)
	var (
		corpusPath  = fs.String("corpus", envOr("CORPUS_PATH", "/corpus/smoke.txtar"), "path to the TXTAR corpus file")
		tempoGRPC   = fs.String("tempo-grpc", envOr("TEMPO_GRPC_ADDR", "localhost:23095"), "reference Tempo gRPC (query-frontend StreamingQuerier) address")
		cerberusURL = fs.String("cerberus-grpc", envOr("CERBERUS_GRPC_ADDR", "localhost:29092"), "cerberus gRPC/h2c (StreamingQuerier) address — same host:port as its HTTP listener, dual-stack")
		reportPath  = fs.String("report", envOr("REPORT_PATH_GRPC", "/reports/diff-grpc.md"), "markdown report output path")
		scorePath   = fs.String("score", envOr("SCORE_PATH_GRPC", "/reports/compat-score-grpc.json"), "shields.io endpoint-badge score JSON output path")
		casesPath   = fs.String("cases", envOr("CASES_PATH_GRPC", "/reports/compat-cases-grpc.json"), "per-case parity roster JSON output path")
		overall     = fs.Duration("timeout", 5*time.Minute, "overall driver timeout")
		searchLimit = fs.Int("search-limit", 200, "Search RPC Limit field")
		anchorIn    = fs.String("anchor", "", "fixture anchor RFC3339 override; empty means roll the anchor to (now - anchorOffset), matching the seeder")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), *overall)
	defer cancel()

	cases, err := LoadCorpus(*corpusPath)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	// Same floor as runDiff — protects against an accidental corpus
	// shrink. The floor is on the RAW corpus size, not the gRPC-eligible
	// subset: grpcSupportsEndpoint below filters per case and reports
	// what it skipped, so this check stays a single source of truth
	// shared with the HTTP transport.
	if len(cases) < 25 {
		return fmt.Errorf("smoke corpus shrunk: got %d cases, want >= 25", len(cases))
	}
	logger.Info("loaded corpus", "path", *corpusPath, "cases", len(cases))

	var anchorTS time.Time
	if *anchorIn == "" {
		anchorTS = currentAnchor()
	} else {
		parsed, err := time.Parse(time.RFC3339, *anchorIn)
		if err != nil {
			return fmt.Errorf("parse anchor %q: %w", *anchorIn, err)
		}
		anchorTS = parsed
	}
	startTS := anchorTS.Add(-1 * time.Hour)
	endTS := anchorTS.Add(1 * time.Hour)

	// grpc.NewClient (post-1.65 API) returns a lazy channel; no TLS —
	// both cerberus's h2c listener and reference Tempo's query-frontend
	// gRPC listener are plaintext in this harness (mirrors seeder.go's
	// pushOTLP dial). Errors surface on the first RPC call rather than
	// at dial time.
	tempoConn, err := grpc.NewClient(*tempoGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial reference tempo grpc %s: %w", *tempoGRPC, err)
	}
	defer func() { _ = tempoConn.Close() }()
	cerbConn, err := grpc.NewClient(*cerberusURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial cerberus grpc %s: %w", *cerberusURL, err)
	}
	defer func() { _ = cerbConn.Close() }()

	tempoClient := tempopb.NewStreamingQuerierClient(tempoConn)
	cerbClient := tempopb.NewStreamingQuerierClient(cerbConn)

	opts := caseOpts{
		startTS:     startTS,
		endTS:       endTS,
		searchLimit: *searchLimit,
	}

	var results []CaseResult
	var skippedNoRPC []CorpusCase
	var skippedStatusParity []CorpusCase
	for _, tc := range cases {
		tc := tc
		switch {
		case !grpcSupportsEndpoint(tc.Endpoint):
			skippedNoRPC = append(skippedNoRPC, tc)
			continue
		case tc.ExpectedStatus != 0:
			// The status-parity axis (-- expect_status --, diff.go's
			// diffStatusParityCase) compares HTTP status ints. gRPC
			// surfaces failures as codes.Code via status.FromError — a
			// different domain with no canonical mapping — and
			// diffCaseGRPC treats any non-nil RPC error as an
			// unconditional HardError, so running an ExpectedStatus case
			// through this transport unmodified would silently mis-grade
			// it. Tracked as a distinct gRPC-side axis in #1714; until
			// that lands, these cases are HTTP-transport-only, reported
			// here rather than dropped.
			skippedStatusParity = append(skippedStatusParity, tc)
			continue
		}
		logger.Info("diffing case (grpc)", "name", tc.Name, "endpoint", tc.Endpoint)
		results = append(results, diffCaseGRPC(ctx, tempoClient, cerbClient, tc, opts))
	}
	logger.Info(
		"grpc corpus filtered",
		"ran", len(results),
		"skipped_no_grpc_rpc", len(skippedNoRPC),
		"skipped_status_parity", len(skippedStatusParity),
	)

	if err := writeReportGRPC(*reportPath, results, skippedNoRPC, skippedStatusParity); err != nil {
		return fmt.Errorf("write grpc report: %w", err)
	}
	logger.Info("wrote grpc markdown report", "path", *reportPath)

	passed, total := computeScore(results)
	s := score.Compute("TraceQL compat (gRPC/h2c)", passed, total)
	if err := score.Write(*scorePath, s); err != nil {
		return fmt.Errorf("write grpc score: %w", err)
	}
	if err := score.WriteCases(*casesPath, grpcHeadName, caseSet(results)); err != nil {
		return fmt.Errorf("write grpc cases: %w", err)
	}
	logger.Info(
		"wrote grpc compat score",
		"path", *scorePath,
		"passed", passed,
		"total", total,
		"percent", s.Percent,
		"color", s.Color,
		"skipped_no_grpc_rpc", len(skippedNoRPC),
		"skipped_status_parity", len(skippedStatusParity),
	)
	return nil
}

// diffCaseGRPC executes one corpus case over gRPC end-to-end (request
// build -> stream drain -> proto-to-JSON-shape conversion on both sides
// -> assertions -> structural diff). Deliberately mirrors diffCase
// (diff.go) step for step, reusing assertCaseForEndpoint /
// compareForEndpoint / RunSemanticChecks completely unchanged — only the
// fetch step differs between the two transports.
func diffCaseGRPC(ctx context.Context, tempoClient, cerbClient tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) CaseResult {
	res := CaseResult{Case: tc}

	tempoBody, terr := fetchGRPCForEndpoint(ctx, tempoClient, tc, opts)
	cerbBody, cerr := fetchGRPCForEndpoint(ctx, cerbClient, tc, opts)
	if terr != nil {
		res.HardError = fmt.Sprintf("tempo grpc: %v", terr)
	}
	if cerr != nil {
		if res.HardError == "" {
			res.HardError = fmt.Sprintf("cerberus grpc: %v", cerr)
		} else {
			res.HardError = res.HardError + "; cerberus grpc: " + cerr.Error()
		}
	}
	if res.HardError != "" {
		return res
	}

	tempoReasons, err := assertCaseForEndpoint(tc, tempoBody, "tempo")
	if err != nil {
		res.HardError = fmt.Sprintf("assert tempo: %v", err)
		return res
	}
	cerbReasons, err := assertCaseForEndpoint(tc, cerbBody, "cerberus")
	if err != nil {
		res.HardError = fmt.Sprintf("assert cerberus: %v", err)
		return res
	}
	res.Assertions = append(res.Assertions, tempoReasons...)
	res.Assertions = append(res.Assertions, cerbReasons...)

	if len(tc.SemanticChecks) > 0 && (tc.Endpoint == "metrics_range" || tc.Endpoint == "metrics_instant") {
		tempoSem, err := RunSemanticChecks(tc, tempoBody, "tempo")
		if err != nil {
			res.HardError = fmt.Sprintf("semantic tempo: %v", err)
			return res
		}
		cerbSem, err := RunSemanticChecks(tc, cerbBody, "cerberus")
		if err != nil {
			res.HardError = fmt.Sprintf("semantic cerberus: %v", err)
			return res
		}
		res.Assertions = append(res.Assertions, tempoSem...)
		res.Assertions = append(res.Assertions, cerbSem...)
	}

	d, err := compareForEndpoint(tc, tempoBody, cerbBody)
	if err != nil {
		res.HardError = fmt.Sprintf("diff: %v", err)
		return res
	}
	res.Diff = d
	return res
}

// fetchGRPCForEndpoint dispatches to the per-endpoint gRPC fetcher and
// returns the differ-JSON-shaped bytes assertCaseForEndpoint /
// compareForEndpoint already know how to decode. Unreachable for an
// endpoint grpcSupportsEndpoint rejects — the caller (runDiffGRPC)
// filters those out before this is ever invoked.
func fetchGRPCForEndpoint(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	switch tc.Endpoint {
	case "search":
		return fetchGRPCSearch(ctx, client, tc, opts)
	case "tags_v1":
		return fetchGRPCTagsV1(ctx, client, tc, opts)
	case "tags_v2":
		return fetchGRPCTagsV2(ctx, client, tc, opts)
	case "tag_values_v1":
		return fetchGRPCTagValuesV1(ctx, client, tc, opts)
	case "tag_values_v2":
		return fetchGRPCTagValuesV2(ctx, client, tc, opts)
	case "metrics_range":
		return fetchGRPCMetricsRange(ctx, client, tc, opts)
	case "metrics_instant":
		return fetchGRPCMetricsInstant(ctx, client, tc, opts)
	default:
		return nil, fmt.Errorf("unsupported grpc endpoint %q (no StreamingQuerier RPC)", tc.Endpoint)
	}
}

// fetchGRPCSearch opens the Search RPC, drains every TraceSearchMetadata
// frame to EOF (cerberus batches searchFrameSize=20 summaries per frame;
// see internal/api/tempo/grpc/search.go), converts each to the differ's
// TraceSummary shape, and marshals the flattened SearchResponse{Traces}
// envelope — the same shape Compare() (differ.go) decodes from the HTTP
// body.
func fetchGRPCSearch(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	req := &tempopb.SearchRequest{
		Query: tc.Query,
		//nolint:gosec // opts.searchLimit is a CLI flag, bounded well below uint32 range in practice.
		Limit: uint32(opts.searchLimit),
		Start: unixSeconds32(opts.startTS),
		End:   unixSeconds32(opts.endTS),
	}
	if tc.Spss > 0 {
		//nolint:gosec // tc.Spss is corpus-declared and validated positive by the loader.
		req.SpansPerSpanSet = uint32(tc.Spss)
	}
	stream, err := client.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open Search stream: %w", err)
	}
	var traces []TraceSummary
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv Search frame: %w", err)
		}
		for _, t := range frame.Traces {
			traces = append(traces, traceSummaryFromProto(t))
		}
	}
	return marshalDiffBody(SearchResponse{Traces: traces})
}

// fetchGRPCTagsV1 drains the SearchTags stream (single frame on both
// backends; see internal/api/tempo/grpc/tags.go) into the differ's
// TagNamesResponseV1 shape.
//
// The case's query rides along as SearchTagsRequest.Query, the proto
// spelling of the HTTP `?q=`, so the RPC is driven with exactly what its
// HTTP twin gets. V1 ignores it on both backends — that is what the
// `q`-carrying V1 case asserts — and sending it anyway is what makes a
// backend that started honouring it visible here and not only over HTTP.
func fetchGRPCTagsV1(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	req := &tempopb.SearchTagsRequest{
		Query: tc.Query,
		Start: unixSeconds32(opts.startTS),
		End:   unixSeconds32(opts.endTS),
	}
	stream, err := client.SearchTags(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open SearchTags stream: %w", err)
	}
	var names []string
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv SearchTags frame: %w", err)
		}
		names = append(names, frame.TagNames...)
	}
	return marshalDiffBody(TagNamesResponseV1{TagNames: names})
}

// fetchGRPCTagsV2 drains the SearchTagsV2 stream into the differ's
// TagNamesResponseV2 shape (per-scope tag lists).
//
// V2 is the tag-name route that honours a narrowing query, so the case's
// query is sent as SearchTagsRequest.Query and the answer must be the
// narrowed key set — the same one the HTTP `?q=` produces. Dropping it
// here would leave the RPC graded against the wide answer, which is a
// pass the corpus's absent-key assertion was written to refuse.
func fetchGRPCTagsV2(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	req := &tempopb.SearchTagsRequest{
		Scope: tc.Scope,
		Query: tc.Query,
		Start: unixSeconds32(opts.startTS),
		End:   unixSeconds32(opts.endTS),
	}
	stream, err := client.SearchTagsV2(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open SearchTagsV2 stream: %w", err)
	}
	var scopes []TagNamesScope
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv SearchTagsV2 frame: %w", err)
		}
		for _, sc := range frame.Scopes {
			scopes = append(scopes, tagsV2ScopeFromProto(sc))
		}
	}
	return marshalDiffBody(TagNamesResponseV2{Scopes: scopes})
}

// fetchGRPCTagValuesV1 drains the SearchTagValues stream into the
// differ's TagValuesResponseV1 shape.
func fetchGRPCTagValuesV1(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	req := &tempopb.SearchTagValuesRequest{
		TagName: tc.TagName,
		Start:   unixSeconds32(opts.startTS),
		End:     unixSeconds32(opts.endTS),
	}
	stream, err := client.SearchTagValues(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open SearchTagValues stream: %w", err)
	}
	var values []string
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv SearchTagValues frame: %w", err)
		}
		values = append(values, frame.TagValues...)
	}
	return marshalDiffBody(TagValuesResponseV1{TagValues: values})
}

// fetchGRPCTagValuesV2 drains the SearchTagValuesV2 stream into the
// differ's TagValuesResponseV2 shape (typed {type, value} entries).
func fetchGRPCTagValuesV2(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	req := &tempopb.SearchTagValuesRequest{
		TagName: tc.TagName,
		Start:   unixSeconds32(opts.startTS),
		End:     unixSeconds32(opts.endTS),
	}
	stream, err := client.SearchTagValuesV2(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open SearchTagValuesV2 stream: %w", err)
	}
	var values []TagValueV2
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv SearchTagValuesV2 frame: %w", err)
		}
		for _, v := range frame.TagValues {
			values = append(values, tagValueV2FromProto(v))
		}
	}
	return marshalDiffBody(TagValuesResponseV2{TagValues: values})
}

// fetchGRPCMetricsRange drains the MetricsQueryRange stream (single frame
// per internal/api/tempo/grpc/metrics.go's doc comment) into the differ's
// MetricsResponse shape.
func fetchGRPCMetricsRange(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	step, err := time.ParseDuration(tc.Step)
	if err != nil {
		return nil, fmt.Errorf("parse -- step -- %q: %w", tc.Step, err)
	}
	req := &tempopb.QueryRangeRequest{
		Query: tc.Query,
		//nolint:gosec // corpus-derived compatibility-window timestamps; never negative, never near int64 overflow.
		Start: uint64(opts.startTS.UnixNano()),
		//nolint:gosec // see Start above.
		End: uint64(opts.endTS.UnixNano()),
		//nolint:gosec // step is a corpus-declared small duration (seconds-to-minutes range).
		Step: uint64(step.Nanoseconds()),
	}
	stream, err := client.MetricsQueryRange(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open MetricsQueryRange stream: %w", err)
	}
	var series []MetricsSeriesEntry
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv MetricsQueryRange frame: %w", err)
		}
		for _, ts := range frame.Series {
			series = append(series, metricsSeriesFromProtoRange(ts))
		}
	}
	return marshalDiffBody(MetricsResponse{Series: series})
}

// fetchGRPCMetricsInstant drains the MetricsQueryInstant stream into the
// differ's MetricsResponse shape (each series carries a scalar Value
// rather than a Samples slice).
func fetchGRPCMetricsInstant(ctx context.Context, client tempopb.StreamingQuerierClient, tc CorpusCase, opts caseOpts) ([]byte, error) {
	req := &tempopb.QueryInstantRequest{
		Query: tc.Query,
		//nolint:gosec // see fetchGRPCMetricsRange.
		Start: uint64(opts.startTS.UnixNano()),
		//nolint:gosec // see fetchGRPCMetricsRange.
		End: uint64(opts.endTS.UnixNano()),
	}
	stream, err := client.MetricsQueryInstant(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("open MetricsQueryInstant stream: %w", err)
	}
	var series []MetricsSeriesEntry
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv MetricsQueryInstant frame: %w", err)
		}
		for _, is := range frame.Series {
			if is == nil {
				continue
			}
			v := is.Value
			series = append(series, MetricsSeriesEntry{
				Labels: keyValuesToLabels(is.Labels),
				Value:  &v,
			})
		}
	}
	return marshalDiffBody(MetricsResponse{Series: series})
}

// metricsSeriesFromProtoRange converts one streamed tempopb.TimeSeries
// into the differ's MetricsSeriesEntry shape (range form: Samples
// populated, Value nil).
func metricsSeriesFromProtoRange(ts *tempopb.TimeSeries) MetricsSeriesEntry {
	if ts == nil {
		return MetricsSeriesEntry{}
	}
	entry := MetricsSeriesEntry{Labels: keyValuesToLabels(ts.Labels)}
	for _, s := range ts.Samples {
		entry.Samples = append(entry.Samples, MetricsSample{
			TimestampMs: FlexInt64(s.TimestampMs),
			Value:       s.Value,
		})
	}
	for _, ex := range ts.Exemplars {
		entry.Exemplars = append(entry.Exemplars, MetricsExemplar{
			Labels:      keyValuesToLabels(ex.Labels),
			Value:       ex.Value,
			TimestampMs: FlexInt64(ex.TimestampMs),
		})
	}
	return entry
}

// writeReportGRPC renders the gRPC transport's markdown report: the
// shared per-case body (writeReport / renderReport, diff.go) for every
// case that ran, plus explicit "skipped" sections for corpus cases this
// transport genuinely can't run — documented, not silently dropped.
// skippedNoRPC is endpoints with no StreamingQuerier RPC counterpart at
// all (traces/traces_v2); skippedStatusParity is expect_status cases
// this transport doesn't yet grade (tracked in #1714).
func writeReportGRPC(path string, results []CaseResult, skippedNoRPC, skippedStatusParity []CorpusCase) error {
	if err := writeReport(path, grpcReportTitle, grpcStatusLabel, results); err != nil {
		return err
	}
	if len(skippedNoRPC) == 0 && len(skippedStatusParity) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: report path is a trusted CLI argument, same as writeReport's os.Create
	if err != nil {
		return fmt.Errorf("reopen report for skipped-section append: %w", err)
	}
	defer func() { _ = f.Close() }()
	if len(skippedNoRPC) > 0 {
		if err := renderSkippedSection(
			f, skippedNoRPC,
			"## Skipped — no StreamingQuerier RPC for this endpoint",
			"tempopb.StreamingQuerier exposes 7 RPCs (Search, SearchTags[V2], "+
				"SearchTagValues[V2], MetricsQueryRange, MetricsQueryInstant) — "+
				"there is no trace-by-id or search/recent RPC on either reference "+
				"Tempo or cerberus. The %d case(s) below are genuinely inapplicable "+
				"to this transport and are excluded from the score denominator "+
				"rather than counted as failures:\n\n",
		); err != nil {
			return err
		}
	}
	if len(skippedStatusParity) > 0 {
		if err := renderSkippedSection(
			f, skippedStatusParity,
			"## Skipped — expect_status axis not yet implemented for gRPC",
			"These corpus cases carry -- expect_status --, a rejection-parity "+
				"axis compared against the HTTP status int (diff.go's "+
				"diffStatusParityCase). gRPC surfaces failures as "+
				"codes.Code via status.FromError, a different domain with no "+
				"canonical mapping to HTTP status, and this transport doesn't "+
				"grade that yet (tracked in #1714). The %d case(s) below are "+
				"excluded from the score denominator rather than counted as "+
				"failures:\n\n",
		); err != nil {
			return err
		}
	}
	return nil
}

// renderSkippedSection appends a documented "skipped, and why" section,
// sorted by case name for reproducibility across runs.
func renderSkippedSection(w io.Writer, skipped []CorpusCase, heading, explanation string) error {
	sorted := append([]CorpusCase(nil), skipped...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	if _, err := fmt.Fprintln(w, heading); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, explanation, len(sorted)); err != nil {
		return err
	}
	for _, tc := range sorted {
		if _, err := fmt.Fprintf(w, "- `%s` (endpoint: `%s`)\n", tc.Name, tc.Endpoint); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}
