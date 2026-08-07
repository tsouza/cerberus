package main

// Status-parity differential pass — the LogQL sibling of the Tempo /
// TraceQL "-- expect_status --" axis added alongside this file (see
// compatibility/tempo/driver/corpus.go's -- expect_status -- section
// and diff.go's diffStatusParityCase, landed for #1487 / #1435 /
// #1593 / #1484 / #1626).
//
// The corpus + burndown passes (main.go's queryOne/doQuery,
// burndown_value_parity.go) both fold ANY non-200 response into an
// UnexpectedFailure via doQuery's status check (main.go's queryOne ->
// doQuery, ~"resp.StatusCode != http.StatusOK") — correct for a
// "should return 200 with this body" case, but it means neither pass
// can express "both backends must REJECT this request" or catch
// cerberus 400-ing where reference Loki 200s (or vice versa). This
// pass closes that gap the same way the Tempo axis does: a small,
// fixed set of known-invalid requests, fetched from both backends
// WITHOUT the 200-only fold, asserting the HTTP status itself is the
// parity signal.
//
// Case selection deliberately avoids the five open rejection-parity
// issues above — those are about specific label/matcher shapes
// upstream ACCEPTS that cerberus currently rejects (or vice versa),
// exactly the kind of case this axis exists to eventually pin once
// fixed. The two cases here are uncontroversial, protocol-level
// rejections both backends have always agreed on: a missing `query`
// parameter (internal/api/loki/detected_fields.go and the sibling
// query/query_range handlers 400 with ErrBadData on this; reference
// Loki has rejected a missing query since the endpoint existed) and
// syntactically invalid LogQL (internal/api/loki's
// TestQuery_InvalidLogQL pins cerberus's 400; upstream's parser 400s
// the same class of input). They exist to prove the axis is wired
// end-to-end without touching any of the open bugs.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// statusParitySource is the report `testCase.source` for this pass,
// mirroring burndownSource's naming convention.
const statusParitySource = "cerberus/status-parity"

// statusParityWindow is an arbitrary recent window for the two cases
// below. Neither case's rejection depends on the seeded dataset or
// the specific time range — both backends validate the request shape
// before ever touching storage — so a wall-clock-relative window
// keeps this pass independent of the harness's seeded fixture.
const statusParityWindow = time.Hour

// statusParityCase is one fixed request this pass issues against both
// backends, asserting both return the SAME expected (non-2xx) status.
type statusParityCase struct {
	desc   string
	path   string
	params func(start, end time.Time) url.Values
	expect int
}

// statusParityCases is the fixed case list. Extend it as more
// protocol-level rejection-parity shapes need pinning; each entry
// must be a rejection BOTH backends already agree on (see the
// file-level comment) — a case tied to an open bug belongs in the
// bug's own issue, not here, because the committed
// compatibility/parity-baseline.json roster requires full parity.
func statusParityCases() []statusParityCase {
	return []statusParityCase{
		{
			desc: "missing query parameter on /query_range",
			path: "/loki/api/v1/query_range",
			params: func(start, end time.Time) url.Values {
				v := url.Values{}
				v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
				v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
				return v
			},
			expect: http.StatusBadRequest,
		},
		{
			desc: "syntactically invalid LogQL (unbalanced selector) on /query_range",
			path: "/loki/api/v1/query_range",
			params: func(start, end time.Time) url.Values {
				v := url.Values{}
				v.Set("query", `{`)
				v.Set("start", strconv.FormatInt(start.UnixNano(), 10))
				v.Set("end", strconv.FormatInt(end.UnixNano(), 10))
				return v
			},
			expect: http.StatusBadRequest,
		},
	}
}

// compareStatusParityAll runs the fixed status-parity case list and
// returns one Result per case. Results join the same report + score
// pipeline as every other pass — no separate bucket, no allow-list.
func compareStatusParityAll(c *http.Client, f flags) []Result {
	end := time.Now().UTC()
	start := end.Add(-statusParityWindow)

	cases := statusParityCases()
	results := make([]Result, 0, len(cases))
	for _, sc := range cases {
		results = append(results, compareStatusParityOne(c, f, sc, start, end))
	}
	return results
}

func compareStatusParityOne(c *http.Client, f flags, sc statusParityCase, start, end time.Time) Result {
	params := sc.params(start, end)
	result := Result{TestCase: TestCase{
		Query:       params.Get("query"),
		Source:      statusParitySource,
		Description: sc.desc,
		Kind:        "status_parity",
		Direction:   "n/a",
		Start:       start.Format(time.RFC3339Nano),
		End:         end.Format(time.RFC3339Nano),
	}}

	type fetched struct {
		status int
		body   string
		err    error
	}
	out := make([]fetched, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for idx, addr := range []string{f.addr1, f.addr2} {
		idx, addr := idx, addr
		go func() {
			defer wg.Done()
			status, body, err := fetchRawStatus(c, addr, sc.path, params)
			out[idx] = fetched{status: status, body: body, err: err}
		}()
	}
	wg.Wait()

	refErr, testErr := out[0].err, out[1].err
	switch {
	case refErr != nil:
		result.UnexpectedFailure = fmt.Sprintf("reference (-addr-1) request failed: %v", refErr)
		return result
	case testErr != nil:
		result.UnexpectedFailure = fmt.Sprintf("test endpoint (-addr-2) request failed: %v", testErr)
		return result
	}

	refStatus, testStatus := out[0].status, out[1].status
	switch {
	case refStatus != sc.expect && testStatus != sc.expect:
		result.UnexpectedFailure = fmt.Sprintf(
			"status parity: reference status=%d test status=%d, both expected %d (ref body=%s; test body=%s)",
			refStatus, testStatus, sc.expect, errorBodySnippet(out[0].body), errorBodySnippet(out[1].body),
		)
	case refStatus != sc.expect:
		result.UnexpectedFailure = fmt.Sprintf(
			"status parity: reference (-addr-1) returned status=%d, expected %d (body=%s)",
			refStatus, sc.expect, errorBodySnippet(out[0].body),
		)
	case testStatus != sc.expect:
		result.UnexpectedFailure = fmt.Sprintf(
			"status parity: test endpoint (-addr-2) returned status=%d, expected %d (body=%s)",
			testStatus, sc.expect, errorBodySnippet(out[1].body),
		)
	}
	return result
}

// fetchRawStatus issues a single GET and returns the raw status code
// WITHOUT folding a non-2xx response into an error — that fold (see
// doQuery in main.go) is exactly the blind spot this pass exists to
// avoid. A non-nil error here means the request never completed at
// all (network failure, timeout), which is a genuine harness problem
// distinct from a — possibly mismatched — status code.
func fetchRawStatus(c *http.Client, addr, path string, params url.Values) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()

	u := strings.TrimRight(addr, "/") + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", fmt.Errorf("new request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if readErr != nil {
		return 0, "", fmt.Errorf("read body: %w", readErr)
	}
	return resp.StatusCode, strings.TrimSpace(string(body)), nil
}
