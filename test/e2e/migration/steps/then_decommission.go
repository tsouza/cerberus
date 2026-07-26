package steps

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
	"github.com/prometheus/common/model"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// residualReaderQueries is how many times the planted residual reader
// queries the incumbent. It lives in Go, not in feature text, per the "no
// number in step text" discipline: the relation the Then asserts — the audit
// refuses on a non-zero delta — is what the scenario names; this is the data
// the reader actually generates.
const residualReaderQueries = 3

// decommissionStageOrder pins the teardown order docs/migration-testing.md
// section 6's DECOMMISSION row documents for MIG-25: the old read path first,
// then the ruler/Alertmanager, then the ingest/write leg last. It is declared
// once, as data, so a future edit that silently reorders it is a diff a
// reviewer sees rather than a runbook fact that only ever lived in prose.
var decommissionStageOrder = []string{"read-path", "ruler-alertmanager", "ingest-write"}

// decommissionStagePhrases names each stage the way the runbook names it.
// thenStagedOrderMatchesRunbook locates these phrases inside the runbook's own
// MIG-25 row and requires them to appear there in the order the list above
// gates them, so neither side can drift alone: reorder the Go list, or reword
// the runbook, and the assertion fails.
//
// The runbook is the oracle because nothing the PRODUCT emits states this
// order yet — `cerberus migrate gate`'s stages are the evidence stages
// (verify, classify, inventory, rulegraph), not cutover/teardown stages. When
// the gate grows a cutover stage this assertion moves onto its output and
// stops reading prose; until then the scenario says so in its own text rather
// than implying an enforcement that does not exist.
var decommissionStagePhrases = map[string]string{
	"read-path":          "old read path first",
	"ruler-alertmanager": "then ruler/Alertmanager",
	"ingest-write":       "then ingest/write leg last",
}

// decommissionRunbook is the document whose story row states that order,
// resolved against the repository root the World was built on;
// decommissionStoryID is the story that row names.
const (
	decommissionRunbook = "docs/migration-testing.md"
	decommissionStoryID = "MIG-25"
)

// queryHandlerRequestsMetric is the reference Prometheus's own self-exposed
// counter for requests its query handler has served — the signal a
// decommission audit reads to detect a reader still depending on the
// incumbent's read path.
const queryHandlerRequestsMetric = `prometheus_http_requests_total{handler="/api/v1/query"`

// scrapeMetricsTimeout bounds one /metrics scrape, and scrapeMetricsBodyLimit
// caps how much of it is parsed.
const (
	scrapeMetricsTimeout   = 10 * time.Second
	scrapeMetricsBodyLimit = 8 << 20
)

// complianceRetentionFixture is the committed per-archetype declaration of
// the compliance-retention mandate MIG-26's tier-1 half compares the live
// ClickHouse retention against.
const complianceRetentionFixture = "compliance-retention.json"

// decommissionRun is one archetype's MIG-25 run: the baseline read-traffic
// counter, the residual delta a planted reader produced, and the audit's own
// refuse/allow verdict.
type decommissionRun struct {
	metric        string
	baseline      float64
	residualDelta float64
	refused       bool
}

// retentionGateRun is one archetype's MIG-26 tier-1 run: the declared
// compliance-retention mandate and what the live ClickHouse retention proves
// against it.
type retentionGateRun struct {
	mandate       time.Duration
	liveRetention time.Duration
	liveProven    bool
	meetsMandate  bool
}

// complianceRetention is the committed fixture shape: the compliance window
// an operator must prove ClickHouse's retention covers, per signal.
type complianceRetention struct {
	Metrics model.Duration `json:"metrics"`
}

