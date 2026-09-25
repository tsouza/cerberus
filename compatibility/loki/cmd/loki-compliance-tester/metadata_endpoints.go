package main

// Differential pass for the Loki metadata endpoints:
//
//	GET /loki/api/v1/labels
//	GET /loki/api/v1/label/{name}/values
//	GET /loki/api/v1/series
//	GET /loki/api/v1/index/stats
//	GET /loki/api/v1/index/volume            (aggregateBy=series and =labels)
//	GET /loki/api/v1/detected_labels
//
// The bench corpus exercises /query and /query_range only, and the
// sibling passes cover /detected_fields, /detected_field/{name}/values
// and /patterns. Grafana's datasource UI, label browser and Logs
// Drilldown are driven off THESE six routes, so a wrong label set, a
// wrong series inventory or a mis-ranked volume row reaches the user
// while every corpus case stays green. This pass requests each route
// from both backends over the seeded corpus window, compares the HTTP
// status, decodes the body exactly as the consumer does, and diffs the
// data set. Results join the same report + score pipeline as the
// corpus cases — no separate bucket, no allow-list: a mismatch is a
// FAIL counted in the total.
//
// What is graded, per route, and why the comparison has the shape it
// has:
//
//   - /labels, /label/{name}/values, /series, /detected_labels — the
//     data SET, order-insensitive. Upstream's query frontend splits a
//     metadata request by interval and merges the split responses in
//     encounter order (LokiLabelNamesResponse / LokiSeriesResponse in
//     pkg/querier/queryrange/codec.go; countLabelsAndCardinality in
//     pkg/querier/querier.go iterates a Go map), so wire order is not
//     part of the reference's contract and Grafana sorts client-side.
//     /detected_labels grades each label's cardinality alongside its
//     name; the `sketch` field — the hyperloglog's serialised state,
//     whose only consumer is the querier that merges shards — is not
//     on the wire from cerberus and is not graded: the number it
//     estimates is.
//   - /index/stats — `streams` and `entries` exactly. `chunks` and
//     `bytes` are chunk-storage quantities the reference reports from
//     its TSDB index (per-chunk counts and `KB<<10`, the chunk's
//     uncompressed size rounded to whole kilobytes, timestamp and
//     structured-metadata bytes included — pkg/storage/stores/shipper/
//     indexshipper/tsdb/index/chunk.go and tsdb/store.go) and cerberus has no
//     chunk model: it reports `chunks` as 0 by construction and `bytes`
//     as `sum(length(Body))` (internal/api/loki/index_stats.go). Those
//     two fields cannot agree between a chunk store and a row store and
//     are not compared; a tolerance wide enough to absorb the encoding
//     overhead would be a tuned threshold, not a grade.
//   - /index/volume — the SET of metric label sets the response carries,
//     for both aggregateBy modes, with and without targetLabels, and
//     under selectors naming different labels (upstream's series-mode
//     key without targetLabels is the set of label names the matchers
//     name — see metadataPerServiceSelector). The byte VALUES
//     are the same chunk-KB quantity as /index/stats and are not
//     compared. The RANKING is graded through the tie-break case:
//     `aggregateBy=labels` over every seeded stream gives every label
//     name the same volume on both backends (every stream carries every
//     label, so each name's total is the whole corpus), and a `limit`
//     below the label count then selects rows purely by upstream's
//     tie-break — volume descending, then bare label name ascending
//     (MapToVolumeResponse, pkg/storage/stores/index/seriesvolume/
//     volume.go). A backend ranking ties on any other key (the
//     `{name=""}` rendering, the stored key rather than the served one)
//     returns a different set and fails. Series-mode cases use a limit
//     above the seeded stream count so that ranking over incomparable
//     byte values never decides which rows are present.
//
// Emptiness follows the corpus convention: an empty reference answer is
// a harness/seed problem reported as an UnexpectedFailure, not a parity
// datapoint; an empty test answer against a non-empty reference is a
// FAIL. A non-200 from the test endpoint against a 200 from the
// reference is a status diff, not a fold into "failed": the status IS
// one of the graded axes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	bench "github.com/tsouza/cerberus/compatibility/loki/upstream/loki-bench"
)

