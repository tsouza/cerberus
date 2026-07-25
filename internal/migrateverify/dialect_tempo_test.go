package migrateverify

import (
	"math"
	"strings"
	"testing"
)

// TestDecodeTempoMetrics_ReferenceAndCerberusShapes pins that the decoder reads
// BOTH wire shapes this lane sees for the same data:
//
//   - reference Tempo, via gogo/protobuf jsonpb: timestampMs is a QUOTED integer,
//     and a zero `value` is OMITTED entirely (EmitDefaults is off).
//   - cerberus's own Tempo head, via encoding/json: timestampMs is a NUMBER and
//     `value` is always present.
//
// It also asserts the two decode to BIT-IDENTICAL Sample.T, because Compare
// aligns samples on exact float64 map keys: a one-ULP difference between the two
// sides would report every step as "no sample at this step".
func TestDecodeTempoMetrics_ReferenceAndCerberusShapes(t *testing.T) {
	const refBody = `{"series":[{"labels":[{"key":"span.name","value":{"stringValue":"GET /x"}}],` +
		`"samples":[{"timestampMs":"1700000000000","value":1.5},{"timestampMs":"1700000060000"}]}]}`
	const cerBody = `{"series":[{"labels":[{"key":"span.name","value":{"stringValue":"GET /x"}}],` +
		`"samples":[{"timestampMs":1700000000000,"value":1.5},{"timestampMs":1700000060000,"value":0}],` +
		`"exemplars":[]}]}`

	ref, err := decodeTempoMetrics([]byte(refBody))
	if err != nil {
		t.Fatalf("decode reference shape: %v", err)
	}
	cer, err := decodeTempoMetrics([]byte(cerBody))
	if err != nil {
		t.Fatalf("decode cerberus shape: %v", err)
	}

	// A synthesised matrix result type is mandatory: without it verifyOne classes
	// every TraceQL query "unsupported" and the lane compares nothing.
	for name, res := range map[string]RangeResult{"reference": ref, "cerberus": cer} {
		if res.ResultType != resultTypeMatrix {
			t.Errorf("%s ResultType = %q, want %q", name, res.ResultType, resultTypeMatrix)
		}
		if len(res.Series) != 1 {
			t.Fatalf("%s Series = %+v, want exactly 1", name, res.Series)
		}
		if got := res.Series[0].Labels["span.name"]; got != "GET /x" {
			t.Errorf("%s label span.name = %q, want GET /x", name, got)
		}
		if len(res.Series[0].Samples) != 2 {
			t.Fatalf("%s Samples = %+v, want 2", name, res.Series[0].Samples)
		}
		if got := res.Series[0].Samples[0].V; got != 1.5 {
			t.Errorf("%s Samples[0].V = %v, want 1.5", name, got)
		}
		// The omitted-value / explicit-zero pair must both read as 0.
		if got := res.Series[0].Samples[1].V; got != 0 {
			t.Errorf("%s Samples[1].V = %v, want 0 (a zero value is omitted on the reference wire)", name, got)
		}
	}

	for i := range ref.Series[0].Samples {
		rb := math.Float64bits(ref.Series[0].Samples[i].T)
		cb := math.Float64bits(cer.Series[0].Samples[i].T)
		if rb != cb {
			t.Errorf("Samples[%d].T bits differ across the two wire shapes: ref %#x vs cerberus %#x; Compare aligns on exact float keys, so any difference reports every step as missing",
				i, rb, cb)
		}
	}
}

