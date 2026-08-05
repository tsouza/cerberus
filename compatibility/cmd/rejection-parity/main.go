// Command rejection-parity is the differential harness for cerberus's
// deliberate rejections. It consumes the rejection catalogue
// (the shard directory test/rejection-parity/catalogue/ — see that
// package's doc for the full mechanism) and, for every class=rejection or
// class=divergence entry of the selected head, sends the entry's
// trigger query to BOTH the reference backend and cerberus, then
// compares the *status class* — never message text, because the two
// backends phrase rejections differently by construction. The
// comparison direction depends on the case's class:
//
// class=rejection (the claim: both backends reject this query):
//
//   - both 4xx                    → parity (the claim holds)
//   - reference 2xx, cerberus 4xx → wrong_rejection — cerberus rejects a
//     query the reference backend answers; a real bug to fix at the
//     source (the `kind != nil` class), never an allow-list entry
//   - cerberus 2xx                → stale_catalogue — the catalogue says
//     cerberus rejects this, but the live binary accepts it; the
//     catalogue needs regenerating
//   - 5xx / transport failure     → hard_error (infrastructure, not parity)
//
// class=divergence (the claim: cerberus rejects, the reference
// answers — see test/rejection-parity's doc.go for why this is a
// ratchet, not an allow-list):
//
//   - cerberus 2xx                 → divergence_resolved — cerberus now
//     answers a query the entry says it rejects; the divergence closed
//     from cerberus's side and the entry must be deleted
//   - cerberus 4xx, reference 2xx  → divergence_confirmed (the claim
//     holds; this is the expected, passing state for a live divergence)
//   - cerberus 4xx, reference 4xx  → divergence_closed — the reference
//     backend now also rejects; the divergence closed from the
//     reference's side and the entry must be reclassified to
//     `rejection`
//   - 5xx / transport failure      → hard_error (infrastructure, not parity)
//
// The corpus is the catalogue itself (rejectionparity.BuildCases), so
// corpus-case count == catalogue rejection+divergence-entry count by
// construction; the meta-tests under test/rejection-parity pin the
// remaining legs of the ratchet (site scan == catalogue, triggers
// exercise their sites).
//
// Exit semantics: ordinary numeric parity drift in the sibling
// promql-compliance-tester report stays report-only (task #68), but
// this driver's own verdicts are claims this package makes about
// itself, not observational drift — a wrong_rejection, a
// divergence_resolved, or a divergence_closed verdict means one of
// the catalogue's own assertions is now false. run() writes the JSON
// report first (the artifact survives either way) and then returns a
// non-zero exit whenever any of those three verdicts occurred, so the
// enclosing compat shell script's `set -e` fails the check instead of
// shipping green on a stale claim. Only stale_catalogue and hard_error
// stay non-fatal here: stale_catalogue needs a regenerate-and-curate
// pass a CI run cannot perform on its own, and hard_error is
// infrastructure, not a parity verdict.
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

	// Class is the case's catalogue classification (ClassRejection or
	// ClassDivergence), carried through so the report is
	// self-describing without a join back to the catalogue.
	Class string `json:"class"`

	// Verdict: for class=rejection, "parity" | "wrong_rejection" |
	// "stale_catalogue" | "hard_error". For class=divergence,
	// "divergence_confirmed" | "divergence_resolved" |
	// "divergence_closed" | "hard_error". See the package doc for the
	// meaning of each.
	Verdict string `json:"verdict"`
	// Detail carries the transport error or a snippet of the
	// unexpected response body for triage.
	Detail string `json:"detail,omitempty"`
}

// Report is the on-disk JSON artifact.
type Report struct {
	Head   string `json:"head"`
	Total  int    `json:"total"`
	Parity int    `json:"parity"`

	// WrongRejection / StaleCatalogue count class=rejection verdicts.
	WrongRejection int `json:"wrongRejection"`
	StaleCatalogue int `json:"staleCatalogue"`

	// DivergenceConfirmed / DivergenceResolved / DivergenceClosed count
	// class=divergence verdicts — see the package doc.
	DivergenceConfirmed int `json:"divergenceConfirmed"`
	DivergenceResolved  int `json:"divergenceResolved"`
	DivergenceClosed    int `json:"divergenceClosed"`

	HardErrors int          `json:"hardErrors"`
	Cases      []CaseResult `json:"cases"`
}

// Fatal reports whether the report's verdict counts include a claim
// this package makes about ITSELF that turned out false — a
// wrong_rejection, a divergence_resolved, or a divergence_closed. See
// the package doc's exit-semantics section for why these three (and
// only these three) are fatal.
func (r Report) Fatal() bool {
	return r.WrongRejection > 0 || r.DivergenceResolved > 0 || r.DivergenceClosed > 0
}

