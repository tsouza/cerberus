package migrateverify

import "testing"

func lokiDiscoveryQuery(surface, tagName string) Query {
	return Query{Head: HeadLoki, Lang: "logql", Kind: KindTagDiscovery, Surface: surface, TagName: tagName, Source: discoverySourceTag}
}

func tempoDiscoveryQuery(surface, tagName string) Query {
	return Query{Head: HeadTempo, Lang: "traceql", Kind: KindTagDiscovery, Surface: surface, TagName: tagName, Source: discoverySourceTag}
}

func decodeLokiDiscoveryResult(t *testing.T, body string) lokiDiscoveryPayload {
	t.Helper()
	decoded, err := decodeLokiDiscovery([]byte(body))
	if err != nil {
		t.Fatalf("decodeLokiDiscovery: %v", err)
	}
	res, ok := decoded.(lokiDiscoveryPayload)
	if !ok {
		t.Fatalf("decodeLokiDiscovery returned %T, want lokiDiscoveryPayload", decoded)
	}
	return res
}

func decodeTempoDiscoveryResult(t *testing.T, body string) tempoDiscoveryPayload {
	t.Helper()
	decoded, err := decodeTempoDiscovery([]byte(body))
	if err != nil {
		t.Fatalf("decodeTempoDiscovery: %v", err)
	}
	res, ok := decoded.(tempoDiscoveryPayload)
	if !ok {
		t.Fatalf("decodeTempoDiscovery returned %T, want tempoDiscoveryPayload", decoded)
	}
	return res
}

func TestCompareTagDiscovery_LokiLabelsMatch(t *testing.T) {
	body := `{"status":"success","data":["job","app"]}`
	ref := decodeLokiDiscoveryResult(t, body)
	cer := decodeLokiDiscoveryResult(t, body)
	out := compareTagDiscovery(lokiDiscoveryQuery(SurfaceLokiLabels, ""), ref, cer, Params{})
	if out.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (first-diff: %+v)", out.Verdict, out.FirstDiff)
	}
	if out.Compared != 2 {
		t.Errorf("Compared = %d, want 2 (the union of {job,app})", out.Compared)
	}
}

func TestCompareTagDiscovery_EmptyVsEmptyIsNotProvenParity(t *testing.T) {
	body := `{"status":"success","data":[]}`
	ref := decodeLokiDiscoveryResult(t, body)
	cer := decodeLokiDiscoveryResult(t, body)
	out := compareTagDiscovery(lokiDiscoveryQuery(SurfaceLokiLabels, ""), ref, cer, Params{})
	if out.Verdict != VerdictMatch {
		t.Fatalf("verdict = %q, want match (two empty label sets do agree)", out.Verdict)
	}
	if out.Compared != 0 {
		t.Errorf("Compared = %d, want 0: nothing was diffed", out.Compared)
	}
}

func TestCompareTagDiscovery_LokiLabelValueMissingDiverges(t *testing.T) {
	ref := decodeLokiDiscoveryResult(t, `{"status":"success","data":["api","web"]}`)
	cer := decodeLokiDiscoveryResult(t, `{"status":"success","data":["api"]}`)
	out := compareTagDiscovery(lokiDiscoveryQuery(SurfaceLokiLabelValues, "job"), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.ReasonCode != ReasonTagValueMissing || out.FirstDiff.Tag != "job" {
		t.Errorf("FirstDiff = %+v, want tag=job reason=%s", out.FirstDiff, ReasonTagValueMissing)
	}
	if out.FirstDiff.RefValue != "web" || out.FirstDiff.CerberusValue != missingTagValue {
		t.Errorf("FirstDiff values = ref=%q cerberus=%q, want the missing value named", out.FirstDiff.RefValue, out.FirstDiff.CerberusValue)
	}
}

func TestCompareTagDiscovery_TempoTagsV1Match(t *testing.T) {
	body := `{"tagNames":["service.name","http.status_code"]}`
	ref := decodeTempoDiscoveryResult(t, body)
	cer := decodeTempoDiscoveryResult(t, body)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagsV1, ""), ref, cer, Params{})
	if out.Verdict != VerdictMatch || out.Compared != 2 {
		t.Fatalf("verdict/compared = %q/%d, want match/2", out.Verdict, out.Compared)
	}
}

