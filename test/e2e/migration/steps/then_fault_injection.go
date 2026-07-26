package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// faultReplicas is how many times the heavy range query is replayed to
// compute p50/p95/p99 — small (this is a Docker Compose harness, not a load
// rig), but real: every replica is a genuine HTTP round trip against the live
// stack, never a stubbed timing, and every replica's BODY is checked against
// the fixture's declared cardinality.
const faultReplicas = 9

// faultConcurrency bounds how many of the faultReplicas run at once, so the
// replay exercises more than strictly-sequential request handling without
// pretending this small stack models "production QPS" literally.
const faultConcurrency = 3

// faultGatewayQueryTimeout is the per-query wall-clock cap the Tier-1
// cerberus service runs under — it MUST stay equal to CERBERUS_QUERY_TIMEOUT
// in docker-compose.dual.yml. MIG-08's whole claim is that a ClickHouse
// outage is bounded BY THE GATEWAY, so the bound has to come from cerberus's
// own budget, never from the test client giving up first. cerberus's default
// is two minutes; leaving it defaulted would make a client-side deadline the
// expected outcome, and "the harness hung" would read as "cerberus degraded".
//
// The coupling is asserted behaviourally rather than by re-reading the
// compose file: raise or drop the compose value and the degraded reply lands
// past faultDegradeBudget (or never lands inside faultQueryTimeout at all),
// which fails thenHeavyQueryDegradesCleanly outright.
const faultGatewayQueryTimeout = 15 * time.Second

// faultDegradeBudget is the wall clock a degraded reply must arrive inside:
// the gateway's own cap plus one equal slack for request admission, error
// classification and response write. A reply later than this means the bound
// that ended the request was not the gateway's.
const faultDegradeBudget = 2 * faultGatewayQueryTimeout

// faultQueryTimeout bounds a single heavy-query round trip on the CLIENT
// side. It sits strictly above faultDegradeBudget so it is never the bound
// that ends a request: a client deadline here is a hang, and MIG-08 reports
// it as a failure rather than counting it as "degraded cleanly".
const faultQueryTimeout = 3 * faultGatewayQueryTimeout

// faultBaselineP99Budget caps the healthy replay's p99. It is the gateway's
// own query budget: a baseline p99 at or above the cap means the heavy query
// only survived by a hair, and the "degraded vs healthy" contrast MIG-08
// draws would be measuring runner noise rather than the injected fault.
const faultBaselineP99Budget = faultGatewayQueryTimeout

// faultComposeService is the compose service MIG-08 fault-injects — the
// ClickHouse container cerberus reads every sample from, so pausing it is the
// most direct way to force cerberus's own query path to degrade without
// touching the reference backend the rollback assertion depends on staying
// healthy.
const faultComposeService = "clickhouse"

// faultComposeFile is the Tier-1 stack's compose file, relative to the
// repository root.
const faultComposeFile = "test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml"

// faultUnpauseWait bounds how long the recovery probe waits for cerberus to
// answer again — with the full baseline series set, not merely with a 200 —
// after ClickHouse is unpaused.
const faultUnpauseWait = 60 * time.Second

// faultLatencyReportName is the file the measured percentiles are captured
// into, written beside the seeder's manifest so it outlives the per-scenario
// workspace and can be read after a red run.
const faultLatencyReportName = "mig08-latency"

// faultReportFileMode is the mode the captured latency report is written
// with: readable evidence, not an executable.
const faultReportFileMode = 0o644

// unchurnedGaugeGroups is how many result series the fixture's churn-LESS
// gauge series collapse into under `sum by (<churn label>)`: exactly one,
// since the whole service × status-code cross-join shares the same (absent)
// churn value.
const unchurnedGaugeGroups = 1

// faultState is MIG-08's heavy-query replay — the query it replayed, the
// cardinality that query's own fixture declares, and the latencies it
// measured — plus which compose service (if any) fault injection currently
// has paused, so the harness can always restore it — including from World's
// own After hook, if the scenario's own restoring Then step never runs.
type faultState struct {
	archetype     string
	queryURL      string
	promQuery     string
	wantSeries    int
	maxPoints     int
	latencies     []time.Duration
	pausedService string
}

