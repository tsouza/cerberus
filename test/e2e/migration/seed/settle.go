package seed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Ingestion skew is closed by TWO mechanisms, and they answer two different
// questions. Closed-window querying — the parity lane reads the manifest's
// [verify_start, verify_end] rather than a window ending at `now` — answers
// "do both sides cover the same interval?". The gates in this file answer "has
// the reference finished ingesting?". Neither implies the other.
//
// Only the reference side needs a gate: cerberus reads ClickHouse directly, so
// its visibility is bounded by the INSERT round-trip.
//
// Discipline: no percentage floors, no cardinality tolerances, no sticky
// latches, no retry-then-continue. Exact counts only, and a missing upstream
// metric never makes a gate pass.
//
// "Never makes a gate pass" is sharper than "always a hard error", and the
// difference is load-bearing. A gate reads two kinds of signal:
//
//   - QUIESCENCE signals, asserted `== 0` (the flush queue is empty, no chunks
//     are held in memory). These are plain gauges, registered at boot. If one
//     of them vanished — an upstream rename — coercing it to zero would
//     satisfy the condition and the gate would read as instantly settled. So
//     absence is a HARD ERROR, checked on every poll.
//
//   - PROGRESS signals, asserted `>= n` (chunks flushed since the baseline,
//     index uploads, completed blocks, blocklist length). These are counter
//     and gauge VECTORS whose children do not exist until the event they count
//     first happens, so absence at the baseline is the honest reading of "zero
//     so far", not a rename. Coercing them to zero cannot make a gate pass —
//     zero fails `>= n` — so the gate keeps waiting and eventually FAILS. What
//     absence does change is the diagnosis, so a progress signal that was
//     never observed at all is named as such in the timeout error.
//
// Neither branch tolerates anything: no reading lets a gate through that a
// present-and-unsatisfied reading would have blocked.

// Reference-backend readiness signals. These metric names are upstream's, and
// the same names the compatibility harnesses gate on; a regression test pins
// the two lists equal so an upstream rename fails in one place.
const (
	lokiFlushQueueLengthMetric = "loki_ingester_flush_queue_length"
	lokiMemoryChunksMetric     = "loki_ingester_memory_chunks"
	lokiChunksFlushedMetric    = "loki_ingester_chunks_flushed_total"
	lokiShipperUploadsMetric   = `loki_tsdb_shipper_tables_upload_operation_total{status="success"`
	tempoBlocksCompletedMetric = "tempo_live_store_blocks_completed_total"
	tempoBlocklistLengthMetric = "tempodb_blocklist_length"
)

// QuiescenceMetrics are the signals asserted `== 0`. Absence of one of these
// is a hard error on every poll: coerced to zero it would SATISFY the gate.
var QuiescenceMetrics = []string{
	lokiFlushQueueLengthMetric,
	lokiMemoryChunksMetric,
}

// ProgressMetrics are the signals asserted `>= n`. Absence reads as "the
// counted event has not happened yet" and keeps the gate waiting; it can never
// let the gate through.
var ProgressMetrics = []string{
	lokiChunksFlushedMetric,
	lokiShipperUploadsMetric,
	tempoBlocksCompletedMetric,
	tempoBlocklistLengthMetric,
}

// Gate cadence. Every one is a bounded deadline on a poll loop: when it
// expires the gate FAILS, it never continues.
const (
	settleInterval = 500 * time.Millisecond
	// metricsFetchTimeout bounds one /metrics scrape.
	metricsFetchTimeout = 10 * time.Second
	// metricsBodyLimit caps how much of a /metrics body is parsed. Reference
	// Loki's exposition is a few hundred KiB; a body larger than this means
	// something other than a reference backend is answering.
	metricsBodyLimit = 8 << 20
	// lokiShipperUploadsNeeded is the number of successful TSDB index uploads
	// that must land after the flush before a query can see the pushed data.
	lokiShipperUploadsNeeded = 1
	// tempoBlocksNeeded / tempoBlocklistNeeded are the live-store block and
	// blocklist minimums Tempo's search path needs.
	tempoBlocksNeeded    = 1
	tempoBlocklistNeeded = 1
)

