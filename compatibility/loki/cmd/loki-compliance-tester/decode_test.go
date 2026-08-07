package main

import (
	"math"
	"strings"
	"testing"
)

// The decoder is the harness's only reading of a Loki response body, so
// every parity verdict downstream is a statement about what these
// functions produced. A decoder that silently drops an entry, rounds a
// timestamp to the wrong unit, or swallows a non-`success` envelope
// turns a real divergence into a green row — the failure mode is a
// harness that agrees with itself. The tests below pin each shape's
// field mapping and each error path's message.

// TestDecodeResponse_Streams pins the streams shape: the `stream` object
// becomes the label set, and each `[ts, line]` pair becomes an entry
// with the timestamp parsed as unix NANOS (streams are the one shape
// Loki serialises in nanoseconds).
func TestDecodeResponse_Streams(t *testing.T) {
	t.Parallel()
	body := []byte(`{"status":"success","data":{"resultType":"streams","result":[
		{"stream":{"service_name":"api","level":"info"},"values":[["1700000000000000001","first line"],["1700000000000000002","second line"]]},
		{"stream":{"service_name":"web"},"values":[]}
	]}}`)
	got, err := decodeResponse(body)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if got.kind != "streams" {
		t.Fatalf("kind = %q, want streams", got.kind)
	}
	if len(got.streams) != 2 {
		t.Fatalf("streams = %d, want 2", len(got.streams))
	}
	if got.streams[0].Labels["service_name"] != "api" || got.streams[0].Labels["level"] != "info" {
		t.Fatalf("stream[0] labels = %v", got.streams[0].Labels)
	}
	if len(got.streams[0].Entries) != 2 {
		t.Fatalf("stream[0] entries = %d, want 2", len(got.streams[0].Entries))
	}
	if got.streams[0].Entries[0].Timestamp != 1700000000000000001 {
		t.Fatalf("entry[0] timestamp = %d, want 1700000000000000001 (unix nanos, verbatim)", got.streams[0].Entries[0].Timestamp)
	}
	if got.streams[0].Entries[0].Line != "first line" {
		t.Fatalf("entry[0] line = %q", got.streams[0].Entries[0].Line)
	}
	if len(got.streams[1].Entries) != 0 {
		t.Fatalf("stream[1] entries = %d, want 0", len(got.streams[1].Entries))
	}
}

// TestDecodeResponse_StreamsBadTimestamp — a stream timestamp that is
// not an integer is a hard decode error naming the offending token, not
// a silently dropped entry.
func TestDecodeResponse_StreamsBadTimestamp(t *testing.T) {
	t.Parallel()
	body := []byte(`{"data":{"resultType":"streams","result":[{"stream":{"a":"b"},"values":[["not-a-number","line"]]}]}}`)
	_, err := decodeResponse(body)
	if err == nil {
		t.Fatal("decodeResponse = nil error, want a stream-timestamp parse failure")
	}
	if !strings.Contains(err.Error(), "not-a-number") {
		t.Fatalf("error %q should name the unparseable timestamp", err.Error())
	}
}

// TestDecodeResponse_Vector pins the vector shape and the sample-pair
// unit conversion: Loki serialises the sample timestamp as float
// SECONDS, and the comparator deals in unix MILLIS.
func TestDecodeResponse_Vector(t *testing.T) {
	t.Parallel()
	body := []byte(`{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"level":"error"},"value":[1700000000.5,"42.25"]}
	]}}`)
	got, err := decodeResponse(body)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if got.kind != "vector" || len(got.vector) != 1 {
		t.Fatalf("kind=%q len=%d, want vector/1", got.kind, len(got.vector))
	}
	if got.vector[0].T != 1700000000500 {
		t.Fatalf("T = %d, want 1700000000500 (float seconds converted to unix millis)", got.vector[0].T)
	}
	if got.vector[0].F != 42.25 {
		t.Fatalf("F = %v, want 42.25", got.vector[0].F)
	}
	if got.vector[0].Metric["level"] != "error" {
		t.Fatalf("metric = %v", got.vector[0].Metric)
	}
}

