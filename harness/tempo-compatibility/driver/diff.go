// Diff subcommand: drives the TraceQL corpus through both backends,
// applies per-case assertions, computes the structural diff, and
// emits a markdown report.
//
// PR 4 of docs/tempo-compliance-plan.md. Smoke is the only corpus
// shipped today (~20 cases lifted from the in-tree shadow corpus);
// metrics endpoints + tag endpoints land in PRs 5+6. The subcommand
// is wired into main.go's switch on os.Args[1].
//
// Flag surface mirrors the seeder so a script that scripts the seeder
// can re-target the differ with the same env-or-flag triple.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// runDiff is the subcommand entry point. Wired from main.go.
func runDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	var (
		corpusPath  = fs.String("corpus", envOr("CORPUS_PATH", "/corpus/smoke.txtar"), "path to the TXTAR corpus file")
		tempoHTTP   = fs.String("tempo-http", envOr("TEMPO_HTTP_URL", "http://tempo:3200"), "Tempo HTTP base URL")
		cerberusURL = fs.String("cerberus", envOr("CERBERUS_URL", "http://cerberus-tempo:29092"), "cerberus HTTP base URL")
		reportPath  = fs.String("report", envOr("REPORT_PATH", "/reports/diff.md"), "markdown report output path")
		overall     = fs.Duration("timeout", 5*time.Minute, "overall driver timeout")
		perReq      = fs.Duration("request-timeout", 30*time.Second, "per-HTTP-request timeout")
		failOnDiff  = fs.Bool("fail-on-diff", false, "exit non-zero if any case reports a diff (otherwise just write report)")
		searchLimit = fs.Int("search-limit", 200, "Tempo /api/search ?limit= value")
		anchorIn    = fs.String("anchor", anchor, "fixture anchor RFC3339; used to compute search start/end window")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, cancel := context.WithTimeout(context.Background(), *overall)
	defer cancel()

	cases, err := LoadCorpus(*corpusPath)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	if len(cases) < 20 {
		// Same shape as harness/.../shadow/traceql_shadow_test.go's
		// `len(cases) < 20` guard — protects against a corpus author
		// accidentally trimming the smoke set below the agreed floor.
		return fmt.Errorf("smoke corpus shrunk: got %d cases, want >= 20", len(cases))
	}
	logger.Info("loaded corpus", "path", *corpusPath, "cases", len(cases))

	anchorTS, err := time.Parse(time.RFC3339, *anchorIn)
	if err != nil {
		return fmt.Errorf("parse anchor %q: %w", *anchorIn, err)
	}
	// Search window: one hour either side of the anchor. The seeder
	// pushes traces at anchor + (svcIdx*25 + traceIdx) seconds, so the
	// last span lands at anchor + ~400s; ±1h is generous slack.
	startTS := anchorTS.Add(-1 * time.Hour)
	endTS := anchorTS.Add(1 * time.Hour)

	client := &http.Client{Timeout: *perReq}
	opts := caseOpts{
		tempoHTTP:   *tempoHTTP,
		cerberusURL: *cerberusURL,
		startTS:     startTS,
		endTS:       endTS,
		searchLimit: *searchLimit,
	}
	results := make([]CaseResult, 0, len(cases))

	for _, tc := range cases {
		tc := tc
		if tc.SkipReason != "" {
			logger.Info("skipping case", "name", tc.Name, "reason", tc.SkipReason)
			results = append(results, CaseResult{Case: tc, Skipped: true})
			continue
		}
		logger.Info("diffing case", "name", tc.Name, "endpoint", tc.Endpoint)
		results = append(results, diffCase(ctx, client, tc, opts))
	}

	if err := writeReport(*reportPath, results); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	logger.Info("wrote markdown report", "path", *reportPath)

	// Per the plan: PR 4 is informational only (continue-on-error at
	// the workflow level). The differ still respects --fail-on-diff so
	// a local repro can hard-fail on a regression.
	if *failOnDiff {
		for _, r := range results {
			if r.Skipped {
				continue
			}
			if r.HardError != "" || !r.Diff.Equal || len(r.Assertions) > 0 {
				return fmt.Errorf("diffs reported; see %s", *reportPath)
			}
		}
	}
	return nil
}