// registerDecommissionSteps binds the MIG-25 (residual-read-traffic audit)
// and MIG-26 tier-1 (live-retention-vs-compliance-mandate) steps.
func (w *World) registerDecommissionSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the incumbent's read-traffic baseline captured before any residual reader runs$`, w.givenReadTrafficBaseline)
	ctx.Step(`^the incumbent's read traffic since the baseline is exactly zero, before any residual reader has run$`, w.thenBaselineDeltaIsZero)
	ctx.Step(`^a residual reader keeps querying the incumbent after cutover would have begun$`, w.whenResidualReaderRuns)
	ctx.Step(`^the operator audits incumbent read traffic for decommission authorization$`, w.whenAuditReadTraffic)
	ctx.Step(`^the audit refuses authorization while the incumbent still serves the residual reader's traffic$`, w.thenAuditRefuses)
	ctx.Step(`^the audit's refusal names the exact residual request count it measured$`, w.thenAuditNamesResidualCount)
	ctx.Step(`^the staged decommission order the runbook documents still gates the read path before the ruler and the ingest leg$`,
		w.thenStagedOrderMatchesRunbook)
	ctx.Step(`^the residual reader stops and the operator re-establishes the read-traffic baseline$`, w.whenResidualReaderStops)
	ctx.Step(`^the same audit authorizes decommission once the incumbent serves no residual read traffic at all$`, w.thenAuditAuthorizes)

	ctx.Step(`^the declared compliance-retention mandate for each tagged archetype$`, w.givenComplianceMandate)
	ctx.Step(`^the operator reads the retention ClickHouse actually provisions, live$`, w.whenReadLiveComplianceRetention)
	ctx.Step(`^the retention the gate decides on was actually read off the live ClickHouse$`, w.thenLiveRetentionWasActuallyRead)
	ctx.Step(`^the live retention does not yet prove the compliance-retention mandate is met$`, w.thenLiveRetentionDoesNotProveMandate)
}

// --- MIG-25: decommission is blocked while the incumbent still serves reads

// scrapeCounterSum fetches baseURL's /metrics exposition and sums every
// sample whose rendered key matches selector — a bare metric name for an
// unlabelled sample, or "name{partial-label" for a family whose rendered
// label set contains the fragment. This mirrors the matching convention
// test/e2e/migration/seed/settle.go's own settle gates use; it is
// reimplemented here, rather than imported, because it is a private helper
// in that package and this audit needs exactly one counter, not the settle
// machinery built around it.
func scrapeCounterSum(ctx context.Context, baseURL, selector string) (float64, error) {
	reqCtx, cancel := context.WithTimeout(ctx, scrapeMetricsTimeout)
	defer cancel()
	target := strings.TrimRight(baseURL, "/") + "/metrics"
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("migration harness: build metrics scrape request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("migration harness: scrape %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("migration harness: %s returned %d", target, resp.StatusCode)
	}
	var total float64
	var matched bool
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, scrapeMetricsBodyLimit))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), bufio.MaxScanTokenSize)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rawValue, ok := strings.Cut(line, " ")
		if !ok || !matchesSelector(name, selector) {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(rawValue), 64)
		if err != nil {
			continue
		}
		total += value
		matched = true
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("migration harness: read %s: %w", target, err)
	}
	if !matched {
		return 0, fmt.Errorf("migration harness: %s exposes no metric matching %q", target, selector)
	}
	return total, nil
}

// matchesSelector reports whether an exposition key matches selector, which
// is either a bare metric name or "name{partial-label-fragment".
func matchesSelector(key, selector string) bool {
	if key == selector {
		return true
	}
	brace := strings.IndexRune(selector, '{')
	if brace < 0 {
		return strings.HasPrefix(key, selector+"{")
	}
	name := selector[:brace]
	labels := selector[brace+1:]
	if !strings.HasPrefix(key, name+"{") {
		return false
	}
	end := strings.IndexRune(key, '}')
	if end < 0 {
		return false
	}
	return strings.Contains(key[len(name)+1:end], labels)
}

func (w *World) givenReadTrafficBaseline() error {
	if err := w.requireArchetypes("the read-traffic baseline"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return fmt.Errorf("archetype %s: capture the read-traffic baseline: %w", a, err)
		}
		w.decommission[a] = decommissionRun{baseline: count}
	}
	return nil
}

