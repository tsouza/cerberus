package prom_test

// REGRESSION (Layer 5 — emitted-shape invariant).
//
// A catalog endpoint answers with a flat set of strings: label names, or
// the values of ONE label. Both are known before the plan is built, so
// the answer is resolvable at the scan. Anything a match[]-driven catalog
// query emits ABOVE the scan that reconstructs per-row series identity is
// pure overhead whose cost scales with the scan, not with the answer:
//
//   - a map-valued GROUP BY key forces ClickHouse to hash every row's
//     whole attribute map to build groups the answer never reads;
//   - the read-path attribute merge (mapUpdate over two mapFromArrays
//     over a per-key regex) rebuilds every row's renamed label map so a
//     projection above it can subscript one key back out;
//   - any(TimeUnix) / any(Value) aggregate columns that appear in no
//     label-names or label-values response at all.
//
// These assertions pin the ABSENCE of each. They are deliberately
// written against the SQL text rather than the plan: the plan shape is
// already pinned byte-for-byte by the Layer 2a fixtures under
// test/spec/promql/metadata_label_*.txtar, and what must never come back
// is the emitted overhead, whichever stage would reintroduce it.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// catalogOverheadFragments are the SQL fragments that must not appear in
// any match[]-driven catalog query, each with the reason it would be
// wrong if it did.
var catalogOverheadFragments = map[string]string{
	"GROUP BY":         "per-series collapse — the answer is a flat set, not a series list",
	"mapUpdate(":       "read-path attribute merge — rebuilds each row's whole label map",
	"mapFromArrays(":   "renamed-map construction — same, one map allocated per row",
	"replaceRegexpAll": "per-key label-name regex — resolvable at plan time",
	"any(":             "Sample-shaped meta-aggregation — no such column in the response",
}

// catalogRequests are the match[]-driven catalog endpoints, one request
// each, spanning the shapes that reach different lowering paths: a plain
// gauge/sum selector, the `__name__` projection, a classic-histogram
// bucket selector (arrayJoin bucket fan-out), and a histogram companion
// selector (UNION-ALL of per-table arms).
var catalogRequests = map[string]string{
	"label values":               "/api/v1/label/region/values?match%5B%5D=" + url.QueryEscape(`requests_total{job="api"}`),
	"label values __name__":      "/api/v1/label/__name__/values?match%5B%5D=" + url.QueryEscape(`{job="api"}`),
	"label values le on buckets": "/api/v1/label/le/values?match%5B%5D=" + url.QueryEscape(`latency_seconds_bucket`),
	"label values on companion":  "/api/v1/label/region/values?match%5B%5D=" + url.QueryEscape(`latency_seconds_count`),
	"label names":                "/api/v1/labels?match%5B%5D=" + url.QueryEscape(`requests_total{job="api"}`),
	"label names on buckets":     "/api/v1/labels?match%5B%5D=" + url.QueryEscape(`latency_seconds_bucket`),
}

func TestMetadataCatalog_EmitsNoPerSeriesOverhead(t *testing.T) {
	t.Parallel()

	for name, path := range catalogRequests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			q := &stubQuerier{strings: []string{"eu-west", "us-east"}}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
			if len(q.allSQL) == 0 {
				t.Fatalf("endpoint issued no query — nothing to assert against")
			}
			for i, sql := range q.allSQL {
				for frag, why := range catalogOverheadFragments {
					if strings.Contains(sql, frag) {
						t.Errorf("query %d emits %q (%s):\n%s", i, frag, why, sql)
					}
				}
			}
		})
	}
}

// TestMetadataCatalog_ResolvesTheAnswerAtTheLeaf is the positive half of
// the invariant above: it is not enough that the overhead is gone, the
// single answer column has to be produced by the innermost SELECT. If a
// future change moved the projection back up above a row-shaped
// subquery, the negative assertions could still all pass while the scan
// carried whole rows again.
func TestMetadataCatalog_ResolvesTheAnswerAtTheLeaf(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ path, wantOuter string }{
		"label values": {
			path:      "/api/v1/label/region/values?match%5B%5D=" + url.QueryEscape(`requests_total{job="api"}`),
			wantOuter: "SELECT DISTINCT `value` FROM",
		},
		"label names": {
			path:      "/api/v1/labels?match%5B%5D=" + url.QueryEscape(`requests_total{job="api"}`),
			wantOuter: "SELECT DISTINCT `name` FROM",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			q := &stubQuerier{strings: []string{"eu-west"}}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
			}
			// The outer query does nothing but dedupe one column …
			if !strings.HasPrefix(q.lastSQL, tc.wantOuter) {
				t.Errorf("outer query should only dedupe the answer column, want prefix %q; got %q",
					tc.wantOuter, q.lastSQL)
			}
			// … and every subquery under it is one FROM away from a table:
			// the answer column is projected by the SELECT that reads the
			// scan, so exactly one `SELECT … FROM (SELECT * FROM <scan>)`
			// level sits between the dedupe and storage.
			if strings.Count(q.lastSQL, "SELECT") > maxCatalogSelectDepth {
				t.Errorf("catalog query grew an intermediate stage (%d SELECTs, want at most %d): %s",
					strings.Count(q.lastSQL, "SELECT"), maxCatalogSelectDepth, q.lastSQL)
			}
		})
	}
}

// maxCatalogSelectDepth is the SELECT count a single-arm catalog query
// needs: the outer dedupe, the arm's answer projection, and the scan's
// own `SELECT * FROM <table>`. Anything more means a stage that carries
// rows rather than the answer crept back in.
const maxCatalogSelectDepth = 3
