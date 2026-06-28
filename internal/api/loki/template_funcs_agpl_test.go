//go:build agpl_oracle

// A/B oracle: cerberus's in-house template funcmap must expose exactly
// the same set of function names as upstream Loki's
// AddLineAndTimestampFunctions, so a future upstream addition surfaces
// here rather than silently diverging.
//
// This is the ONLY loki-template test that imports the AGPL pkg/logql/log
// package, gated behind the `agpl_oracle` build tag. Run with:
//
//	CGO_ENABLED=1 go test -tags agpl_oracle ./internal/api/loki/
package loki

import (
	"testing"

	loglib "github.com/grafana/loki/v3/pkg/logql/log"
)

func TestTemplateFunc_ParityWithUpstreamFuncmap(t *testing.T) {
	t.Parallel()
	cer := templateFuncs(func() string { return "" }, func() int64 { return 0 })
	up := loglib.AddLineAndTimestampFunctions(func() string { return "" }, func() int64 { return 0 })
	if len(cer) != len(up) {
		t.Fatalf("funcmap size mismatch: cerberus=%d upstream=%d", len(cer), len(up))
	}
	for k := range up {
		if _, ok := cer[k]; !ok {
			t.Errorf("cerberus funcmap missing upstream func %q", k)
		}
	}
	for k := range cer {
		if _, ok := up[k]; !ok {
			t.Errorf("cerberus funcmap has extra func %q not in upstream", k)
		}
	}
}
