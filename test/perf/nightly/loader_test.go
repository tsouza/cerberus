package nightly

import (
	"strings"
	"testing"
)

func TestMapArraysExpr_PlainKeys(t *testing.T) {
	got := mapArraysExpr("Attributes", []string{"app_id", "method"})
	want := "mapFromArrays(['app_id', 'method'], [Attributes.`app_id`, Attributes.`method`])"
	if got != want {
		t.Errorf("mapArraysExpr =\n%s\nwant\n%s", got, want)
	}
}

func TestMapArraysExpr_DottedKeys(t *testing.T) {
	got := mapArraysExpr("ResourceAttributes", []string{"service.name", "k8s.namespace.name"})
	// Dotted field names need backtick-quoted dot access — ClickHouse would
	// otherwise try to parse the dot as nested member access on an
	// intermediate identifier that doesn't exist.
	if !strings.Contains(got, "ResourceAttributes.`service.name`") {
		t.Errorf("mapArraysExpr must backtick-quote dotted keys, got: %s", got)
	}
	if !strings.Contains(got, "ResourceAttributes.`k8s.namespace.name`") {
		t.Errorf("mapArraysExpr must backtick-quote dotted keys, got: %s", got)
	}
}

func TestMapArraysExpr_EmptyKeys(t *testing.T) {
	got := mapArraysExpr("Attributes", nil)
	want := "mapFromArrays([], [])"
	if got != want {
		t.Errorf("mapArraysExpr(no keys) = %q, want %q", got, want)
	}
}

func TestSampleAttributeKeys_CoverEveryMetric(t *testing.T) {
	for _, metric := range []string{histogramMetric, sumMetric, gaugeMetric} {
		keys, ok := sampleAttributeKeys[metric]
		if !ok || len(keys) == 0 {
			t.Errorf("sampleAttributeKeys has no entry for metric %q", metric)
		}
	}
}

func TestSampleResourceAttributeKeys_NonEmpty(t *testing.T) {
	if len(sampleResourceAttributeKeys) == 0 {
		t.Fatal("sampleResourceAttributeKeys is empty")
	}
}