// metadataEndpointsSource is the report `source` every case in this
// pass carries; it is part of the parity-roster identity.
const metadataEndpointsSource = "cerberus/metadata-endpoints"

// The report `kind` per route. Part of the parity-roster identity.
const (
	metadataKindLabels         = "labels"
	metadataKindLabelValues    = "label_values"
	metadataKindSeries         = "series"
	metadataKindIndexStats     = "index_stats"
	metadataKindIndexVolume    = "index_volume"
	metadataKindDetectedLabels = "detected_labels"
)

// metadataCorpusSelector matches every seeded corpus stream and nothing
// else: the corpus streams sit in `cluster-0` / `cluster-1`
// (cmd/seed/main.go serviceConfigs) while the now-anchored /patterns
// fixture sits in its own `live-patterns` cluster (cmd/seed/
// live_patterns.go). Every case carries a selector — this one or a
// subset of it — because upstream's TSDB label discovery is bounded by
// MATCHERS, not by the request window: `TSDBIndex.LabelNames` /
// `LabelValues` discard `from`/`through` (pkg/storage/stores/shipper/
// indexshipper/tsdb/single_file_index.go), so a selector-less /labels,
// /label/{name}/values or /detected_labels answers with every stream in
// every index file that overlaps the window — and the live fixture,
// pushed seconds after the corpus, shares the ingester's head index with
// it until that head rotates. A selector-less case's reference answer
// would therefore depend on head-rotation timing rather than on the
// data; a corpus-bounded selector makes it a function of the data on
// both backends. The window is the full corpus span for the same
// reason: within an index, upstream does not narrow discovery to the
// window at all.
const metadataCorpusSelector = `{cluster=~"cluster-.+"}`

// metadataPerServiceSelector also matches every corpus stream, through
// a matcher on `service_name` — the one label that takes a distinct
// value on every seeded stream. Its /index/volume series-mode case pins
// upstream's key rule: with no `targetLabels`, the series key is the
// set of label names the MATCHERS name (`PrepareLabelsAndMatchers`,
// pkg/util/series_volume.go, feeding `labelsToMatch` in the volume
// walks), so this selector yields one `{service_name="<svc>"}` row per
// stream while metadataCorpusSelector yields one `{cluster="<c>"}` row
// per cluster.
const metadataPerServiceSelector = `{service_name=~".+"}`

// metadataSubsetSelector matches the cluster-0 half of the seeded
// streams — a proper subset, so a backend ignoring the selector on a
// selector-taking route answers with the full inventory and fails.
const metadataSubsetSelector = `{cluster="cluster-0"}`

// metadataSubsetLabel is the label whose values are graded under
// metadataSubsetSelector: `service_name` takes a different value on
// every stream, so the selected subset is visibly smaller than the
// unselected inventory.
const metadataSubsetLabel = "service_name"

// volumeNoTruncationLimit is a /index/volume `limit` above the number
// of rows any seeded case can produce (one per corpus stream at most),
// so the byte-volume ranking — which is not comparable across the two
// backends, see the file comment — never decides which rows the
// response carries.
const volumeNoTruncationLimit = 1000

// volumeAllLabelsTarget is the roster-facing spelling of the
// /index/volume case whose `targetLabels` is every label the reference
// advertises. The names are resolved at run time from the reference's
// /labels answer (so the case tracks the fixture), but the roster
// identity must not depend on that answer — it carries this placeholder
// instead.
const volumeAllLabelsTarget = "<every advertised label>"

// volumeTieBreakLimit is the `limit` of the aggregateBy=labels ranking
// case. It must be below the number of labels every seeded stream
// carries (9: cluster, container, datacenter, env, namespace, pod,
// region, service, service_name — cmd/seed/main.go buildStreams) so
// that the response is cut, and the cut falls entirely on tied rows.
const volumeTieBreakLimit = 3

