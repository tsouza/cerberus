package prom

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestGroupExemplars_TraceIDByteExactPassthrough pins the trace_id / span_id
// encoding contract at the PromQL exemplars wire boundary: cerberus's
// storage layer (schema.Traces.TraceIDColumn / schema.Logs.TraceIDColumn /
// the metrics Exemplars.TraceId Nested field, all `String` columns the
// upstream OTel ClickHouse Exporter fills via `hex.EncodeToString` — a
// canonical, lowercase, zero-padded 32-char hex form for trace IDs and
// 16-char hex for span IDs — is the SAME representation cerberus's Tempo
// head accepts as a `/api/traces/{id}` lookup key (see
// internal/api/tempo/traceid_hex_test.go's "already_32_hex" case, whose
// `normaliseTraceID` leaves that exact form unchanged).
//
// The wire chain from ClickHouse to the Prometheus `/api/v1/query_exemplars`
// envelope is: chclient.ExemplarRow.TraceID (scanned positionally, no
// transform) -> groupExemplars -> projectExemplar -> Exemplar.Labels["trace_id"].
// Nothing on that path may fold case, strip leading zeros, or otherwise
// reshape the value — any such mutation would silently break the
// exemplar-to-trace correlation hop (a value cerberus itself wrote to CH
// becoming unrecognisable to cerberus's own Tempo head, or vice versa).
//
// This test constructs rows carrying the exact literal trace/span ID forms
// traceid_hex_test.go treats as canonical (including its zero-padded
// "tempo_real_world_stripped" case) plus an uppercase-hex row proving the
// Prometheus emit path performs no case-folding of its own (case
// normalisation, where it happens at all, is an INPUT-side Tempo lookup
// concern — internal/api/tempo's normaliseTraceID — never an emit-side
// mutation cerberus applies to a value already read verbatim from CH).
func TestGroupExemplars_TraceIDByteExactPassthrough(t *testing.T) {
	t.Parallel()

	const metricName = "http_request_duration_seconds_bucket"

	cases := []struct {
		name        string
		traceID     string
		spanID      string
		wantTraceID string
		wantSpanID  string
		wantTraceOK bool
		wantSpanOK  bool
	}{
		{
			// The exact padded form internal/api/tempo/traceid_hex_test.go's
			// "tempo_real_world_stripped" case normalises a stripped wire
			// value INTO. Pinning it here proves the two heads agree on
			// the canonical shape without either side needing to touch it.
			name:        "canonical_32_char_lowercase_hex",
			traceID:     "00af843259b0a78f5cbe59e11cbaf66b",
			spanID:      "0af843259b0a78f5",
			wantTraceID: "00af843259b0a78f5cbe59e11cbaf66b",
			wantSpanID:  "0af843259b0a78f5",
			wantTraceOK: true,
			wantSpanOK:  true,
		},
		{
			// Leading zeros MUST survive — the exact defect issue #209
			// fixed on the Tempo emit side (stripLeadingHexZeros violated
			// the OTel/Tempo spec). The Prometheus exemplars path must
			// never introduce that same defect independently.
			name:        "leading_zeros_preserved",
			traceID:     "0000000000000000000000000000abcd",
			spanID:      "000000000000abcd",
			wantTraceID: "0000000000000000000000000000abcd",
			wantSpanID:  "000000000000abcd",
			wantTraceOK: true,
			wantSpanOK:  true,
		},
		{
			// Cerberus's own storage columns are always lowercase (the
			// exporter writes hex.EncodeToString, which is lowercase), but
			// the wire path must not itself fold case on whatever string it
			// is handed — proving projectExemplar is a pure passthrough,
			// not a normaliser (normalisation, where warranted, is
			// internal/api/tempo's INPUT-side job, not the Prometheus
			// head's emit-side job).
			name:        "case_not_folded",
			traceID:     "AF843259B0A78F5CBE59E11CBAF66B0",
			spanID:      "AF843259B0A78F5C",
			wantTraceID: "AF843259B0A78F5CBE59E11CBAF66B0",
			wantSpanID:  "AF843259B0A78F5C",
			wantTraceOK: true,
			wantSpanOK:  true,
		},
		{
			// Empty TraceID/SpanID columns (a data point exported with no
			// trace linkage) must drop the reserved key entirely rather
			// than surface an empty-string label — matching Prom's
			// documented behaviour for exemplars without trace linkage.
			name:        "empty_ids_drop_reserved_keys",
			traceID:     "",
			spanID:      "",
			wantTraceOK: false,
			wantSpanOK:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rows := []chclient.ExemplarRow{
				{
					MetricName:         metricName,
					Attributes:         map[string]string{"job": "api"},
					ServiceName:        "checkout",
					Timestamp:          time.Unix(1717999200, 0),
					Value:              0.5,
					TraceID:            tc.traceID,
					SpanID:             tc.spanID,
					ExemplarAttributes: map[string]string{},
				},
			}

			series := groupExemplars(rows, metricName)
			if len(series) != 1 {
				t.Fatalf("len(series) = %d; want 1", len(series))
			}
			if len(series[0].Exemplars) != 1 {
				t.Fatalf("len(exemplars) = %d; want 1", len(series[0].Exemplars))
			}
			labels := series[0].Exemplars[0].Labels

			gotTraceID, gotTraceOK := labels["trace_id"]
			if gotTraceOK != tc.wantTraceOK {
				t.Fatalf("trace_id present = %v; want %v (labels=%+v)", gotTraceOK, tc.wantTraceOK, labels)
			}
			if tc.wantTraceOK && gotTraceID != tc.wantTraceID {
				t.Errorf("trace_id = %q; want %q (byte-exact passthrough of the CH column value)", gotTraceID, tc.wantTraceID)
			}

			gotSpanID, gotSpanOK := labels["span_id"]
			if gotSpanOK != tc.wantSpanOK {
				t.Fatalf("span_id present = %v; want %v (labels=%+v)", gotSpanOK, tc.wantSpanOK, labels)
			}
			if tc.wantSpanOK && gotSpanID != tc.wantSpanID {
				t.Errorf("span_id = %q; want %q (byte-exact passthrough of the CH column value)", gotSpanID, tc.wantSpanID)
			}
		})
	}
}
