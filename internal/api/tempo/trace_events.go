package tempo

import (
	"encoding/json"
	"strconv"

	tracev1 "github.com/grafana/tempo/pkg/tempopb/trace/v1"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Sub-fields of the OTel-CH `Events` / `Links` Nested columns. They are
// fixed by the exporter's DDL: only the parent column name is
// schema-configurable (schema.Traces.EventsColumn / LinksColumn).
const (
	nestedTimestampSubfield  = "Timestamp"
	nestedNameSubfield       = "Name"
	nestedTraceIDSubfield    = "TraceId"
	nestedSpanIDSubfield     = "SpanId"
	nestedTraceStateSubfield = "TraceState"
)

// traceByIDEventLambdaParam names the arrayMap lambda parameter that binds
// one element of the events Timestamp array.
const traceByIDEventLambdaParam = "t"

// nestedSubcolumn references one sub-field of a Nested column, rendered
// `<col>`.`<sub>` — the same identifier shape chsql emits for
// chplan.NestedArrayExists.
func nestedSubcolumn(col, sub string) chplan.Expr {
	return &chplan.ColumnRef{Qualifier: col, Name: sub}
}

// traceByIDEventsJSON renders a span's Nested events column as a JSON
// array of positional [timeUnixNano, name, attributes] tuples:
//
//	toJSONString(arrayZip(
//	    arrayMap(t -> toString(toUnixTimestamp64Nano(t)), `Events`.`Timestamp`),
//	    `Events`.`Name`,
//	    `Events`.`Attributes`))
//
// The timestamp is stringified before encoding so a 19-digit nanosecond
// count survives the JSON round trip exactly (a JSON number decoded as
// float64 would not) and does not depend on the server's
// output_format_json_quote_64bit_integers setting. A span with no events
// renders `[]`.
func traceByIDEventsJSON(col string) chplan.Expr {
	timesNanos := &chplan.FuncCall{
		Fn: chplan.FnArrayMap,
		Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{traceByIDEventLambdaParam},
				Body: &chplan.FuncCall{Fn: chplan.FnToString, Args: []chplan.Expr{
					&chplan.FuncCall{Fn: chplan.FnToUnixNanos, Args: []chplan.Expr{
						&chplan.BareIdent{Name: traceByIDEventLambdaParam},
					}},
				}},
			},
			nestedSubcolumn(col, nestedTimestampSubfield),
		},
	}
	return &chplan.FuncCall{Fn: chplan.FnToJSONString, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayZip, Args: []chplan.Expr{
			timesNanos,
			nestedSubcolumn(col, nestedNameSubfield),
			nestedSubcolumn(col, nestedAttributesSubfield),
		}},
	}}
}

// traceByIDLinksJSON renders a span's Nested links column as a JSON array
// of positional [traceId, spanId, traceState, attributes] tuples. A span
// with no links renders `[]`.
func traceByIDLinksJSON(col string) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnToJSONString, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayZip, Args: []chplan.Expr{
			nestedSubcolumn(col, nestedTraceIDSubfield),
			nestedSubcolumn(col, nestedSpanIDSubfield),
			nestedSubcolumn(col, nestedTraceStateSubfield),
			nestedSubcolumn(col, nestedAttributesSubfield),
		}},
	}}
}

// spanEvent is the decoded form of one traceByIDEventsJSON tuple.
type spanEvent struct {
	TimeUnixNano uint64
	Name         string
	Attributes   map[string]string
}

// spanLink is the decoded form of one traceByIDLinksJSON tuple.
type spanLink struct {
	TraceID    string
	SpanID     string
	TraceState string
	Attributes map[string]string
}

// decodeSpanEvents parses the traceByIDKeyEventsJSON value. Like the
// SpanAttributes decode in splitTraceByIDLabels it is defensive: a
// malformed blob drops the events rather than failing the whole trace
// view, and so does a malformed tuple (only that tuple is dropped).
func decodeSpanEvents(raw string) []spanEvent {
	var tuples []json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &tuples) != nil {
		return nil
	}
	out := make([]spanEvent, 0, len(tuples))
	for _, t := range tuples {
		var (
			ts    string
			ev    spanEvent
			attrs map[string]string
		)
		if !decodeTuple(t, &ts, &ev.Name, &attrs) {
			continue
		}
		nanos, err := strconv.ParseUint(ts, 10, 64)
		if err != nil {
			continue
		}
		ev.TimeUnixNano = nanos
		ev.Attributes = attrs
		out = append(out, ev)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeSpanLinks parses the traceByIDKeyLinksJSON value, with the same
// defensive contract as decodeSpanEvents.
func decodeSpanLinks(raw string) []spanLink {
	var tuples []json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &tuples) != nil {
		return nil
	}
	out := make([]spanLink, 0, len(tuples))
	for _, t := range tuples {
		var l spanLink
		if !decodeTuple(t, &l.TraceID, &l.SpanID, &l.TraceState, &l.Attributes) {
			continue
		}
		out = append(out, l)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// decodeTuple decodes one positional JSON array into dst, element by
// element. It reports false when the array does not have exactly
// len(dst) elements or any element fails to decode.
func decodeTuple(raw json.RawMessage, dst ...any) bool {
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil || len(elems) != len(dst) {
		return false
	}
	for i, e := range elems {
		if json.Unmarshal(e, dst[i]) != nil {
			return false
		}
	}
	return true
}

// spanEventsJSON maps decoded events onto the v1 JSON wire shape.
func spanEventsJSON(events []spanEvent) []SpanEventEntry {
	if len(events) == 0 {
		return nil
	}
	out := make([]SpanEventEntry, 0, len(events))
	for _, e := range events {
		out = append(out, SpanEventEntry{
			TimeUnixNano: strconv.FormatUint(e.TimeUnixNano, 10),
			Name:         e.Name,
			Attributes:   e.Attributes,
		})
	}
	return out
}

// spanLinksJSON maps decoded links onto the v1 JSON wire shape.
func spanLinksJSON(links []spanLink) []SpanLinkEntry {
	if len(links) == 0 {
		return nil
	}
	out := make([]SpanLinkEntry, 0, len(links))
	for _, l := range links {
		out = append(out, SpanLinkEntry(l))
	}
	return out
}

// spanEventsProto maps decoded events onto the OTLP proto shape.
func spanEventsProto(events []spanEvent) []*tracev1.Span_Event {
	if len(events) == 0 {
		return nil
	}
	out := make([]*tracev1.Span_Event, 0, len(events))
	for _, e := range events {
		out = append(out, &tracev1.Span_Event{
			TimeUnixNano: e.TimeUnixNano,
			Name:         e.Name,
			Attributes:   attrMapToKVList(e.Attributes),
		})
	}
	return out
}

// spanLinksProto maps decoded links onto the OTLP proto shape. Link IDs
// are stored hex-encoded and full-width (the exporter writes them
// verbatim), and decode to raw bytes the same way span IDs do.
func spanLinksProto(links []spanLink) []*tracev1.Span_Link {
	if len(links) == 0 {
		return nil
	}
	out := make([]*tracev1.Span_Link, 0, len(links))
	for _, l := range links {
		out = append(out, &tracev1.Span_Link{
			TraceId:    hexToBytesPadded(l.TraceID, traceIDByteLen),
			SpanId:     hexToBytesPadded(l.SpanID, spanIDByteLen),
			TraceState: l.TraceState,
			Attributes: attrMapToKVList(l.Attributes),
		})
	}
	return out
}