// The two `aggregateBy` modes of /index/volume (pkg/storage/stores/
// index/seriesvolume/volume.go Series / Labels).
const (
	volumeAggregateBySeries = "series"
	volumeAggregateByLabels = "labels"
)

// metadataCase is one graded request: the route, its query parameters,
// and the roster identity fields.
type metadataCase struct {
	kind        string
	description string
	path        string
	// query is the selector the case sends (via `query` or `match[]`),
	// recorded on the roster so two cases on one route stay distinct.
	query  string
	params url.Values
}

// metadataBody is the decoded, comparable projection of one route's
// response. Exactly one of the per-kind fields is populated, selected
// by kind — the same tagged-union shape as typedResult.
type metadataBody struct {
	kind string
	// names carries /labels and /label/{name}/values.
	names []string
	// labelSets carries /series (one entry per stream) and
	// /index/volume (one entry per vector sample's `metric`).
	labelSets []map[string]string
	// stats carries /index/stats.
	stats indexStatsWire
	// cardinality carries /detected_labels, keyed by label name.
	cardinality map[string]uint64
}

// indexStatsWire mirrors upstream's bare IndexStatsResponse JSON
// (pkg/logproto: streams, chunks, entries, bytes).
type indexStatsWire struct {
	Streams uint64 `json:"streams"`
	Chunks  uint64 `json:"chunks"`
	Entries uint64 `json:"entries"`
	Bytes   uint64 `json:"bytes"`
}

// detectedLabelsWire mirrors upstream's bare DetectedLabelsResponse
// JSON. `sketch` is deliberately absent — see the file comment.
type detectedLabelsWire struct {
	DetectedLabels []struct {
		Label       string `json:"label"`
		Cardinality uint64 `json:"cardinality"`
	} `json:"detectedLabels"`
}

// isEmpty reports whether the body carries no data at all, in the
// sense the corpus path's "baseline returned empty" convention uses.
func (b metadataBody) isEmpty() bool {
	switch b.kind {
	case metadataKindLabels, metadataKindLabelValues:
		return len(b.names) == 0
	case metadataKindSeries, metadataKindIndexVolume:
		return len(b.labelSets) == 0
	case metadataKindIndexStats:
		return b.stats.Streams == 0 && b.stats.Entries == 0
	case metadataKindDetectedLabels:
		return len(b.cardinality) == 0
	}
	return true
}

// compareMetadataEndpointsAll runs every metadata case over the corpus
// window and returns one Result per case. The label-values cases and
// the every-label `targetLabels` volume case are driven by the
// REFERENCE's own /labels answer, as the detected-field values pass is
// driven by /detected_fields: whatever upstream advertises, cerberus
// must answer for, so the roster cannot quietly omit a label one
// backend forgot to advertise.
func compareMetadataEndpointsAll(c *http.Client, f flags, metadata *bench.DatasetMetadata) []Result {
	start, end := metadata.TimeRange.Start, metadata.TimeRange.End

	var results []Result
	for _, mc := range metadataFixedCases(start, end) {
		results = append(results, compareMetadataOne(c, f, mc, start, end))
	}

	advertised, err := fetchMetadata(c, f.addr1, metadataLabelsCase(metadataCorpusSelector, start, end))
	switch {
	case err != nil:
		results = append(results, Result{
			TestCase: metadataTestCase(metadataLabelValuesCase("*", metadataCorpusSelector, start, end), start, end),
			UnexpectedFailure: fmt.Sprintf(
				"reference (-addr-1) /labels failed, cannot enumerate label names: %v", err,
			),
		})
	case advertised.status != http.StatusOK || len(advertised.body.names) == 0:
		results = append(results, Result{
			TestCase: metadataTestCase(metadataLabelValuesCase("*", metadataCorpusSelector, start, end), start, end),
			UnexpectedFailure: fmt.Sprintf(
				"reference (-addr-1) /labels advertised no label names (status=%d body=%s)",
				advertised.status, errorBodySnippet(advertised.raw),
			),
		})
	default:
		// Every advertised label is diffed, in sorted order, for the
		// same reason the detected-field values pass diffs every
		// field: a sampled prefix would make the roster read as the
		// complete surface while the rest was never asked.
		names := slices.Clone(advertised.body.names)
		sort.Strings(names)
		for _, name := range names {
			results = append(results, compareMetadataOne(c, f, metadataLabelValuesCase(name, metadataCorpusSelector, start, end), start, end))
		}
		// Projecting every advertised label is the one series-mode
		// request whose rows carry a stream's FULL label set (the
		// matcher-derived default key — see metadataPerServiceSelector
		// — never does), so this is the case that grades one row per
		// seeded stream.
		allLabels := metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateBySeries, volumeNoTruncationLimit, volumeAllLabelsTarget, start, end)
		allLabels.params.Set("targetLabels", strings.Join(names, ","))
		results = append(results, compareMetadataOne(c, f, allLabels, start, end))
	}
	return results
}