// thenBaselineDeltaIsZero proves the audit is not a rubber stamp that always
// refuses: read immediately after the baseline, before any residual reader
// has run, the delta must read exactly zero.
func (w *World) thenBaselineDeltaIsZero() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return dr, fmt.Errorf("archetype %s: re-read read traffic before the residual reader runs: %w", a, err)
		}
		if delta := count - dr.baseline; delta != 0 {
			return dr, fmt.Errorf("archetype %s: read traffic already advanced by %v since the baseline, before any residual reader ran", a, delta)
		}
		return dr, nil
	})
}

// whenResidualReaderRuns plants the reader the story's fixture calls for: a
// client that keeps querying the incumbent after cutover would have begun.
func (w *World) whenResidualReaderRuns() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		m, err := w.live.LoadManifest(a)
		if err != nil {
			return dr, err
		}
		if m.GaugeMetric == "" {
			return dr, fmt.Errorf("archetype %s: the seeder's manifest names no gauge metric for the residual reader to query", a)
		}
		dr.metric = m.GaugeMetric
		for range residualReaderQueries {
			if _, err := lib.QueryInstant(context.Background(), w.live.PromURL, dr.metric, m.VerifyEnd); err != nil {
				return dr, fmt.Errorf("archetype %s: the residual reader's query failed: %w", a, err)
			}
		}
		return dr, nil
	})
}

// whenAuditReadTraffic is the decommission-authorization audit itself: it
// re-reads the same counter and refuses whenever the delta since the
// baseline is non-zero.
//
// The scenario runs it TWICE — once with the planted reader's traffic on the
// incumbent, once after that traffic has stopped and the baseline has been
// re-established — and asserts opposite verdicts from the identical
// computation. That is what stops an audit that refuses unconditionally from
// passing: hard-code the refusal and the second run fails.
func (w *World) whenAuditReadTraffic() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return dr, fmt.Errorf("archetype %s: audit incumbent read traffic: %w", a, err)
		}
		dr.residualDelta = count - dr.baseline
		dr.refused = dr.residualDelta > 0
		return dr, nil
	})
}

func (w *World) thenAuditRefuses() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		if !dr.refused {
			return dr, fmt.Errorf("archetype %s: the audit authorized teardown while the residual reader's traffic delta was %v", a, dr.residualDelta)
		}
		return dr, nil
	})
}

// thenAuditNamesResidualCount asserts the refusal names the EXACT residual
// count it measured — the count the planted reader actually issued, read back
// through the incumbent's own request counter — rather than merely "some"
// non-zero number. The oracle is residualReaderQueries: the harness knows how
// many queries it planted, and the incumbent's counter has to have moved by
// exactly that much for the audit's figure to mean anything.
//
// The PASS cell also calls for the audit result to be captured as a
// decommission-authorization ARTIFACT. Nothing in `cerberus migrate` emits one
// today — the gate's stages are evidence stages, not teardown stages — so this
// step asserts the figure and no more, and the scenario's own text says so
// rather than naming an artifact that does not exist.
func (w *World) thenAuditNamesResidualCount() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		if dr.residualDelta != float64(residualReaderQueries) {
			return dr, fmt.Errorf("archetype %s: the audit's refused-on count is %v, not the %d request(s) the planted reader actually issued",
				a, dr.residualDelta, residualReaderQueries)
		}
		return dr, nil
	})
}

// whenResidualReaderStops ends the planted reader's traffic and re-establishes
// the baseline the audit measures from. The incumbent's counter is cumulative,
// so a quiet incumbent is not one whose counter reads zero — it is one whose
// counter has stopped moving, which is exactly what re-baselining here and
// re-running the SAME audit measures.
func (w *World) whenResidualReaderStops() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		if dr.residualDelta <= 0 {
			return dr, fmt.Errorf("archetype %s: no residual traffic was ever measured, so there is nothing for the reader to stop generating", a)
		}
		count, err := scrapeCounterSum(context.Background(), w.live.PromURL, queryHandlerRequestsMetric)
		if err != nil {
			return dr, fmt.Errorf("archetype %s: re-establish the read-traffic baseline once the residual reader stopped: %w", a, err)
		}
		dr.baseline = count
		return dr, nil
	})
}

