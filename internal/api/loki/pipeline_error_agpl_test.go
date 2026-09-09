//go:build agpl_oracle

package loki

import (
	"testing"

	"github.com/grafana/loki/v3/pkg/logqlmodel"
	"github.com/prometheus/prometheus/model/labels"
)

// TestPipelineErrorMessageMatchesUpstream compares cerberus's rejection
// text against the reference type that produces it —
// logqlmodel.PipelineError.Error() — rather than against a copy of the
// string. The message is a wire contract: a Grafana user reads it, and
// the compatibility differ compares it, so a drift in upstream's wording
// must surface here as a disagreement instead of being mirrored into a
// golden.
func TestPipelineErrorMessageMatchesUpstream(t *testing.T) {
	t.Parallel()

	lbls := map[string]string{
		"job":                        logqlmodel.ErrorLabel, // a value with an underscore-heavy shape
		"latency":                    "oops",
		logqlmodel.ErrorLabel:        logqlmodel.ErrorLabel,
		logqlmodel.ErrorDetailsLabel: `time: invalid duration "oops"`,
	}
	lbls[logqlmodel.ErrorLabel] = "SampleExtractionErr"

	want := logqlmodel.NewPipelineErr(labels.FromMap(lbls)).Error()
	got := pipelineErrorMessage(lbls[logqlmodel.ErrorLabel], lbls)
	if got != want {
		t.Fatalf("pipeline error message diverges from upstream's own\n got: %q\nwant: %q", got, want)
	}
}
