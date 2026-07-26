package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// correlationFixtureName is the seed declaration MIG-21 rebuilds from the
// live manifest to get ground truth for the log record and trace it probes —
// the same declaration MIG-16/MIG-17's verify corpora are hand-authored
// against, since three-signal is the only archetype the Tier-1 stack seeds.
const correlationFixtureName = "fixture.json"

// correlationHTTPTimeout bounds one HTTP round-trip against the live
// cerberus during the correlation hop.
const correlationHTTPTimeout = 30 * time.Second

// correlationResponseCap bounds how much of a response this probe buffers, so
// a misbehaving endpoint fails loudly rather than exhausting memory.
const correlationResponseCap = 32 << 20

// traceIDInLineRe extracts the fixture's embedded trace_id out of a returned
// log line, in whichever of the three fixture formats (json/logfmt/
// unstructured) rendered it: all three carry a literal `trace_id=` or
// `"trace_id":"...")` token followed by the 32 lowercase hex characters
// seed.Fixture derives the id from. Reading it back OUT OF THE RESPONSE BODY
// — rather than reusing the value the Given step already knows from
// rebuilding the fixture — is what makes the When step a real hop: the trace
// id driving the trace-by-id fetch is the one cerberus's own log query
// actually returned, not one the scenario already had in hand.
var traceIDInLineRe = regexp.MustCompile(`trace_id"?[:=]\s*"?([0-9a-f]{32})`)

// correlationProbe is the state MIG-21's Given establishes and its When and
// Then steps read: which log record the fixture wrote with a trace_id, the
// trace that id resolves to, and what the live hop through cerberus actually
// returned.
type correlationProbe struct {
	established bool
	archetype   string

	// logSelector is the LogQL stream selector the fixture's own log job
	// resolves to, and logStart/logEnd bound the query to the manifest's
	// pinned window rather than a live one that keeps sliding.
	logSelector string
	logStart    time.Time
	logEnd      time.Time

	// wantTraceID is ground truth from the rebuilt fixture: the trace_id a
	// real log record in this window actually carries. It seeds the LogQL
	// query's time bound but is deliberately NOT what drives the trace-by-id
	// fetch — see traceIDInLineRe.
	wantTraceID   string
	wantSpanCount int

	// Populated by the When step from what cerberus's own responses said.
	logLineTraceID  string
	resolvedSpans   int
	resolvedTraceID string
}

// registerCorrelationSteps binds MIG-21: trace_id as a first-class indexed
// column, and the logs-to-trace correlation hop resolved against cerberus's
// own Loki- and Tempo-compatible read paths.
func (w *World) registerCorrelationSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^a log record from the live fixture carrying a trace_id$`, w.givenCorrelationProbe)
	ctx.Step(`^the operator resolves that log line's trace_id back through cerberus to its trace$`,
		w.whenResolveCorrelation)
	ctx.Step(`^trace_id is a first-class indexed column in both the logs table and the traces table$`,
		w.thenTraceIDIsIndexed)
	ctx.Step(`^the log line cerberus returned carries the trace_id the fixture wrote for it$`,
		w.thenLogLineCarriesFixtureTraceID)
	ctx.Step(`^the trace cerberus resolves for that trace_id carries every span the fixture wrote for it$`,
		w.thenResolvedTraceMatchesFixture)
}

