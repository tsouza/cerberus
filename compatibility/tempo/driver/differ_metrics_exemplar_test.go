package main

import (
	"strings"
	"testing"
)

// This pins #3182. The exemplar comparison was a count check and nothing else,
// and it was marked informational, so it could not fail. That is how a real
// wire divergence — cerberus emitting `timestamp_ms` where reference Tempo
// emits `timestampMs` — sat inside the compared responses and scored PASS: the
// differ's own decoder read the cerberus timestamp as zero, and comparing only
// `len()` never looked.
//
// The rule the fix rests on is that a sampling difference changes how MANY
// exemplars a side emits and never the spelling of a field. So counts stay
// lenient and SHAPE is blocking, checked per side.

func metricsBodyWithExemplar(exemplar string) []byte {
	return []byte(`{"series":[{"labels":[{"key":"k","value":{"stringValue":"v"}}],` +
		`"samples":[{"timestampMs":1000,"value":1.5}],"exemplars":[` + exemplar + `]}]}`)
}

func TestCompareMetrics_ExemplarWithUpstreamKeyIsClean(t *testing.T) {
	good := `{"value":1,"timestampMs":1700000000000}`
	d, err := CompareMetrics(metricsBodyWithExemplar(good), metricsBodyWithExemplar(good), "tempo", "cerberus", DiffOptions{})
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Errorf("two well-formed exemplars must compare equal; reasons=%+v", d.Reasons)
	}
}

func TestCompareMetrics_ExemplarUnderTheWrongKeyIsBlocking(t *testing.T) {
	// The EXACT divergence that was invisible: reference Tempo emits
	// `timestampMs`, cerberus emitted `timestamp_ms`. Both sides carry ONE
	// exemplar, so the count check — the only check there used to be — is
	// satisfied and reports nothing.
	good := `{"value":1,"timestampMs":1700000000000}`
	wrong := `{"value":1,"timestamp_ms":1700000000000}`
	d, err := CompareMetrics(metricsBodyWithExemplar(good), metricsBodyWithExemplar(wrong), "tempo", "cerberus", DiffOptions{})
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if d.Equal {
		t.Fatal("an exemplar emitted under a key reference Tempo does not use must NOT compare equal")
	}
	var named bool
	for _, r := range d.Reasons {
		if r.Kind == reasonKindFieldMismatch && strings.Contains(r.Detail, "cerberus exemplar[0]") {
			named = true
		}
	}
	if !named {
		t.Errorf("the failing side and index must be named; reasons=%+v", d.Reasons)
	}
}

// A count difference must remain informational — the leniency the fix keeps.
// Without this, "make exemplars strict" would have quietly become "fail on any
// sampling difference", which is a different and wrong gate.
func TestCompareMetrics_ExemplarCountDifferenceStaysInformational(t *testing.T) {
	one := `{"value":1,"timestampMs":1700000000000}`
	two := `{"value":1,"timestampMs":1700000000000},{"value":2,"timestampMs":1700000001000}`
	d, err := CompareMetrics(metricsBodyWithExemplar(two), metricsBodyWithExemplar(one), "tempo", "cerberus", DiffOptions{})
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Errorf("differing exemplar COUNTS must stay informational; reasons=%+v", d.Reasons)
	}
	var informed bool
	for _, r := range d.Reasons {
		if r.Kind == "exemplar_count" {
			informed = true
		}
	}
	if !informed {
		t.Errorf("the count divergence must still be reported; reasons=%+v", d.Reasons)
	}
}

// Presence is not a count. A count difference stays informational because the
// two backends SAMPLE exemplars differently; a side that emits NONE where the
// reference emits some has not sampled differently, it has stopped emitting
// exemplars — the shape the producer degrades to on an emit or execute failure
// (internal/api/tempo/metrics_exec.go keeps an empty slice), which a count-only
// check scored as parity. The #3182 timestamp bug was caught only because at
// least one exemplar happened to reach the wire; this makes zero-where-some-
// expected a mismatch in its own right.
func TestCompareMetrics_ExemplarAbsenceAgainstAPopulatedReferenceIsAMismatch(t *testing.T) {
	one := `{"value":1,"timestampMs":1700000000000}`
	d, err := CompareMetrics(metricsBodyWithExemplar(one), metricsBodyWithExemplar(""), "tempo", "cerberus", DiffOptions{})
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if d.Equal {
		t.Fatalf("a reference with exemplars and a cerberus side with none must NOT compare equal; reasons=%+v", d.Reasons)
	}
	var named bool
	for _, r := range d.Reasons {
		if r.Kind == reasonKindFieldMismatch && strings.Contains(r.Detail, "exemplar presence") && strings.Contains(r.Detail, "cerberus=0") {
			named = true
		}
	}
	if !named {
		t.Errorf("the missing side must be named as an exemplar-presence mismatch; reasons=%+v", d.Reasons)
	}

	// The other direction is a sampling difference in cerberus's favour, not a
	// gap: the reference having sampled none says nothing about what cerberus
	// should have emitted.
	d, err = CompareMetrics(metricsBodyWithExemplar(""), metricsBodyWithExemplar(one), "tempo", "cerberus", DiffOptions{})
	if err != nil {
		t.Fatalf("CompareMetrics: %v", err)
	}
	if !d.Equal {
		t.Errorf("exemplars on the cerberus side alone must stay informational; reasons=%+v", d.Reasons)
	}
}
