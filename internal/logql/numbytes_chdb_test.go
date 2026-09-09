//go:build chdb

// Differential coverage for [newBytesParse] against its own reference.
//
// Reference Loki converts a bytes-valued label with
// `humanize.ParseBytes` — literally, at both sites
// (pkg/logql/log/label_filter.go's BytesLabelFilter.Process and
// pkg/logql/log/metrics_extraction.go's convertBytes). So the oracle
// here is that function itself, called in-process, and the assertion is
// that the SQL cerberus emits answers what it answers: same accept /
// reject verdict, same byte count, and — where they disagree — an
// explicit statement of why.
//
// The corpus is a cross product of number shapes and EVERY unit
// spelling in humanize's bytesSizeTable, plus the malformed shapes, so
// no spelling can be silently unmodelled. That closure is the point:
// the defect this file was written for (cerberus issue #3183) was four
// whole spellings — the empty unit, the single letters, the binary
// prefixes without a trailing `b`, and comma-separated numbers — that
// the emitted SQL accepted as valid and then handed to ClickHouse's
// `parseReadableSize`, which THREW on all four and aborted the whole
// query with a 502.

package logql

import (
	"database/sql"
	"testing"

	"github.com/dustin/go-humanize"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// bytesUnitSpellings is humanize's bytesSizeTable key set, spelled out
// rather than derived, so a table change upstream surfaces as a
// disagreement here instead of being mirrored into the test.
var bytesUnitSpellings = []string{
	"", "b",
	"k", "kb", "ki", "kib",
	"m", "mb", "mi", "mib",
	"g", "gb", "gi", "gib",
	"t", "tb", "ti", "tib",
	"p", "pb", "pi", "pib",
	"e", "eb", "ei", "eib",
}

// bytesNumberShapes are the number runs humanize peels off the front.
// The fractional ones matter because humanize TRUNCATES the scaled
// value (`uint64(f)`) — ClickHouse's parseReadableSize rounds, which is
// how `1.5 B` used to answer 2 instead of 1.
var bytesNumberShapes = []string{"0", "5", "1.5", "1.7", "0.5", ".5", "5.", "1024", "5,000", "9007199254740993"}

// bytesMalformedValues are the shapes humanize REJECTS. Each must be
// reported invalid, with humanize's own error text.
var bytesMalformedValues = []string{
	"", "oops", "5xyz", "-5kb", "kb", "5 5 kb", "5..5kb", "1e3kb",
	// Overflow: humanize's third rejection, `too large: <s>`.
	"5000e", "20000000000000000000",
}

func TestBytesParseMatchesHumanize(t *testing.T) {
	corpus := make([]string, 0, len(bytesNumberShapes)*len(bytesUnitSpellings)*3+len(bytesMalformedValues))
	for _, n := range bytesNumberShapes {
		for _, u := range bytesUnitSpellings {
			corpus = append(corpus, n+u)
			if u != "" {
				// humanize trims whitespace and lowercases the unit, so
				// the spaced and upper-case spellings must land on the
				// same answer.
				corpus = append(corpus, n+" "+u, n+" "+upperASCII(u))
			}
		}
	}
	corpus = append(corpus, bytesMalformedValues...)

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE bytes_corpus (idx UInt32, Raw String) ENGINE = Memory"); err != nil {
		t.Fatalf("create corpus table: %v", err)
	}
	for i, v := range corpus {
		if _, err := db.Exec("INSERT INTO bytes_corpus (idx, Raw) VALUES (?, ?)", i, v); err != nil {
			t.Fatalf("seed %q: %v", v, err)
		}
	}

	parse := newBytesParse(&chplan.ColumnRef{Name: "Raw"})
	validSQL, validArgs := emitBytesExpr(t, parse.valid)
	valueSQL, valueArgs := emitBytesExpr(t, parse.value)
	detailsSQL, detailsArgs := emitBytesExpr(t, parse.details)

	args := append(append(append([]any{}, validArgs...), valueArgs...), detailsArgs...)
	query := "SELECT `idx`, " + validSQL + ", " + valueSQL + ", " + detailsSQL +
		" FROM `bytes_corpus` ORDER BY `idx`"
	rows, err := db.Query(query, args...)
	if err != nil {
		// A throw here is the original bug: the emitted expression must
		// be total over every string, because ClickHouse aborts the
		// WHOLE query on one bad row where reference Loki keeps it.
		t.Fatalf("emitted bytes parse threw over the corpus (reference Loki never aborts a query on an unparseable value): %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var idx int
		var valid uint8
		var value float64
		var details string
		if err := rows.Scan(&idx, &valid, &value, &details); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		raw := corpus[idx]

		want, wantErr := humanize.ParseBytes(raw)
		if wantErr != nil {
			if valid != 0 {
				t.Errorf("%q: emitted parse accepted it (value %v); humanize rejects with %q", raw, value, wantErr)
				continue
			}
			if details != wantErr.Error() {
				t.Errorf("%q: __error_details__ = %q, want humanize's own %q", raw, details, wantErr.Error())
			}
			continue
		}
		if valid == 0 {
			t.Errorf("%q: emitted parse rejected it with %q; humanize accepts it as %d bytes", raw, details, want)
			continue
		}
		if value != float64(want) {
			t.Errorf("%q: emitted parse = %v bytes, humanize = %d bytes", raw, value, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if seen != len(corpus) {
		t.Fatalf("read %d rows for a %d-entry corpus", seen, len(corpus))
	}
}

// upperASCII upper-cases an ASCII unit spelling. humanize lowercases the
// unit before its table lookup, so `KiB` and `kib` are the same unit.
func upperASCII(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 'a' - 'A'
		}
	}
	return string(out)
}

func emitBytesExpr(t *testing.T, e chplan.Expr) (string, []any) {
	t.Helper()
	b := chsql.NewBuilder()
	if err := b.Expr(e); err != nil {
		t.Fatalf("emit: %v", err)
	}
	sqlStr, args, err := b.Build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return sqlStr, args
}