// CaseResult is one corpus case's outcome. Populated incrementally
// (HTTP first, assertions next, diff last) so the report can show
// partial info when a step failed.
type CaseResult struct {
	Case CorpusCase

	// Skipped means the case carried a SkipReason; the report lists
	// these in a separate section.
	Skipped bool

	// HardError is set when the case failed before the diff could run
	// (URL build, HTTP error, non-2xx status). Mutually exclusive with
	// Diff / Assertions being meaningful.
	HardError string

	TempoStatus    int
	CerberusStatus int

	// Assertions is the union of per-side assertion failures (tempo's
	// list, then cerberus's). Empty means both sides met the case's
	// expectations.
	Assertions []DiffReason

	// Diff is the structural diff between the two response bodies. Its
	// Equal field is true when the canonical-key sets agreed and every
	// matched entry passed field-by-field tolerance.
	Diff Diff
}

// caseOpts bundles the per-run URL / window inputs threaded through
// every corpus case. Pulled out of runDiff's loop to keep that function
// under funlen and to make the per-case driver (diffCase) trivially
// testable.
type caseOpts struct {
	tempoHTTP   string
	cerberusURL string
	startTS     time.Time
	endTS       time.Time
	searchLimit int
}

// diffCase executes a single corpus case end-to-end (URL build → HTTP
// fetch on both sides → per-side assertions → structural diff). The
// returned CaseResult is populated incrementally so a hard error at any
// step short-circuits but still surfaces partial context.
func diffCase(ctx context.Context, client *http.Client, tc CorpusCase, opts caseOpts) CaseResult {
	res := CaseResult{Case: tc}

	tempoURL, err := buildURL(opts.tempoHTTP, tc, "tempo", opts.startTS, opts.endTS, opts.searchLimit)
	if err != nil {
		res.HardError = fmt.Sprintf("build tempo url: %v", err)
		return res
	}
	cerbURL, err := buildURL(opts.cerberusURL, tc, "cerberus", opts.startTS, opts.endTS, opts.searchLimit)
	if err != nil {
		res.HardError = fmt.Sprintf("build cerberus url: %v", err)
		return res
	}

	tempoBody, tempoStatus, terr := fetchJSON(ctx, client, tempoURL)
	cerbBody, cerbStatus, cerr := fetchJSON(ctx, client, cerbURL)
	res.TempoStatus = tempoStatus
	res.CerberusStatus = cerbStatus
	if terr != nil {
		res.HardError = fmt.Sprintf("tempo fetch: %v", terr)
	}
	if cerr != nil {
		if res.HardError == "" {
			res.HardError = fmt.Sprintf("cerberus fetch: %v", cerr)
		} else {
			res.HardError = res.HardError + "; cerberus fetch: " + cerr.Error()
		}
	}
	// /api/search returns 200 on empty matches; treat any non-2xx
	// as a hard error since the harness's assertion checks expect
	// a well-formed envelope.
	if res.HardError == "" && (tempoStatus/100 != 2 || cerbStatus/100 != 2) {
		res.HardError = fmt.Sprintf("non-2xx: tempo=%d cerberus=%d", tempoStatus, cerbStatus)
	}
	if res.HardError != "" {
		return res
	}

	// Per-side assertions first. They are cheap and surface
	// "cerberus returned 0 rows where corpus said >=N" before the
	// diff complains about cardinality.
	tempoReasons, err := AssertCase(tc, tempoBody, "tempo")
	if err != nil {
		res.HardError = fmt.Sprintf("assert tempo: %v", err)
		return res
	}
	cerbReasons, err := AssertCase(tc, cerbBody, "cerberus")
	if err != nil {
		res.HardError = fmt.Sprintf("assert cerberus: %v", err)
		return res
	}
	res.Assertions = append(res.Assertions, tempoReasons...)
	res.Assertions = append(res.Assertions, cerbReasons...)

	// Differential diff. The case sets some upper bound on what we
	// can expect to agree on; the structural diff is independent.
	// Dispatch by endpoint kind: trace endpoints use the SearchResponse
	// canonical-key diff; the four tag endpoints diff the string-list
	// envelope; tag-values v2 additionally checks the Type field per
	// matched value. Keeping the dispatch here (vs threading a closure
	// through every helper) keeps the runtime branches local to where
	// the response shapes diverge.
	d, err := compareForEndpoint(tc, tempoBody, cerbBody)
	if err != nil {
		res.HardError = fmt.Sprintf("diff: %v", err)
		return res
	}
	res.Diff = d
	return res
}

