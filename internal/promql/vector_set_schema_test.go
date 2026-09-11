package promql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

func TestRequireVectorSetOpSampleKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []chplan.SampleKind{
		chplan.SampleKindFloat,
		chplan.SampleKindHistogram,
		chplan.SampleKindMixed,
	} {
		if err := requireVectorSetOpSampleKind("left", kind); err != nil {
			t.Errorf("requireVectorSetOpSampleKind(%s) = %v, want success", kind, err)
		}
	}
	for _, kind := range []chplan.SampleKind{
		chplan.SampleKindOpaque,
		chplan.SampleKindInvalid,
	} {
		err := requireVectorSetOpSampleKind("right", kind)
		if err == nil {
			t.Fatalf("requireVectorSetOpSampleKind(%s) succeeded", kind)
		}
		for _, want := range []string{"right", kind.String()} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	}
}
