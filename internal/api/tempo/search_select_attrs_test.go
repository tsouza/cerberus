package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearch_SelectAttributes_Surfaced pins the /api/search response
// shaping for `| select(...)` queries: the wrap projection smuggles
// each selected value through a reserved `__cerberus_sel_*` Labels
// key and toTraceSummaries pivots them into
// spanSets[].spans[].attributes — int-typed entries surface as OTLP
// intValue (nested-set intrinsics; zero values INCLUDED, matching
// reference Tempo's explicit-selection behaviour), string entries as
// stringValue with empty values dropped (absent map keys), and the
// selected span name lands on the dedicated `name` field exactly
// where reference Tempo puts it (tempopb.Span.Name), not in the
// attribute list.
func TestSearch_SelectAttributes_Surfaced(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{
		{
			MetricName: "GET /home",
			Labels: map[string]string{
				"service.name":                       "frontend",
				"__cerberus_traceID":                 "a1",
				"__cerberus_parentSpanID":            "",
				"__cerberus_spanID":                  "1000000000000001",
				"__cerberus_sel_name":                "GET /home",
				"__cerberus_sel_int_nestedSetParent": "-1",
				"__cerberus_sel_int_nestedSetLeft":   "1",
				"__cerberus_sel_int_nestedSetRight":  "8",
				"__cerberus_sel_str_status":          "unset",
				"__cerberus_sel_str_service.name":    "frontend",
				// Absent map attribute — the Map(String,String)
				// subscript returns ''; the shaper must drop it.
				"__cerberus_sel_str_http.method": "",
			},
			Timestamp: ts,
			Value:     1000,
		},
		{
			MetricName: "checkout",
			Labels: map[string]string{
				"service.name":                       "checkout",
				"__cerberus_traceID":                 "a1",
				"__cerberus_parentSpanID":            "1000000000000001",
				"__cerberus_spanID":                  "1000000000000003",
				"__cerberus_sel_name":                "checkout",
				"__cerberus_sel_int_nestedSetParent": "1",
				"__cerberus_sel_int_nestedSetLeft":   "4",
				"__cerberus_sel_int_nestedSetRight":  "7",
				"__cerberus_sel_str_status":          "error",
				"__cerberus_sel_str_service.name":    "checkout",
				"__cerberus_sel_str_http.method":     "",
			},
			Timestamp: ts.Add(2 * time.Millisecond),
			Value:     700,
		},
	}}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)

	query := url.QueryEscape(`({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(status, resource.service.name, name, nestedSetParent, nestedSetLeft, nestedSetRight)`)
	resp, err := http.Get(srv.URL + "/api/search?q=" + query + "&spss=20")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// The emitted query must carry the reserved select slots so the
	// rows can round-trip the selected values. The map() keys are
	// LitString expressions → bound as `?` args, so assert on the
	// bound arg list rather than the SQL text.
	boundArgs := make(map[string]bool, len(q.lastArgs))
	for _, a := range q.lastArgs {
		if s, ok := a.(string); ok {
			boundArgs[s] = true
		}
	}
	for _, needle := range []string{
		"__cerberus_sel_int_nestedSetLeft",
		"__cerberus_sel_int_nestedSetRight",
		"__cerberus_sel_int_nestedSetParent",
		"__cerberus_sel_str_status",
		"__cerberus_sel_str_service.name",
		"__cerberus_sel_name",
	} {
		if !boundArgs[needle] {
			t.Errorf("emitted query missing reserved slot %q in bound args\nargs: %v", needle, q.lastArgs)
		}
	}

	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(sr.Traces) != 1 || len(sr.Traces[0].SpanSets) != 1 {
		t.Fatalf("want 1 trace with 1 spanset, got %s", body)
	}
	spans := sr.Traces[0].SpanSets[0].Spans
	if len(spans) != 2 {
		t.Fatalf("want 2 spans in spanset, got %d (%s)", len(spans), body)
	}

	type wantSpan struct {
		name  string
		ints  map[string]string
		strs  map[string]string
		nKeys int
	}
	wants := map[string]wantSpan{
		"1000000000000001": {
			name: "GET /home",
			ints: map[string]string{"nestedSetParent": "-1", "nestedSetLeft": "1", "nestedSetRight": "8"},
			strs: map[string]string{"status": "unset", "service.name": "frontend"},
			// 3 int + 2 str; the empty http.method entry is dropped.
			nKeys: 5,
		},
		"1000000000000003": {
			name:  "checkout",
			ints:  map[string]string{"nestedSetParent": "1", "nestedSetLeft": "4", "nestedSetRight": "7"},
			strs:  map[string]string{"status": "error", "service.name": "checkout"},
			nKeys: 5,
		},
	}
	for _, sp := range spans {
		want, ok := wants[sp.SpanID]
		if !ok {
			t.Errorf("unexpected span %q in spanset", sp.SpanID)
			continue
		}
		if sp.Name != want.name {
			t.Errorf("span %s name = %q, want %q", sp.SpanID, sp.Name, want.name)
		}
		if len(sp.Attributes) != want.nKeys {
			t.Errorf("span %s has %d attributes, want %d: %+v", sp.SpanID, len(sp.Attributes), want.nKeys, sp.Attributes)
		}
		got := map[string]tempo.AnyValue{}
		for i, kv := range sp.Attributes {
			got[kv.Key] = kv.Value
			// Deterministic alphabetical key order.
			if i > 0 && sp.Attributes[i-1].Key >= kv.Key {
				t.Errorf("span %s attributes not sorted: %q before %q", sp.SpanID, sp.Attributes[i-1].Key, kv.Key)
			}
		}
		for k, v := range want.ints {
			av, ok := got[k]
			if !ok || av.IntValue == nil || *av.IntValue != v {
				t.Errorf("span %s attr %q = %+v, want intValue %q", sp.SpanID, k, av, v)
			}
		}
		for k, v := range want.strs {
			av, ok := got[k]
			if !ok || av.StringValue == nil || *av.StringValue != v {
				t.Errorf("span %s attr %q = %+v, want stringValue %q", sp.SpanID, k, av, v)
			}
		}
		if _, ok := got["http.method"]; ok {
			t.Errorf("span %s carries empty-valued http.method attr; reference omits absent attributes", sp.SpanID)
		}
	}
}
