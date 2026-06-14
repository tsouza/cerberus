//go:build !chaos_sleep

package chsql

import "context"

// chaosSleepWrap is the production (default-build) no-op: it returns the
// rendered SQL and args completely unchanged. The deterministic-chaos
// sleep injection lives only in the `chaos_sleep`-tagged sibling file
// (chaos_sleep.go), so production builds compile the splice out entirely
// — there is no header read, no ClickHouse sleep, and no extra context
// key in the binary.
//
// Emit calls this unconditionally; the build tag selects which
// implementation is linked. Keeping the call site tag-free keeps emit.go
// free of conditional compilation.
func chaosSleepWrap(_ context.Context, sql string, args []any) (string, []any) {
	return sql, args
}
