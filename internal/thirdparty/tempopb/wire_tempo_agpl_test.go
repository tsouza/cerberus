//go:build agpl_oracle

package tempopb_test

import (
	"reflect"
	"testing"

	"github.com/gogo/protobuf/jsonpb"
	upstream "github.com/grafana/tempo/pkg/tempopb"
)

// upstreamFresh is, per wire case, an empty value of Tempo's own type for the
// same message, from the AGPL-3.0 module this package's copy was taken from.
var upstreamFresh = map[string]func() wireMessage{
	"Trace":                     func() wireMessage { return &upstream.Trace{} },
	"TraceByIDResponse":         func() wireMessage { return &upstream.TraceByIDResponse{} },
	"SearchRequest":             func() wireMessage { return &upstream.SearchRequest{} },
	"SearchResponse":            func() wireMessage { return &upstream.SearchResponse{} },
	"SearchTagsV2Response":      func() wireMessage { return &upstream.SearchTagsV2Response{} },
	"SearchTagValuesV2Response": func() wireMessage { return &upstream.SearchTagValuesV2Response{} },
	"QueryRangeRequest":         func() wireMessage { return &upstream.QueryRangeRequest{} },
	"QueryRangeResponse":        func() wireMessage { return &upstream.QueryRangeResponse{} },
	"QueryInstantResponse":      func() wireMessage { return &upstream.QueryInstantResponse{} },
}

// fieldJSON renders a message by field name, independently of its binary
// field numbers, so two types agree on it only when their fields hold the same
// values under the same names.
func fieldJSON(t *testing.T, m wireMessage) string {
	t.Helper()
	out, err := (&jsonpb.Marshaler{OrigName: true}).MarshalToString(m)
	if err != nil {
		t.Fatalf("jsonpb marshal %T: %v", m, err)
	}
	return out
}

// TestWireMatchesUpstreamTempopb cross-encodes every wire case between the
// in-house copy and Tempo's own pkg/tempopb, in both directions. Each
// direction encodes with one implementation, decodes with the other, and
// compares the two by field name, so a field renumbered, dropped or retyped in
// the copy fails even when its own marshal and unmarshal agree with each other.
func TestWireMatchesUpstreamTempopb(t *testing.T) {
	t.Parallel()

	cases := wireCases()
	if len(cases) != len(upstreamFresh) {
		t.Fatalf("%d wire cases but %d upstream constructors; every case needs its upstream type", len(cases), len(upstreamFresh))
	}
	for name, tc := range cases {
		newUpstream, ok := upstreamFresh[name]
		if !ok {
			t.Fatalf("wire case %q has no upstream constructor", name)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			want := fieldJSON(t, tc.in)

			// cerberus encodes, Tempo decodes.
			wire, err := tc.in.Marshal()
			if err != nil {
				t.Fatalf("in-house marshal: %v", err)
			}
			theirs := newUpstream()
			if err := theirs.Unmarshal(wire); err != nil {
				t.Fatalf("upstream unmarshal of in-house bytes: %v", err)
			}
			if got := fieldJSON(t, theirs); got != want {
				t.Fatalf("Tempo decodes cerberus's encoding differently:\nin-house: %s\nupstream: %s", want, got)
			}

			// Tempo encodes, cerberus decodes. The upstream value is built from
			// the in-house one by field name, never by field number.
			src := newUpstream()
			if err := jsonpb.UnmarshalString(want, src); err != nil {
				t.Fatalf("upstream jsonpb unmarshal: %v", err)
			}
			wire, err = src.Marshal()
			if err != nil {
				t.Fatalf("upstream marshal: %v", err)
			}
			ours := tc.fresh()
			if err := ours.Unmarshal(wire); err != nil {
				t.Fatalf("in-house unmarshal of upstream bytes: %v", err)
			}
			if !reflect.DeepEqual(tc.in, ours) {
				t.Fatalf("cerberus decodes Tempo's encoding differently:\n want: %v\n  got: %v", tc.in, ours)
			}
		})
	}
}