// registerFaultInjectionSteps binds MIG-08's Given/When/Then steps.
func (w *World) registerFaultInjectionSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator replays the heaviest query against cerberus repeatedly$`, w.whenReplayHeavyQuery)
	ctx.Step(`^every replay returns the fixture's declared series set and cerberus reports latency percentiles$`,
		w.thenReplayLatencyPercentiles)
	ctx.Step(`^the operator pauses the ClickHouse container$`, w.whenPauseClickHouse)
	ctx.Step(`^cerberus degrades the same heavy query cleanly instead of hanging$`, w.thenHeavyQueryDegradesCleanly)
	ctx.Step(`^the reference Prometheus still answers the rollback query with the same series set$`,
		w.thenReferenceStillAnswersDirectly)
	ctx.Step(`^the operator resumes the ClickHouse container and cerberus recovers the same series set$`,
		w.thenResumeAndRecover)
}

// heavyQueryGroupCount is how many series the heavy query must return,
// derived from the archetype's own declaration rather than pinned as a
// literal: `sum by (<churn label>)` collapses the fixture's gauge series into
// one group per declared churn value, plus unchurnedGaugeGroups for the
// series carrying no churn label at all. Fewer means a side silently dropped
// series (the failure mode an "HTTP 200" check cannot see); more means it
// invented one.
func heavyQueryGroupCount(d seed.Declaration) int {
	return d.ChurnCardinality + unchurnedGaugeGroups
}

// heavyRangeQueryURL builds the widest-window, highest-cardinality query this
// harness's seeded fixture supports: a range sum grouped by the archetype's
// declared high-churn label, over the manifest's own published window — the
// closed [VerifyStart, VerifyEnd] interval every live-backend scenario reads
// from, never a live one ending at `now`.
func heavyRangeQueryURL(base, promQuery string, m seed.Manifest) (string, error) {
	step, err := m.StepDuration()
	if err != nil {
		return "", err
	}
	v := url.Values{}
	v.Set("query", promQuery)
	v.Set("start", strconv.FormatInt(m.VerifyStart.Unix(), 10))
	v.Set("end", strconv.FormatInt(m.VerifyEnd.Unix(), 10))
	v.Set("step", step.String())
	return base + "/api/v1/query_range?" + v.Encode(), nil
}

// bindHeavyQuery resolves the scenario's single archetype, loads its fixture
// declaration and the seeder's manifest, and records BOTH the query and the
// oracle every later step asserts against: the declared group count and the
// manifest's own step count. Every MIG-08 step derives its expectation from
// this one binding, so the baseline, the rollback and the recovery poll can
// never drift into asserting three different things.
func (w *World) bindHeavyQuery(step string) error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	archetype, err := w.singleArchetype(step)
	if err != nil {
		return err
	}
	decl, err := seed.LoadDeclaration(harnessPath(w.root, archetypeDir, archetype, "seed", "fixture.json"))
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	if decl.ChurnLabel == "" {
		return fmt.Errorf(
			"migration harness: archetype %s declares no churn_label, so there is no high-cardinality dimension for MIG-08's heaviest query to group by",
			archetype,
		)
	}
	manifest, err := w.live.LoadManifest(archetype)
	if err != nil {
		return err
	}
	if manifest.VerifySteps <= 0 {
		return fmt.Errorf("migration harness: the seeder's manifest for %s declares %d verify steps; the heavy query has no grid to read",
			archetype, manifest.VerifySteps)
	}
	promQuery := fmt.Sprintf("sum by (%s) (%s)", decl.ChurnLabel, decl.GaugeMetric)
	reqURL, err := heavyRangeQueryURL(w.live.CerberusURL, promQuery, manifest)
	if err != nil {
		return err
	}
	w.fault.archetype = archetype
	w.fault.promQuery = promQuery
	w.fault.queryURL = reqURL
	w.fault.wantSeries = heavyQueryGroupCount(decl)
	w.fault.maxPoints = manifest.VerifySteps
	return nil
}

// requireBoundHeavyQuery fails a step that runs before the When step bound
// the query and its oracle, rather than letting it dial an empty URL and
// assert against a zero expectation.
func (w *World) requireBoundHeavyQuery() error {
	if w.fault.queryURL == "" {
		return fmt.Errorf("migration harness: no heavy query has been bound for this scenario; the replay step must run first")
	}
	return nil
}

