// Proto -> differ-JSON-shape converters for the gRPC/h2c StreamingQuerier
// transport (#1453; see grpc_diff.go for the orchestration this feeds).
//
// The differ's comparators (Compare / CompareTagNames / CompareTagValues /
// CompareMetrics / AssertCase / AssertMetricsCase / RunSemanticChecks, all
// in differ.go / differ_metrics.go) operate on raw JSON bytes decoded into
// the small package-local structs (SearchResponse, TraceSummary,
// TagNamesResponseV1/V2, TagValuesResponseV1/V2, MetricsResponse, ...).
// Those structs mirror the JSON wire shape both backends' HTTP endpoints
// emit — the proto3-JSON projection of the SAME tempopb messages the gRPC
// StreamingQuerier service streams back.
//
// Rather than teach the comparators a second (proto) representation, this
// file converts each streamed tempopb response into the identical
// differ-local struct the HTTP path already decodes into, so
// json.Marshal-ing the result produces byte-shape-compatible input to the
// unchanged comparator. This is what keeps the gRPC transport a true
// "transport-only difference from the comparator's perspective": every
// field-tolerance rule, every canonical-key alignment, every assertion
// lives in exactly one place.
//
// Two encoding conventions carry over from the HTTP/protojson wire shape
// and need to be reproduced by hand here (the gRPC proto structs give
// native Go types, not JSON strings):
//
//   - uint64 fields that proto3-JSON renders as decimal strings
//     (startTimeUnixNano, durationNanos) are formatted via strconv, and
//     — mirroring proto3 JSON's own "omit the zero value" rule, which
//     the differ-local structs already rely on via `omitempty` — a zero
//     value formats to "" rather than "0" so a genuinely-absent field
//     round-trips the same way through both transports.
//   - label AnyValue variants collapse to the differ's flat string form
//     (metricsLabelsToKeyValues's inverse), matching MetricsLabel's
//     UnmarshalJSON tolerance for the same variants on the HTTP side.
package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/grafana/tempo/pkg/tempopb"
	v1 "github.com/grafana/tempo/pkg/tempopb/common/v1"
)

// marshalDiffBody marshals one of the differ-local response structs
// (SearchResponse, TagNamesResponseV1/V2, TagValuesResponseV1/V2,
// MetricsResponse) to JSON — the same bytes shape assertCaseForEndpoint /
// compareForEndpoint decode from an HTTP response body. Centralised so
// every gRPC fetcher in grpc_diff.go wraps a marshal failure identically.
func marshalDiffBody(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal accumulated grpc response: %w", err)
	}
	return body, nil
}

// formatUint64OrEmpty renders a uint64 as a decimal string, mapping zero
// to the empty string so the differ-local struct's `omitempty` JSON tag
// behaves identically whether the value arrived over HTTP/protojson (which
// omits zero-valued fields outright) or gRPC (which always populates the
// Go field, zero included).
func formatUint64OrEmpty(v uint64) string {
	if v == 0 {
		return ""
	}
	return strconv.FormatUint(v, 10)
}

// traceSummaryFromProto converts one streamed tempopb.TraceSearchMetadata
// into the differ's TraceSummary shape (differ.go). Field-for-field
// mirror of the HTTP /api/search JSON envelope.
func traceSummaryFromProto(t *tempopb.TraceSearchMetadata) TraceSummary {
	if t == nil {
		return TraceSummary{}
	}
	out := TraceSummary{
		TraceID:           t.TraceID,
		RootServiceName:   t.RootServiceName,
		RootTraceName:     t.RootTraceName,
		StartTimeUnixNano: formatUint64OrEmpty(t.StartTimeUnixNano),
		DurationMs:        int(t.DurationMs),
	}
	if t.SpanSet != nil {
		ss := spanSetFromProto(t.SpanSet)
		out.SpanSet = &ss
	}
	for _, s := range t.SpanSets {
		out.SpanSets = append(out.SpanSets, spanSetFromProto(s))
	}
	return out
}

// spanSetFromProto converts one tempopb.SpanSet into the differ's
// SpanSetJSON shape.
func spanSetFromProto(s *tempopb.SpanSet) SpanSetJSON {
	if s == nil {
		return SpanSetJSON{}
	}
	out := SpanSetJSON{Matched: int(s.Matched)}
	for _, sp := range s.Spans {
		if sp == nil {
			continue
		}
		out.Spans = append(out.Spans, SpanJSON{
			SpanID:            sp.SpanID,
			Name:              sp.Name,
			StartTimeUnixNano: formatUint64OrEmpty(sp.StartTimeUnixNano),
			DurationNanos:     formatUint64OrEmpty(sp.DurationNanos),
		})
	}
	return out
}

// tagsV2ScopeFromProto converts one tempopb.SearchTagsV2Scope into the
// differ's TagNamesScope shape (differ.go).
func tagsV2ScopeFromProto(s *tempopb.SearchTagsV2Scope) TagNamesScope {
	if s == nil {
		return TagNamesScope{}
	}
	return TagNamesScope{Name: s.Name, Tags: append([]string(nil), s.Tags...)}
}

// tagValueV2FromProto converts one tempopb.TagValue into the differ's
// TagValueV2 shape.
func tagValueV2FromProto(v *tempopb.TagValue) TagValueV2 {
	if v == nil {
		return TagValueV2{}
	}
	return TagValueV2{Type: v.Type, Value: v.Value}
}

// keyValuesToLabels converts a tempopb common/v1 KeyValue slice (the
// label shape TimeSeries / InstantSeries / Exemplar all carry) into the
// differ's flat MetricsLabel shape (differ_metrics.go). The inverse of
// internal/api/tempo/grpc/metrics.go::metricsLabelsToKeyValues.
func keyValuesToLabels(kvs []v1.KeyValue) []MetricsLabel {
	out := make([]MetricsLabel, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, MetricsLabel{Key: kv.Key, Value: anyValueToString(kv.Value)})
	}
	return out
}

// anyValueToString collapses a tempopb common/v1 AnyValue to the flat
// string form MetricsLabel.UnmarshalJSON already tolerates on the HTTP
// side (differ_metrics.go): stringValue verbatim, intValue as its decimal
// string (matching proto3 JSON's int64-as-string convention), doubleValue
// / boolValue via fmt.Sprint, and an empty/unset AnyValue as "" (Tempo's
// "nil" group-by bucket).
func anyValueToString(v *v1.AnyValue) string {
	if v == nil {
		return ""
	}
	switch val := v.Value.(type) {
	case *v1.AnyValue_StringValue:
		return val.StringValue
	case *v1.AnyValue_IntValue:
		return strconv.FormatInt(val.IntValue, 10)
	case *v1.AnyValue_DoubleValue:
		// Mirrors differ_metrics.go's UnmarshalJSON fallback
		// (`fmt.Sprint(*anyV.DoubleValue)`) exactly.
		return fmt.Sprint(val.DoubleValue)
	case *v1.AnyValue_BoolValue:
		return fmt.Sprint(val.BoolValue)
	default:
		return ""
	}
}
