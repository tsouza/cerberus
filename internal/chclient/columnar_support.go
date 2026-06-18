package chclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClickHouse/ch-go"
	chproto "github.com/ClickHouse/ch-go/proto"
	"github.com/ClickHouse/clickhouse-go/v2"
)

// columnar_support.go — the conn-agnostic glue the ch-go matrix path needs:
// positional `?` arg binding (ch-go takes a raw SQL body, not clickhouse-go's
// `?` placeholders), settings translation, progress bridging, and the error
// sentinels the decoder uses to signal "not the matrix shape" / "budget
// exceeded" without conflating them with transport failures.

// errMatrixMismatch is the sentinel an OnResult callback returns when the
// streamed block is NOT the four-column matrix projection (or carries an arg
// type the columnar binder does not handle): QueryCursor then falls back to the
// row path. It is never surfaced to the caller.
var errMatrixMismatch = errors.New("chclient: not the columnar matrix shape")

// errBudgetExceeded is the sentinel an OnResult callback returns to STOP the
// ch-go stream once the sample budget is crossed. The decoder has already
// latched the real *TooManySamplesError on the cursor; this sentinel only
// unwinds pool.Do without misclassifying the stop as a CH failure.
var errBudgetExceeded = errors.New("chclient: columnar sample budget exceeded")

func isMatrixMismatch(err error) bool { return errors.Is(err, errMatrixMismatch) }
func isBudgetErr(err error) bool      { return errors.Is(err, errBudgetExceeded) }

// chSettings translates the per-query clickhouse.Settings map (max_memory_usage
// / max_execution_time / timeout_overflow_mode / plan-shape settings) into
// ch-go's []ch.Setting so the columnar dial runs under the IDENTICAL server
// settings the row path applies. Values are stringified to match ch-go's wire
// contract (Setting.Value is a string). A nil/empty map yields nil settings.
func chSettings(s clickhouse.Settings) []ch.Setting {
	if len(s) == 0 {
		return nil
	}
	out := make([]ch.Setting, 0, len(s))
	for k, v := range s {
		out = append(out, ch.Setting{
			Key:       k,
			Value:     fmt.Sprint(v),
			Important: true,
		})
	}
	return out
}

// progressBridge adapts ch-go's OnProgress (which reports per-packet DELTAS)
// onto the existing progressRecorder (which latches running totals). It
// accumulates the deltas into a running total and feeds the recorder the same
// max-of-snapshot shape its clickhouse-go onProgress path sees, so the
// rows/bytes histograms observe the per-query totals identically.
func progressBridge(rec *progressRecorder) func(context.Context, chproto.Progress) error {
	var rows, bytes uint64
	return func(_ context.Context, p chproto.Progress) error {
		rows += p.Rows
		bytes += p.Bytes
		rec.onProgress(&clickhouse.Progress{Rows: rows, Bytes: bytes})
		return nil
	}
}

// bindArgs splices the positional `?` arguments into sql to produce the raw
// query body ch-go sends. clickhouse-go binds `?` client-side before the wire;
// ch-go takes the final SQL, so the columnar path must do the same binding.
//
// The matrix `query_range` projection's args are a closed set — string, int,
// and float scalars (chsql renders label keys/values, metric names, patterns
// via LitString, and predict_linear's t / threshold via LitFloat/LitInt; time
// bounds are emitted as inline DateTime64 literals, never `?` args). The format
// mirrors clickhouse-go's bind.go: strings single-quoted with `\`/`'` escaped,
// numerics via fmt.Sprint. An UNHANDLED arg type (or a count mismatch) returns
// "", false so queryCursorColumnar falls back to the row path rather than risk
// an incorrectly-bound query — the columnar decode is an optimisation, never a
// correctness gamble.
func bindArgs(sql string, args []any) (string, bool) {
	if len(args) == 0 {
		// No placeholders to bind (a bare `?` with no arg is a malformed
		// matrix query — let the row path's binder surface the error).
		if strings.Contains(sql, "?") {
			return "", false
		}
		return sql, true
	}
	var b strings.Builder
	b.Grow(len(sql) + 16*len(args))
	argIdx := 0
	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		if ch != '?' {
			b.WriteByte(ch)
			continue
		}
		// An escaped `\?` is a literal question mark, not a placeholder —
		// mirror clickhouse-go bindPositional.
		if i > 0 && sql[i-1] == '\\' {
			// Drop the backslash we already wrote, emit a literal '?'.
			s := b.String()
			b.Reset()
			b.WriteString(s[:len(s)-1])
			b.WriteByte('?')
			continue
		}
		if argIdx >= len(args) {
			return "", false // more placeholders than args
		}
		lit, ok := formatArg(args[argIdx])
		if !ok {
			return "", false
		}
		b.WriteString(lit)
		argIdx++
	}
	if argIdx != len(args) {
		return "", false // more args than placeholders
	}
	return b.String(), true
}

// formatArg renders one positional arg as a ClickHouse SQL literal, matching
// clickhouse-go's format() for the closed set the matrix path emits. It returns
// false for any other type so the caller falls back to the row path.
func formatArg(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return "'" + sqlStringEscaper.Replace(x) + "'", true
	case bool:
		if x {
			return "1", true
		}
		return "0", true
	case int:
		return fmt.Sprint(x), true
	case int32:
		return fmt.Sprint(x), true
	case int64:
		return fmt.Sprint(x), true
	case uint64:
		return fmt.Sprint(x), true
	case float32:
		return fmt.Sprint(x), true
	case float64:
		return fmt.Sprint(x), true
	case nil:
		return "NULL", true
	default:
		return "", false
	}
}

// sqlStringEscaper mirrors clickhouse-go's stringQuoteReplacer: backslash and
// single-quote are the two characters that must be escaped inside a single-
// quoted ClickHouse string literal.
var sqlStringEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`)
