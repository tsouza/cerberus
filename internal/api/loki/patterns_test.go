package loki_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
)

type patternsResponse struct {
	Status string         `json:"status"`
	Data   []loki.Pattern `json:"data"`
}

// TestPatterns_EmptyPeekWindow — when the peek SQL returns zero rows the
// drain miner emits no clusters and the handler returns the empty-data
// envelope. Grafana renders this gracefully (the panel just shows "no
// data"). The wire shape pins `data` as a top-level JSON array (not a
// `{patterns:[...]}` wrapper) — matches upstream Loki's
// `WriteQueryPatternsResponseJSON`.
func TestPatterns_EmptyPeekWindow(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out patternsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q", out.Status)
	}
	if out.Data == nil || len(out.Data) != 0 {
		t.Fatalf("data=%+v want []", out.Data)
	}
}

// TestPatterns_DrainExtractsCommonTemplate — feeds the handler a peek
// window of three structurally-equivalent HTTP request lines and asserts
// drain collapses them into a single cluster. Drain's template format
// substitutes `<_>` for variable tokens; the assertion is structural
// (substring + placeholder count) rather than against an exact frozen
// string so upstream tokeniser tweaks don't churn the test.
func TestPatterns_DrainExtractsCommonTemplate(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		tsLines: []chclient.TimestampedLine{
			{Timestamp: base, Body: "GET /api/users/1 status=200 latency=5ms"},
			{Timestamp: base.Add(1 * time.Second), Body: "GET /api/users/2 status=200 latency=7ms"},
			{Timestamp: base.Add(2 * time.Second), Body: "GET /api/users/42 status=200 latency=4ms"},
			{Timestamp: base.Add(3 * time.Second), Body: "GET /api/users/1337 status=200 latency=11ms"},
		},
	}

	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1778760000&end=1778760100`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var out patternsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "success" {
		t.Fatalf("status=%q", out.Status)
	}
	if len(out.Data) == 0 {
		t.Fatalf("expected at least 1 pattern; got %+v", out.Data)
	}
	// Find the cluster carrying the GET/status-200 template — drain may
	// emit additional micro-clusters depending on tokenisation, so we
	// only assert that ONE of them carries the expected structure.
	var hit *loki.Pattern
	for i := range out.Data {
		p := out.Data[i]
		if strings.Contains(p.Pattern, "GET") && strings.Contains(p.Pattern, "200") {
			hit = &p
			break
		}
	}
	if hit == nil {
		t.Fatalf("no cluster contained both GET and 200; data=%+v", out.Data)
		return
	}
	if !strings.Contains(hit.Pattern, "<_>") {
		t.Errorf("expected variable placeholder <_> in pattern; got %q", hit.Pattern)
	}
	if len(hit.Samples) == 0 {
		t.Errorf("expected at least one sample tuple; got pattern=%q samples=%+v", hit.Pattern, hit.Samples)
	}
	// Per upstream WriteQueryPatternsResponseJSON: each sample is
	// [unix_seconds, count]. Sum of counts across all samples for this
	// cluster must equal the number of trained lines (4).
	var total int64
	for _, s := range hit.Samples {
		total += s[1]
	}
	if total != 4 {
		t.Errorf("expected sample count sum of 4 (one per trained line); got %d", total)
	}
	// Sample timestamps are unix SECONDS (not ms / ns). 2026-05-14T12:00:00 UTC = 1778760000.
	const baseUnix = 1778760000
	for _, s := range hit.Samples {
		if s[0] < baseUnix-86400 || s[0] > baseUnix+86400 {
			t.Errorf("sample ts=%d out of expected window [%d ± 1d]; likely a unit mis-shift", s[0], baseUnix)
		}
	}
	// Level is empty in PR B — per-cluster bucketing lands in a
	// follow-up (see patterns.go doc + plan § 5).
	if hit.Level != "" {
		t.Errorf("expected empty Level in PR B; got %q", hit.Level)
	}
}

// TestPatterns_PushesLineLimitToSQL pins the contract that the
// `line_limit` query parameter reaches the peek SQL. The default (1000)
// is also asserted — its presence in the SQL guards against a future
// refactor accidentally dropping the LIMIT clause.
func TestPatterns_PushesLineLimitToSQL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		url       string
		wantLimit string
	}{
		{
			name:      "default line_limit",
			url:       `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`,
			wantLimit: "LIMIT 1000",
		},
		{
			name:      "explicit line_limit",
			url:       `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200&line_limit=250`,
			wantLimit: "LIMIT 250",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{}
			srv := newServer(q)
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d", resp.StatusCode)
			}
			gotSQL := q.LastSQL()
			if !strings.Contains(gotSQL, tc.wantLimit) {
				t.Errorf("expected SQL to contain %q; got: %s", tc.wantLimit, gotSQL)
			}
			// Peek SQL must project both Timestamp and Body — drain
			// needs the (ts, line) pair to bucket per-cluster samples.
			if !strings.Contains(gotSQL, "`Timestamp`") || !strings.Contains(gotSQL, "`Body`") {
				t.Errorf("expected SQL to project both Timestamp and Body; got: %s", gotSQL)
			}
		})
	}
}

// TestPatterns_BadInput — missing/broken parameters still return 400
// so a misconfigured client gets a useful error rather than a silent
// "no data".
func TestPatterns_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing query", `/loki/api/v1/patterns?start=1&end=2`},
		{"bad query", `/loki/api/v1/patterns?query=%7Bnot+a+selector`},
		{"bad start", `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=banana&end=2`},
		{"bad line_limit", `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1&end=2&line_limit=banana`},
		{"non-positive line_limit", `/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1&end=2&line_limit=0`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}