// metadataFixedCases is the static case table: every route except
// label values (which is enumerated from the reference at run time),
// each with a corpus-wide request and a subset-selector request, plus
// the /index/volume mode, key-rule, targetLabels and tie-break cases.
func metadataFixedCases(start, end time.Time) []metadataCase {
	return []metadataCase{
		metadataLabelsCase(metadataCorpusSelector, start, end),
		metadataLabelsCase(metadataSubsetSelector, start, end),
		metadataLabelValuesCase(metadataSubsetLabel, metadataSubsetSelector, start, end),
		metadataSeriesCase(metadataCorpusSelector, start, end),
		metadataSeriesCase(metadataSubsetSelector, start, end),
		metadataIndexStatsCase(metadataCorpusSelector, start, end),
		metadataIndexStatsCase(metadataSubsetSelector, start, end),
		metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateBySeries, volumeNoTruncationLimit, "", start, end),
		metadataIndexVolumeCase(metadataPerServiceSelector, volumeAggregateBySeries, volumeNoTruncationLimit, "", start, end),
		metadataIndexVolumeCase(metadataSubsetSelector, volumeAggregateBySeries, volumeNoTruncationLimit, "", start, end),
		metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateBySeries, volumeNoTruncationLimit, "cluster", start, end),
		metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateBySeries, volumeNoTruncationLimit, "cluster,namespace", start, end),
		metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateByLabels, volumeNoTruncationLimit, "", start, end),
		metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateByLabels, volumeNoTruncationLimit, "cluster,namespace", start, end),
		metadataIndexVolumeCase(metadataCorpusSelector, volumeAggregateByLabels, volumeTieBreakLimit, "", start, end),
		metadataDetectedLabelsCase(metadataCorpusSelector, start, end),
		metadataDetectedLabelsCase(metadataSubsetSelector, start, end),
	}
}

func windowParams(start, end time.Time) url.Values {
	v := url.Values{}
	v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
	v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
	return v
}

// selectorDescription renders the selector half of a case description.
// Every case carries one — see metadataCorpusSelector for why a
// selector-less discovery request is not a well-posed differential.
func selectorDescription(selector string) string {
	return "selector=" + selector
}

func metadataLabelsCase(selector string, start, end time.Time) metadataCase {
	params := windowParams(start, end)
	if selector != "" {
		params.Set("query", selector)
	}
	return metadataCase{
		kind:        metadataKindLabels,
		description: "labels parity: " + selectorDescription(selector),
		path:        "/loki/api/v1/labels",
		query:       selector,
		params:      params,
	}
}

func metadataLabelValuesCase(name, selector string, start, end time.Time) metadataCase {
	params := windowParams(start, end)
	if selector != "" {
		params.Set("query", selector)
	}
	return metadataCase{
		kind:        metadataKindLabelValues,
		description: "label values parity: label=" + name + " " + selectorDescription(selector),
		path:        "/loki/api/v1/label/" + url.PathEscape(name) + "/values",
		query:       selector,
		params:      params,
	}
}

