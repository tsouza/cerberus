package traceql_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

func TestZZMatrix(t *testing.T) {
	s := schema.DefaultOTelTraces()
	for _, q := range []string{
		`{ span.cache.hit = true }`, `{ resource.cache.hit = true }`, `{ .cache.hit = true }`,
		`{ span.cache.hit = false }`, `{ resource.cache.hit = false }`, `{ .cache.hit = false }`,
		`{ .attempt = 1 }`, `{ .ratio = 0.95 }`, `{ .name2 = "x" }`, `{ .name2 =~ "x.*" }`, `{ .attempt > 1 }`,
	} {
		_, args := emitTraceQLWithArgs(t, q, s)
		bad := ""
		for _, a := range args {
			if _, isBool := a.(bool); isBool {
				bad = "  <-- BINDS A GO BOOL (ClickHouse 386)"
			}
		}
		t.Logf("%-30s ARGS: %#v%s", q, args, bad)
	}
}