// TestDecodeTempoMetrics_MillisecondsToSeconds pins the unit conversion against a
// LITERAL expected value rather than recomputing the decoder's own arithmetic: a
// decoder dividing by 1e6 instead of 1e3 would pass a self-referential assertion.
func TestDecodeTempoMetrics_MillisecondsToSeconds(t *testing.T) {
	res, err := decodeTempoMetrics([]byte(`{"series":[{"labels":[],"samples":[{"timestampMs":"1700000000123","value":1}]}]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	const wantT = 1700000000.123
	if got := res.Series[0].Samples[0].T; got != wantT {
		t.Errorf("Sample.T = %v, want %v (Tempo ships integer milliseconds; every lane's Sample.T is Unix seconds)", got, wantT)
	}
}

// TestDecodeTempoMetrics_OmittedSeries pins that an absent / empty series list is
// zero series and NOT an error: jsonpb omits nil slices, so an empty result body
// is `{}`.
func TestDecodeTempoMetrics_OmittedSeries(t *testing.T) {
	for _, body := range []string{`{}`, `{"series":null}`, `{"series":[]}`} {
		res, err := decodeTempoMetrics([]byte(body))
		if err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		if len(res.Series) != 0 {
			t.Errorf("decode %s: Series = %+v, want none", body, res.Series)
		}
		if res.ResultType != resultTypeMatrix {
			t.Errorf("decode %s: ResultType = %q, want %q", body, res.ResultType, resultTypeMatrix)
		}
	}
}

// TestDecodeTempoMetrics_NonFiniteValues pins jsonpb's three quoted non-finite
// tokens, and that any OTHER string is a hard error rather than a silent zero.
func TestDecodeTempoMetrics_NonFiniteValues(t *testing.T) {
	mk := func(v string) string {
		return `{"series":[{"labels":[],"samples":[{"timestampMs":"1700000000000","value":` + v + `}]}]}`
	}

	nan, err := decodeTempoMetrics([]byte(mk(`"NaN"`)))
	if err != nil {
		t.Fatalf(`decode "NaN": %v`, err)
	}
	if !math.IsNaN(nan.Series[0].Samples[0].V) {
		t.Errorf(`"NaN" decoded to %v, want NaN`, nan.Series[0].Samples[0].V)
	}

	pos, err := decodeTempoMetrics([]byte(mk(`"Infinity"`)))
	if err != nil {
		t.Fatalf(`decode "Infinity": %v`, err)
	}
	if !math.IsInf(pos.Series[0].Samples[0].V, 1) {
		t.Errorf(`"Infinity" decoded to %v, want +Inf`, pos.Series[0].Samples[0].V)
	}

	neg, err := decodeTempoMetrics([]byte(mk(`"-Infinity"`)))
	if err != nil {
		t.Fatalf(`decode "-Infinity": %v`, err)
	}
	if !math.IsInf(neg.Series[0].Samples[0].V, -1) {
		t.Errorf(`"-Infinity" decoded to %v, want -Inf`, neg.Series[0].Samples[0].V)
	}

	if _, err := decodeTempoMetrics([]byte(mk(`"banana"`))); err == nil {
		t.Error(`a non-numeric, non-jsonpb sample value must error, not compare as zero`)
	}
}

// TestDecodeTempoMetrics_TimestampShapes pins that a timestamp neither a number
// nor a quoted integer is an error rather than a zero-valued sample landing on the
// epoch (which would compare against nothing and read as a coverage gap).
func TestDecodeTempoMetrics_TimestampShapes(t *testing.T) {
	bad := []string{`"not-a-number"`, `true`, `{}`}
	for _, ts := range bad {
		body := `{"series":[{"labels":[],"samples":[{"timestampMs":` + ts + `,"value":1}]}]}`
		if _, err := decodeTempoMetrics([]byte(body)); err == nil {
			t.Errorf("timestampMs=%s must error, not decode to 0", ts)
		}
	}
}

// TestDecodeTempoMetrics_LabelVariants pins every AnyValue variant this lane
// flattens, including the flat `"value":"str"` shape cerberus's own decoder
// accepts and the empty `{}` / null / absent cases.
func TestDecodeTempoMetrics_LabelVariants(t *testing.T) {
	const body = `{"series":[{"labels":[
		{"key":"s","value":{"stringValue":"api"}},
		{"key":"i","value":{"intValue":"42"}},
		{"key":"in","value":{"intValue":42}},
		{"key":"d","value":{"doubleValue":0.99}},
		{"key":"b","value":{"boolValue":true}},
		{"key":"bf","value":{"boolValue":false}},
		{"key":"empty","value":{}},
		{"key":"null","value":null},
		{"key":"absent"},
		{"key":"flat","value":"x"}
	],"samples":[]}]}`
	res, err := decodeTempoMetrics([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]string{
		"s": "api", "i": "42", "in": "42", "d": "0.99", "b": "true", "bf": "false",
		"empty": "", "null": "", "absent": "", "flat": "x",
	}
	got := res.Series[0].Labels
	if len(got) != len(want) {
		t.Fatalf("labels = %+v, want %+v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %q = %q, want %q", k, got[k], v)
		}
	}
}

// TestDecodeTempoMetrics_DoubleAndStringPhiAgree pins the quantile-series key
// agreement: reference Tempo ships the `p` label as a typed doubleValue while
// cerberus ships it stringified, so the decoder must render the double with the
// SAME formatter cerberus uses (strconv.FormatFloat 'f', -1, 64). Without this,
// every quantile_over_time series lands under a different canonical key on the two
// sides and the whole query reports as two missing series.
// The last row is the one that pins the FORMATTER rather than merely the
// agreement: for 1e-7 the shortest-'g' rendering ("1e-07") and the plain-decimal
// rendering ("0.0000001") differ, so a decoder using fmt.Sprint / 'g' produces a
// key cerberus's own FormatFloat('f', -1, 64) writer can never match.
func TestDecodeTempoMetrics_DoubleAndStringPhiAgree(t *testing.T) {
	for _, phi := range []string{"0.99", "0.5", "0.999", "0.0000001"} {
		refBody := `{"series":[{"labels":[{"key":"p","value":{"doubleValue":` + phi + `}}],"samples":[]}]}`
		cerBody := `{"series":[{"labels":[{"key":"p","value":{"stringValue":"` + phi + `"}}],"samples":[]}]}`
		ref, err := decodeTempoMetrics([]byte(refBody))
		if err != nil {
			t.Fatalf("decode reference phi %s: %v", phi, err)
		}
		cer, err := decodeTempoMetrics([]byte(cerBody))
		if err != nil {
			t.Fatalf("decode cerberus phi %s: %v", phi, err)
		}
		refKey := canonicalLabels(ref.Series[0].Labels)
		cerKey := canonicalLabels(cer.Series[0].Labels)
		if refKey != cerKey {
			t.Errorf("phi %s: reference key %s != cerberus key %s; quantile series would not align", phi, refKey, cerKey)
		}
		if ref.Series[0].Labels["p"] != phi {
			t.Errorf("phi %s: doubleValue rendered as %q", phi, ref.Series[0].Labels["p"])
		}
	}
}

// TestDecodeTempoMetrics_DoubleAndStringBucketAgree pins the OTHER numeric label
// space, which does NOT share the `p` label's formatter.
//
// Reference Tempo buckets a duration with Log2Bucketize(nanos)/1e9 and ships the
// edge as the `__bucket` label's doubleValue. Cerberus's own Tempo head ships the
// same edge as a stringValue normalised with strconv.FormatFloat(f, 'g', -1, 64)
// — internal/api/tempo/metrics_query_range_histogram.go's
// normalizeHistogramBucketLabels, pinned to "1.024e-06" by that package's own
// tests — chosen so a sub-100µs edge does not read as ClickHouse's plain-decimal
// "0.000001024". A decoder that renders every doubleValue with the `p` label's 'f'
// verb therefore keys the reference's 1.024e-06 as "0.000001024" while cerberus
// keys it "1.024e-06", and EVERY histogram_over_time series below 1e-4 s reports
// as present-on-one-side-only: a whole-lane divergence that does not exist.
//
// The magnitudes span both ends of the plain-decimal window: 1.024e-06 and
// 1.28e-07 are typical span-latency edges, 1.048576e+06 is an int-attribute
// histogram edge above the 1e21-free but 'f'-vs-'g' divergent range, and 0.25 is
// inside the window where the two verbs agree (so the test also proves the fix
// does not perturb the common case).
func TestDecodeTempoMetrics_DoubleAndStringBucketAgree(t *testing.T) {
	for _, bucket := range []string{"1.024e-06", "1.28e-07", "1.048576e+06", "0.25"} {
		refBody := `{"series":[{"labels":[{"key":"__bucket","value":{"doubleValue":` + bucket + `}}],"samples":[]}]}`
		cerBody := `{"series":[{"labels":[{"key":"__bucket","value":{"stringValue":"` + bucket + `"}}],"samples":[]}]}`
		ref, err := decodeTempoMetrics([]byte(refBody))
		if err != nil {
			t.Fatalf("decode reference bucket %s: %v", bucket, err)
		}
		cer, err := decodeTempoMetrics([]byte(cerBody))
		if err != nil {
			t.Fatalf("decode cerberus bucket %s: %v", bucket, err)
		}
		refKey := canonicalLabels(ref.Series[0].Labels)
		cerKey := canonicalLabels(cer.Series[0].Labels)
		if refKey != cerKey {
			t.Errorf("bucket %s: reference key %s != cerberus key %s; histogram series would not align", bucket, refKey, cerKey)
		}
		if got := ref.Series[0].Labels["__bucket"]; got != bucket {
			t.Errorf("bucket %s: doubleValue rendered as %q, but cerberus's Tempo head writes %q", bucket, got, bucket)
		}
	}
}

// TestTempoFloatVerb_IsPerLabelNotGlobal pins WHY the verb is a per-key lookup:
// cerberus's two numeric-label writers disagree by design, so a single global verb
// must mis-key one of the two label spaces. Collapsing tempoFloatVerb back to one
// constant fails here as well as in the two agreement tests above.
func TestTempoFloatVerb_IsPerLabelNotGlobal(t *testing.T) {
	if got := tempoFloatVerb(tempoBucketLabel); got != tempoBucketFloatVerb {
		t.Errorf("tempoFloatVerb(%q) = %q, want %q (normalizeHistogramBucketLabels writes 'g')",
			tempoBucketLabel, got, tempoBucketFloatVerb)
	}
	if got := tempoFloatVerb(tempoQuantileLabel); got != tempoScalarFloatVerb {
		t.Errorf("tempoFloatVerb(%q) = %q, want %q (formatPhi writes 'f')",
			tempoQuantileLabel, got, tempoScalarFloatVerb)
	}
	if got := tempoFloatVerb("span.duration"); got != tempoScalarFloatVerb {
		t.Errorf("tempoFloatVerb on a grouped attribute = %q, want %q (ClickHouse toString is plain-decimal)",
			got, tempoScalarFloatVerb)
	}
}

// TestDecodeTempoMetrics_UnsupportedLabelVariantsError pins that a composite
// AnyValue is a HARD ERROR naming the variant, never flattened to "". Collapsing
// arrayValue to "" (the lenient default branch) folds two genuinely distinct
// series into one canonical key and reports a false match.
func TestDecodeTempoMetrics_UnsupportedLabelVariantsError(t *testing.T) {
	cases := map[string]string{
		"arrayValue":  `{"arrayValue":{"values":[{"intValue":"1"}]}}`,
		"kvlistValue": `{"kvlistValue":{"values":[{"key":"a","value":{"stringValue":"b"}}]}}`,
		"bytesValue":  `{"bytesValue":"AAEC"}`,
	}
	for variant, value := range cases {
		body := `{"series":[{"labels":[{"key":"k","value":` + value + `}],"samples":[]}]}`
		_, err := decodeTempoMetrics([]byte(body))
		if err == nil {
			t.Errorf("%s must be a hard error: collapsing it to \"\" folds distinct series together", variant)
			continue
		}
		if !strings.Contains(err.Error(), variant) {
			t.Errorf("%s error should name the variant, got %q", variant, err)
		}
	}
}

// TestDecodeTempoMetrics_DuplicateLabelKeyErrors pins that Tempo's ORDERED label
// list is not silently last-wins-collapsed into a map. Cerberus can never emit a
// duplicate (its labels come from a Go map), so one is a genuine wire anomaly.
func TestDecodeTempoMetrics_DuplicateLabelKeyErrors(t *testing.T) {
	const body = `{"series":[{"labels":[
		{"key":"k","value":{"stringValue":"a"}},
		{"key":"k","value":{"stringValue":"b"}}
	],"samples":[]}]}`
	_, err := decodeTempoMetrics([]byte(body))
	if err == nil {
		t.Fatal("a duplicate label key must error, not last-wins-collapse two distinct series into one")
	}
	if !strings.Contains(err.Error(), `"k"`) {
		t.Errorf("duplicate-key error should name the key, got %q", err)
	}
}

// TestDecodeTempoMetrics_PartialStatusErrors pins that a truncated reference
// result is rejected at the DECODER, so the lane records an error instead of
// diffing a partial baseline against a complete cerberus result and manufacturing
// a false divergence.
func TestDecodeTempoMetrics_PartialStatusErrors(t *testing.T) {
	const partial = `{"status":"PARTIAL","message":"exceeded max blocks","series":[]}`
	_, err := decodeTempoMetrics([]byte(partial))
	if err == nil {
		t.Fatal("a PARTIAL tempo response must error: a truncated result is not a trustworthy baseline")
	}
	if !strings.Contains(err.Error(), "PARTIAL") {
		t.Errorf("error should name the status, got %q", err)
	}
	if !strings.Contains(err.Error(), "exceeded max blocks") {
		t.Errorf("error should carry the backend message, got %q", err)
	}

	// COMPLETE and an absent status are both trustworthy: reference Tempo omits
	// the enum when it is the zero value (COMPLETE).
	for _, body := range []string{`{"status":"COMPLETE","series":[]}`, `{"series":[]}`} {
		if _, err := decodeTempoMetrics([]byte(body)); err != nil {
			t.Errorf("decode %s: %v (an empty status means COMPLETE)", body, err)
		}
	}
}

// TestDecodeTempoMetrics_ToleratesUnknownFields pins that the decoder does NOT use
// DisallowUnknownFields: reference bodies carry `metrics` and `exemplars` this
// lane deliberately ignores, and rejecting them would fail every Tempo query.
func TestDecodeTempoMetrics_ToleratesUnknownFields(t *testing.T) {
	const body = `{"series":[{"labels":[],"samples":[{"timestampMs":"1700000000000","value":2}],
		"exemplars":[{"value":1,"timestamp_ms":1700000000000,"traceID":"abc"}]}],
		"metrics":{"inspectedBytes":"12345","completedJobs":3}}`
	res, err := decodeTempoMetrics([]byte(body))
	if err != nil {
		t.Fatalf("a body carrying metrics/exemplars must decode: %v", err)
	}
	if len(res.Series) != 1 || len(res.Series[0].Samples) != 1 || res.Series[0].Samples[0].V != 2 {
		t.Errorf("Series = %+v, want the one 2-valued sample", res.Series)
	}
}

// TestDecodeTempoMetrics_MalformedBodyErrors pins that a non-JSON body is a decode
// error rather than an empty-and-therefore-matching result.
func TestDecodeTempoMetrics_MalformedBodyErrors(t *testing.T) {
	if _, err := decodeTempoMetrics([]byte(`{not json`)); err == nil {
		t.Error("a malformed body must error: an empty result would compare as a match against an empty cerberus result")
	}
}
