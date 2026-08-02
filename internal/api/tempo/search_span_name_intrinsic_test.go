package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearch_SpanNamePopulatedWhenQueryNamesIntrinsic pins
// spanSets[].spans[].name against reference Tempo's rule, which is
// neither "always" nor "only with | select(name)":
// asTraceSearchMetadata (pkg/traceql/engine.go) sets tempopb.Span.Name
// iff NewIntrinsic(IntrinsicName) is in the span's AllAttributes, and an
// intrinsic lands there iff the query put a condition on it. Conditions
// are collected query-wide (RootExpr.extractConditions folds every stage
// into one FetchSpansRequest), so either side of a structural operator
// counts, and the scoped `span:name` spelling counts because upstream's
// grammar folds it to the same IntrinsicName.
//
// The stub returns the same rows for every case; only the query text
// differs, so the assertion isolates the response-shaping decision.
func TestSearch_SpanNamePopulatedWhenQueryNamesIntrinsic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		query    string
		wantName bool
		why      string
	}{
		{
			name:     "bare_name_equality",
			query:    `{ name = "GET /home" }`,
			wantName: true,
			why:      "the query names the intrinsic, so reference emits the span name",
		},
		{
			name:     "name_not_nil",
			query:    `{ name != nil }`,
			wantName: true,
			why:      "an existence condition on name still fetches it",
		},
		{
			name:     "scoped_span_name",
			query:    `{ span:name = "GET /home" }`,
			wantName: true,
			why:      "span:name is the same intrinsic (expr.y:480 folds it to IntrinsicName)",
		},
		{
			name:     "name_on_one_side_of_structural",
			query:    `{ name = "GET /home" } >> { .child = 1 }`,
			wantName: true,
			why:      "conditions are query-wide, so the returned descendants carry name too",
		},
		{
			name:     "no_name_reference",
			query:    `{ resource.service.name = "frontend" }`,
			wantName: false,
			why:      "reference emits name:\"\" when nothing named the intrinsic",
		},
		{
			name:     "different_intrinsic",
			query:    `{ status = error }`,
			wantName: false,
			why:      "a condition on another intrinsic must not populate name",
		},
	}

	const spanName = "GET /home"
	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{
				// The structural case routes through the two-phase split,
				// whose phase A ranks trace ids via QueryStrings before the
				// wide phase-B Query; without an id phase B is skipped and
				// the response is empty. Harmless on the other cases, which
				// never call QueryStrings.
				strings: []string{"a1"},
				samples: []chclient.Sample{{
					// canonicalSampleProjections aliases the SpanName column
					// to MetricName on every search projection, so this is
					// the value the shaper has available for the name field.
					MetricName: spanName,
					Labels: map[string]string{
						"__cerberus_traceID":      "a1",
						"__cerberus_spanID":       "1000000000000001",
						"__cerberus_parentSpanID": "",
					},
					Timestamp: ts,
					Value:     1000,
				}},
			}
			srv := newServer(q, "v-test")
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/search?q=" + url.QueryEscape(tc.query))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}

			// Guard against a hollow pass: the shaper can only source the
			// name from MetricName if the emitted projection actually
			// aliases SpanName to it. Assert that in the SQL rather than
			// trusting the stub's hand-written row.
			if !strings.Contains(q.lastSQL, "MetricName") || !strings.Contains(q.lastSQL, "SpanName") {
				t.Fatalf("emitted SQL must project SpanName as MetricName; got:\n%s", q.lastSQL)
			}

			var sr tempo.SearchResponse
			if err := json.Unmarshal([]byte(body), &sr); err != nil {
				t.Fatalf("decode: %v\nbody: %s", err, body)
			}
			if len(sr.Traces) != 1 || len(sr.Traces[0].SpanSets) != 1 {
				t.Fatalf("want 1 trace with 1 spanset, got %s", body)
			}
			spans := sr.Traces[0].SpanSets[0].Spans
			if len(spans) != 1 {
				t.Fatalf("want 1 span, got %d (%s)", len(spans), body)
			}

			want := ""
			if tc.wantName {
				want = spanName
			}
			if spans[0].Name != want {
				t.Errorf("spanSets[0].spans[0].name = %q, want %q (%s)", spans[0].Name, want, tc.why)
			}
		})
	}
}

// TestSearch_SelectNameWinsOverIntrinsicFallback pins the precedence
// between the two routes to the same field. `| select(name)` puts the
// span name in the reserved __cerberus_sel_name slot, and that value is
// what the projection actually selected, so it wins over the MetricName
// fallback even when the two disagree.
func TestSearch_SelectNameWinsOverIntrinsicFallback(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	q := &stubQuerier{samples: []chclient.Sample{{
		MetricName: "metric-name-column",
		Labels: map[string]string{
			"__cerberus_traceID":      "a1",
			"__cerberus_spanID":       "1000000000000001",
			"__cerberus_parentSpanID": "",
			"__cerberus_sel_name":     "selected-name",
		},
		Timestamp: ts,
		Value:     1000,
	}}}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)

	query := url.QueryEscape(`{ name = "GET /home" } | select(name)`)
	resp, err := http.Get(srv.URL + "/api/search?q=" + query)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	if len(sr.Traces) != 1 || len(sr.Traces[0].SpanSets) != 1 || len(sr.Traces[0].SpanSets[0].Spans) != 1 {
		t.Fatalf("want 1 trace / 1 spanset / 1 span, got %s", body)
	}
	if got := sr.Traces[0].SpanSets[0].Spans[0].Name; got != "selected-name" {
		t.Errorf("spanSets[0].spans[0].name = %q, want %q", got, "selected-name")
	}
}
