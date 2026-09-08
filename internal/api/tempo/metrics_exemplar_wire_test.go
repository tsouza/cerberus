package tempo

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/tempo/pkg/tempopb"
)

// This pins #3182. Reference Tempo serialises tempopb through gogo/protobuf
// jsonpb, which takes the JSON key from the proto `json=` orig-name and NOT
// from the Go struct tag. `tempopb.Exemplar.TimestampMs` is declared
// `name=timestamp_ms,json=timestampMs`, so a reference backend emits
// `timestampMs` — while cerberus emitted `timestamp_ms`, a key Grafana's Tempo
// datasource does not read.
//
// The assertion derives the expected key from the UPSTREAM struct tag rather
// than restating the string, so an upstream rename fails here instead of
// leaving a mirror-constant test agreeing with itself. That is the same defect
// class this issue catalogues, and there is no reason to reintroduce it in the
// test written to close it.

// jsonpbName returns the JSON key gogo/protobuf jsonpb emits for a field: the
// `json=` orig-name from the protobuf tag when present, else the proto field
// name.
func jsonpbName(t *testing.T, msg any, field string) string {
	t.Helper()
	f, ok := reflect.TypeOf(msg).FieldByName(field)
	if !ok {
		t.Fatalf("upstream %T has no field %s — the accessor this test reads was renamed", msg, field)
	}
	tag := f.Tag.Get("protobuf")
	if tag == "" {
		t.Fatalf("upstream %T.%s carries no protobuf tag to read a wire name from", msg, field)
	}
	var protoName string
	for _, part := range strings.Split(tag, ",") {
		switch {
		case strings.HasPrefix(part, "json="):
			return strings.TrimPrefix(part, "json=")
		case strings.HasPrefix(part, "name="):
			protoName = strings.TrimPrefix(part, "name=")
		}
	}
	if protoName == "" {
		t.Fatalf("upstream %T.%s protobuf tag %q names no field", msg, field, tag)
	}
	return protoName
}

func TestExemplarTimestampKeyMatchesUpstreamWireName(t *testing.T) {
	want := jsonpbName(t, tempopb.Exemplar{}, "TimestampMs")
	if want == "" {
		t.Fatal("derived an empty wire name — the deriver is broken, not the emitter")
	}

	body, err := json.Marshal(Exemplar{Value: 1, Timestamp: 1700000000000, TraceID: "abc"})
	if err != nil {
		t.Fatalf("marshalling an Exemplar: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("re-reading the marshalled Exemplar: %v", err)
	}
	if _, ok := got[want]; !ok {
		t.Errorf(
			"cerberus emits an exemplar without the %q key that reference Tempo emits; got %s. "+
				"Grafana's Tempo datasource reads the upstream spelling, so a divergent key "+
				"delivers exemplars whose timestamp silently reads as absent.",
			want, body,
		)
	}
	for k := range got {
		if k != want && strings.EqualFold(strings.ReplaceAll(k, "_", ""), strings.ReplaceAll(want, "_", "")) {
			t.Errorf("cerberus emits %q where reference Tempo emits %q — same field, divergent spelling", k, want)
		}
	}
}

// The sibling field has always been right, and holding both to the SAME
// derivation is what stops the pair diverging again: they are the identical
// proto shape, so a test that pinned only one would let the next copy drift.
func TestMetricsSampleTimestampKeyMatchesUpstreamWireName(t *testing.T) {
	want := jsonpbName(t, tempopb.Sample{}, "TimestampMs")

	body, err := json.Marshal(MetricsSample{TimestampMs: 1700000000000, Value: 1})
	if err != nil {
		t.Fatalf("marshalling a MetricsSample: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("re-reading the marshalled MetricsSample: %v", err)
	}
	if _, ok := got[want]; !ok {
		t.Errorf("cerberus emits a sample without the %q key reference Tempo emits; got %s", want, body)
	}
}