func run() error {
	var (
		head      = flag.String("head", "", "head to run: promql | logql | traceql")
		catalogue = flag.String("catalogue", "test/rejection-parity/catalogue", "path to the rejection-catalogue shard directory")
		refURL    = flag.String("ref", "", "reference backend base URL")
		cerbURL   = flag.String("cerberus", "", "cerberus base URL")
		report    = flag.String("report", "", "JSON report output path")
		timeout   = flag.Duration("timeout", 30*time.Second, "per-request HTTP timeout")
		evalTime  = flag.String("eval-time", "", "RFC3339 evaluation time (default: now) — point it inside the harness fixture's window so data-dependent reference guards fire")
	)
	flag.Parse()
	if *head == "" || *refURL == "" || *cerbURL == "" || *report == "" {
		return fmt.Errorf("-head, -ref, -cerberus and -report are all required")
	}
	now, err := resolveEvalTime(*evalTime)
	if err != nil {
		return err
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
		case "divergence_confirmed":
			rep.DivergenceConfirmed++
		case "divergence_resolved":
			rep.DivergenceResolved++
		case "divergence_closed":
			rep.DivergenceClosed++
		default:
			rep.HardErrors++
		}
		rep.Cases = append(rep.Cases, res)
	}

	if err := writeReport(*report, rep); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"==> rejection-parity %s: total=%d parity=%d wrong_rejection=%d stale_catalogue=%d "+
			"divergence_confirmed=%d divergence_resolved=%d divergence_closed=%d hard_errors=%d -> %s\n",
		rep.Head, rep.Total, rep.Parity, rep.WrongRejection, rep.StaleCatalogue,
		rep.DivergenceConfirmed, rep.DivergenceResolved, rep.DivergenceClosed, rep.HardErrors, *report)
	for _, c := range rep.Cases {
		if c.Verdict != "parity" && c.Verdict != "divergence_confirmed" {
			fmt.Fprintf(os.Stderr, "    [%s] %s (%s): ref=%d cerberus=%d query=%s\n",
				c.Verdict, c.Name, c.Endpoint, c.RefStatus, c.CerberusStatus, c.Query)
		}
	}

	// A wrong_rejection, divergence_resolved, or divergence_closed
	// verdict means one of the catalogue's own assertions is now
	// false — this is not observational drift, it's a broken claim
	// this package makes about itself. Fail the check (see the
	// package doc's exit-semantics section); the report above already
	// landed on disk so the artifact survives this branch.
	if rep.Fatal() {
		return fmt.Errorf(
			"rejection-parity %s: %d wrong_rejection + %d divergence_resolved + %d divergence_closed verdict(s) — "+
				"see %s for detail; a wrong_rejection is a bug to fix at the source, a divergence_resolved means the "+
				"catalogue entry must be deleted, a divergence_closed means it must be reclassified to \"rejection\"",
			rep.Head, rep.WrongRejection, rep.DivergenceResolved, rep.DivergenceClosed, *report,
		)
	}
	return nil
}

// runCase fires the trigger query at both backends and classifies the
// status pair. The comparison direction depends on c.Class — see the
// package doc.
func runCase(client *http.Client, c rejectionparity.Case, refURL, cerbURL string, now time.Time) CaseResult {
	res := CaseResult{Name: c.Name, Endpoint: c.Endpoint, Query: c.Query, Class: c.Class}

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
	case c.Class == rejectionparity.ClassDivergence:
		res.Verdict, res.Detail = divergenceVerdict(cerbStatus, refStatus, refBody, cerbBody)
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

// divergenceVerdict classifies a class=divergence case's status pair.
// The entry's claim is inverted relative to class=rejection: cerberus
// REJECTS the trigger query and the reference backend ACCEPTS it.
func divergenceVerdict(cerbStatus, refStatus int, refBody, cerbBody []byte) (string, string) {
	switch {
	case cerbStatus/100 == 2:
		// cerberus now answers a query the entry says it rejects —
		// the divergence closed from cerberus's side.
		return "divergence_resolved", fmt.Sprintf("cerberus accepted (divergence entry expects 4xx); ref=%q", snippet(refBody))
	case refStatus/100 == 2:
		// The expected, passing state: cerberus still rejects, the
		// reference still answers.
		return "divergence_confirmed", ""
	case refStatus/100 == 4:
		// The reference now also rejects — the divergence closed from
		// the reference's side; reclassify to "rejection".
		return "divergence_closed", fmt.Sprintf("reference now rejects (divergence entry expects 2xx); ref=%q cerberus=%q", snippet(refBody), snippet(cerbBody))
	default:
		return "hard_error", fmt.Sprintf("unclassifiable status pair; ref=%q cerberus=%q", snippet(refBody), snippet(cerbBody))
	}
}

// resolveEvalTime parses the -eval-time flag, defaulting to now.
//
// Most rejections are shape-based: both backends decide at parse /
// type-check time, so any syntactically valid window works. Some
// reference guards are NOT — upstream Prometheus range-checks
// `double_exponential_smoothing`'s smoothing and trend factors inside
// the per-series evaluation, so with no series in the lookback window
// the check never runs and the reference answers 200. Evaluating at a
// wall-clock "now" against a harness whose fixture is anchored in a
// fixed past window therefore makes those guards structurally
// unreachable, and a `rejection` entry covering one of them would be
// scored `wrong_rejection` for a data reason rather than a semantic
// one. Pointing -eval-time inside the fixture window closes that hole;
// it can only ever ADD reference rejections (data lets eval-time
// guards fire), never remove one.
func resolveEvalTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("-eval-time %q is not RFC3339: %w", s, err)
	}
	return t.UTC(), nil
}

// buildURL composes the per-endpoint query URL, anchored at the
// resolved evaluation time (see resolveEvalTime): an instant query at
// `now` for promql, a ±1h window ending at `now` for the range/search
// endpoints.
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