func TestCompareTagDiscovery_TempoTagsV2PerScopeDiverges(t *testing.T) {
	ref := decodeTempoDiscoveryResult(t, `{"scopes":[
		{"name":"resource","tags":["service.name"]},
		{"name":"span","tags":["http.status_code","http.method"]}
	]}`)
	cer := decodeTempoDiscoveryResult(t, `{"scopes":[
		{"name":"resource","tags":["service.name"]},
		{"name":"span","tags":["http.status_code"]}
	]}`)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagsV2, ""), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: the span scope disagrees", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.Scope != "span" || out.FirstDiff.Tag != "http.method" {
		t.Errorf("FirstDiff = %+v, want scope=span tag=http.method", out.FirstDiff)
	}
	// resource(1) + span union(2, "http.status_code" and "http.method") +
	// intrinsic(0, both empty) = 3.
	if out.Compared != 3 {
		t.Errorf("Compared = %d, want 3 (every scope's union, walked in the fixed canonical order)", out.Compared)
	}
}

func TestCompareTagDiscovery_TempoTagValuesV1Match(t *testing.T) {
	body := `{"tagValues":["200","500"]}`
	ref := decodeTempoDiscoveryResult(t, body)
	cer := decodeTempoDiscoveryResult(t, body)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagValuesV1, "http.status_code"), ref, cer, Params{})
	if out.Verdict != VerdictMatch || out.Compared != 2 {
		t.Fatalf("verdict/compared = %q/%d, want match/2", out.Verdict, out.Compared)
	}
}

func TestCompareTagDiscovery_TempoTagValuesV2TypeMismatchDiverges(t *testing.T) {
	ref := decodeTempoDiscoveryResult(t, `{"tagValues":[{"type":"int","value":"200"}]}`)
	cer := decodeTempoDiscoveryResult(t, `{"tagValues":[{"type":"string","value":"200"}]}`)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagValuesV2, "http.status_code"), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: same value, disagreeing type", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.ReasonCode != ReasonTagTypeDiffers {
		t.Errorf("FirstDiff = %+v, want reason=%s", out.FirstDiff, ReasonTagTypeDiffers)
	}
	if out.FirstDiff.RefValue != "int" || out.FirstDiff.CerberusValue != "string" {
		t.Errorf("FirstDiff values = ref=%q cerberus=%q, want the two types", out.FirstDiff.RefValue, out.FirstDiff.CerberusValue)
	}
}

func TestCompareTagDiscovery_PartialReferenceIsUndecidable(t *testing.T) {
	ref := decodeTempoDiscoveryResult(t, `{"tagNames":["service.name"],"metrics":{"totalJobs":10,"completedJobs":3}}`)
	cer := decodeTempoDiscoveryResult(t, `{"tagNames":["service.name"]}`)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagsV1, ""), ref, cer, Params{})
	if out.Verdict != VerdictUndecidable {
		t.Fatalf("verdict = %q, want undecidable: the reference reported its own search as partial", out.Verdict)
	}
	lim := limitationByCode(t, out.Limitations, LimitTagDiscoveryRefPartial)
	if lim.Count != 1 {
		t.Errorf("LimitTagDiscoveryRefPartial count = %d, want 1", lim.Count)
	}
}

func TestCompareTagDiscovery_PartialReferenceStillDivergesOnAMissingTag(t *testing.T) {
	// A partial reference must not MASK a real divergence: a tag the REFERENCE
	// has but cerberus does not is still a bug even under partiality — cerberus
	// never truncates its own enumeration, so its absence is unexplainable by
	// the reference's partiality.
	ref := decodeTempoDiscoveryResult(t, `{"tagNames":["service.name","only.ref"],"metrics":{"totalJobs":10,"completedJobs":3}}`)
	cer := decodeTempoDiscoveryResult(t, `{"tagNames":["service.name"]}`)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagsV1, ""), ref, cer, Params{})
	if out.Verdict != VerdictDiverge {
		t.Fatalf("verdict = %q, want diverge: the reference has a tag cerberus does not", out.Verdict)
	}
	if out.FirstDiff == nil || out.FirstDiff.ReasonCode != ReasonTagMissing || out.FirstDiff.RefValue != "only.ref" {
		t.Errorf("FirstDiff = %+v, want the reference-only tag named", out.FirstDiff)
	}
}