// TestDecodeResponse_Matrix pins the matrix shape: one entry per series,
// each carrying its own point list in order.
func TestDecodeResponse_Matrix(t *testing.T) {
	t.Parallel()
	body := []byte(`{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"level":"warn"},"values":[[1700000000,"1"],[1700000060,"2"]]}
	]}}`)
	got, err := decodeResponse(body)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if got.kind != "matrix" || len(got.matrix) != 1 {
		t.Fatalf("kind=%q len=%d, want matrix/1", got.kind, len(got.matrix))
	}
	pts := got.matrix[0].Floats
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	if pts[0].T != 1700000000000 || pts[1].T != 1700000060000 {
		t.Fatalf("point timestamps = %d,%d, want 1700000000000,1700000060000", pts[0].T, pts[1].T)
	}
	if pts[0].F != 1 || pts[1].F != 2 {
		t.Fatalf("point values = %v,%v, want 1,2", pts[0].F, pts[1].F)
	}
}

// TestDecodeResponse_StringTimestamps — Loki's own convention is to
// serialise a sample timestamp as a quoted decimal, so decodeSamplePair
// carries a string arm alongside the JSON-number arm. Both must apply
// the same seconds → unix-millis conversion; a string arm that skipped
// the scaling would put every point a thousand times too early on the
// axis while the number arm looked correct.
func TestDecodeResponse_StringTimestamps(t *testing.T) {
	t.Parallel()
	body := []byte(`{"status":"success","data":{"resultType":"matrix","result":[
		{"metric":{"level":"warn"},"values":[["1700000000.5","1"],["1700000060","2"]]}
	]}}`)
	got, err := decodeResponse(body)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	pts := got.matrix[0].Floats
	if len(pts) != 2 {
		t.Fatalf("points = %d, want 2", len(pts))
	}
	if pts[0].T != 1700000000500 {
		t.Fatalf("T = %d, want 1700000000500 (string float seconds converted to unix millis)", pts[0].T)
	}
	if pts[1].T != 1700000060000 {
		t.Fatalf("T = %d, want 1700000060000", pts[1].T)
	}
	if pts[0].F != 1 || pts[1].F != 2 {
		t.Fatalf("point values = %v,%v, want 1,2", pts[0].F, pts[1].F)
	}
}