// doHeavyQuery issues one GET against reqURL with faultQueryTimeout, so a
// degraded backend fails this call within a bound instead of hanging the
// scenario, and reports how long the round trip actually took — the number
// that separates "the gateway bounded it" from "the client gave up".
func doHeavyQuery(reqURL string) (status int, body []byte, elapsed time.Duration, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), faultQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("migration harness: build the heavy-query request: %w", err)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A transport-level error (including a context deadline) is data
		// here, not a harness failure: MIG-08 asserts on it directly.
		return 0, nil, time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()
	b, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return resp.StatusCode, nil, time.Since(start), fmt.Errorf("migration harness: read the heavy-query response: %w", readErr)
	}
	return resp.StatusCode, b, time.Since(start), nil
}

// faultRangeEnvelope is the Prometheus query_range wire shape, carrying both
// the success arm (data.result) and the error arm (errorType/error). MIG-08
// decodes the SAME envelope on every hop, because the two things it has to
// tell apart — "answered with the fixture's data" and "refused with a named
// backend failure" — are two arms of one shape.
type faultRangeEnvelope struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string    `json:"metric"`
			Values [][2]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// promMatrixResultType is the resultType a range query answers with. A
// backend that answered with a vector or a scalar answered a different
// question than the one MIG-08 asked.
const promMatrixResultType = "matrix"

// promStatusSuccess / promStatusError are the two values Prometheus's
// envelope `status` field takes.
const (
	promStatusSuccess = "success"
	promStatusError   = "error"
)

// assertHeavySeriesSet decodes one heavy-query response and asserts it
// carries the fixture's declared series set with real samples in it. This is
// what stops MIG-08 passing on an empty matrix: `{"status":"success",
// "data":{"result":[]}}` is an HTTP 200, and an HTTP 200 was the whole
// assertion before.
//
// The per-series point count is bounded on BOTH sides against the manifest's
// own grid: at least one point (an empty series is a hollow group) and never
// more than the manifest's verify-step count (a range query cannot return
// more instants than the grid it was asked for — more means the step grid
// itself is wrong).
func assertHeavySeriesSet(side string, body []byte, wantSeries, maxPoints int) error {
	var env faultRangeEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("migration harness: decode %s's heavy-query response: %w (body: %s)", side, err, string(body))
	}
	if env.Status != promStatusSuccess {
		return fmt.Errorf("migration harness: %s answered the heavy query with status %q (errorType %q: %s)",
			side, env.Status, env.ErrorType, env.Error)
	}
	if env.Data.ResultType != promMatrixResultType {
		return fmt.Errorf("migration harness: %s answered the heavy query with resultType %q, want %q",
			side, env.Data.ResultType, promMatrixResultType)
	}
	if len(env.Data.Result) != wantSeries {
		return fmt.Errorf(
			"migration harness: %s returned %d series for the heavy query, want the fixture's declared %d (one group per declared churn value plus the un-churned group)",
			side, len(env.Data.Result), wantSeries,
		)
	}
	for i, series := range env.Data.Result {
		switch {
		case len(series.Values) == 0:
			return fmt.Errorf("migration harness: %s returned series %d (%v) with no samples; an empty group is not an answer",
				side, i, series.Metric)
		case len(series.Values) > maxPoints:
			return fmt.Errorf(
				"migration harness: %s returned %d samples for series %d (%v), more than the manifest's %d verify steps",
				side, len(series.Values), i, series.Metric, maxPoints,
			)
		}
	}
	return nil
}

// whenReplayHeavyQuery replays the heaviest query this fixture supports
// faultReplicas times at faultConcurrency, asserting each replica's BODY
// against the fixture's declared series set and recording its latency. A
// single failure under normal (non-faulted) conditions fails the step
// outright — this is the baseline the later fault-injected replay is
// contrasted against, and a baseline that never proved it read the fixture
// would make that contrast meaningless.
func (w *World) whenReplayHeavyQuery() error {
	if err := w.bindHeavyQuery("the operator replays the heaviest query against cerberus repeatedly"); err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		lats     = make([]time.Duration, 0, faultReplicas)
		firstErr error
		sem      = make(chan struct{}, faultConcurrency)
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for i := 0; i < faultReplicas; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			status, body, elapsed, err := doHeavyQuery(w.fault.queryURL)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				fail(fmt.Errorf("migration harness: baseline heavy-query replay failed: %w", err))
			case status != http.StatusOK:
				fail(fmt.Errorf("migration harness: baseline heavy-query replay returned HTTP %d: %s", status, string(body)))
			default:
				if serr := assertHeavySeriesSet("cerberus (baseline replay)", body, w.fault.wantSeries, w.fault.maxPoints); serr != nil {
					fail(serr)
					return
				}
				lats = append(lats, elapsed)
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	w.fault.latencies = lats
	return nil
}

