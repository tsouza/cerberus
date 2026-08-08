// Command metadata-parity is the differential harness for the
// Prometheus metadata endpoints' `match[]` selector grammar
// (`/api/v1/series`, `/api/v1/labels`, `/api/v1/label/<name>/values`).
//
// #1487 fixed cerberus's `match[]` parsing on these endpoints to use
// upstream's ParseMetricSelector grammar (a bare instant-vector
// selector only) instead of the general ParseExpr grammar, so a
// selector upstream rejects — a range-vector call, `offset`, `@`, an
// aggregation, or the empty `{}` selector — now 400s in cerberus too
// (internal/api/prom/handler.go's parseMatchSelector +
// requireNonEmptyMatcher). That fix is pinned by
// internal/api/prom/metadata_test.go's matchSelectorRejectedShapes
// table, but ONLY as a handler-level unit test against the documented
// upstream contract — not as a live differential diff against a real
// reference Prometheus. #1729 closes that gap: this driver fires the
// SAME shape set the unit tests pin at both a reference Prometheus and
// cerberus, so a future regression (someone reverting to ParseExpr, or
// a new metadata endpoint acquiring the same bug) fails a compatibility
// gate instead of passing every currently-required one.
//
// This mirrors compatibility/loki/cmd/loki-compliance-tester/
// status_parity.go's comparison granularity — a small, fixed case
// table, each entry asserting BOTH backends return the exact SAME
// expected status for a given request shape — but is packaged as its
// own hard-failing driver (mirroring
// compatibility/cmd/rejection-parity's exit semantics) rather than
// folded into a report-only corpus pass: there is no cerberus-owned
// "compliance-tester" binary on the Prometheus side to fold into (the
// Prometheus lane drives the upstream-owned promql-compliance-tester
// for the noisy /query and /query_range corpus), and the whole point
// of this gap is a small, deliberately-chosen, uncontroversial case set
// that should fail the build the moment it regresses.
//
// query_exemplars (also named in #1729's title) is deliberately NOT
// covered here: its parse boundary is a structurally different
// contract (a `query=` parameter walked through the general ParseExpr
// grammar and the post-parse singleVectorSelector check in
// internal/api/prom/exemplars.go, not match[]'s ParseMetricSelector),
// its required-parameter shape differs (mandatory `start`/`end`, no
// bare-window default), and — unlike the map[] endpoints — `offset`
// and `@` are valid there on both backends (they set fields on the
// same *parser.VectorSelector node, so singleVectorSelector's type
// assertion accepts them same as upstream's identical check), so the
// case table this driver deliberately keeps in lock-step with the
// match[] unit tests does not transfer. Exercising it soundly would
// also need the reference container's exemplar-storage feature state
// confirmed live — this environment cannot run the compose stack (see
// docs/test-strategy.md's CI-only compat lanes). #1729 stays open for
// that endpoint.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "metadata-parity: %v\n", err)
		os.Exit(1)
	}
}

// metadataEndpoint is one of the three match[]-driven metadata routes
// this driver targets.
type metadataEndpoint struct {
	desc string
	path string
}

// metadataEndpoints are the three endpoints #1487 fixed match[] parsing
// on. label_values targets an arbitrary label name ("job") — the
// selector grammar rejection happens before the name is ever resolved
// against data, so which name is used doesn't matter, mirroring
// TestLabelValues_MatchSelectorRejectsNonSelector's choice.
func metadataEndpoints() []metadataEndpoint {
	return []metadataEndpoint{
		{desc: "series", path: "/api/v1/series"},
		{desc: "labels", path: "/api/v1/labels"},
		{desc: "label_values", path: "/api/v1/label/job/values"},
	}
}

// matchShape is one match[] value this driver fires at every endpoint,
// paired with the status BOTH backends must answer with.
type matchShape struct {
	desc   string
	value  string
	expect int
}

// matchShapes is kept in lock-step with
// internal/api/prom/metadata_test.go's matchSelectorRejectedShapes
// table (the range-vector call, offset, @, and aggregation shapes) plus
// the empty `{}` selector handler.go's requireNonEmptyMatcher rejects
// and one syntactically valid selector that must be ACCEPTED — so this
// driver's shape set and the unit tests' shape set can't silently
// drift apart. None of these shapes' outcomes depend on the seeded
// fixture: both backends decide at the parse/matcher-emptiness boundary
// before ever touching storage, exactly like status_parity.go's two
// cases.
func matchShapes() []matchShape {
	return []matchShape{
		{
			desc:   "valid selector",
			value:  "cerberus_metadata_parity_probe",
			expect: http.StatusOK,
		},
		{
			desc:   "empty {} selector",
			value:  "{}",
			expect: http.StatusBadRequest,
		},
		{
			desc:   "range-vector call",
			value:  "rate(http_requests_total[5m])",
			expect: http.StatusBadRequest,
		},
		{
			desc:   "offset modifier",
			value:  "foo offset 1h",
			expect: http.StatusBadRequest,
		},
		{
			desc:   "@ modifier",
			value:  "foo @ 1600000000",
			expect: http.StatusBadRequest,
		},
		{
			desc:   "aggregation",
			value:  "sum(foo)",
			expect: http.StatusBadRequest,
		},
	}
}

