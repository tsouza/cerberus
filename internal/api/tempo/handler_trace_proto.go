package tempo

import (
	"encoding/hex"
	"strings"

	"github.com/grafana/tempo/pkg/tempopb"
	commonv1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
	resourcev1 "github.com/grafana/tempo/pkg/tempopb/resource/v1"
	tracev1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// negotiateTraceByIDProto inspects the Accept header for a Grafana-Tempo
// proto request. Grafana 11.x's Tempo datasource plugin sends
// `Accept: application/protobuf` and proto.Unmarshal-s the response
// body into a *tempopb.Trace — JSON bodies surface as
// "proto: illegal wireType …" errors on the Grafana side.
//
// Accepted proto MIME types (case-insensitive, semicolon-stripped):
//   - application/protobuf       (Grafana's Tempo plugin)
//   - application/x-protobuf     (alternative convention)
//   - application/grpc           (gRPC over HTTP/1.1 transcoding clients)
//
// Any other value (or empty) falls back to JSON so existing callers
// (curl, the cerberus conformance suite, the JSON-mode dashboard sweep)
// keep getting the documented JSON shape.
func negotiateTraceByIDProto(accept string) bool {
	for _, raw := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(raw)
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
		switch strings.ToLower(strings.TrimSpace(mt)) {
		case "application/protobuf",
			"application/x-protobuf",
			"application/grpc":
			return true
		}
	}
	return false
}

// groupBatchesProto is the proto-shaped sibling of groupBatches. It
// consumes the same []chclient.Sample input (already wrapped via
// traceByIDProjections; see splitTraceByIDLabels for the reserved-key
// contract) and builds the equivalent *tempopb.Trace that Grafana's
// Tempo datasource plugin proto.Unmarshal-s on /api/traces/{id}.
//
// Design choice — sibling, not reuse:
//
//   - groupBatches returns the JSON wire-format types (ResourceSpans /
//     SpanEntry / SpanStatus) whose field names + JSON tags mirror the
//     documented Tempo HTTP shape.
//   - The proto shape uses the upstream tempopb types
//     (*tempopb.Trace, *trace/v1.ResourceSpans, *trace/v1.ScopeSpans,
//     *trace/v1.Span, *resource/v1.Resource, *common/v1.KeyValue) with
//     byte-array trace/span IDs and enum-int kind/status fields.
//   - The two structures diverge on type (string-keyed map vs
//     []*KeyValue) and on identifier encoding (hex string vs raw bytes),
//     so a single helper would either over-abstract or carry a tag
//     parameter that costs more than it saves. Keeping them as siblings
//     fed by the same Sample slice + the same splitTraceByIDLabels
//     partition means the two paths can never drift on field
//     semantics — both call splitTraceByIDLabels, both group by the
//     same format.CanonicalKey, both pull SpanKind / StatusCode out
//     of the meta map.
//
// The parity test in handler_trace_proto_test.go pins this equivalence
// (same span count, same trace ID, same attribute keys) so a future
// edit to one helper that forgets the other surfaces in CI.
func groupBatchesProto(samples []chclient.Sample) *tempopb.Trace {
	bucket := map[string]*tracev1.ResourceSpans{}
	keys := []string{} // stable order so the marshaled body is deterministic
	for _, s := range samples {
		resourceAttrs, spanAttrs, meta := splitTraceByIDLabels(s.Labels)
		key := format.CanonicalKey(resourceAttrs)
		rs, ok := bucket[key]
		if !ok {
			rs = &tracev1.ResourceSpans{
				Resource: &resourcev1.Resource{
					Attributes: attrMapToKVList(resourceAttrs),
				},
				ScopeSpans: []*tracev1.ScopeSpans{{
					// One ScopeSpans per ResourceSpans is enough — cerberus
					// flattens the scope dimension on the JSON path
					// (groupBatches' ResourceSpans has no scope_spans
					// field), so mirroring "one ScopeSpans bucket" keeps
					// the two outputs structurally aligned.
					Scope: nil,
				}},
			}
			bucket[key] = rs
			keys = append(keys, key)
		}
		rs.ScopeSpans[0].Spans = append(rs.ScopeSpans[0].Spans, &tracev1.Span{
			TraceId:           hexToBytesPadded(meta[traceByIDKeyTraceID], 16),
			SpanId:            hexToBytesPadded(meta[traceByIDKeySpanID], 8),
			ParentSpanId:      hexToBytesPadded(meta[traceByIDKeyParentSpanID], 8),
			Name:              s.MetricName,
			Kind:              spanKindFromCH(meta[traceByIDKeySpanKind]),
			StartTimeUnixNano: uint64(s.Timestamp.UnixNano()),
			// DurationNanos is carried as Value on the Sample; the proto
			// shape has no Duration field, so we encode start + end so
			// EndTimeUnixNano - StartTimeUnixNano = the duration the JSON
			// path exposes verbatim. durationNanosFromValue clamps the
			// float64 duration to a non-negative uint64 so the gosec G115
			// signed-to-unsigned conversion check stays happy on the cast.
			EndTimeUnixNano: uint64(s.Timestamp.UnixNano()) + durationNanosFromValue(s.Value),
			Status:          &tracev1.Status{Code: statusCodeFromCH(meta[traceByIDKeyStatusCode])},
			Attributes:      attrMapToKVList(spanAttrs),
		})
	}
	out := &tempopb.Trace{ResourceSpans: make([]*tracev1.ResourceSpans, 0, len(bucket))}
	for _, k := range keys {
		out.ResourceSpans = append(out.ResourceSpans, bucket[k])
	}
	return out
}