// compareForEndpoint runs the right structural-diff function for the
// case's endpoint kind. The four tag endpoints share `CompareTagNames`
// (envelope is a flat string set on V1 + a flatten-the-scopes view on
// V2) and `CompareTagValues` (envelope is a flat list on V1, typed
// objects on V2 — the differ unifies on the `Value` field and reports
// `Type` mismatches as field_mismatch reasons).
func compareForEndpoint(tc CorpusCase, tempoBody, cerbBody []byte) (Diff, error) {
	switch tc.Endpoint {
	case "tags_v1":
		return CompareTagNames(tempoBody, cerbBody, "tempo", "cerberus", false)
	case "tags_v2":
		return CompareTagNames(tempoBody, cerbBody, "tempo", "cerberus", true)
	case "tag_values_v1":
		return CompareTagValues(tempoBody, cerbBody, "tempo", "cerberus", false)
	case "tag_values_v2":
		return CompareTagValues(tempoBody, cerbBody, "tempo", "cerberus", true)
	default:
		return Compare(tempoBody, cerbBody, "tempo", "cerberus", DefaultDiffOptions())
	}
}

// buildURL composes the per-endpoint URL for a corpus case. The
// `backend` argument is purely for error messages.
func buildURL(base string, tc CorpusCase, backend string, startTS, endTS time.Time, searchLimit int) (string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return "", fmt.Errorf("%s base url: %w", backend, err)
	}
	q := url.Values{}
	switch tc.Endpoint {
	case "search":
		u.Path += "/api/search"
		q.Set("q", tc.Query)
		q.Set("limit", fmt.Sprintf("%d", searchLimit))
		q.Set("start", fmt.Sprintf("%d", startTS.Unix()))
		q.Set("end", fmt.Sprintf("%d", endTS.Unix()))
	case "search_recent":
		u.Path += "/api/search/recent"
		q.Set("limit", fmt.Sprintf("%d", searchLimit))
	case "traces":
		id, err := deriveTraceIDFromTemplate(tc.TraceIDTemplate)
		if err != nil {
			return "", err
		}
		u.Path += "/api/traces/" + id
	case "tags_v1":
		u.Path += "/api/search/tags"
		q.Set("start", fmt.Sprintf("%d", startTS.Unix()))
		q.Set("end", fmt.Sprintf("%d", endTS.Unix()))
	case "tags_v2":
		u.Path += "/api/v2/search/tags"
		q.Set("start", fmt.Sprintf("%d", startTS.Unix()))
		q.Set("end", fmt.Sprintf("%d", endTS.Unix()))
		if tc.Scope != "" {
			q.Set("scope", tc.Scope)
		}
	case "tag_values_v1":
		u.Path += "/api/search/tag/" + tc.TagName + "/values"
		q.Set("start", fmt.Sprintf("%d", startTS.Unix()))
		q.Set("end", fmt.Sprintf("%d", endTS.Unix()))
	case "tag_values_v2":
		u.Path += "/api/v2/search/tag/" + tc.TagName + "/values"
		q.Set("start", fmt.Sprintf("%d", startTS.Unix()))
		q.Set("end", fmt.Sprintf("%d", endTS.Unix()))
	default:
		return "", fmt.Errorf("unsupported endpoint %q", tc.Endpoint)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// deriveTraceIDFromTemplate converts a corpus template like
// "checkout/3" into the hex-encoded 16-byte trace ID the seeder used.
// Mirrors seeder.go::deriveTraceID byte-for-byte (intentionally
// duplicates the hash logic so the corpus file authors don't need to
// reach into the seeder's internals).
func deriveTraceIDFromTemplate(tmpl string) (string, error) {
	parts := strings.SplitN(tmpl, "/", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("traceid_template %q: want <service>/<idx>", tmpl)
	}
	svc := parts[0]
	var idx int
	if _, err := fmt.Sscanf(parts[1], "%d", &idx); err != nil {
		return "", fmt.Errorf("traceid_template %q: parse idx: %w", tmpl, err)
	}
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(idx)) //nolint:gosec // idx non-negative by construction
	h := sha256.Sum256(append([]byte("cerberus-tempo-trace:"+svc+":"), b[:]...))
	return hex.EncodeToString(h[:16]), nil
}