// thenAuditAuthorizes is the audit's negative control: with the reader stopped
// and the baseline re-established, the identical audit has to AUTHORIZE, on a
// residual delta of exactly none. Without it, an audit that refuses whatever
// it measures — `refused = true` — satisfies every other assertion in this
// scenario, and the block it reports would say nothing about the reader it
// claims to have blocked on.
func (w *World) thenAuditAuthorizes() error {
	return w.eachDecommission(func(a string, dr decommissionRun) (decommissionRun, error) {
		if dr.residualDelta != 0 {
			return dr, fmt.Errorf("archetype %s: the incumbent served %v further request(s) after the residual reader stopped, so the audit was never handed a quiet incumbent to authorize",
				a, dr.residualDelta)
		}
		if dr.refused {
			return dr, fmt.Errorf("archetype %s: the audit still refuses on a residual delta of %v; an audit that refuses whatever it measures proves nothing about the reader it refused on",
				a, dr.residualDelta)
		}
		return dr, nil
	})
}

// thenStagedOrderMatchesRunbook holds the staged teardown order the harness
// gates to the runbook's own words: each stage's documented phrase must be
// present in the runbook's MIG-25 row, and must appear there in the order the
// harness gates them. Comparing the list against its own literals proved
// nothing; comparing it against the document that states the runbook is a
// claim either side can falsify.
func (w *World) thenStagedOrderMatchesRunbook() error {
	path := filepath.Join(w.root, decommissionRunbook)
	doc, err := os.ReadFile(path) //nolint:gosec // repository-relative runbook path
	if err != nil {
		return fmt.Errorf("migration harness: read the decommission runbook %s: %w", path, err)
	}
	return stagedOrderMatchesRunbook(string(doc))
}

// stagedOrderMatchesRunbook is the comparison itself, over the runbook's text
// rather than its path, so the same assertion the live scenario makes is also
// made offline against the committed document (see the steps package tests).
func stagedOrderMatchesRunbook(doc string) error {
	row, err := runbookStageRow(doc)
	if err != nil {
		return err
	}
	if len(decommissionStageOrder) != len(decommissionStagePhrases) {
		return fmt.Errorf("the harness gates %d stage(s) but names %d of them in the runbook's words; every gated stage has to be one the runbook states",
			len(decommissionStageOrder), len(decommissionStagePhrases))
	}
	prev := -1
	for _, stage := range decommissionStageOrder {
		phrase, ok := decommissionStagePhrases[stage]
		if !ok {
			return fmt.Errorf("the harness gates the %q stage, which nothing in the runbook names", stage)
		}
		at := strings.Index(row, phrase)
		if at < 0 {
			return fmt.Errorf("the runbook's %s row no longer documents the %s stage as %q: %s",
				decommissionStoryID, stage, phrase, row)
		}
		if at <= prev {
			return fmt.Errorf("the runbook documents %q ahead of the stage the harness gates before it, so the harness order %v is not the documented one",
				phrase, decommissionStageOrder)
		}
		prev = at
	}
	return nil
}

// runbookStageRow returns the runbook row that states MIG-25's staged teardown
// order. The story is named by two tables — section 4 states the story,
// section 6 states its PASS assertion — so the row is identified by naming the
// story AND stating the first documented stage, and exactly one row must do
// both: none means the runbook stopped documenting the order at all, more than
// one means the harness cannot say which statement it is holding itself to.
func runbookStageRow(doc string) (string, error) {
	rowPrefix := "| " + decommissionStoryID + " |"
	anchor := decommissionStagePhrases[decommissionStageOrder[0]]
	var found []string
	for _, line := range strings.Split(doc, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, rowPrefix) && strings.Contains(trimmed, anchor) {
			found = append(found, trimmed)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%s holds %d row(s) naming %s and stating %q; the staged teardown order has to be documented exactly once",
			decommissionRunbook, len(found), decommissionStoryID, anchor)
	}
	return found[0], nil
}

// eachDecommission runs fn over every tagged archetype's decommission-audit
// state, writing back whatever fn returns.
func (w *World) eachDecommission(fn func(archetype string, dr decommissionRun) (decommissionRun, error)) error {
	if err := w.requireArchetypes("the decommission audit"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		dr, ok := w.decommission[a]
		if !ok {
			return fmt.Errorf("archetype %s: no read-traffic baseline established; the scenario must establish one first", a)
		}
		next, err := fn(a, dr)
		if err != nil {
			return err
		}
		w.decommission[a] = next
	}
	return nil
}