// durationNanosFromValue converts the float64 duration carried on
// chclient.Sample.Value into the uint64 EndTimeUnixNano expects.
// Negative durations (a CH typing bug or a row that slipped past the
// duration_ns >= 0 invariant) are clamped to zero so the resulting
// EndTimeUnixNano never wraps below StartTimeUnixNano — and so gosec
// G115 (signed→unsigned conversion) stays satisfied with the cast
// being unconditionally non-negative.
func durationNanosFromValue(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}

// attrMapToKVList renders a string→string attribute map into the
// []*commonv1.KeyValue shape OTel-proto uses (one StringValue per key).
// Returns nil for nil/empty input so the parity-with-JSON test treats
// "missing" and "empty map" the same way groupBatches does (it emits
// `omitempty` on the JSON map too).
func attrMapToKVList(m map[string]string) []*commonv1.KeyValue {
	if len(m) == 0 {
		return nil
	}
	// Stable key order so repeated marshals are byte-identical.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([]*commonv1.KeyValue, 0, len(keys))
	for _, k := range keys {
		out = append(out, &commonv1.KeyValue{
			Key: k,
			Value: &commonv1.AnyValue{
				Value: &commonv1.AnyValue_StringValue{StringValue: m[k]},
			},
		})
	}
	return out
}

// sortStrings keeps the proto helper file self-contained (no
// sort-package import bleed into the handler proper). Plain insertion
// sort is fine — attribute maps are bounded by the OTel cardinality
// limits (default 128 keys per span, ~50 per resource).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// hexToBytesPadded re-inflates the leading-zero-stripped hex string the
// CH stripLeadingHexZeros UDF emits (see traceByIDProjections) back to
// the fixed-width byte array OTel-proto requires (16 bytes for trace
// IDs, 8 bytes for span IDs). Returns nil if the input is empty so the
// proto wire layer omits the field — Grafana's plugin tolerates a
// missing ParentSpanId (root span) but not a malformed one.
func hexToBytesPadded(stripped string, byteLen int) []byte {
	if stripped == "" {
		return nil
	}
	// Restore the leading-zero padding the CH UDF stripped. The hex
	// string is the lower-case form (the OTel-CH exporter writes hex
	// lower-case); a caller that supplied an uppercase hex value would
	// be normalized by hex.DecodeString below regardless.
	want := byteLen * 2
	if len(stripped) > want {
		// Truncate from the left — a value longer than the column width
		// means a bogus payload made it past the projection layer; the
		// proto Span_id / Trace_id contract demands the fixed byte
		// length, so keep the trailing bytes and let the consumer
		// reject the impossible ID rather than panic here.
		stripped = stripped[len(stripped)-want:]
	} else if len(stripped) < want {
		stripped = strings.Repeat("0", want-len(stripped)) + stripped
	}
	b, err := hex.DecodeString(stripped)
	if err != nil {
		// Bad hex (non-[0-9a-f] characters) means the row is malformed;
		// drop the ID rather than fail the whole trace fetch — the
		// per-span proto contract allows a zero-length id (it's
		// "considered invalid" by the spec but the wire layer doesn't
		// reject it, which lets Grafana surface the bad span without
		// 500-ing the whole trace).
		return nil
	}
	return b
}

// spanKindFromCH maps the cerberus CH SpanKind string literal (the
// `LowCardinality(String)` value the OTel-CH exporter writes:
// "Internal" / "Server" / "Client" / "Producer" / "Consumer", plus an
// empty / unknown value) onto the OTel-proto enum. Mirrors the inverse
// of `compatibility/tempo/driver/seeder.go::spanKindCH`.
func spanKindFromCH(s string) tracev1.Span_SpanKind {
	switch s {
	case "Internal":
		return tracev1.Span_SPAN_KIND_INTERNAL
	case "Server":
		return tracev1.Span_SPAN_KIND_SERVER
	case "Client":
		return tracev1.Span_SPAN_KIND_CLIENT
	case "Producer":
		return tracev1.Span_SPAN_KIND_PRODUCER
	case "Consumer":
		return tracev1.Span_SPAN_KIND_CONSUMER
	default:
		return tracev1.Span_SPAN_KIND_UNSPECIFIED
	}
}

// statusCodeFromCH maps the cerberus CH StatusCode string literal
// ("Unset" / "Ok" / "Error", which is what the OTel-CH exporter writes
// via the LowCardinality(String) Status_code column) onto the
// OTel-proto Status_StatusCode enum.
func statusCodeFromCH(s string) tracev1.Status_StatusCode {
	switch s {
	case "Ok":
		return tracev1.Status_STATUS_CODE_OK
	case "Error":
		return tracev1.Status_STATUS_CODE_ERROR
	default:
		return tracev1.Status_STATUS_CODE_UNSET
	}
}