func metadataSeriesCase(selector string, start, end time.Time) metadataCase {
	params := windowParams(start, end)
	params.Set("match[]", selector)
	return metadataCase{
		kind:        metadataKindSeries,
		description: "series parity: " + selectorDescription(selector),
		path:        "/loki/api/v1/series",
		query:       selector,
		params:      params,
	}
}

func metadataIndexStatsCase(selector string, start, end time.Time) metadataCase {
	params := windowParams(start, end)
	params.Set("query", selector)
	return metadataCase{
		kind:        metadataKindIndexStats,
		description: "index stats parity (streams, entries): " + selectorDescription(selector),
		path:        "/loki/api/v1/index/stats",
		query:       selector,
		params:      params,
	}
}

func metadataIndexVolumeCase(selector, aggregateBy string, limit int, targetLabels string, start, end time.Time) metadataCase {
	params := windowParams(start, end)
	params.Set("query", selector)
	params.Set("aggregateBy", aggregateBy)
	params.Set("limit", strconv.Itoa(limit))
	desc := "index volume parity (label sets): aggregateBy=" + aggregateBy + " limit=" + strconv.Itoa(limit)
	if targetLabels != "" {
		params.Set("targetLabels", targetLabels)
		desc += " targetLabels=" + targetLabels
	}
	return metadataCase{
		kind:        metadataKindIndexVolume,
		description: desc + " " + selectorDescription(selector),
		path:        "/loki/api/v1/index/volume",
		query:       selector,
		params:      params,
	}
}

func metadataDetectedLabelsCase(selector string, start, end time.Time) metadataCase {
	params := windowParams(start, end)
	if selector != "" {
		params.Set("query", selector)
	}
	return metadataCase{
		kind:        metadataKindDetectedLabels,
		description: "detected labels parity (label, cardinality): " + selectorDescription(selector),
		path:        "/loki/api/v1/detected_labels",
		query:       selector,
		params:      params,
	}
}

func metadataTestCase(mc metadataCase, start, end time.Time) TestCase {
	return TestCase{
		Query:       mc.query,
		Source:      metadataEndpointsSource,
		Description: mc.description,
		Kind:        mc.kind,
		Direction:   "n/a",
		Start:       start.UTC().Format(time.RFC3339Nano),
		End:         end.UTC().Format(time.RFC3339Nano),
	}
}

// metadataFetch is one backend's answer to one case: the raw status
// and body (for the report), plus the decoded body when the status was
// 200 and the body decoded.
type metadataFetch struct {
	status int
	raw    string
	body   metadataBody
}