// percentile returns the p-th percentile (0..1) of a COPY of lats, sorted
// ascending; it never mutates the caller's slice.
func percentile(lats []time.Duration, p float64) time.Duration {
	sorted := append([]time.Duration(nil), lats...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// Percentile points MIG-08's report names explicitly, so the assertion below
// reads as the same p50/p95/p99 the PASS bullet documents.
const (
	p50 = 0.50
	p95 = 0.95
	p99 = 0.99
)

// faultLatencyReport is MIG-08's captured latency evidence — the "p50/p95/p99
// captured" half of the doc's PASS cell. It records what was measured AND
// what it was measured over, so a reader of a red run can tell a slow stack
// from a query that changed shape.
type faultLatencyReport struct {
	Archetype       string `json:"archetype"`
	Query           string `json:"query"`
	Replicas        int    `json:"replicas"`
	Concurrency     int    `json:"concurrency"`
	SeriesPerReplay int    `json:"series_per_replay"`
	P50Millis       int64  `json:"p50_millis"`
	P95Millis       int64  `json:"p95_millis"`
	P99Millis       int64  `json:"p99_millis"`
	P99BudgetMillis int64  `json:"p99_budget_millis"`
}

// thenReplayLatencyPercentiles asserts the baseline replay produced one
// latency per replica (never a hollow "it returned 200 once" pass), that the
// three percentiles are ordered and positive, and that p99 sits inside
// faultBaselineP99Budget — then CAPTURES them, to the run log and to a file
// beside the seeder's manifest, because "captured" is what MIG-08's PASS cell
// asks for and a number computed then discarded is not captured.
func (w *World) thenReplayLatencyPercentiles() error {
	if err := w.requireBoundHeavyQuery(); err != nil {
		return err
	}
	if len(w.fault.latencies) != faultReplicas {
		return fmt.Errorf("migration harness: recorded %d latenc(y/ies), want %d — a hollow replay proves nothing",
			len(w.fault.latencies), faultReplicas)
	}
	v50, v95, v99 := percentile(w.fault.latencies, p50), percentile(w.fault.latencies, p95), percentile(w.fault.latencies, p99)
	if v50 <= 0 {
		return fmt.Errorf("migration harness: p50 replay latency is %s; a real HTTP round trip cannot take no time", v50)
	}
	if v50 > v95 || v95 > v99 {
		return fmt.Errorf("migration harness: replay percentiles are not ordered: p50=%s p95=%s p99=%s", v50, v95, v99)
	}
	if v99 > faultBaselineP99Budget {
		return fmt.Errorf(
			"migration harness: baseline p99 replay latency is %s, over the %s budget — the healthy stack is already at cerberus's own query cap, so the fault-injected contrast would measure noise",
			v99, faultBaselineP99Budget,
		)
	}
	return w.captureLatencyReport(faultLatencyReport{
		Archetype:       w.fault.archetype,
		Query:           w.fault.promQuery,
		Replicas:        faultReplicas,
		Concurrency:     faultConcurrency,
		SeriesPerReplay: w.fault.wantSeries,
		P50Millis:       v50.Milliseconds(),
		P95Millis:       v95.Milliseconds(),
		P99Millis:       v99.Milliseconds(),
		P99BudgetMillis: faultBaselineP99Budget.Milliseconds(),
	})
}

// captureLatencyReport prints the measured percentiles to the suite's output
// (godog writes to os.Stdout, so they land in the CI log of the run that
// measured them) and writes them beside the seeder's manifest, which outlives
// the per-scenario workspace the After hook removes.
func (w *World) captureLatencyReport(r faultLatencyReport) error {
	fmt.Fprintf(os.Stdout, "MIG-08 %s heavy-query replay: p50=%dms p95=%dms p99=%dms (budget %dms, %d series/replay, query %q)\n",
		r.Archetype, r.P50Millis, r.P95Millis, r.P99Millis, r.P99BudgetMillis, r.SeriesPerReplay, r.Query)
	if w.live.ManifestPath == "" {
		return fmt.Errorf("migration harness: no manifest path bound; the latency report has nowhere to be captured")
	}
	path := filepath.Join(filepath.Dir(w.live.ManifestPath), faultLatencyReportName+"-"+r.Archetype+".json")
	buf, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("migration harness: marshal the MIG-08 latency report: %w", err)
	}
	if err := os.WriteFile(path, append(buf, '\n'), faultReportFileMode); err != nil {
		return fmt.Errorf("migration harness: capture the MIG-08 latency report to %s: %w", path, err)
	}
	return nil
}

// dockerCompose runs `docker compose -f faultComposeFile <args...>` rooted at
// the repository root, returning combined output on failure so a scenario
// failure names the real docker error rather than just a non-zero exit.
func dockerCompose(root string, args ...string) error {
	full := append([]string{"compose", "-f", faultComposeFile}, args...)
	cmd := exec.Command("docker", full...) //nolint:gosec // harness-authored fixed compose file + fixed subcommand
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("migration harness: docker %v: %w: %s", full, err, string(out))
	}
	return nil
}