// --- MIG-26 tier-1: live retention against the compliance mandate ----------

func loadComplianceMandate(path string) (time.Duration, error) {
	data, err := lib.ReadArtifact(path)
	if err != nil {
		return 0, err
	}
	var cr complianceRetention
	if err := lib.DecodeArtifact(data, &cr); err != nil {
		return 0, err
	}
	return time.Duration(cr.Metrics), nil
}

func (w *World) givenComplianceMandate() error {
	if err := w.requireArchetypes("the compliance-retention mandate"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		mandate, err := loadComplianceMandate(w.goldenPath(a, complianceRetentionFixture))
		if err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		if mandate <= 0 {
			return fmt.Errorf("archetype %s: the declared compliance-retention mandate is %s, which bounds nothing", a, mandate)
		}
		w.retentionGate[a] = retentionGateRun{mandate: mandate}
	}
	return nil
}

// whenReadLiveComplianceRetention reads the live ClickHouse retention (never
// the schema renderer's own text) and compares it against the declared
// mandate. It goes through requireLiveSignalRetention, so a read that
// established nothing is an error here and not a retention of zero the
// comparison below would silently refuse on: "we read nothing" and "what we
// read falls short" are the two answers this scenario must never conflate.
func (w *World) whenReadLiveComplianceRetention() error {
	return w.eachRetentionGate(func(a string, rg retentionGateRun) (retentionGateRun, error) {
		have, err := w.requireLiveSignalRetention(context.Background(), signalMetrics)
		if err != nil {
			return rg, fmt.Errorf("archetype %s: %w", a, err)
		}
		rg.liveProven = true
		rg.liveRetention = have
		rg.meetsMandate = have >= rg.mandate
		return rg, nil
	})
}

// thenLiveRetentionWasActuallyRead is the refusal's own precondition, asserted
// in its own right rather than assumed: the mandate comparison is only worth
// something if a real, positive retention came off the live cluster and the
// mandate it is held against is itself a real window. Without this, a harness
// that read no retention at all refuses for every possible cluster, which is
// exactly the shape this scenario used to have.
func (w *World) thenLiveRetentionWasActuallyRead() error {
	return w.eachRetentionGate(func(a string, rg retentionGateRun) (retentionGateRun, error) {
		if !rg.liveProven || rg.liveRetention <= 0 {
			return rg, fmt.Errorf("archetype %s: no live ClickHouse retention was read, so the compliance comparison rests on nothing", a)
		}
		if rg.mandate <= 0 {
			return rg, fmt.Errorf("archetype %s: the compliance mandate read %s, so nothing could fall short of it", a, rg.mandate)
		}
		return rg, nil
	})
}

func (w *World) thenLiveRetentionDoesNotProveMandate() error {
	return w.eachRetentionGate(func(a string, rg retentionGateRun) (retentionGateRun, error) {
		if !rg.liveProven {
			return rg, fmt.Errorf("archetype %s: the live retention was never read, so this gate refuses on no evidence at all", a)
		}
		if rg.meetsMandate {
			return rg, fmt.Errorf("archetype %s: the live retention now proves the compliance-retention mandate is met (%s covers %s); this scenario asserts the gate still refuses",
				a, rg.liveRetention, rg.mandate)
		}
		return rg, nil
	})
}

// eachRetentionGate runs fn over every tagged archetype's live-retention
// comparison state, writing back whatever fn returns.
func (w *World) eachRetentionGate(fn func(archetype string, rg retentionGateRun) (retentionGateRun, error)) error {
	if err := w.requireArchetypes("the live retention gate"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		rg, ok := w.retentionGate[a]
		if !ok {
			return fmt.Errorf("archetype %s: no compliance-retention mandate established; the scenario must establish one first", a)
		}
		next, err := fn(a, rg)
		if err != nil {
			return err
		}
		w.retentionGate[a] = next
	}
	return nil
}