// FlushLoki forces reference Loki's ingester to drain its in-memory chunks
// synchronously. The handler runs sweepUsers(immediate=true) and returns only
// once every chunk has been written to the object store, so the settle gate
// that follows is waiting on index publication rather than on chunk rotation
// timers that are measured in tens of minutes.
func FlushLoki(ctx context.Context, baseURL string) error {
	reqCtx, cancel := context.WithTimeout(ctx, lokiPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/flush", nil)
	if err != nil {
		return fmt.Errorf("new loki flush request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("loki flush: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		if readErr != nil {
			return fmt.Errorf("loki flush returned %d (body unreadable: %w)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("loki flush returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// LokiCounters is the pair of counters the Loki settle gate anchors on. It is
// read once BEFORE the push so the gate waits for deltas: an absolute
// threshold would already be satisfied by a previous run's data.
type LokiCounters struct {
	ChunksFlushed  float64
	ShipperUploads float64
}

// ReadLokiCounters snapshots the two anchor counters. Both are progress
// signals, so a freshly booted Loki that has never flushed a chunk reports
// them absent and the baseline is genuinely zero.
func ReadLokiCounters(ctx context.Context, baseURL string) (LokiCounters, error) {
	metrics, err := fetchMetrics(ctx, baseURL)
	if err != nil {
		return LokiCounters{}, err
	}
	flushed, _ := progressMetric(metrics, lokiChunksFlushedMetric)
	uploads, _ := progressMetric(metrics, lokiShipperUploadsMetric)
	return LokiCounters{ChunksFlushed: flushed, ShipperUploads: uploads}, nil
}

// WaitLokiSettled blocks until four conditions hold IN A SINGLE poll:
//
//   - the ingester's flush queue is empty,
//   - no chunks remain in memory,
//   - chunks flushed has advanced by at least streams since the baseline —
//     positive evidence that THIS push went through the flush path, not that a
//     stale reading happened to satisfy the queue checks,
//   - at least one successful TSDB shipper upload has landed since the
//     baseline, because /query_range consults the published index.
//
// Requiring them in one poll is what rules out a latch: a gate that remembers
// "queue was empty once" can pass while the current push is still in flight.
func WaitLokiSettled(ctx context.Context, baseURL string, streams int, baseline LokiCounters, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	unseen := newProgressTracker(lokiChunksFlushedMetric, lokiShipperUploadsMetric)
	var last error
	for {
		metrics, err := fetchMetrics(ctx, baseURL)
		if err == nil {
			var queue, memory float64
			queue, err = quiescenceMetric(metrics, lokiFlushQueueLengthMetric)
			if err == nil {
				memory, err = quiescenceMetric(metrics, lokiMemoryChunksMetric)
			}
			if err == nil {
				flushed, flushedSeen := progressMetric(metrics, lokiChunksFlushedMetric)
				uploads, uploadsSeen := progressMetric(metrics, lokiShipperUploadsMetric)
				unseen.observe(lokiChunksFlushedMetric, flushedSeen)
				unseen.observe(lokiShipperUploadsMetric, uploadsSeen)

				flushedDelta := flushed - baseline.ChunksFlushed
				uploadsDelta := uploads - baseline.ShipperUploads
				if queue == 0 && memory == 0 &&
					flushedDelta >= float64(streams) &&
					uploadsDelta >= lokiShipperUploadsNeeded {
					return nil
				}
				err = fmt.Errorf(
					"flush_queue_length=%v memory_chunks=%v chunks_flushed_delta=%v (need %d) shipper_uploads_delta=%v (need %d)%s",
					queue, memory, flushedDelta, streams, uploadsDelta, lokiShipperUploadsNeeded, unseen.suffix(),
				)
			}
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("reference loki did not settle within %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// WaitTempoSettled blocks until BOTH hold in a single poll:
//
//   - the live store has completed a block AND the querier's blocklist has
//     picked it up — /api/search only consults blocks it sees in the blocklist,
//     so a completed block the poller has not discovered is invisible,
//   - every trace THIS run pushed comes back from /api/search, per service,
//     as an exact set.
//
// The second condition is the binding one, and it is why the gate takes the
// fixture rather than only a URL. The live store cuts a block on
// max_trace_idle / max_block_duration, both of which are seconds: a push that
// spans more than one cut window flips the two counters while the traces sent
// after the cut are still in the WAL. An absolute counter reading therefore
// says nothing about whether the fixture is searchable, which is the only
// question the parity lane goes on to ask. Both of the sibling gates are
// already payload-bound — WaitLokiSettled scales to the pushed stream count,
// WaitPrometheusSettled queries the data back — and this one now matches them.
func WaitTempoSettled(ctx context.Context, baseURL string, f Fixture, timeout time.Duration) error {
	want := f.TraceIDsByService()
	if len(want) == 0 {
		return fmt.Errorf("the fixture holds no traces, so this gate would wait for nothing and " +
			"report a settled reference tempo over an empty push")
	}
	deadline := time.Now().Add(timeout)
	unseen := newProgressTracker(tempoBlocksCompletedMetric, tempoBlocklistLengthMetric)
	var last error
	for {
		last = checkTempoHoldsFixture(ctx, baseURL, f.Window, want, unseen)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reference tempo did not hold the fixture within %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// checkTempoHoldsFixture is one poll of the Tempo gate: the live-store progress
// signals first (they name the cheaper failure), then the per-service search
// that proves the pushed traces are actually retrievable.
func checkTempoHoldsFixture(
	ctx context.Context, baseURL string, w Window, want []ServiceTraces, unseen *progressTracker,
) error {
	metrics, err := fetchMetrics(ctx, baseURL)
	if err != nil {
		return err
	}
	blocks, blocksSeen := progressMetric(metrics, tempoBlocksCompletedMetric)
	blocklist, blocklistSeen := progressMetric(metrics, tempoBlocklistLengthMetric)
	unseen.observe(tempoBlocksCompletedMetric, blocksSeen)
	unseen.observe(tempoBlocklistLengthMetric, blocklistSeen)
	if blocks < tempoBlocksNeeded || blocklist < tempoBlocklistNeeded {
		return fmt.Errorf("blocks_completed=%v (need %d) blocklist_length=%v (need %d)%s",
			blocks, tempoBlocksNeeded, blocklist, tempoBlocklistNeeded, unseen.suffix())
	}
	for _, svc := range want {
		got, err := searchTempoTraceIDs(ctx, baseURL, svc.Service, w)
		if err != nil {
			return err
		}
		if len(got) != len(svc.TraceIDs) {
			return fmt.Errorf("search for %s returned %d traces, want the %d this run pushed",
				svc.Service, len(got), len(svc.TraceIDs))
		}
		for i := range svc.TraceIDs {
			if got[i] != svc.TraceIDs[i] {
				return fmt.Errorf("search for %s returned trace id %q at position %d, want %q",
					svc.Service, got[i], i, svc.TraceIDs[i])
			}
		}
	}
	return nil
}

// tempoSearchResponse is the subset of Tempo's /api/search response the gate
// reads.
type tempoSearchResponse struct {
	Traces []struct {
		TraceID string `json:"traceID"`
	} `json:"traces"`
}

// searchTempoTraceIDs runs one service search and returns the trace ids it
// yielded, sorted, in the reference backend's own wire rendering.
func searchTempoTraceIDs(ctx context.Context, baseURL, service string, w Window) ([]string, error) {
	start, end := w.TraceSearchRange()
	target := strings.TrimRight(baseURL, "/") + "/api/search?" + url.Values{
		"q":     {TraceServiceQuery(service)},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"limit": {strconv.Itoa(TraceSearchLimit)},
	}.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, metricsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, metricsBodyLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", target, resp.StatusCode, truncate(string(body)))
	}
	var got tempoSearchResponse
	if err := json.Unmarshal(body, &got); err != nil {
		return nil, fmt.Errorf("decode %s body %s: %w", target, truncate(string(body)), err)
	}
	out := make([]string, 0, len(got.Traces))
	for _, tr := range got.Traces {
		out = append(out, tr.TraceID)
	}
	sort.Strings(out)
	return out, nil
}

// WaitPrometheusSettled proves the reference Prometheus has ingested the whole
// fixture, data-side. Its remote-write receiver exposes no flush stage, so
// there is no counter that means "done"; the honest proof is that an instant
// query at the anchor returns EXACTLY the declared series count with its last
// sample at exactly the anchor. A count that is merely non-zero would pass
// while half the batch is still being appended.
func WaitPrometheusSettled(ctx context.Context, baseURL, metric string, wantSeries int, anchor time.Time, timeout time.Duration) error {
	query := strings.TrimRight(baseURL, "/") + "/api/v1/query?query=" + url.QueryEscape(metric) +
		"&time=" + strconv.FormatInt(anchor.Unix(), 10)
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = checkPrometheusAnchor(ctx, query, wantSeries, anchor)
		if last == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("reference prometheus did not hold the fixture within %s: %w", timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleInterval):
		}
	}
}

// promInstantResponse is the subset of Prometheus's instant-query response the
// settle gate reads.
type promInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func checkPrometheusAnchor(ctx context.Context, query string, wantSeries int, anchor time.Time) error {
	reqCtx, cancel := context.WithTimeout(ctx, metricsFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, query, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, metricsBodyLimit))
	if err != nil {
		return fmt.Errorf("read query body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("query returned %d: %s", resp.StatusCode, truncate(string(body)))
	}
	var got promInstantResponse
	if err := json.Unmarshal(body, &got); err != nil {
		return fmt.Errorf("decode query body %s: %w", truncate(string(body)), err)
	}
	if got.Status != "success" {
		return fmt.Errorf("query status = %q, want %q", got.Status, "success")
	}
	if len(got.Data.Result) != wantSeries {
		return fmt.Errorf("holds %d series at the anchor, want exactly %d", len(got.Data.Result), wantSeries)
	}
	for _, r := range got.Data.Result {
		if len(r.Value) == 0 {
			return fmt.Errorf("series %v carries no sample at the anchor", r.Metric)
		}
		var ts float64
		if err := json.Unmarshal(r.Value[0], &ts); err != nil {
			return fmt.Errorf("decode sample timestamp for %v: %w", r.Metric, err)
		}
		if int64(ts) != anchor.Unix() {
			return fmt.Errorf("series %v last sample at %d, want the anchor %d", r.Metric, int64(ts), anchor.Unix())
		}
	}
	return nil
}

// fetchMetrics scrapes a backend's /metrics endpoint into a name→value map.
// The parser is line-oriented on purpose: the gates look up a handful of
// names, and pulling in a full exposition-format parser would add a dependency
// for nothing.
func fetchMetrics(ctx context.Context, baseURL string) (map[string]float64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, metricsFetchTimeout)
	defer cancel()
	target := strings.TrimRight(baseURL, "/") + "/metrics"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		return nil, fmt.Errorf("%s returned %d: %s", target, resp.StatusCode, truncate(string(body)))
	}

	out := map[string]float64{}
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, metricsBodyLimit))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), bufio.MaxScanTokenSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawValue, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
		if err != nil {
			continue
		}
		out[name] += value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", target, err)
	}
	return out, nil
}

// sumMetric collapses a metric family to one scalar by summing every sample
// the selector matches. The second result reports whether the selector matched
// anything at all, which the two callers below interpret according to the
// signal's role.
func sumMetric(metrics map[string]float64, selector string) (float64, bool) {
	var total float64
	var matched bool
	for name, value := range metrics {
		if matchesMetricKey(name, selector) {
			total += value
			matched = true
		}
	}
	return total, matched
}

// matchesMetricKey encodes the two selector shapes the gates use.
//
//   - A bare metric name (`loki_ingester_flush_queue_length`) matches the
//     exact key for an unlabelled scalar, or `<name>{…}` for a labelled
//     family. The `{` is required rather than a plain prefix, so `foo_total`
//     cannot match `foo_total_bucket`.
//   - A name plus a partial label selector
//     (`loki_tsdb_shipper_tables_upload_operation_total{status="success"`,
//     deliberately unterminated) matches any key with that metric name whose
//     rendered label set CONTAINS the selector. A prefix test on the label
//     portion would not work: the exposition format sorts labels
//     alphabetically, so `component` lands before `status` and the selector
//     sits mid-string.
func matchesMetricKey(key, selector string) bool {
	if key == selector {
		return true
	}
	brace := strings.IndexRune(selector, '{')
	if brace < 0 {
		return strings.HasPrefix(key, selector+"{")
	}
	name := selector[:brace]
	labels := selector[brace+1:]
	if !strings.HasPrefix(key, name+"{") {
		return false
	}
	end := strings.IndexRune(key, '}')
	if end < 0 {
		return false
	}
	return strings.Contains(key[len(name)+1:end], labels)
}

// quiescenceMetric reads a signal the gate asserts `== 0`. Absence is a hard
// error: coerced to zero it would satisfy the condition and let the gate
// through, which is exactly the failure an upstream rename would cause.
func quiescenceMetric(metrics map[string]float64, prefix string) (float64, error) {
	value, ok := sumMetric(metrics, prefix)
	if !ok {
		return 0, fmt.Errorf(
			"reference backend exposes no metric matching %q. It is asserted == 0, so treating it as "+
				"absent-means-zero would make this gate report settled without waiting for anything; "+
				"upstream has renamed it and the gate must follow", prefix,
		)
	}
	return value, nil
}

// progressMetric reads a signal the gate asserts `>= n`. These are counter and
// gauge vectors whose children do not exist until the counted event first
// happens, so absence is the honest reading of "none yet" — and it can only
// keep the gate waiting, never let it through.
func progressMetric(metrics map[string]float64, prefix string) (float64, bool) {
	return sumMetric(metrics, prefix)
}

// progressTracker remembers which progress signals a gate has ever observed,
// so a timeout can distinguish "the event never happened" from "upstream
// renamed the metric". It changes only the diagnosis; a gate that has not met
// its condition fails either way.
type progressTracker struct {
	seen map[string]bool
}

func newProgressTracker(names ...string) *progressTracker {
	t := &progressTracker{seen: make(map[string]bool, len(names))}
	for _, n := range names {
		t.seen[n] = false
	}
	return t
}

func (t *progressTracker) observe(name string, present bool) {
	if present {
		t.seen[name] = true
	}
}

// suffix names every tracked signal that has not been observed once, in a
// stable order.
func (t *progressTracker) suffix() string {
	var missing []string
	for name, seen := range t.seen {
		if !seen {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return " [never observed: " + strings.Join(missing, ", ") +
		" — either the counted event has not happened, or upstream renamed the metric]"
}

// truncate caps a quoted response body in an error message.
func truncate(s string) string {
	if len(s) <= errBodyLimit {
		return s
	}
	return s[:errBodyLimit]
}