// CaseResult is one (endpoint, shape) pair's outcome on both backends.
type CaseResult struct {
	Endpoint string `json:"endpoint"`
	Shape    string `json:"shape"`
	Match    string `json:"match"`

	RefStatus      int `json:"refStatus"`
	CerberusStatus int `json:"cerberusStatus"`
	Expect         int `json:"expect"`

	// Verdict: "parity" (both backends answered `expect`), "mismatch"
	// (at least one backend diverged from `expect` — a real bug, never
	// an allow-list entry), or "hard_error" (transport failure or a 5xx
	// on either side — infrastructure, not a parity verdict).
	Verdict string `json:"verdict"`
	Detail  string `json:"detail,omitempty"`
}

// Report is the on-disk JSON artifact.
type Report struct {
	Total     int          `json:"total"`
	Parity    int          `json:"parity"`
	Mismatch  int          `json:"mismatch"`
	HardError int          `json:"hardError"`
	Cases     []CaseResult `json:"cases"`
}

// Fatal reports whether any case mismatched its expected status on
// either backend — the exact blind spot #1729 describes: a future
// regression that would otherwise pass every currently-required
// compatibility gate.
func (r Report) Fatal() bool {
	return r.Mismatch > 0
}

func run() error {
	var (
		refURL  = flag.String("ref", "", "reference backend base URL")
		cerbURL = flag.String("cerberus", "", "cerberus base URL")
		report  = flag.String("report", "", "JSON report output path")
		timeout = flag.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
	)
	flag.Parse()
	if *refURL == "" || *cerbURL == "" || *report == "" {
		return fmt.Errorf("-ref, -cerberus and -report are all required")
	}

	client := &http.Client{Timeout: *timeout}
	var rep Report
	for _, ep := range metadataEndpoints() {
		for _, sh := range matchShapes() {
			res := runCase(client, *refURL, *cerbURL, ep, sh)
			rep.Total++
			switch res.Verdict {
			case "parity":
				rep.Parity++
			case "mismatch":
				rep.Mismatch++
			default:
				rep.HardError++
			}
			rep.Cases = append(rep.Cases, res)
		}
	}

	if err := writeReport(*report, rep); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"==> metadata-parity: total=%d parity=%d mismatch=%d hard_error=%d -> %s\n",
		rep.Total, rep.Parity, rep.Mismatch, rep.HardError, *report)
	for _, c := range rep.Cases {
		if c.Verdict != "parity" {
			fmt.Fprintf(os.Stderr, "    [%s] %s match[]=%q (want %d): ref=%d cerberus=%d %s\n",
				c.Verdict, c.Endpoint, c.Match, c.Expect, c.RefStatus, c.CerberusStatus, c.Detail)
		}
	}

	// A mismatch means at least one backend answered a status other
	// than the shape's expected one — the report above already landed
	// on disk so the artifact survives this branch. Only hard_error
	// (transport failure, 5xx) stays non-fatal: that's infrastructure,
	// not a parity verdict.
	if rep.Fatal() {
		return fmt.Errorf(
			"metadata-parity: %d mismatch(es) — see %s for detail; a mismatch is a real match[]-grammar "+
				"parity bug to fix at internal/api/prom/handler.go's parseMatchSelector, never an allow-list entry",
			rep.Mismatch, *report,
		)
	}
	return nil
}

// runCase fires the endpoint+shape's request at both backends and
// classifies the status pair against the shape's expected status.
func runCase(client *http.Client, refURL, cerbURL string, ep metadataEndpoint, sh matchShape) CaseResult {
	res := CaseResult{Endpoint: ep.desc, Shape: sh.desc, Match: sh.value, Expect: sh.expect}

	params := url.Values{}
	params.Set("match[]", sh.value)

	refStatus, refBody, refErr := fetch(client, refURL, ep.path, params)
	cerbStatus, cerbBody, cerbErr := fetch(client, cerbURL, ep.path, params)
	res.RefStatus, res.CerberusStatus = refStatus, cerbStatus

	switch {
	case refErr != nil:
		res.Verdict = "hard_error"
		res.Detail = "reference fetch: " + refErr.Error()
	case cerbErr != nil:
		res.Verdict = "hard_error"
		res.Detail = "cerberus fetch: " + cerbErr.Error()
	case refStatus/100 == 5 || cerbStatus/100 == 5:
		res.Verdict = "hard_error"
		res.Detail = fmt.Sprintf("5xx: ref=%q cerberus=%q", snippet(refBody), snippet(cerbBody))
	case refStatus == sh.expect && cerbStatus == sh.expect:
		res.Verdict = "parity"
	case refStatus != sh.expect && cerbStatus != sh.expect:
		res.Verdict = "mismatch"
		res.Detail = fmt.Sprintf("both diverged from expected %d (ref body=%q; cerberus body=%q)",
			sh.expect, snippet(refBody), snippet(cerbBody))
	case refStatus != sh.expect:
		res.Verdict = "mismatch"
		res.Detail = fmt.Sprintf("reference diverged from expected %d (body=%q)", sh.expect, snippet(refBody))
	default:
		res.Verdict = "mismatch"
		res.Detail = fmt.Sprintf("cerberus diverged from expected %d (body=%q)", sh.expect, snippet(cerbBody))
	}
	return res
}

func fetch(client *http.Client, base, path string, params url.Values) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
	defer cancel()

	u := strings.TrimRight(base, "/") + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, body, nil
}

func snippet(b []byte) string {
	const maxSnippetLen = 300
	s := strings.TrimSpace(string(b))
	if len(s) > maxSnippetLen {
		s = s[:maxSnippetLen] + "…"
	}
	return s
}

func writeReport(path string, rep Report) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	out, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o600)
}
