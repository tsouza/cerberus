// Command rejection-parity is the differential harness for cerberus's
// deliberate rejections. It consumes the rejection catalogue
// (test/rejection-parity/catalogue.json — see that package's doc for
// the full mechanism) and, for every class=rejection entry of the
// selected head, sends the entry's trigger query to BOTH the reference
// backend and cerberus, then compares the rejection *status class*:
//
//   - both 4xx                → parity (cerberus's rejection claim holds)
//   - reference 2xx, cerberus 4xx → wrong_rejection — cerberus rejects a
//     query the reference backend answers; a real bug to fix at the
//     source (the `kind != nil` class), never an allow-list entry
//   - cerberus 2xx            → stale_catalogue — the catalogue says
//     cerberus rejects this, but the live binary accepts it; the
//     catalogue needs regenerating
//   - 5xx / transport failure → hard_error (infrastructure, not parity)
//
// Only the status class is compared — never message text — because
// the two backends phrase rejections differently by construction.
//
// The corpus is the catalogue itself (rejectionparity.BuildCases), so
// corpus-case count == catalogue rejection-entry count by
// construction; the meta-tests under test/rejection-parity pin the
// remaining legs of the ratchet (site scan == catalogue, triggers
// exercise their sites).
//
// Exit semantics mirror the other compat drivers (task #68,
// report-only): parity drift — including wrong_rejection — is recorded
// in the JSON report and the stderr summary but does not change the
// exit code. Only driver-wide hard errors (catalogue load, report
// write) escalate to a non-zero rc.
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
	"strconv"
	"strings"
	"time"

	rejectionparity "github.com/tsouza/cerberus/test/rejection-parity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rejection-parity: %v\n", err)
		os.Exit(1)
	}
}

// CaseResult is one trigger query's outcome on both backends.
type CaseResult struct {
	// Name is the catalogue site key (greppable back to the
	// error-construction site in the lowering source).
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Query    string `json:"query"`

	RefStatus      int `json:"refStatus"`
	CerberusStatus int `json:"cerberusStatus"`

	// Verdict: "parity" | "wrong_rejection" | "stale_catalogue" |
	// "hard_error".
	Verdict string `json:"verdict"`
	// Detail carries the transport error or a snippet of the
	// unexpected response body for triage.
	Detail string `json:"detail,omitempty"`
}

// Report is the on-disk JSON artifact.
type Report struct {
	Head           string       `json:"head"`
	Total          int          `json:"total"`
	Parity         int          `json:"parity"`
	WrongRejection int          `json:"wrongRejection"`
	StaleCatalogue int          `json:"staleCatalogue"`
	HardErrors     int          `json:"hardErrors"`
	Cases          []CaseResult `json:"cases"`
}

func run() error {
	var (
		head      = flag.String("head", "", "head to run: promql | logql | traceql")
		catalogue = flag.String("catalogue", "test/rejection-parity/catalogue.json", "path to the rejection catalogue")
		refURL    = flag.String("ref", "", "reference backend base URL")
		cerbURL   = flag.String("cerberus", "", "cerberus base URL")
		report    = flag.String("report", "", "JSON report output path")
		timeout   = flag.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
	)
	flag.Parse()
	if *head == "" || *refURL == "" || *cerbURL == "" || *report == "" {
		return fmt.Errorf("-head, -ref, -cerberus and -report are all required")
	}

	cat, err := rejectionparity.LoadCatalogue(*catalogue)
	if err != nil {
		return fmt.Errorf("load catalogue: %w", err)
	}
	cases, err := rejectionparity.BuildCases(cat, *head)
	if err != nil {
		return fmt.Errorf("build cases: %w", err)
	}
	if len(cases) == 0 {
		return fmt.Errorf("catalogue has zero rejection entries for head %s", *head)
	}

	client := &http.Client{Timeout: *timeout}
	now := time.Now().UTC()
	rep := Report{Head: *head, Total: len(cases)}
	for _, c := range cases {
		res := runCase(client, c, *refURL, *cerbURL, now)
		switch res.Verdict {
		case "parity":
			rep.Parity++
		case "wrong_rejection":
			rep.WrongRejection++
		case "stale_catalogue":
			rep.StaleCatalogue++
		default:
			rep.HardErrors++
		}
		rep.Cases = append(rep.Cases, res)
	}

	if err := writeReport(*report, rep); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"==> rejection-parity %s: total=%d parity=%d wrong_rejection=%d stale_catalogue=%d hard_errors=%d -> %s\n",
		rep.Head, rep.Total, rep.Parity, rep.WrongRejection, rep.StaleCatalogue, rep.HardErrors, *report)
	for _, c := range rep.Cases {
		if c.Verdict != "parity" {
			fmt.Fprintf(os.Stderr, "    [%s] %s (%s): ref=%d cerberus=%d query=%s\n",
				c.Verdict, c.Name, c.Endpoint, c.RefStatus, c.CerberusStatus, c.Query)
		}
	}
	return nil
}