func compareMetadataOne(c *http.Client, f flags, mc metadataCase, start, end time.Time) Result {
	result := Result{TestCase: metadataTestCase(mc, start, end)}

	type fetched struct {
		fetch metadataFetch
		err   error
	}
	out := make([]fetched, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for idx, addr := range []string{f.addr1, f.addr2} {
		go func() {
			defer wg.Done()
			fetch, err := fetchMetadata(c, addr, mc)
			out[idx] = fetched{fetch: fetch, err: err}
		}()
	}
	wg.Wait()

	refErr, testErr := out[0].err, out[1].err
	switch {
	case refErr != nil:
		result.UnexpectedFailure = fmt.Sprintf("reference (-addr-1) failed: %v", refErr)
		return result
	case testErr != nil:
		result.UnexpectedFailure = fmt.Sprintf("test endpoint (-addr-2) failed: %v", testErr)
		return result
	}

	ref, test := out[0].fetch, out[1].fetch
	if ref.status != http.StatusOK {
		result.UnexpectedFailure = fmt.Sprintf(
			"reference (-addr-1) returned status=%d body=%s", ref.status, errorBodySnippet(ref.raw),
		)
		return result
	}
	if test.status != http.StatusOK {
		result.Diff = fmt.Sprintf(
			"status differs: reference=%d test endpoint=%d (test body=%s)",
			ref.status, test.status, errorBodySnippet(test.raw),
		)
		return result
	}
	if ref.body.isEmpty() {
		result.UnexpectedFailure = "baseline returned empty"
		return result
	}
	if test.body.isEmpty() {
		result.UnexpectedFailure = "test endpoint returned empty"
		return result
	}
	if diff := diffMetadataBody(ref.body, test.body); diff != "" {
		result.Diff = diff
	}
	return result
}

// fetchMetadata issues the case's GET against one backend and decodes
// the body when the status is 200. A non-200 is returned as data, not
// as an error — the status is compared by the caller. An error means
// the request never completed, or a 200 body did not decode as the
// route's documented shape (which is itself a parity finding the
// caller reports against the side that produced it).
func fetchMetadata(c *http.Client, addr string, mc metadataCase) (metadataFetch, error) {
	status, raw, err := fetchRawStatus(c, addr, mc.path, mc.params)
	if err != nil {
		return metadataFetch{}, err
	}
	fetch := metadataFetch{status: status, raw: raw}
	if status != http.StatusOK {
		return fetch, nil
	}
	body, err := decodeMetadataBody(mc.kind, []byte(raw))
	if err != nil {
		return metadataFetch{}, fmt.Errorf("%s: decode %s response: %w", mc.path, mc.kind, err)
	}
	fetch.body = body
	return fetch, nil
}

// decodeMetadataBody decodes one route's 200 body into its comparable
// projection, exactly as the consumer reads it: the enveloped routes
// (`{status, data}`) must carry status=success, and the bare routes
// (/index/stats, /detected_labels — upstream serialises the proto
// message verbatim) must NOT be wrapped in an envelope, the same
// consumer-grade check the detected-fields pass applies.
func decodeMetadataBody(kind string, body []byte) (metadataBody, error) {
	out := metadataBody{kind: kind}
	switch kind {
	case metadataKindLabels, metadataKindLabelValues:
		var data []string
		if err := decodeSuccessEnvelope(body, &data); err != nil {
			return out, err
		}
		out.names = data
	case metadataKindSeries:
		var data []map[string]string
		if err := decodeSuccessEnvelope(body, &data); err != nil {
			return out, err
		}
		out.labelSets = data
	case metadataKindIndexVolume:
		typed, err := decodeResponse(body)
		if err != nil {
			return out, err
		}
		if typed.kind != "vector" {
			return out, fmt.Errorf("resultType %q, want vector", typed.kind)
		}
		for _, sample := range typed.vector {
			out.labelSets = append(out.labelSets, sample.Metric)
		}
	case metadataKindIndexStats:
		if err := rejectEnvelope(body); err != nil {
			return out, err
		}
		if err := json.Unmarshal(body, &out.stats); err != nil {
			return out, err
		}
	case metadataKindDetectedLabels:
		if err := rejectEnvelope(body); err != nil {
			return out, err
		}
		var wire detectedLabelsWire
		if err := json.Unmarshal(body, &wire); err != nil {
			return out, err
		}
		out.cardinality = make(map[string]uint64, len(wire.DetectedLabels))
		for _, dl := range wire.DetectedLabels {
			out.cardinality[dl.Label] = dl.Cardinality
		}
	default:
		return out, fmt.Errorf("unknown metadata kind %q", kind)
	}
	return out, nil
}

// decodeSuccessEnvelope decodes a `{status, data}` body into data,
// requiring status=success.
func decodeSuccessEnvelope(body []byte, data any) error {
	var env struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if env.Status != "success" {
		return fmt.Errorf("envelope status=%q, want success", env.Status)
	}
	if len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, data)
}

// rejectEnvelope fails a body that carries a `data` key: the bare
// routes serialise their proto message at the top level, and an
// enveloped body decodes to zero fields in every consumer.
func rejectEnvelope(body []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return fmt.Errorf("decode top-level object: %w", err)
	}
	if _, ok := top["data"]; ok {
		return fmt.Errorf("response carries a {status,data} envelope; upstream serialises this route bare: %s", errorBodySnippet(string(body)))
	}
	return nil
}

