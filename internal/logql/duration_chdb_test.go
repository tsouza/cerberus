//go:build chdb

// Differential coverage for [newDurationParse]'s seconds against its own
// reference. Reference Loki converts a duration-valued label with
// `time.ParseDuration(s)` and, for `unwrap duration(...)`, reports
// `Duration.Seconds()` (pkg/logql/log/metrics_extraction.go's
// convertDuration). So the oracle is that pair of calls, in-process, and
// the assertion is bit-for-bit float equality: the defect this file was
// written for (cerberus issue #3662) was ClickHouse's parseTimeDelta
// answering `5us` as 4.9999999999999996e-06 where Go answers 5e-06.

package logql

import (
	"database/sql"
	"math"
	"testing"
	"time"

	_ "github.com/chdb-io/chdb-go/chdb/driver"

	"github.com/tsouza/cerberus/internal/chplan"
)

// durationNumberShapes are the number runs crossed with every unit
// spelling. The fractional and many-digit ones exercise Go's
// `float64(frac) * (float64(unit) / scale)` truncation per unit.
var durationNumberShapes = []string{
	"0", "1", "3", "5", "7", "12", "999", "1000", "2562046",
	"1.5", "0.1", "0.3", "2.7", "291.792", ".5", "5.", "0.000001",
	"1.123456789", "3.141592653589793", "0.999999999999999999",
}

func TestDurationSecondsMatchesGo(t *testing.T) {
	units := []string{"ns", "us", "µs", "μs", "ms", "s", "m", "h"}
	corpus := append([]string{}, goDurationCorpus...)
	for _, n := range durationNumberShapes {
		for _, u := range units {
			corpus = append(corpus, n+u, "-"+n+u, n+u+"7ns", "1h"+n+u)
		}
	}

	db, err := sql.Open("chdb", "")
	if err != nil {
		t.Fatalf("open chdb: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE duration_corpus (idx UInt32, Raw String) ENGINE = Memory"); err != nil {
		t.Fatalf("create corpus table: %v", err)
	}
	for i, v := range corpus {
		if _, err := db.Exec("INSERT INTO duration_corpus (idx, Raw) VALUES (?, ?)", i, v); err != nil {
			t.Fatalf("seed %q: %v", v, err)
		}
	}

	parse := newDurationParse(&chplan.ColumnRef{Name: "Raw"})
	validSQL, validArgs := emitBytesExpr(t, parse.valid)
	secondsSQL, secondsArgs := emitBytesExpr(t, parse.seconds)
	args := append(append([]any{}, validArgs...), secondsArgs...)
	query := "SELECT `idx`, " + validSQL + ", " + secondsSQL + " FROM `duration_corpus` ORDER BY `idx`"
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("emitted duration parse threw over the corpus (reference Loki never aborts a query on an unparseable value): %v\nquery: %s", err, query)
	}
	defer func() { _ = rows.Close() }()

	seen := 0
	for rows.Next() {
		var idx int
		var valid uint8
		var seconds float64
		if err := rows.Scan(&idx, &valid, &seconds); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		raw := corpus[idx]

		d, parseErr := time.ParseDuration(raw)
		if parseErr != nil {
			if valid != 0 {
				t.Errorf("%q: emitted parse accepted it (%v s); time.ParseDuration rejects with %q", raw, seconds, parseErr)
			}
			if seconds != 0 {
				t.Errorf("%q: invalid value yields %v s, want 0 (convertDuration's `return 0, err`)", raw, seconds)
			}
			continue
		}
		if valid == 0 {
			t.Errorf("%q: emitted parse rejected it; time.ParseDuration accepts it as %v", raw, d)
			continue
		}
		if want := d.Seconds(); math.Float64bits(seconds) != math.Float64bits(want) {
			t.Errorf("%q: emitted seconds = %v, time.ParseDuration(...).Seconds() = %v", raw, seconds, want)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if seen != len(corpus) {
		t.Fatalf("read %d rows for a %d-entry corpus", seen, len(corpus))
	}
}