// runCase fires the trigger query at both backends and classifies the
// status pair.
func runCase(client *http.Client, c rejectionparity.Case, refURL, cerbURL string, now time.Time) CaseResult {
	res := CaseResult{Name: c.Name, Endpoint: c.Endpoint, Query: c.Query}

	refStatus, refBody, refErr := fetch(client, buildURL(refURL, c, now))
	cerbStatus, cerbBody, cerbErr := fetch(client, buildURL(cerbURL, c, now))
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
	case cerbStatus/100 == 2:
		// The catalogue claims cerberus rejects this query; the live
		// binary accepted it. The catalogue (or the lowering) moved —
		// regenerate + re-curate.
		res.Verdict = "stale_catalogue"
		res.Detail = fmt.Sprintf("cerberus accepted (catalogue expects 4xx); ref=%q", snippet(refBody))
	case refStatus/100 == 2:
		// Cerberus rejects, reference answers: a wrong-rejection bug.
		res.Verdict = "wrong_rejection"
		res.Detail = fmt.Sprintf("reference accepted; cerberus said %q", snippet(cerbBody))
	case refStatus/100 == 4 && cerbStatus/100 == 4:
		res.Verdict = "parity"
	default:
		res.Verdict = "hard_error"
		res.Detail = fmt.Sprintf("unclassifiable status pair; ref=%q cerberus=%q", snippet(refBody), snippet(cerbBody))
	}
	return res
}

// buildURL composes the per-endpoint query URL. The window is ±1h
// around now — rejection parity is shape-based, not data-based, so
// the window only needs to be syntactically valid.
func buildURL(base string, c rejectionparity.Case, now time.Time) string {
	start := now.Add(-1 * time.Hour)
	end := now
	u := strings.TrimRight(base, "/")
	q := url.Values{}
	switch c.Endpoint {
	case rejectionparity.EndpointPromInstant:
		u += "/api/v1/query"
		q.Set("query", c.Query)
		q.Set("time", strconv.FormatInt(end.Unix(), 10))
	case rejectionparity.EndpointLogQLRange:
		u += "/loki/api/v1/query_range"
		q.Set("query", c.Query)
		q.Set("start", strconv.FormatInt(start.UnixNano(), 10))
		q.Set("end", strconv.FormatInt(end.UnixNano(), 10))
		q.Set("step", "30")
		q.Set("limit", "100")
	case rejectionparity.EndpointTraceQLSearch:
		u += "/api/search"
		q.Set("q", c.Query)
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
		q.Set("limit", "20")
	case rejectionparity.EndpointTraceQLMetrics:
		u += "/api/metrics/query_range"
		q.Set("q", c.Query)
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
		q.Set("end", strconv.FormatInt(end.Unix(), 10))
		q.Set("step", "60s")
	}
	return u + "?" + q.Encode()
}

func fetch(client *http.Client, urlStr string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
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
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
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