// TestCompareTagDiscovery_CerberusOnlyTagIsUndecidableNotDiverged pins the
// asymmetry: a tag present in cerberus's answer but absent from the
// reference's is NOT diffed as a hard divergence, because both Tempo and Loki
// cap discovery cardinality server-side and return 200 with a
// silently-truncated set when the cap trips, with no field in either wire
// response naming that it happened. Scoring this a hard Diverge (the old
// behaviour) would false-positive-block a healthy migration the instant the
// reference's own cap trimmed one value cerberus correctly still has; scoring
// it a silent Match would hide a real cerberus over-report behind an allow-
// list wearing a comparator's clothes. The honest answer is a named,
// counted, non-blocking Undecidable.
func TestCompareTagDiscovery_CerberusOnlyTagIsUndecidableNotDiverged(t *testing.T) {
	ref := decodeTempoDiscoveryResult(t, `{"tagNames":["service.name"]}`)
	cer := decodeTempoDiscoveryResult(t, `{"tagNames":["service.name","bogus.extra"]}`)
	out := compareTagDiscovery(tempoDiscoveryQuery(SurfaceTempoTagsV1, ""), ref, cer, Params{})
	if out.Verdict != VerdictUndecidable {
		t.Fatalf("verdict = %q, want undecidable: a cerberus-only tag cannot be told apart from reference-side "+
			"cardinality truncation (first-diff: %+v)", out.Verdict, out.FirstDiff)
	}
	if out.FirstDiff != nil {
		t.Errorf("first-diff = %+v, want none: an undecidable dimension names no anchor to chase", out.FirstDiff)
	}
	lim := limitationByCode(t, out.Limitations, LimitTagDiscoveryRefCardinalityUnknown)
	if lim.Count != 1 {
		t.Errorf("%s count = %d, want 1: exactly one tag is cerberus-only", LimitTagDiscoveryRefCardinalityUnknown, lim.Count)
	}
	// The confirmed shared tag is still counted as compared; the ambiguous
	// cerberus-only tag is not — it is evidence of nothing, not a match.
	if out.Compared != 1 {
		t.Errorf("Compared = %d, want 1: the ambiguous tag is not evidence, only the confirmed shared one is", out.Compared)
	}
}

func TestCompareTagDiscovery_PayloadMismatchIsAnError(t *testing.T) {
	out := compareTagDiscovery(lokiDiscoveryQuery(SurfaceLokiLabels, ""), "wrong type", lokiDiscoveryPayload{}, Params{})
	if out.Verdict != VerdictError {
		t.Errorf("verdict = %q, want error on a wired-to-the-wrong-dialect payload", out.Verdict)
	}
}

func TestCompareTagDiscovery_UnknownHeadIsAnError(t *testing.T) {
	out := compareTagDiscovery(Query{Head: "bogus", Kind: KindTagDiscovery}, lokiDiscoveryPayload{}, lokiDiscoveryPayload{}, Params{})
	if out.Verdict != VerdictError {
		t.Errorf("verdict = %q, want error: tag-discovery has no rule for an unknown head", out.Verdict)
	}
}

func TestDecodeTempoDiscovery_DistinguishesV1AndV2TagValues(t *testing.T) {
	v1, err := decodeTempoDiscovery([]byte(`{"tagValues":["a","b"]}`))
	if err != nil {
		t.Fatalf("decode v1: %v", err)
	}
	if p := v1.(tempoDiscoveryPayload); len(p.TagValues) != 2 || p.TagValuesV2 != nil {
		t.Errorf("v1 payload = %+v, want TagValues=[a b], TagValuesV2=nil", p)
	}
	v2, err := decodeTempoDiscovery([]byte(`{"tagValues":[{"type":"string","value":"a"}]}`))
	if err != nil {
		t.Fatalf("decode v2: %v", err)
	}
	if p := v2.(tempoDiscoveryPayload); p.TagValues != nil || len(p.TagValuesV2) != 1 {
		t.Errorf("v2 payload = %+v, want TagValues=nil, TagValuesV2 with 1 entry", p)
	}
}

func TestDecodeLokiDiscovery_RejectsUnusableBody(t *testing.T) {
	if _, err := decodeLokiDiscovery([]byte("not json")); err == nil {
		t.Error("want a decode error on unparseable JSON")
	}
}
