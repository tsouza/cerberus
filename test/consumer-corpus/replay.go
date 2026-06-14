package consumercorpus

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/tempopb"
)

// Replay sends entry e against baseURL (an httptest server hosting the
// entry's datasource handler), expands ${TOKEN} placeholders from
// tokens, and returns every contract violation found — status
// mismatch, consumer-decode failure, failed predicates. includeData
// selects whether the chdb-only Data predicates run.
//
// All checks run; the slice aggregates every failure so a corpus run
// reports the full picture instead of stopping at the first mismatch.
func Replay(client *http.Client, baseURL string, e Entry, tokens map[string]string, includeData bool) []error {
	req, err := buildRequest(baseURL, e, tokens)
	if err != nil {
		return []error{err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return []error{fmt.Errorf("request failed: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return []error{fmt.Errorf("read body: %w", err)}
	}

	wantStatus := e.Expect.Status
	if wantStatus == 0 {
		wantStatus = http.StatusOK
	}
	var errs []error
	if resp.StatusCode != wantStatus {
		errs = append(errs, fmt.Errorf("status = %d, want %d; body: %s", resp.StatusCode, wantStatus, truncate(body, 500)))
		// A wrong status makes decode + predicate output pure noise;
		// the status error already carries the body.
		return errs
	}

	dec, ok := decoders[e.Expect.Decode]
	if !ok {
		return append(errs, fmt.Errorf("unknown decoder %q", e.Expect.Decode))
	}
	v, err := dec(resp.Header, body)
	if err != nil {
		return append(errs, fmt.Errorf("consumer decode (%s): %w; body: %s", e.Expect.Decode, err, truncate(body, 500)))
	}

	preds := append([]string{}, e.Expect.Wire...)
	if includeData {
		preds = append(preds, e.Expect.Data...)
	}
	for _, p := range preds {
		name, arg, err := splitPredicate(p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fn, ok := predicates[name]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown predicate %q", name))
			continue
		}
		if err := fn(v, arg); err != nil {
			errs = append(errs, fmt.Errorf("predicate %s: %w", p, err))
		}
	}
	return errs
}

// buildRequest assembles the entry's HTTP request with token expansion
// applied to the path and every query value.
func buildRequest(baseURL string, e Entry, tokens map[string]string) (*http.Request, error) {
	path, err := expandTokens(e.Request.Path, tokens)
	if err != nil {
		return nil, err
	}
	vals := url.Values{}
	for k, v := range e.Request.Query {
		ev, err := expandTokens(v, tokens)
		if err != nil {
			return nil, err
		}
		vals.Set(k, ev)
	}
	u := baseURL + path
	if len(vals) > 0 {
		u += "?" + vals.Encode()
	}
	req, err := http.NewRequest(e.Request.Method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range e.Request.Headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// expandTokens substitutes every ${NAME} placeholder and errors on
// placeholders the lane didn't provide — an unexpanded token would
// silently turn into a bogus literal parameter.
func expandTokens(s string, tokens map[string]string) (string, error) {
	out := s
	for k, v := range tokens {
		out = strings.ReplaceAll(out, "${"+k+"}", v)
	}
	if i := strings.Index(out, "${"); i >= 0 {
		return "", fmt.Errorf("unexpanded token in %q — the replay lane must provide it", s)
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// --- Consumer-view response types ----------------------------------
//
// These structs mirror what each consumer READS, field by field.
// Where the consumer strict-decodes (gogo proto), we strict-decode;
// where the consumer is JavaScript reading named keys (JSON.parse),
// the struct pins exactly those keys.

// searchView is the field set Grafana's Tempo datasource
// resultTransformer reads from /api/search (tableType='spans' builds
// the trace-list table exclusively from traces[].spanSets[].spans).
type searchView struct {
	Traces []searchTrace `json:"traces"`
}

type searchTrace struct {
	TraceID  string          `json:"traceID"`
	SpanSets []searchSpanSet `json:"spanSets"`
	SpanSet  *searchSpanSet  `json:"spanSet"`
}

type searchSpanSet struct {
	Spans []searchSpan `json:"spans"`
}

type searchSpan struct {
	SpanID            string       `json:"spanID"`
	Name              string       `json:"name"`
	StartTimeUnixNano string       `json:"startTimeUnixNano"`
	DurationNanos     string       `json:"durationNanos"`
	Attributes        []searchAttr `json:"attributes"`
}

type searchAttr struct {
	Key   string `json:"key"`
	Value struct {
		StringValue *string `json:"stringValue"`
		IntValue    *string `json:"intValue"`
	} `json:"value"`
}

// metricsRangeView is the field set the Traces Drilldown scenes read
// from /api/metrics/query_range — tempopb QueryRangeResponse rendered
// through gogo jsonpb (labels as KeyValue+AnyValue, samples with
// timestampMs).
type metricsRangeView struct {
	Series []metricsSeriesView `json:"series"`
}

type metricsSeriesView struct {
	Labels []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	} `json:"labels"`
	Samples []struct {
		TimestampMs int64   `json:"timestampMs"`
		Value       float64 `json:"value"`
	} `json:"samples"`
	Exemplars json.RawMessage `json:"exemplars"`
}

// detectedFieldsView is the bare logproto.DetectedFieldsResponse JSON
// shape Logs Drilldown reads: top-level `fields`, NO {status, data}
// envelope (upstream pkg/util/marshal WriteDetectedFieldsResponseJSON).
type detectedFieldsView struct {
	Fields []detectedFieldView
	Limit  json.RawMessage
	// raw keeps the top-level key set so the bare-envelope contract
	// (no status/data wrapper) is assertable.
	raw map[string]json.RawMessage
}

type detectedFieldView struct {
	Label       string          `json:"label"`
	Type        string          `json:"type"`
	Cardinality float64         `json:"cardinality"`
	Parsers     json.RawMessage `json:"parsers"`
}

// lokiEnvelopeView is the Loki query-API envelope Grafana's Loki
// datasource backend decodes from /loki/api/v1/query_range.
type lokiEnvelopeView struct {
	ResultType    string
	EncodingFlags []string
	Matrix        []matrixSeriesView
	Streams       []streamView
}

type matrixSeriesView struct {
	Metric map[string]string `json:"metric"`
	Values [][2]any          `json:"values"`
}

type streamView struct {
	Stream map[string]string   `json:"stream"`
	Values [][]json.RawMessage `json:"values"`
}

// checkStreamValueArity validates each stream value tuple against the
// EXACT shape Grafana's strict response parser
// (pkg/promlib/converter.readResult) accepts for the negotiated mode.
// The contract is arity-precise, not a range — Grafana branches on the
// `categorize-labels` encoding flag alone and then reads a fixed tuple
// shape from EVERY value:
//
//   - non-categorized (`readStream`): exactly two elements `[ts, line]`.
//     A stray third element 400s with `ReadArray: expect [ or , or ] or
//     n, but found {` — the #908 plain-query break.
//   - categorized (`readCategorizedStream`): exactly three elements
//     `[ts, line, {…}]`. After the line the parser unconditionally calls
//     `iter.ReadArray()` + `readCategorizedStreamField`, so a TWO-element
//     value (closing `]` where the object is expected) 400s with
//     `ReadObject: expect { or , or } or n, but found ]` — the #908
//     shard-smoke break (run 27503329607). The third element MUST be a
//     JSON object; `readCategorizedStreamField` reads its `structuredMetadata`
//     / `parsed` sub-objects, so a non-object (array / string / null)
//     third element also breaks the parser.
func checkStreamValueArity(streams []streamView, categorized bool) error {
	wantArity := 2
	if categorized {
		wantArity = 3
	}
	for si, s := range streams {
		for vi, val := range s.Values {
			if len(val) != wantArity {
				return fmt.Errorf(
					"stream[%d] value[%d] has %d elements, want exactly %d (categorize-labels=%t) — Grafana's readResult reads a fixed-arity tuple from every value, a mismatch 400s its parser",
					si, vi, len(val), wantArity, categorized,
				)
			}
			if categorized {
				if err := checkCategorizedThird(val[2]); err != nil {
					return fmt.Errorf("stream[%d] value[%d]: %w", si, vi, err)
				}
			}
		}
	}
	return nil
}

// checkCategorizedThird validates the third element of a categorized
// stream value against what Grafana's readCategorizedStreamField reads:
// a JSON object whose recognised keys are `structuredMetadata` and/or
// `parsed`, each mapping to a flat `{string: string}` label set. An empty
// `{}` is valid (a metadata-free row still carries the envelope). A bare
// metadata map (`{"thread":"x"}` instead of `{"structuredMetadata":{…}}`)
// is rejected: the converter would read those keys as label-type fields,
// not as metadata, so the columns would never surface — exactly the
// "zero 3-tuples / no columns" symptom #908's loki_explore_columns guard
// caught.
func checkCategorizedThird(raw json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("categorized third element must be a JSON object, got %s: %w", truncate(raw, 80), err)
	}
	// At least one recognised envelope key must be present (an empty `{}`
	// is allowed — it is the metadata-free row). Any OTHER top-level key
	// means the producer emitted a bare metadata map instead of the
	// categorized envelope.
	for k, v := range obj {
		switch k {
		case "structuredMetadata", "parsed":
			var labels map[string]string
			if err := json.Unmarshal(v, &labels); err != nil {
				return fmt.Errorf("categorized %q must be a {string:string} object, got %s: %w", k, truncate(v, 80), err)
			}
		default:
			return fmt.Errorf(
				"categorized third element carries unexpected top-level key %q — want only structuredMetadata/parsed; a bare metadata map (e.g. {%q:…}) is read as label-type fields, not metadata, so no columns surface",
				k, k,
			)
		}
	}
	return nil
}

// promEnvelopeView is the Prometheus query-API envelope.
type promEnvelopeView struct {
	ResultType string
	Vector     []vectorSampleView
	Matrix     []matrixSeriesView
}

type vectorSampleView struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// promStringsView is the /api/v1/labels and /api/v1/label/X/values
// data shape.
type promStringsView struct{ Data []string }

// promSeriesView is the /api/v1/series data shape.
type promSeriesView struct{ Data []map[string]string }

// promMetadataView is the /api/v1/metadata data shape.
type promMetadataView struct {
	Data map[string][]struct {
		Type string `json:"type"`
		Help string `json:"help"`
		Unit string `json:"unit"`
	}
}

// --- Decoder registry ------------------------------------------------

type decodeFunc func(h http.Header, body []byte) (any, error)

var decoders = map[string]decodeFunc{
	// Grafana 12's Tempo plugin backend unmarshals the
	// /api/v2/traces/{id} proto body as tempopb.TraceByIDResponse —
	// the exact decode that died one message level deep when cerberus
	// served un-enveloped Trace bytes (#764).
	"tempo-trace-v2-proto": func(h http.Header, body []byte) (any, error) {
		if ct := h.Get("Content-Type"); ct != "application/protobuf" {
			return nil, fmt.Errorf("Content-Type = %q, want application/protobuf", ct)
		}
		v := &tempopb.TraceByIDResponse{}
		if err := proto.Unmarshal(body, v); err != nil {
			return nil, fmt.Errorf("tempopb.TraceByIDResponse unmarshal: %w", err)
		}
		return v, nil
	},
	// The v1 endpoint serves the bare trace (pre-Grafana-12 decode
	// and the v1↔v2 divergence pin).
	"tempo-trace-v1-proto": func(h http.Header, body []byte) (any, error) {
		if ct := h.Get("Content-Type"); ct != "application/protobuf" {
			return nil, fmt.Errorf("Content-Type = %q, want application/protobuf", ct)
		}
		v := &tempopb.Trace{}
		if err := proto.Unmarshal(body, v); err != nil {
			return nil, fmt.Errorf("tempopb.Trace unmarshal: %w", err)
		}
		return v, nil
	},
	"tempo-search": func(_ http.Header, body []byte) (any, error) {
		var v searchView
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		if v.Traces == nil {
			return nil, fmt.Errorf("traces is null — Tempo serves [], and Grafana iterates it unconditionally")
		}
		return &v, nil
	},
	"tempo-metrics-range": func(_ http.Header, body []byte) (any, error) {
		var v metricsRangeView
		if err := json.Unmarshal(body, &v); err != nil {
			return nil, err
		}
		return &v, nil
	},
	"loki-detected-fields": func(_ http.Header, body []byte) (any, error) {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, err
		}
		// The consumer reads body.fields at the TOP level; a {status,
		// data} wrapper renders "Fields: 0" with HTTP 200 (#774).
		if _, ok := raw["status"]; ok {
			return nil, fmt.Errorf("body carries a top-level \"status\" key — detected_fields is served BARE, not in the query-API envelope")
		}
		if _, ok := raw["data"]; ok {
			return nil, fmt.Errorf("body carries a top-level \"data\" key — detected_fields is served BARE, not in the query-API envelope")
		}
		fieldsRaw, ok := raw["fields"]
		if !ok {
			return nil, fmt.Errorf("top-level \"fields\" key missing")
		}
		v := detectedFieldsView{Limit: raw["limit"], raw: raw}
		if err := json.Unmarshal(fieldsRaw, &v.Fields); err != nil {
			return nil, fmt.Errorf("fields decode: %w", err)
		}
		if v.Fields == nil {
			return nil, fmt.Errorf("fields is null, want an array")
		}
		return &v, nil
	},
	"loki-envelope": func(_ http.Header, body []byte) (any, error) {
		var env struct {
			Status string `json:"status"`
			Data   struct {
				ResultType    string          `json:"resultType"`
				EncodingFlags []string        `json:"encodingFlags"`
				Result        json.RawMessage `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Status != "success" {
			return nil, fmt.Errorf("status = %q, want success; body: %s", env.Status, truncate(body, 300))
		}
		v := lokiEnvelopeView{ResultType: env.Data.ResultType, EncodingFlags: env.Data.EncodingFlags}
		switch env.Data.ResultType {
		case "matrix", "vector":
			if err := json.Unmarshal(env.Data.Result, &v.Matrix); err != nil {
				return nil, fmt.Errorf("%s result decode: %w", env.Data.ResultType, err)
			}
		case "streams":
			if err := json.Unmarshal(env.Data.Result, &v.Streams); err != nil {
				return nil, fmt.Errorf("streams result decode: %w", err)
			}
			// Mirror Grafana's strict stream parser
			// (promlib/converter.ReadPrometheusStyleResult): a stream value
			// tuple is two-element `[ts, line]` UNLESS the response
			// advertises the `categorize-labels` encoding flag, in which
			// case it carries a categorized `{...}` third element. A bare
			// third element WITHOUT the flag is exactly the #908 break —
			// Grafana's `readStream` branch 400s on it with
			// `ReadArray: expect [ or , or ] or n, but found {`. This guard
			// catches that off-compose, where the lenient Go json.Unmarshal
			// that previously decoded into [][2]string silently dropped the
			// stray element.
			categorized := slices.Contains(env.Data.EncodingFlags, "categorize-labels")
			if err := checkStreamValueArity(v.Streams, categorized); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("resultType = %q, want matrix/vector/streams", env.Data.ResultType)
		}
		return &v, nil
	},
	"prom-envelope": func(_ http.Header, body []byte) (any, error) {
		var env struct {
			Status string `json:"status"`
			Data   struct {
				ResultType string          `json:"resultType"`
				Result     json.RawMessage `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Status != "success" {
			return nil, fmt.Errorf("status = %q, want success; body: %s", env.Status, truncate(body, 300))
		}
		v := promEnvelopeView{ResultType: env.Data.ResultType}
		switch env.Data.ResultType {
		case "vector":
			if err := json.Unmarshal(env.Data.Result, &v.Vector); err != nil {
				return nil, fmt.Errorf("vector result decode: %w", err)
			}
		case "matrix":
			if err := json.Unmarshal(env.Data.Result, &v.Matrix); err != nil {
				return nil, fmt.Errorf("matrix result decode: %w", err)
			}
		default:
			return nil, fmt.Errorf("resultType = %q, want vector/matrix", env.Data.ResultType)
		}
		return &v, nil
	},
	"prom-strings": func(_ http.Header, body []byte) (any, error) {
		var env struct {
			Status string   `json:"status"`
			Data   []string `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Status != "success" {
			return nil, fmt.Errorf("status = %q, want success; body: %s", env.Status, truncate(body, 300))
		}
		if env.Data == nil {
			return nil, fmt.Errorf("data is null, want an array")
		}
		return &promStringsView{Data: env.Data}, nil
	},
	"prom-series": func(_ http.Header, body []byte) (any, error) {
		var env struct {
			Status string              `json:"status"`
			Data   []map[string]string `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Status != "success" {
			return nil, fmt.Errorf("status = %q, want success; body: %s", env.Status, truncate(body, 300))
		}
		if env.Data == nil {
			return nil, fmt.Errorf("data is null, want an array")
		}
		return &promSeriesView{Data: env.Data}, nil
	},
	"prom-metadata": func(_ http.Header, body []byte) (any, error) {
		var env struct {
			Status string `json:"status"`
			Data   map[string][]struct {
				Type string `json:"type"`
				Help string `json:"help"`
				Unit string `json:"unit"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		if env.Status != "success" {
			return nil, fmt.Errorf("status = %q, want success; body: %s", env.Status, truncate(body, 300))
		}
		return &promMetadataView{Data: env.Data}, nil
	},
}

// --- Predicate registry ----------------------------------------------

type predicateFunc func(v any, arg string) error

var predicates = map[string]predicateFunc{
	// tempopb proto predicates.
	"trace-present": func(v any, _ string) error {
		r, ok := v.(*tempopb.TraceByIDResponse)
		if !ok {
			return typeErr(v)
		}
		if r.Trace == nil {
			return fmt.Errorf("envelope.Trace is nil — the trace must ride field 1 of TraceByIDResponse")
		}
		return nil
	},
	"metrics-block-present": func(v any, _ string) error {
		r, ok := v.(*tempopb.TraceByIDResponse)
		if !ok {
			return typeErr(v)
		}
		if r.Metrics == nil {
			return fmt.Errorf("envelope.Metrics is nil — reference Tempo always emits the (zero) metrics block")
		}
		return nil
	},
	"trace-resource-spans-min": func(v any, arg string) error {
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		var got int
		switch r := v.(type) {
		case *tempopb.TraceByIDResponse:
			if r.Trace == nil {
				return fmt.Errorf("envelope.Trace is nil")
			}
			got = len(r.Trace.ResourceSpans)
		case *tempopb.Trace:
			got = len(r.ResourceSpans)
		default:
			return typeErr(v)
		}
		if got < n {
			return fmt.Errorf("resourceSpans = %d, want >= %d", got, n)
		}
		return nil
	},

	// tempo /api/search predicates.
	"traces-min": searchCount(func(n, want int) error {
		if n < want {
			return fmt.Errorf("traces = %d, want >= %d", n, want)
		}
		return nil
	}),
	"traces-max": searchCount(func(n, want int) error {
		if n > want {
			return fmt.Errorf("traces = %d, want <= %d (the request's limit param)", n, want)
		}
		return nil
	}),
	"every-trace-has-spansets": func(v any, _ string) error {
		s, ok := v.(*searchView)
		if !ok {
			return typeErr(v)
		}
		for _, tr := range s.Traces {
			if len(tr.SpanSets) == 0 {
				return fmt.Errorf("trace %s has no spanSets — Grafana's tableType='spans' transform renders zero rows without them (#770)", tr.TraceID)
			}
			if tr.SpanSet == nil {
				return fmt.Errorf("trace %s has no legacy spanSet mirror (tempopb field 6)", tr.TraceID)
			}
			for i, set := range tr.SpanSets {
				if len(set.Spans) == 0 {
					return fmt.Errorf("trace %s spanSets[%d].spans is empty", tr.TraceID, i)
				}
			}
		}
		return nil
	},
	"spanset-spans-max": func(v any, arg string) error {
		s, ok := v.(*searchView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		for _, tr := range s.Traces {
			for i, set := range tr.SpanSets {
				if len(set.Spans) > n {
					return fmt.Errorf("trace %s spanSets[%d] carries %d spans, want <= %d (the request's spss param)", tr.TraceID, i, len(set.Spans), n)
				}
			}
		}
		return nil
	},
	// every-span-int-attr asserts every spanSet span carries each of
	// the comma-separated keys as an OTLP intValue attribute. The
	// Traces Drilldown structure tab's utils.ts nestedSetLeft()
	// THROWS when these are missing.
	"every-span-int-attr": func(v any, arg string) error {
		s, ok := v.(*searchView)
		if !ok {
			return typeErr(v)
		}
		keys := strings.Split(arg, ",")
		for _, tr := range s.Traces {
			for _, set := range tr.SpanSets {
				for _, sp := range set.Spans {
					for _, key := range keys {
						if !spanHasIntAttr(sp, key, nil) {
							return fmt.Errorf("trace %s span %s lacks intValue attribute %q", tr.TraceID, sp.SpanID, key)
						}
					}
				}
			}
		}
		return nil
	},
	// some-span-int-attr asserts at least one span carries key=value
	// as an intValue attribute (form "key=value").
	"some-span-int-attr": func(v any, arg string) error {
		s, ok := v.(*searchView)
		if !ok {
			return typeErr(v)
		}
		key, val, found := strings.Cut(arg, "=")
		if !found {
			return fmt.Errorf("arg %q must be key=value", arg)
		}
		for _, tr := range s.Traces {
			for _, set := range tr.SpanSets {
				for _, sp := range set.Spans {
					if spanHasIntAttr(sp, key, &val) {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("no span carries intValue attribute %s=%s", key, val)
	},
	// some-span-str-attr mirrors some-span-int-attr for stringValue.
	"some-span-str-attr": func(v any, arg string) error {
		s, ok := v.(*searchView)
		if !ok {
			return typeErr(v)
		}
		key, val, found := strings.Cut(arg, "=")
		if !found {
			return fmt.Errorf("arg %q must be key=value", arg)
		}
		for _, tr := range s.Traces {
			for _, set := range tr.SpanSets {
				for _, sp := range set.Spans {
					for _, a := range sp.Attributes {
						if a.Key == key && a.Value.StringValue != nil && *a.Value.StringValue == val {
							return nil
						}
					}
				}
			}
		}
		return fmt.Errorf("no span carries stringValue attribute %s=%s", key, val)
	},

	// tempo /api/metrics/query_range predicates.
	"series-min": func(v any, arg string) error {
		m, ok := v.(*metricsRangeView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		if len(m.Series) < n {
			return fmt.Errorf("series = %d, want >= %d", len(m.Series), n)
		}
		return nil
	},
	// every-series-label-key asserts each series carries a label with
	// the given key — the breakdown scenes key their bar charts on the
	// group-by attribute.
	"every-series-label-key": func(v any, arg string) error {
		m, ok := v.(*metricsRangeView)
		if !ok {
			return typeErr(v)
		}
		for i, s := range m.Series {
			found := false
			for _, l := range s.Labels {
				if l.Key == arg {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("series[%d] lacks label key %q (labels: %+v)", i, arg, s.Labels)
			}
		}
		return nil
	},
	// compare-meta-types asserts each comma-separated __meta_type
	// value appears as the LEADING label of at least one series —
	// the Comparison tab splits cohorts on it.
	"compare-meta-types": func(v any, arg string) error {
		m, ok := v.(*metricsRangeView)
		if !ok {
			return typeErr(v)
		}
		seen := map[string]bool{}
		for _, s := range m.Series {
			if len(s.Labels) == 0 || s.Labels[0].Key != "__meta_type" {
				return fmt.Errorf("compare() series must lead with __meta_type (Tempo label order); got %+v", s.Labels)
			}
			seen[s.Labels[0].Value.StringValue] = true
		}
		for _, want := range strings.Split(arg, ",") {
			if !seen[want] {
				return fmt.Errorf("no series with __meta_type=%s (got %v)", want, seen)
			}
		}
		return nil
	},
	"exemplars-not-null": func(v any, _ string) error {
		m, ok := v.(*metricsRangeView)
		if !ok {
			return typeErr(v)
		}
		for i, s := range m.Series {
			if string(s.Exemplars) == "" || string(s.Exemplars) == "null" {
				return fmt.Errorf("series[%d].exemplars is %q, want a JSON array (stable envelope contract)", i, s.Exemplars)
			}
		}
		return nil
	},

	// loki detected_fields predicates.
	"fields-min": func(v any, arg string) error {
		d, ok := v.(*detectedFieldsView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		if len(d.Fields) < n {
			return fmt.Errorf("fields = %d, want >= %d", len(d.Fields), n)
		}
		return nil
	},
	"fields-shape": func(v any, _ string) error {
		d, ok := v.(*detectedFieldsView)
		if !ok {
			return typeErr(v)
		}
		for i, f := range d.Fields {
			if f.Label == "" {
				return fmt.Errorf("fields[%d].label is empty", i)
			}
			if f.Type == "" {
				return fmt.Errorf("fields[%d] (%s) has empty type", i, f.Label)
			}
			if f.Cardinality <= 0 {
				return fmt.Errorf("fields[%d] (%s) cardinality = %v, want > 0", i, f.Label, f.Cardinality)
			}
			if len(f.Parsers) == 0 {
				return fmt.Errorf("fields[%d] (%s) parsers key missing — it is always on the wire (null for structured metadata)", i, f.Label)
			}
		}
		return nil
	},
	"field-typed": func(v any, arg string) error {
		d, ok := v.(*detectedFieldsView)
		if !ok {
			return typeErr(v)
		}
		label, typ, found := strings.Cut(arg, "=")
		if !found {
			return fmt.Errorf("arg %q must be label=type", arg)
		}
		for _, f := range d.Fields {
			if f.Label == label {
				if f.Type != typ {
					return fmt.Errorf("field %s type = %q, want %q", label, f.Type, typ)
				}
				return nil
			}
		}
		return fmt.Errorf("field %s absent from detected fields", label)
	},

	// loki/prom envelope predicates.
	"resulttype": func(v any, arg string) error {
		switch e := v.(type) {
		case *lokiEnvelopeView:
			if e.ResultType != arg {
				return fmt.Errorf("resultType = %q, want %q", e.ResultType, arg)
			}
		case *promEnvelopeView:
			if e.ResultType != arg {
				return fmt.Errorf("resultType = %q, want %q", e.ResultType, arg)
			}
		default:
			return typeErr(v)
		}
		return nil
	},
	"result-min": func(v any, arg string) error {
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		var got int
		switch e := v.(type) {
		case *lokiEnvelopeView:
			got = len(e.Matrix) + len(e.Streams)
		case *promEnvelopeView:
			got = len(e.Vector) + len(e.Matrix)
		default:
			return typeErr(v)
		}
		if got < n {
			return fmt.Errorf("result entries = %d, want >= %d", got, n)
		}
		return nil
	},
	// matrix-values-finite-positive: every matrix sample parses as a
	// finite float > 0. A duration misparse surfaces here either as a
	// CH error upstream (status != 200) or as garbage values. Series
	// stamped with __error__ are excluded from the bound: reference
	// Loki degrades unparseable unwrap sources to 0-valued samples in
	// an __error__-labelled bypass series rather than aborting, so 0
	// is the CORRECT value there.
	"matrix-values-finite-positive": func(v any, _ string) error {
		series, err := matrixOf(v)
		if err != nil {
			return err
		}
		for _, s := range series {
			if _, errStamped := s.Metric["__error__"]; errStamped {
				continue
			}
			for _, pt := range s.Values {
				f, err := samplePointValue(pt)
				if err != nil {
					return err
				}
				if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
					return fmt.Errorf("series %v sample value %v, want a finite positive float", s.Metric, f)
				}
			}
		}
		return nil
	},
	// matrix-max-value: every matrix sample value <= arg. For the
	// drilldown duration queries this is the unit-sanity oracle: the
	// seeded durations are all <= 1.5 SECONDS, so a value above the
	// bound means a sub-second duration was misparsed (e.g. "812µs"
	// read as 812 of some larger unit).
	"matrix-max-value": func(v any, arg string) error {
		bound, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return fmt.Errorf("arg %q is not a float: %w", arg, err)
		}
		series, err := matrixOf(v)
		if err != nil {
			return err
		}
		for _, s := range series {
			for _, pt := range s.Values {
				f, err := samplePointValue(pt)
				if err != nil {
					return err
				}
				if f > bound {
					return fmt.Errorf("series %v sample value %g exceeds the unit-sanity bound %g", s.Metric, f, bound)
				}
			}
		}
		return nil
	},
	// series-max-value (arg "label=value,bound"): the matrix series
	// carrying label=value must exist and every one of its sample
	// values must be <= bound. This is the sharp per-series unit
	// oracle: e.g. the seeded error-level durations sum to ~0.4008 s,
	// so a µs row misparsed as ms (×1000) or s (×10⁶) breaches the
	// bound while the correct parse stays under it.
	"series-max-value": func(v any, arg string) error {
		spec, boundStr, found := strings.Cut(arg, ",")
		if !found {
			return fmt.Errorf("arg %q must be label=value,bound", arg)
		}
		key, val, found := strings.Cut(spec, "=")
		if !found {
			return fmt.Errorf("arg %q must be label=value,bound", arg)
		}
		bound, err := strconv.ParseFloat(boundStr, 64)
		if err != nil {
			return fmt.Errorf("bound %q is not a float: %w", boundStr, err)
		}
		series, err := matrixOf(v)
		if err != nil {
			return err
		}
		matched := false
		for _, s := range series {
			if s.Metric[key] != val {
				continue
			}
			matched = true
			for _, pt := range s.Values {
				f, err := samplePointValue(pt)
				if err != nil {
					return err
				}
				if f > bound {
					return fmt.Errorf("series %v sample value %g exceeds the per-series bound %g", s.Metric, f, bound)
				}
			}
		}
		if !matched {
			return fmt.Errorf("no matrix series with %s=%s", key, val)
		}
		return nil
	},
	"streams-values-min": func(v any, arg string) error {
		e, ok := v.(*lokiEnvelopeView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		total := 0
		for _, s := range e.Streams {
			total += len(s.Values)
		}
		if total < n {
			return fmt.Errorf("total stream values = %d, want >= %d", total, n)
		}
		return nil
	},
	// encoding-flag: the streams response advertises the named
	// encodingFlag (e.g. categorize-labels) so Grafana's converter takes
	// the matching parser branch. The flag and the per-value arity are one
	// contract — checkStreamValueArity already pins the arity; this pins
	// the advertisement.
	"encoding-flag": func(v any, arg string) error {
		e, ok := v.(*lokiEnvelopeView)
		if !ok {
			return typeErr(v)
		}
		if !slices.Contains(e.EncodingFlags, arg) {
			return fmt.Errorf("encodingFlags %v lacks %q", e.EncodingFlags, arg)
		}
		return nil
	},
	// categorized-metadata-min: at least n stream values carry a NON-EMPTY
	// `structuredMetadata` object in their categorized third element. This
	// is the off-compose mirror of the loki_explore_columns.spec.ts
	// `at least one 3-tuple carries structured metadata` assertion — it
	// fails if the producer advertised categorize-labels but never
	// actually surfaced any metadata (the #908 shard-kiosk "ZERO 3-tuples"
	// symptom).
	"categorized-metadata-min": func(v any, arg string) error {
		e, ok := v.(*lokiEnvelopeView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		withMeta := 0
		for _, s := range e.Streams {
			for _, val := range s.Values {
				if len(val) < 3 {
					continue
				}
				var third struct {
					StructuredMetadata map[string]string `json:"structuredMetadata"`
				}
				if err := json.Unmarshal(val[2], &third); err != nil {
					continue
				}
				if len(third.StructuredMetadata) > 0 {
					withMeta++
				}
			}
		}
		if withMeta < n {
			return fmt.Errorf("stream values carrying non-empty structuredMetadata = %d, want >= %d", withMeta, n)
		}
		return nil
	},
	// every-series-metric-label: each matrix/vector entry carries the
	// given label key — the by(<label>) grouping contract.
	"every-series-metric-label": func(v any, arg string) error {
		var metrics []map[string]string
		switch e := v.(type) {
		case *lokiEnvelopeView:
			for _, s := range e.Matrix {
				metrics = append(metrics, s.Metric)
			}
		case *promEnvelopeView:
			for _, s := range e.Vector {
				metrics = append(metrics, s.Metric)
			}
			for _, s := range e.Matrix {
				metrics = append(metrics, s.Metric)
			}
		default:
			return typeErr(v)
		}
		for i, m := range metrics {
			if _, ok := m[arg]; !ok {
				return fmt.Errorf("result[%d] metric %v lacks label %q", i, m, arg)
			}
		}
		return nil
	},

	// prom strings/series/metadata predicates.
	"strings-contains": func(v any, arg string) error {
		s, ok := v.(*promStringsView)
		if !ok {
			return typeErr(v)
		}
		for _, x := range s.Data {
			if x == arg {
				return nil
			}
		}
		return fmt.Errorf("%q absent from data %v", arg, s.Data)
	},
	"series-count-min": func(v any, arg string) error {
		s, ok := v.(*promSeriesView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		if len(s.Data) < n {
			return fmt.Errorf("series = %d, want >= %d", len(s.Data), n)
		}
		return nil
	},
	"every-series-has-label": func(v any, arg string) error {
		s, ok := v.(*promSeriesView)
		if !ok {
			return typeErr(v)
		}
		for i, m := range s.Data {
			if _, ok := m[arg]; !ok {
				return fmt.Errorf("series[%d] %v lacks label %q", i, m, arg)
			}
		}
		return nil
	},
	"metadata-contains": func(v any, arg string) error {
		m, ok := v.(*promMetadataView)
		if !ok {
			return typeErr(v)
		}
		rows, present := m.Data[arg]
		if !present {
			return fmt.Errorf("metric %q absent from metadata (got %d metrics)", arg, len(m.Data))
		}
		if len(rows) == 0 || rows[0].Type == "" {
			return fmt.Errorf("metric %q metadata carries no typed entry: %+v", arg, rows)
		}
		return nil
	},
}

// searchCount adapts a count comparison over searchView.Traces.
func searchCount(cmp func(n, want int) error) predicateFunc {
	return func(v any, arg string) error {
		s, ok := v.(*searchView)
		if !ok {
			return typeErr(v)
		}
		n, err := intArg(arg)
		if err != nil {
			return err
		}
		return cmp(len(s.Traces), n)
	}
}

func spanHasIntAttr(sp searchSpan, key string, val *string) bool {
	for _, a := range sp.Attributes {
		if a.Key == key && a.Value.IntValue != nil && (val == nil || *a.Value.IntValue == *val) {
			return true
		}
	}
	return false
}

func matrixOf(v any) ([]matrixSeriesView, error) {
	switch e := v.(type) {
	case *lokiEnvelopeView:
		return e.Matrix, nil
	case *promEnvelopeView:
		return e.Matrix, nil
	}
	return nil, typeErr(v)
}

// samplePointValue extracts the stringified float from a [ts, "v"]
// sample pair.
func samplePointValue(pt [2]any) (float64, error) {
	s, ok := pt[1].(string)
	if !ok {
		return 0, fmt.Errorf("sample value %v is not the stringified-float wire shape", pt[1])
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("sample value %q does not parse as float: %w", s, err)
	}
	return f, nil
}

func intArg(arg string) (int, error) {
	n, err := strconv.Atoi(arg)
	if err != nil {
		return 0, fmt.Errorf("predicate arg %q is not an integer: %w", arg, err)
	}
	return n, nil
}

func typeErr(v any) error {
	return fmt.Errorf("predicate not applicable to decoded type %T", v)
}

// KnownDecoder reports whether name is a registered decoder — used by
// the ratchet meta-test to reject corpus entries that name decoders
// the harness can't run.
func KnownDecoder(name string) bool { _, ok := decoders[name]; return ok }

// KnownPredicate mirrors KnownDecoder for predicate names.
func KnownPredicate(name string) bool { _, ok := predicates[name]; return ok }
