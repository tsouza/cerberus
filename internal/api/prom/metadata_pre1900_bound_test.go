package prom_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestMetadataPre1900Bound_SQL pins the SQL bound the no-`match[]`
// discovery arms emit for a `start` before 1900. ClickHouse parses a time
// literal earlier than 1900-01-01 as that day, so the bound compares Unix
// nanoseconds, inline, both in the WHERE of a finite window and in the
// `max(TimeUnix)` HAVING of an open-ended one. A `start` at the earliest
// instant storage holds excludes nothing, so it is omitted.
func TestMetadataPre1900Bound_SQL(t *testing.T) {
	t.Parallel()
	start := time.Date(1800, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	where := fmt.Sprintf("toUnixTimestamp64Nano(`TimeUnix`) >= %d", start.UnixNano())
	having := fmt.Sprintf("toUnixTimestamp64Nano(max(`TimeUnix`)) >= %d", start.UnixNano())
	const clamped = "parseDateTime64BestEffort('1800-"
	const storageMin = "1677-09-21T00:12:43.145224192Z"

	cases := []struct {
		name       string
		start, end string
		want       string // "" asserts no lower bound at all
	}{
		{"finite_window", start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano), where},
		{"open_end", start.Format(time.RFC3339Nano), "", having},
		{"storage_min_finite", storageMin, end.Format(time.RFC3339Nano), ""},
		{"storage_min_open_end", storageMin, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{strings: []string{"api"}}
			srv := newServer(q)
			t.Cleanup(srv.Close)
			v := url.Values{"start": {c.start}}
			if c.end != "" {
				v.Set("end", c.end)
			}
			getDiscard(t, srv.URL+"/api/v1/label/job/values?"+v.Encode())
			if len(q.allSQL) == 0 {
				t.Fatal("no SQL issued")
			}
			for _, sql := range q.allSQL {
				if strings.Contains(sql, clamped) {
					t.Errorf("SQL compares a clamped 1800 literal:\n%s", sql)
				}
				if c.want != "" && !strings.Contains(sql, c.want) {
					t.Errorf("SQL has no %q:\n%s", c.want, sql)
				}
				if c.want == "" && (strings.Contains(sql, ">= parseDateTime64BestEffort(") || strings.Contains(sql, "toUnixTimestamp64Nano(")) {
					t.Errorf("SQL carries a lower bound that excludes nothing:\n%s", sql)
				}
			}
		})
	}
}