// fetchJSON GETs a URL with the Accept: application/json header and
// returns the body + status. Errors include the response body (up to
// 2KB) so a CH error message lands in the report.
func fetchJSON(ctx context.Context, client *http.Client, urlStr string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// Surface a snippet of the error body in the error string so
		// the report explains the 4xx/5xx without forcing the caller
		// to grep container logs.
		snippet := body
		if len(snippet) > 2048 {
			snippet = snippet[:2048]
		}
		return body, resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, string(snippet))
	}
	return body, resp.StatusCode, nil
}

// writeReport renders the case-by-case markdown summary. The format is
// deliberately simple — a top-level summary line + per-case sections —
// so the report renders well as a GH Actions artefact preview and is
// readable as plaintext in the terminal.
func writeReport(path string, results []CaseResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir report dir: %w", err)
	}
	f, err := os.Create(path) //nolint:gosec // G304: report path is a trusted CLI argument
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return renderReport(f, results)
}

func renderReport(w io.Writer, results []CaseResult) error {
	var total, passed, diffed, asserted, skipped, hardErr int
	for _, r := range results {
		total++
		switch {
		case r.Skipped:
			skipped++
		case r.HardError != "":
			hardErr++
		case !r.Diff.Equal:
			diffed++
		case len(r.Assertions) > 0:
			asserted++
		default:
			passed++
		}
	}

	if _, err := fmt.Fprintln(w, "# Tempo / TraceQL compatibility — diff report"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Generated at %s\n\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "## Summary"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Cases: %d\n", total); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Passed: %d\n", passed); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Skipped: %d\n", skipped); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Diff: %d\n", diffed); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Assertion failures: %d\n", asserted); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Hard errors: %d\n", hardErr); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	// Sort by name so the report is reproducible across runs and a
	// reviewer can scan section ordering for regressions without
	// re-ordering the diff in the head.
	sorted := append([]CaseResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Case.Name < sorted[j].Case.Name })

	if _, err := fmt.Fprintln(w, "## Cases"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, r := range sorted {
		if err := renderCase(w, r); err != nil {
			return err
		}
	}
	return nil
}

func renderCase(w io.Writer, r CaseResult) error {
	if _, err := fmt.Fprintf(w, "### `%s`\n\n", r.Case.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- endpoint: `%s`\n", r.Case.Endpoint); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- query: `%s`\n", strings.ReplaceAll(r.Case.Query, "`", "\\`")); err != nil {
		return err
	}
	switch {
	case r.Skipped:
		if _, err := fmt.Fprintf(w, "- status: SKIPPED (%s)\n", r.Case.SkipReason); err != nil {
			return err
		}
	case r.HardError != "":
		if _, err := fmt.Fprintf(w, "- status: ERROR — %s\n", r.HardError); err != nil {
			return err
		}
	case !r.Diff.Equal:
		if _, err := fmt.Fprintf(w, "- status: DIFF (%d reasons)\n", len(r.Diff.Reasons)); err != nil {
			return err
		}
	case len(r.Assertions) > 0:
		if _, err := fmt.Fprintf(w, "- status: ASSERTION (%d reasons)\n", len(r.Assertions)); err != nil {
			return err
		}
	default:
		if _, err := fmt.Fprintln(w, "- status: PASS"); err != nil {
			return err
		}
	}
	if !r.Skipped && r.HardError == "" {
		if _, err := fmt.Fprintf(w, "- tempo HTTP: %d  cerberus HTTP: %d\n", r.TempoStatus, r.CerberusStatus); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "- matched canonical-key entries: %d\n", r.Diff.MatchedCount); err != nil {
			return err
		}
	}
	if len(r.Assertions) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "#### Assertion reasons"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		for _, reason := range r.Assertions {
			if _, err := fmt.Fprintf(w, "- [%s] %s\n", reason.Kind, reason.Detail); err != nil {
				return err
			}
		}
	}
	if !r.Diff.Equal && len(r.Diff.Reasons) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "#### Diff reasons"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		for _, reason := range r.Diff.Reasons {
			if _, err := fmt.Fprintf(w, "- [%s] %s\n", reason.Kind, reason.Detail); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}