// diffMetadataBody compares two decoded bodies of the same kind and
// returns a human-readable diff, or "" when they match. Set-valued
// kinds compare order-insensitively; see the file comment for which
// fields participate on each route.
func diffMetadataBody(expected, actual metadataBody) string {
	if expected.kind != actual.kind {
		return fmt.Sprintf("kind differs: expected=%s actual=%s", expected.kind, actual.kind)
	}
	switch expected.kind {
	case metadataKindLabels, metadataKindLabelValues:
		return diffStringSets("value", quoteAll(expected.names), quoteAll(actual.names))
	case metadataKindSeries, metadataKindIndexVolume:
		return diffStringSets("label set", renderLabelSets(expected.labelSets), renderLabelSets(actual.labelSets))
	case metadataKindIndexStats:
		var diffs []string
		if expected.stats.Streams != actual.stats.Streams {
			diffs = append(diffs, fmt.Sprintf("streams: expected=%d actual=%d", expected.stats.Streams, actual.stats.Streams))
		}
		if expected.stats.Entries != actual.stats.Entries {
			diffs = append(diffs, fmt.Sprintf("entries: expected=%d actual=%d", expected.stats.Entries, actual.stats.Entries))
		}
		return strings.Join(diffs, "; ")
	case metadataKindDetectedLabels:
		return diffCardinalities(expected.cardinality, actual.cardinality)
	}
	return "unknown metadata kind " + expected.kind
}

// diffStringSets compares two multisets of display-ready strings,
// naming every element missing from or unexpected on the test
// endpoint. Duplicates count: a backend that lists one series twice is
// not in agreement with one that lists it once.
func diffStringSets(noun string, expected, actual []string) string {
	exp := slices.Clone(expected)
	act := slices.Clone(actual)
	sort.Strings(exp)
	sort.Strings(act)

	var diffs []string
	i, j := 0, 0
	for i < len(exp) || j < len(act) {
		switch {
		case j >= len(act) || (i < len(exp) && exp[i] < act[j]):
			diffs = append(diffs, fmt.Sprintf("%s %s missing from test endpoint", noun, exp[i]))
			i++
		case i >= len(exp) || act[j] < exp[i]:
			diffs = append(diffs, fmt.Sprintf("%s %s unexpected on test endpoint", noun, act[j]))
			j++
		default:
			i++
			j++
		}
	}
	return strings.Join(diffs, "; ")
}

// quoteAll renders plain strings display-ready for diffStringSets.
func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strconv.Quote(s))
	}
	return out
}

// renderLabelSets renders each label map in canonical `{k="v", ...}`
// form (keys sorted) so two maps compare as strings.
func renderLabelSets(sets []map[string]string) []string {
	out := make([]string, 0, len(sets))
	for _, set := range sets {
		keys := sortedKeys(set)
		pairs := make([]string, 0, len(keys))
		for _, k := range keys {
			pairs = append(pairs, k+"="+strconv.Quote(set[k]))
		}
		out = append(out, "{"+strings.Join(pairs, ", ")+"}")
	}
	return out
}

func diffCardinalities(expected, actual map[string]uint64) string {
	var diffs []string
	expNames := make([]string, 0, len(expected))
	for name := range expected {
		expNames = append(expNames, name)
	}
	sort.Strings(expNames)
	for _, name := range expNames {
		got, ok := actual[name]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("label %q missing from test endpoint", name))
			continue
		}
		if want := expected[name]; want != got {
			diffs = append(diffs, fmt.Sprintf("label %q cardinality: expected=%d actual=%d", name, want, got))
		}
	}
	actNames := make([]string, 0, len(actual))
	for name := range actual {
		actNames = append(actNames, name)
	}
	sort.Strings(actNames)
	for _, name := range actNames {
		if _, ok := expected[name]; !ok {
			diffs = append(diffs, fmt.Sprintf("label %q unexpected on test endpoint", name))
		}
	}
	return strings.Join(diffs, "; ")
}
