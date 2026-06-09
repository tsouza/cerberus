// Proto-aware fetcher for the trace-by-id endpoint.
//
// Background (task #202): the homegrown fetchJSON in diff.go hard-codes
// `Accept: application/json` on every request. Reference Tempo and
// cerberus both return JSON when asked for JSON, so the differ reports
// "identical" even when one side has no protobuf support at all — the
// exact failure mode that crashed Grafana in #199/#650 (cerberus
// silently returned JSON to a client that asked for proto, Grafana
// failed to decode the body, the trace view went blank).
//
// The vendored upstream pkg/httpclient already picks
// `application/protobuf` for QueryTraceEndpoint / QueryTraceV2Endpoint
// (see compatibility/tempo/upstream/pkg/httpclient/client.go), but the
// upstream subtree is `go.mod ignore`d on purpose — it's reference
// material, not consumable. We therefore implement a small sibling
// fetcher here that mirrors upstream's content-type negotiation:
//
//   - set Accept: application/protobuf
//   - read the body up to the same 16 MB limit as fetchJSON
//   - decode via proto.Unmarshal into *tempopb.Trace
//
// The trace-by-id differ then fetches BOTH JSON and proto from each
// side and asserts:
//
//   - JSON decodes on both sides (existing assertion, now explicit)
//   - proto decodes on both sides (new — catches "cerberus returns JSON
//     when proto was asked for" and the symmetric "cerberus returns
//     proto when JSON was asked for")
//   - the JSON-decoded structural projection (span count + sorted
//     span-ID set) matches the proto-decoded structural projection on
//     each side (catches "proto and JSON disagree on what the trace
//     actually is" — the failure where one encoding is well-formed but
//     describes a different trace shape than the other).
//
// All failures are surfaced as DIFF reasons on CaseResult.Diff, not
// HardErrors — same posture as the rest of the differ: per-case parity
// drift is reported, never fatal.

package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/gogo/protobuf/proto"
	"github.com/grafana/tempo/pkg/tempopb"
)

// fetchProto GETs a URL with the Accept: application/protobuf header
// and decodes the body as *tempopb.Trace. Returns the raw bytes (for
// round-trip / size accounting) + the decoded message + status code.
//
// Mirrors fetchJSON's error envelope so callers can lift the snippet
// into a DiffReason without re-shaping. Status-code semantics match
// fetchJSON: a non-2xx is surfaced as an error, the body snippet is
// included for diagnosability.
func fetchProto(ctx context.Context, client *http.Client, urlStr string) ([]byte, *tempopb.Trace, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Accept", "application/protobuf")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		snippet := body
		if len(snippet) > 2048 {
			snippet = snippet[:2048]
		}
		return body, nil, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, string(snippet))
	}
	trace := &tempopb.Trace{}
	if err := proto.Unmarshal(body, trace); err != nil {
		// Surface the body snippet so a reviewer can see whether the
		// server returned JSON, an error envelope, or just garbage.
		snippet := body
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return body, nil, resp.StatusCode, fmt.Errorf("proto.Unmarshal tempopb.Trace: %w (body prefix: %q)", err, string(snippet))
	}
	return body, trace, resp.StatusCode, nil
}

// traceShape is the deterministic projection of a trace used for the
// JSON-vs-proto structural-parity check. We project both encodings down
// to (span count, sorted span-ID hex set) — two trace bodies describe
// the same trace if and only if their shapes match. Field-by-field
// parity is intentionally out of scope here; the project to span-ID
// set is what catches "encoding A produced 3 spans, encoding B produced
// 2" and "encoding A produced span aaaa, encoding B produced span bbbb".
type traceShape struct {
	SpanCount int      // total span count across all batches / scope-spans
	SpanIDs   []string // sorted, lowercase hex; deduped
}

// equal returns true when both shapes describe the same trace.
func (s traceShape) equal(o traceShape) bool {
	if s.SpanCount != o.SpanCount {
		return false
	}
	if len(s.SpanIDs) != len(o.SpanIDs) {
		return false
	}
	for i := range s.SpanIDs {
		if s.SpanIDs[i] != o.SpanIDs[i] {
			return false
		}
	}
	return true
}