// givenCorrelationProbe rebuilds the exact fixture the live seeder wrote —
// Manifest.Window() plus the archetype's committed declaration reproduce it
// byte-for-byte — and picks the first log record carrying a trace_id, plus
// the trace that id belongs to. This is ground truth read from the same data
// declaration the seeder used, never a value invented by this step.
func (w *World) givenCorrelationProbe() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; scenario must establish it first")
	}
	if err := w.requireArchetypes("the correlation probe"); err != nil {
		return err
	}
	if len(w.archetypes) != 1 {
		return fmt.Errorf("the correlation probe reads one archetype's fixture; got %v", w.archetypes)
	}
	a := w.archetypes[0]

	manifest, err := w.live.LoadManifest(a)
	if err != nil {
		return err
	}
	window, err := manifest.Window()
	if err != nil {
		return fmt.Errorf("migration harness: reconstruct the fixture window: %w", err)
	}
	declPath := harnessPath(w.root, archetypeDir, a, "seed", correlationFixtureName)
	decl, err := seed.LoadDeclaration(declPath)
	if err != nil {
		return err
	}
	fixture, err := seed.Build(decl, window)
	if err != nil {
		return fmt.Errorf("migration harness: rebuild the fixture: %w", err)
	}

	record, stream, ok := firstTracedLogRecordInWindow(fixture, manifest.VerifyStart, manifest.VerifyEnd)
	if !ok {
		return fmt.Errorf("archetype %s's fixture writes no in-window log record carrying a trace_id", a)
	}
	spans := traceSpanCount(fixture, record.TraceID)
	if spans == 0 {
		return fmt.Errorf("archetype %s's fixture carries no trace matching log record trace_id %s", a, record.TraceID)
	}

	w.manifest[a] = manifest
	w.correlation = correlationProbe{
		established: true,
		archetype:   a,
		// Every label in the stream (not just job) — the fixture crosses
		// every service with every log format under the SAME job label, so a
		// job-only selector matches every stream at once and the first
		// traced line the query returns need not be THIS record's stream at
		// all. Pinning every label narrows the query to the exact stream the
		// picked record belongs to.
		logSelector:   logQLSelectorFor(stream.Labels),
		logStart:      manifest.VerifyStart,
		logEnd:        manifest.VerifyEnd,
		wantTraceID:   record.TraceID,
		wantSpanCount: spans,
	}
	return nil
}

// firstTracedLogRecordInWindow returns the earliest log record, in fixture
// order, whose TraceID is non-empty AND whose timestamp falls inside
// [from,to], along with the stream it belongs to. The fixture's log records
// span the whole SEED window (from SeedStart), but the live query this probe
// drives is bounded to the manifest's VERIFY window (from SeedStart +
// RangeLookbackMargin) — picking a record outside that bound would pin ground
// truth the query can never observe. Fixture order is deterministic
// (seed.Build draws from one seeded RNG in a fixed order), so two runs
// against the same declaration and window pick the same record.
func firstTracedLogRecordInWindow(f seed.Fixture, from, to time.Time) (seed.LogRecord, seed.LogStream, bool) {
	for _, stream := range f.LogStreams {
		for _, record := range stream.Records {
			if record.TraceID == "" {
				continue
			}
			if record.Time.Before(from) || record.Time.After(to) {
				continue
			}
			return record, stream, true
		}
	}
	return seed.LogRecord{}, seed.LogStream{}, false
}