// TestDecodeResponse_Scalar pins the scalar shape, including the
// hasValue flag isEmpty keys on: a decoded scalar is never "empty",
// however small its value.
func TestDecodeResponse_Scalar(t *testing.T) {
	t.Parallel()
	body := []byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000,"0"]}}`)
	got, err := decodeResponse(body)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	if got.kind != "scalar" {
		t.Fatalf("kind = %q, want scalar", got.kind)
	}
	if !got.hasValue {
		t.Fatal("hasValue = false; a decoded scalar carries a value even when it is zero")
	}
	if got.scalar.T != 1700000000000 || got.scalar.F != 0 {
		t.Fatalf("scalar = (T=%d, F=%v), want (1700000000000, 0)", got.scalar.T, got.scalar.F)
	}
}

// TestDecodeResponse_SpecialFloats — NaN and the infinities survive the
// string-encoded value round-trip. floatEqual gives NaN==NaN special
// handling, which is only reachable if the decoder produced a NaN in
// the first place.
func TestDecodeResponse_SpecialFloats(t *testing.T) {
	t.Parallel()
	body := []byte(`{"data":{"resultType":"matrix","result":[
		{"metric":{},"values":[[1,"NaN"],[2,"+Inf"],[3,"-Inf"]]}
	]}}`)
	got, err := decodeResponse(body)
	if err != nil {
		t.Fatalf("decodeResponse: %v", err)
	}
	pts := got.matrix[0].Floats
	if !math.IsNaN(pts[0].F) {
		t.Fatalf("point[0] = %v, want NaN", pts[0].F)
	}
	if !math.IsInf(pts[1].F, 1) {
		t.Fatalf("point[1] = %v, want +Inf", pts[1].F)
	}
	if !math.IsInf(pts[2].F, -1) {
		t.Fatalf("point[2] = %v, want -Inf", pts[2].F)
	}
}

// TestDecodeResponse_ErrorPaths pins each refusal: a body that is not
// JSON, a non-`success` status, an unknown resultType, a sample value
// that is not a string-encoded float, and a sample timestamp that is
// neither a number nor a string-of-number.
func TestDecodeResponse_ErrorPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		body     string
		wantFrag string
	}{
		{"not-json", `{"data":`, "decode envelope"},
		{"status-error", `{"status":"error","data":{"resultType":"vector","result":[]}}`, `loki status="error"`},
		{"unknown-result-type", `{"status":"success","data":{"resultType":"histogram","result":[]}}`, `unknown resultType "histogram"`},
		{"vector-result-not-array", `{"data":{"resultType":"vector","result":{}}}`, "decode vector"},
		{"matrix-result-not-array", `{"data":{"resultType":"matrix","result":{}}}`, "decode matrix"},
		{"streams-result-not-array", `{"data":{"resultType":"streams","result":{}}}`, "decode streams"},
		{"scalar-result-not-pair", `{"data":{"resultType":"scalar","result":{}}}`, "decode scalar"},
		{"value-not-string", `{"data":{"resultType":"vector","result":[{"metric":{},"value":[1,2]}]}}`, "value decode"},
		{"value-not-float", `{"data":{"resultType":"vector","result":[{"metric":{},"value":[1,"abc"]}]}}`, `value parse "abc"`},
		{"ts-string-not-float", `{"data":{"resultType":"vector","result":[{"metric":{},"value":["abc","1"]}]}}`, `ts parse "abc"`},
		{"ts-neither-number-nor-string", `{"data":{"resultType":"vector","result":[{"metric":{},"value":[{},"1"]}]}}`, "ts decode"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := decodeResponse([]byte(tc.body))
			if err == nil {
				t.Fatalf("decodeResponse(%s) = nil error, want %q", tc.body, tc.wantFrag)
			}
			if !strings.Contains(err.Error(), tc.wantFrag) {
				t.Fatalf("error %q missing %q", err.Error(), tc.wantFrag)
			}
		})
	}
}

// TestTypedResultIsEmpty pins the emptiness predicate compareOne routes
// on. The matrix arm is the interesting one: upstream treats a matrix
// carrying series-but-no-points as empty, so a backend answering with
// bare series headers cannot pass as "rows returned".
func TestTypedResultIsEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   typedResult
		want bool
	}{
		{"zero-value", typedResult{}, true},
		{"streams-none", typedResult{kind: "streams"}, true},
		{"streams-some", typedResult{kind: "streams", streams: []decodedStream{{}}}, false},
		{"vector-none", typedResult{kind: "vector"}, true},
		{"vector-some", typedResult{kind: "vector", vector: []decodedSample{{}}}, false},
		{"matrix-none", typedResult{kind: "matrix"}, true},
		{
			"matrix-series-without-points",
			typedResult{kind: "matrix", matrix: []decodedSeries{{Metric: map[string]string{"a": "b"}}}},
			true,
		},
		{
			"matrix-second-series-carries-points",
			typedResult{kind: "matrix", matrix: []decodedSeries{
				{},
				{Floats: []decodedPoint{{T: 1, F: 1}}},
			}},
			false,
		},
		{"scalar-without-value", typedResult{kind: "scalar"}, true},
		{"scalar-with-value", typedResult{kind: "scalar", hasValue: true}, false},
		{"unknown-kind", typedResult{kind: "histogram"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.isEmpty(); got != tc.want {
				t.Fatalf("isEmpty() = %v, want %v", got, tc.want)
			}
		})
	}
}