// summarise renders the shape as a short string for DiffReason details.
func (s traceShape) summarise() string {
	preview := s.SpanIDs
	if len(preview) > 4 {
		preview = preview[:4]
	}
	return fmt.Sprintf("spans=%d ids=%v", s.SpanCount, preview)
}

// projectProtoShape walks a decoded *tempopb.Trace and returns the
// deterministic span-ID set + count. Both ResourceSpans -> ScopeSpans ->
// Spans nesting levels are flattened.
func projectProtoShape(t *tempopb.Trace) traceShape {
	if t == nil {
		return traceShape{}
	}
	ids := make(map[string]struct{})
	total := 0
	for _, rs := range t.ResourceSpans {
		if rs == nil {
			continue
		}
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			for _, sp := range ss.Spans {
				if sp == nil {
					continue
				}
				total++
				ids[hex.EncodeToString(sp.SpanId)] = struct{}{}
			}
		}
	}
	return traceShape{SpanCount: total, SpanIDs: sortedKeys(ids)}
}

// projectJSONShape decodes a trace-by-id JSON body and returns the same
// deterministic shape. We tolerate two JSON dialects because cerberus
// and Tempo differ in nesting:
//
//   - Tempo's protojson shape: {batches:[{scopeSpans:[{spans:[{spanId}]}]}]}
//     (older Tempo builds also emit instrumentationLibrarySpans; we sum
//     across both nesting variants).
//   - Cerberus's flattened shape: {batches:[{spans:[{spanId}]}]}.
//
// Either way, every "spans" array's elements contribute one span; the
// `spanId` field is the hex ID Tempo's protojson emits. Cerberus's
// SpanEntry.spanId field name matches — see internal/api/tempo/types.go.
func projectJSONShape(body []byte) (traceShape, error) {
	var raw struct {
		Batches []struct {
			Spans      []spanIDRecord `json:"spans"`
			ScopeSpans []struct {
				Spans []spanIDRecord `json:"spans"`
			} `json:"scopeSpans"`
			InstrumentationLibrarySpans []struct {
				Spans []spanIDRecord `json:"spans"`
			} `json:"instrumentationLibrarySpans"`
		} `json:"batches"`
		// Tempo v2 protojson sometimes top-keys "resourceSpans" instead
		// of "batches" depending on the protojson version. Accept both.
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []spanIDRecord `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return traceShape{}, fmt.Errorf("decode trace json: %w", err)
	}
	ids := make(map[string]struct{})
	total := 0
	collect := func(spans []spanIDRecord) {
		for _, sp := range spans {
			total++
			if sp.SpanID != "" {
				ids[canonicalJSONSpanID(sp.SpanID)] = struct{}{}
			}
		}
	}
	for _, b := range raw.Batches {
		collect(b.Spans)
		for _, ss := range b.ScopeSpans {
			collect(ss.Spans)
		}
		for _, ils := range b.InstrumentationLibrarySpans {
			collect(ils.Spans)
		}
	}
	for _, rs := range raw.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			collect(ss.Spans)
		}
	}
	return traceShape{SpanCount: total, SpanIDs: sortedKeys(ids)}, nil
}

// canonicalJSONSpanID normalises a span ID lifted from a JSON trace
// body to the 16-char lowercase-hex form projectProtoShape produces
// (hex.EncodeToString over the 8 raw bytes). Reference Tempo's JSON
// encoder follows the proto3 JSON mapping and emits bytes fields as
// standard base64 ("C9WjFN3vISs="); cerberus emits the hex form
// directly. Both designate the same 8 bytes, so the cross-encoding
// parity check must compare the decoded value, not the textual shape.
// Hex is tried first: a 16-char hex string is also valid base64 but
// decodes to 12 bytes, so the 8-byte length check disambiguates.
// Anything that decodes to neither form passes through untouched and
// diffs loudly.
func canonicalJSONSpanID(s string) string {
	if b, err := hex.DecodeString(s); err == nil && len(b) == 8 {
		return strings.ToLower(s)
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 8 {
		return hex.EncodeToString(b)
	}
	return s
}

// spanIDRecord tolerates both `spanId` (Tempo / cerberus camelCase) and
// `span_id` (some upstream JSON variants). Both backends in the harness
// emit camelCase today; the snake_case fallback is cheap insurance.
type spanIDRecord struct {
	SpanID      string `json:"spanId"`
	SpanIDSnake string `json:"span_id"`
}

// UnmarshalJSON merges the two field names so callers can read SpanID
// without caring which spelling came in.
func (s *spanIDRecord) UnmarshalJSON(b []byte) error {
	var raw struct {
		SpanID      string `json:"spanId"`
		SpanIDSnake string `json:"span_id"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw.SpanID != "" {
		s.SpanID = raw.SpanID
	} else {
		s.SpanID = raw.SpanIDSnake
	}
	return nil
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// diffTracesEndpoint runs the proto-aware differ for the trace-by-id
// case. Called from diffCase when tc.Endpoint == "traces". Mutates res
// in place; returns nothing — every failure mode is recorded as a
// DiffReason on res.Diff (never a HardError) so per-case proto drift is
// reported but doesn't crash the run.
//
// The function fetches JSON + proto from each side and surfaces:
//
//   - "proto_decode": proto.Unmarshal failed on a side (catches "this
//     side doesn't actually serve proto, just falls back to JSON"; the
//     concrete failure mode in #199/#650).
//   - "json_decode": JSON decode failed on a side (defensive — should
//     never fire on a 2xx, but if it does we want a clear signal).
//   - "encoding_mismatch": both decoders succeeded on a side, but the
//     JSON-decoded shape and the proto-decoded shape describe different
//     traces (different span counts, different span-ID sets).
//   - "cardinality" + "missing_in_a"/"missing_in_b": the existing cross-
//     backend diff — applied to the proto-decoded shapes (since that's
//     the canonical Tempo wire format).
//
// res.Diff.MatchedCount counts spans matched across backends on the
// proto-decoded shapes — visible in the markdown report alongside the
// other endpoint kinds.
func diffTracesEndpoint(ctx context.Context, client *http.Client, tempoURL, cerbURL string, res *CaseResult) {
	res.Diff = Diff{Equal: true}

	// Fetch JSON from both sides (existing wire format). Status is
	// recorded on the CaseResult so the markdown report's per-case
	// "tempo HTTP / cerberus HTTP" line still renders.
	tempoJSON, tempoStatus, tjErr := fetchJSON(ctx, client, tempoURL)
	cerbJSON, cerbStatus, cjErr := fetchJSON(ctx, client, cerbURL)
	res.TempoStatus = tempoStatus
	res.CerberusStatus = cerbStatus
	// Fetch proto from both sides (the wire format Grafana actually
	// asks for on /api/traces/<id>).
	_, tempoProto, _, tpErr := fetchProto(ctx, client, tempoURL)
	_, cerbProto, _, cpErr := fetchProto(ctx, client, cerbURL)

	// (a)+(b): record per-side decode failures. These are the high-
	// signal reasons — a proto decode failure on the cerberus side IS
	// the #199/#650 bug class.
	if tjErr != nil {
		appendReason(&res.Diff, "json_decode", fmt.Sprintf("tempo JSON fetch failed: %v", tjErr))
	}
	if cjErr != nil {
		appendReason(&res.Diff, "json_decode", fmt.Sprintf("cerberus JSON fetch failed: %v", cjErr))
	}
	if tpErr != nil {
		appendReason(&res.Diff, "proto_decode", fmt.Sprintf("tempo proto fetch/decode failed: %v", tpErr))
	}
	if cpErr != nil {
		appendReason(&res.Diff, "proto_decode", fmt.Sprintf("cerberus proto fetch/decode failed: %v", cpErr))
	}

	// (c)+(d): per-side cross-encoding parity. We only run the parity
	// check when BOTH encodings decoded on that side; otherwise the
	// decode-failure reason above already covers the signal.
	if tjErr == nil && tpErr == nil {
		ts, err := projectJSONShape(tempoJSON)
		if err != nil {
			appendReason(&res.Diff, "json_decode", fmt.Sprintf("tempo JSON shape: %v", err))
		} else {
			ps := projectProtoShape(tempoProto)
			if !ts.equal(ps) {
				appendReason(&res.Diff, "encoding_mismatch", fmt.Sprintf("tempo: json=(%s) vs proto=(%s)", ts.summarise(), ps.summarise()))
			}
		}
	}
	if cjErr == nil && cpErr == nil {
		cs, err := projectJSONShape(cerbJSON)
		if err != nil {
			appendReason(&res.Diff, "json_decode", fmt.Sprintf("cerberus JSON shape: %v", err))
		} else {
			ps := projectProtoShape(cerbProto)
			if !cs.equal(ps) {
				appendReason(&res.Diff, "encoding_mismatch", fmt.Sprintf("cerberus: json=(%s) vs proto=(%s)", cs.summarise(), ps.summarise()))
			}
		}
	}

	// Cross-backend trace-shape parity, using the proto-decoded shape
	// when available (the canonical wire format), falling back to the
	// JSON-decoded shape if proto failed. We want SOME comparison even
	// when one side's proto path is broken — that case will surface as
	// a proto_decode reason above PLUS the JSON-shape diff below.
	tempoShape, tempoOK := preferredShape(tempoProto, tempoJSON, tpErr == nil)
	cerbShape, cerbOK := preferredShape(cerbProto, cerbJSON, cpErr == nil)
	if tempoOK && cerbOK {
		if tempoShape.SpanCount != cerbShape.SpanCount {
			appendReason(&res.Diff, "cardinality", fmt.Sprintf("tempo=%d spans, cerberus=%d spans", tempoShape.SpanCount, cerbShape.SpanCount))
		}
		tempoSet := stringSet(tempoShape.SpanIDs)
		cerbSet := stringSet(cerbShape.SpanIDs)
		matched := 0
		var missingInTempo, missingInCerb []string
		for _, id := range cerbShape.SpanIDs {
			if tempoSet[id] {
				matched++
			} else {
				missingInTempo = append(missingInTempo, id)
			}
		}
		for _, id := range tempoShape.SpanIDs {
			if !cerbSet[id] {
				missingInCerb = append(missingInCerb, id)
			}
		}
		sort.Strings(missingInTempo)
		sort.Strings(missingInCerb)
		for _, id := range missingInTempo {
			appendReason(&res.Diff, "missing_in_a", fmt.Sprintf("span %s present in cerberus but missing in tempo", id))
		}
		for _, id := range missingInCerb {
			appendReason(&res.Diff, "missing_in_b", fmt.Sprintf("span %s present in tempo but missing in cerberus", id))
		}
		res.Diff.MatchedCount = matched
	}
}

// preferredShape returns the proto-decoded shape when the proto fetch
// succeeded, else falls back to the JSON-decoded shape. The bool reports
// whether we got a usable shape at all (false means both encodings
// failed, in which case the cross-backend compare is skipped).
func preferredShape(protoMsg *tempopb.Trace, jsonBody []byte, protoOK bool) (traceShape, bool) {
	if protoOK && protoMsg != nil {
		return projectProtoShape(protoMsg), true
	}
	if len(jsonBody) > 0 {
		shape, err := projectJSONShape(jsonBody)
		if err == nil {
			return shape, true
		}
	}
	return traceShape{}, false
}

// appendReason updates Diff.Equal + appends one reason. Keeps the
// Equal-flip discipline local so each call site doesn't risk forgetting
// to flip Equal.
func appendReason(d *Diff, kind, detail string) {
	d.Equal = false
	d.Reasons = append(d.Reasons, DiffReason{Kind: kind, Detail: detail})
}