// whenPauseClickHouse fault-injects by pausing (not killing) the ClickHouse
// container — reversible, and it leaves the reference Prometheus this
// scenario's rollback assertion depends on completely untouched. It records
// the pause on World so the harness can always restore it, including from
// World's own After hook if this scenario never reaches its own restoring
// Then step.
func (w *World) whenPauseClickHouse() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	if err := dockerCompose(w.root, "pause", faultComposeService); err != nil {
		return err
	}
	w.fault.pausedService = faultComposeService
	return nil
}

// faultDegradedStatuses are the two HTTP statuses cerberus's own Prom-head
// classifier emits when the ClickHouse it reads from stops answering: 503
// (the per-query wall-clock budget fired, or the chclient breaker is OPEN)
// and 502 (the driver surfaced the dead transport before the budget did).
// Which of the two wins is a race between the handler's deadline and the
// socket's, so both are the contract; anything else is not graceful
// degradation. An HTTP 200 means no degradation happened at all, and a 404 or
// a connection refusal means the gateway itself died rather than degraded.
var faultDegradedStatuses = []int{http.StatusBadGateway, http.StatusServiceUnavailable}

// isDegradedStatus reports whether status is one of the documented
// backend-failure statuses.
func isDegradedStatus(status int) bool {
	for _, s := range faultDegradedStatuses {
		if s == status {
			return true
		}
	}
	return false
}

// thenHeavyQueryDegradesCleanly replays the SAME heavy query against cerberus
// while ClickHouse is paused, and asserts every part of "degrades cleanly
// instead of hanging" separately:
//
//   - the request COMPLETED (err == nil). A client-side context deadline is
//     the hang this step exists to catch, so it can never be the pass path;
//   - it completed inside faultDegradeBudget, which is a multiple of the
//     GATEWAY's own CERBERUS_QUERY_TIMEOUT — the bound has to be cerberus's,
//     not the test client's;
//   - the status is one of the documented backend-failure statuses, so a 200
//     (no degradation) and a 404 / dead gateway both fail;
//   - the body is a Prometheus-shaped error envelope naming the failure, so a
//     bare status with an empty or success-shaped body fails too.
func (w *World) thenHeavyQueryDegradesCleanly() error {
	if w.fault.pausedService == "" {
		return fmt.Errorf("migration harness: no fault has been injected for this scenario")
	}
	if err := w.requireBoundHeavyQuery(); err != nil {
		return err
	}
	status, body, elapsed, err := doHeavyQuery(w.fault.queryURL)
	if err != nil {
		return fmt.Errorf(
			"migration harness: the heavy query never completed with ClickHouse paused — the client deadline (%s) ended it after %s, which is the hang this step exists to catch: %w",
			faultQueryTimeout, elapsed, err,
		)
	}
	if elapsed > faultDegradeBudget {
		return fmt.Errorf(
			"migration harness: cerberus took %s to refuse the heavy query, past the %s budget derived from its own %s query cap — the bound that ended the request was not the gateway's",
			elapsed, faultDegradeBudget, faultGatewayQueryTimeout,
		)
	}
	if !isDegradedStatus(status) {
		return fmt.Errorf(
			"migration harness: cerberus answered the heavy query with HTTP %d while ClickHouse was paused, want one of %v (body: %s)",
			status, faultDegradedStatuses, string(body),
		)
	}
	var env faultRangeEnvelope
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		return fmt.Errorf("migration harness: cerberus's degraded reply is not a Prometheus envelope: %w (body: %s)", uerr, string(body))
	}
	if env.Status != promStatusError || env.ErrorType == "" || env.Error == "" {
		return fmt.Errorf(
			"migration harness: cerberus's degraded reply does not name the backend failure: status=%q errorType=%q error=%q (HTTP %d)",
			env.Status, env.ErrorType, env.Error, status,
		)
	}
	return nil
}

