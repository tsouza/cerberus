package loki

import (
	"net/http"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestPipelineErrorFor_SelectsLikeReference pins the three-way contract
// of pkg/logql/evaluator.go's RangeVectorEvaluator.Next: reject on the
// FIRST sample carrying `__error__`, unless that sample also carries
// `__preserve_error__="true"`; otherwise answer.
func TestPipelineErrorFor_SelectsLikeReference(t *testing.T) {
	t.Parallel()

	good := chclient.Sample{Labels: map[string]string{"job": "api"}, Value: 3}
	errored := chclient.Sample{Labels: map[string]string{
		"job":               "api",
		"__error__":         "SampleExtractionErr",
		"__error_details__": `strconv.ParseFloat: parsing "oops": invalid syntax`,
	}}
	preserved := chclient.Sample{Labels: map[string]string{
		"job":                "api",
		"__error__":          "SampleExtractionErr",
		"__preserve_error__": "true",
	}}
	emptyKind := chclient.Sample{Labels: map[string]string{"job": "api", "__error__": ""}}

	cases := []struct {
		name    string
		samples []chclient.Sample
		reject  bool
	}{
		{name: "no error sample answers", samples: []chclient.Sample{good}, reject: false},
		{name: "an error sample rejects", samples: []chclient.Sample{good, errored}, reject: true},
		{name: "preserve_error opts back into an answer", samples: []chclient.Sample{good, preserved}, reject: false},
		// `| __error__=""` keeps the label present and empty; upstream's
		// Has() is false for a label it never set, and cerberus's own
		// error-mark expression stamps "" on the rows that did not fail,
		// so an empty value must not reject.
		{name: "empty error value answers", samples: []chclient.Sample{good, emptyKind}, reject: false},
		{name: "no samples answers", samples: nil, reject: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pipelineErrorFor(tc.samples)
			if tc.reject {
				if got == nil {
					t.Fatalf("pipelineErrorFor returned nil; reference Loki refuses this query with ErrPipeline")
				}
				if got.Status != http.StatusBadRequest {
					t.Fatalf("status = %d, want %d (upstream maps ErrPipeline to 400 in pkg/util/server/error.go)", got.Status, http.StatusBadRequest)
				}
				return
			}
			if got != nil {
				t.Fatalf("pipelineErrorFor rejected with %v; reference Loki answers this query", got.Err)
			}
		})
	}
}

// TestPromLabelsString pins the label rendering upstream interpolates
// into the message: prometheus/model/labels.Labels.String().
func TestPromLabelsString(t *testing.T) {
	t.Parallel()

	got := promLabelsString(map[string]string{
		"job":       "api",
		"__error__": "SampleExtractionErr",
		"quoted":    `a"b`,
	})
	want := `{__error__="SampleExtractionErr", job="api", quoted="a\"b"}`
	if got != want {
		t.Fatalf("promLabelsString = %s, want %s", got, want)
	}
}