// logQLSelectorFor renders every label in labels as one LogQL stream
// selector, sorted so the query string is deterministic across runs.
func logQLSelectorFor(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(labels[k])
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

// traceSpanCount walks f.Traces directly for the trace whose spans carry
// hexID, returning its span count. TraceIDsByService renders ids through
// seed.RenderTempoTraceID (which may reformat the id), so this reads the raw
// Trace slice instead of trusting that rendering round-trips.
func traceSpanCount(f seed.Fixture, hexID string) int {
	for _, t := range f.Traces {
		if len(t.Spans) == 0 {
			continue
		}
		if hexTraceID(t) == hexID {
			return len(t.Spans)
		}
	}
	return 0
}

// hexTraceID renders a fixture trace's id the same way seed.LogRecord.TraceID
// is written, so the two are comparable byte-for-byte.
func hexTraceID(t seed.Trace) string {
	if len(t.Spans) == 0 {
		return ""
	}
	id := t.Spans[0].TraceID
	return fmt.Sprintf("%032x", id[:])
}

// whenResolveCorrelation drives the real hop: query cerberus's own Loki-
// compatible API for the fixture's log stream, extract whatever trace_id the
// RETURNED line actually carries, then fetch that id's trace through
// cerberus's own Tempo-compatible trace-by-id endpoint.
func (w *World) whenResolveCorrelation() error {
	if !w.correlation.established {
		return fmt.Errorf("no correlation probe established; the scenario must establish one first")
	}

	line, err := correlationQueryLogs(w.live.CerberusURL, w.correlation.logSelector, w.correlation.logStart, w.correlation.logEnd)
	if err != nil {
		return fmt.Errorf("migration harness: query cerberus's log API: %w", err)
	}
	m := traceIDInLineRe.FindStringSubmatch(line)
	if m == nil {
		return fmt.Errorf("cerberus returned a log line carrying no recognisable trace_id: %q", line)
	}
	w.correlation.logLineTraceID = m[1]

	spans, err := correlationFetchTraceSpanCount(w.live.CerberusURL, w.correlation.logLineTraceID)
	if err != nil {
		return fmt.Errorf("migration harness: fetch the trace cerberus resolves for %s: %w", w.correlation.logLineTraceID, err)
	}
	w.correlation.resolvedTraceID = w.correlation.logLineTraceID
	w.correlation.resolvedSpans = spans
	return nil
}

// lokiQueryRangeResponse is the standard Loki wire envelope, the same shape
// tier1_stack_test.go's assertLokiProbe already proves works against a Loki-
// wire-format server running in this stack.
type lokiQueryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Stream map[string]string `json:"stream"`
			Values [][]string        `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// correlationLogLimit bounds how many entries the probe's query_range asks
// for. It is generous relative to the fixture's per-stream record count, so a
// complete window is never truncated into a false negative.
const correlationLogLimit = 500

// correlationQueryLogs issues one LogQL query_range against base (cerberus's
// own base URL) and returns the first returned line carrying any trace_id
// token, failing when the query itself fails or returns nothing at all.
func correlationQueryLogs(base, selector string, start, end time.Time) (string, error) {
	q := base + "/loki/api/v1/query_range?query=" + url.QueryEscape(selector) +
		"&start=" + strconv.FormatInt(start.UnixNano(), 10) +
		"&end=" + strconv.FormatInt(end.UnixNano(), 10) +
		"&direction=forward&limit=" + strconv.Itoa(correlationLogLimit)
	body, err := correlationGET(q)
	if err != nil {
		return "", err
	}
	var resp lokiQueryRangeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decode loki query_range response: %w", err)
	}
	if resp.Status != "success" {
		return "", fmt.Errorf("loki query_range status %q, want success", resp.Status)
	}
	for _, res := range resp.Data.Result {
		for _, v := range res.Values {
			if len(v) < 2 {
				continue
			}
			if traceIDInLineRe.MatchString(v[1]) {
				return v[1], nil
			}
		}
	}
	return "", fmt.Errorf("cerberus returned no log line carrying a trace_id for selector %s", selector)
}

// tempoTraceByIDResponse is cerberus's trace-by-id JSON envelope: one
// resource batch per distinct resource, each carrying its spans directly
// (cerberus's OTLP-JSON trace-by-id shape nests spans straight under
// `batches[].spans`, not under an intermediate `scopeSpans` level).
type tempoTraceByIDResponse struct {
	Batches []struct {
		Spans []struct {
			Name string `json:"name"`
		} `json:"spans"`
	} `json:"batches"`
}

// correlationFetchTraceSpanCount fetches base's trace-by-id endpoint for
// hexID and returns the total span count across every batch.
func correlationFetchTraceSpanCount(base, hexID string) (int, error) {
	body, err := correlationGET(base + "/api/traces/" + hexID)
	if err != nil {
		return 0, err
	}
	var resp tempoTraceByIDResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("decode trace-by-id response: %w", err)
	}
	var n int
	for _, b := range resp.Batches {
		n += len(b.Spans)
	}
	return n, nil
}

// correlationGET issues one bounded GET, requiring a 200 and capping the body
// it reads back — the same discipline migrateverify.HTTPBackend applies,
// reimplemented narrowly here because this probe reads two different result
// kinds (a log stream, a single trace) that migrateverify's KindDialect
// surface does not expose as typed values outside that package.
func correlationGET(rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), correlationHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, correlationResponseCap+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(body)) > correlationResponseCap {
		return nil, fmt.Errorf("response body exceeds the %d-byte cap", correlationResponseCap)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", rawURL, resp.StatusCode, string(body))
	}
	return body, nil
}

// thenTraceIDIsIndexed asserts TraceId is a real column on both the logs and
// the traces table, and appears in each table's sorting key — ClickHouse's
// primary index — rather than merely existing as an unordered column. This is
// a structural probe against the live schema, not a re-assertion of a value
// this scenario already assumed.
func (w *World) thenTraceIDIsIndexed() error {
	if !w.liveSet {
		return fmt.Errorf("the tier-1 stack has not been established; scenario must establish it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveRetentionBudget)
	defer cancel()
	conn, err := seed.DialCH(ctx, seed.CHConfig{
		Addr:     w.live.CHAddr,
		Database: w.live.CHDatabase,
		Username: w.live.CHUsername,
		Password: w.live.CHPassword,
	})
	if err != nil {
		return fmt.Errorf("migration harness: dial the live clickhouse for the trace_id index probe: %w", err)
	}
	defer func() { _ = conn.Close() }()

	for _, table := range []string{logsTableName, tracesTableName} {
		var colCount uint64
		if err := conn.QueryRow(
			ctx,
			"SELECT count() FROM system.columns WHERE database = ? AND table = ? AND name = 'TraceId'",
			w.live.CHDatabase, table,
		).Scan(&colCount); err != nil {
			return fmt.Errorf("migration harness: probe %s.TraceId: %w", table, err)
		}
		if colCount == 0 {
			return fmt.Errorf("table %s carries no first-class TraceId column", table)
		}

		// TraceId is not part of either table's ORDER BY (both are ordered
		// for time-range scans: otel_logs by (time-bucket, ServiceName,
		// Timestamp), otel_traces by (ServiceName, SpanName, Timestamp)).
		// The OTel-CH exporter's own DDL (sqltemplates/logs_table.sql,
		// traces_table.sql) instead declares a dedicated secondary skip
		// index over TraceId on both tables — a bloom filter (or, with
		// full-text search enabled, a tokenized text index) — which is what
		// makes a `WHERE TraceId = ?` probe skip granules instead of
		// scanning them. That secondary index, not sorting-key membership,
		// is what "indexed" means for this column.
		var idxCount uint64
		if err := conn.QueryRow(
			ctx,
			"SELECT count() FROM system.data_skipping_indices WHERE database = ? AND table = ? AND expr = 'TraceId'",
			w.live.CHDatabase, table,
		).Scan(&idxCount); err != nil {
			return fmt.Errorf("migration harness: probe %s's secondary indices: %w", table, err)
		}
		if idxCount == 0 {
			return fmt.Errorf("table %s carries no secondary index over TraceId", table)
		}
	}
	return nil
}

// logsTableName / tracesTableName are the OTel-CH table names the live
// collector provisions, matching seed.ExporterTables.
const (
	logsTableName   = "otel_logs"
	tracesTableName = "otel_traces"
)

// thenLogLineCarriesFixtureTraceID asserts the trace_id cerberus's own log
// query actually returned is the SAME one the fixture wrote — not merely that
// some line came back, and not merely that some trace_id-shaped token was
// found.
func (w *World) thenLogLineCarriesFixtureTraceID() error {
	if err := w.requireCorrelationResolved(); err != nil {
		return err
	}
	if w.correlation.logLineTraceID != w.correlation.wantTraceID {
		return fmt.Errorf("cerberus's log query returned trace_id %s, the fixture wrote %s for this record",
			w.correlation.logLineTraceID, w.correlation.wantTraceID)
	}
	return nil
}

// thenResolvedTraceMatchesFixture asserts the trace cerberus resolved for the
// log's trace_id carries exactly as many spans as the fixture declared for
// that trace — the cross-signal hop returning a real, complete trace rather
// than an empty or partial one.
func (w *World) thenResolvedTraceMatchesFixture() error {
	if err := w.requireCorrelationResolved(); err != nil {
		return err
	}
	if w.correlation.resolvedSpans != w.correlation.wantSpanCount {
		return fmt.Errorf("cerberus resolved %d span(s) for trace %s, the fixture wrote %d",
			w.correlation.resolvedSpans, w.correlation.resolvedTraceID, w.correlation.wantSpanCount)
	}
	return nil
}

// requireCorrelationResolved fails when the When step never ran, so a Then
// cannot read zero-valued correlation state and pass by accident.
func (w *World) requireCorrelationResolved() error {
	if !w.correlation.established {
		return fmt.Errorf("no correlation probe established; the scenario must establish one first")
	}
	if w.correlation.resolvedTraceID == "" {
		return fmt.Errorf("the correlation hop has not been resolved; the scenario must run it first")
	}
	return nil
}