// thenReferenceStillAnswersDirectly asserts the reference Prometheus —
// entirely untouched by the ClickHouse fault — still answers the SAME query
// with the SAME declared series set cerberus answered before the fault. This
// is MIG-08's "working datasource-flip rollback": while cerberus degrades,
// the backend an operator would flip Grafana back to is proven to still serve
// the data, not merely to still be listening.
func (w *World) thenReferenceStillAnswersDirectly() error {
	if w.fault.pausedService == "" {
		return fmt.Errorf("migration harness: no fault has been injected for this scenario")
	}
	if err := w.requireBoundHeavyQuery(); err != nil {
		return err
	}
	archetype := w.fault.archetype
	manifest, err := w.live.LoadManifest(archetype)
	if err != nil {
		return err
	}
	reqURL, err := heavyRangeQueryURL(w.live.PromURL, w.fault.promQuery, manifest)
	if err != nil {
		return err
	}
	status, body, _, err := doHeavyQuery(reqURL)
	if err != nil {
		return fmt.Errorf("migration harness: the reference Prometheus rollback query failed while cerberus was faulted: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("migration harness: the reference Prometheus rollback query returned HTTP %d: %s", status, string(body))
	}
	return assertHeavySeriesSet("the reference Prometheus (rollback)", body, w.fault.wantSeries, w.fault.maxPoints)
}

// thenResumeAndRecover unpauses ClickHouse and polls the SAME heavy query
// against cerberus until it answers with the SAME declared series set again.
// Recovery is the baseline restored, not a 200: a gateway that came back
// serving an empty matrix has not recovered, and the poll would happily have
// accepted that before. A 200 carrying the wrong series set is treated as
// not-yet-recovered and retried until faultUnpauseWait expires, at which
// point it is reported as the failure it is.
func (w *World) thenResumeAndRecover() error {
	if err := w.restorePausedService(); err != nil {
		return err
	}
	if err := w.requireBoundHeavyQuery(); err != nil {
		return err
	}
	deadline := time.Now().Add(faultUnpauseWait)
	var lastErr error
	for {
		status, body, _, err := doHeavyQuery(w.fault.queryURL)
		switch {
		case err != nil:
			lastErr = err
		case status != http.StatusOK:
			lastErr = fmt.Errorf("HTTP %d: %s", status, string(body))
		default:
			lastErr = assertHeavySeriesSet("cerberus (after ClickHouse resumed)", body, w.fault.wantSeries, w.fault.maxPoints)
			if lastErr == nil {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration harness: cerberus never recovered the fixture's declared series set within %s after ClickHouse resumed: %w",
				faultUnpauseWait, lastErr)
		}
		time.Sleep(bridgePollInterval)
	}
}

// restorePausedService unpauses whatever compose service fault injection
// currently has paused, if any. It is idempotent — called both by the
// scenario's own recovery Then step and, as a safety net, by World's After
// hook — so a scenario that never reaches its own recovery step (an earlier
// assertion failed first) still never leaves the shared Tier-1 stack
// degraded for whatever scenario runs next.
func (w *World) restorePausedService() error {
	if w.fault.pausedService == "" {
		return nil
	}
	service := w.fault.pausedService
	w.fault.pausedService = ""
	if err := dockerCompose(w.root, "unpause", service); err != nil {
		return fmt.Errorf("migration harness: restore the fault-injected %s service: %w", service, err)
	}
	return nil
}
