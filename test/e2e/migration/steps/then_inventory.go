package steps

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrateinventory"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// inventoryReportName is the workspace filename `cerberus migrate inventory
// --json --out` writes its artifact to.
const inventoryReportName = "inventory.json"

// inventoryTopN is the --top rank width the scenario drives the CLI with,
// matching section 6's own documented CLI example (`--top 50`) and
// migrateinventory's own DefaultTop.
const inventoryTopN = migrateinventory.DefaultTop

// statusTSDBPath mirrors migrateinventory's own unexported statusTSDBPath
// constant: the mandatory Prometheus endpoint the probe calls first. It is
// duplicated here (rather than exported from migrateinventory) because the
// value is a fixed fact of the Prometheus HTTP API, not a
// migrateinventory-internal detail — the same posture tier1_stack_test.go
// already takes with the "service_name" / "http_status_code" label names it
// names directly.
const statusTSDBPath = "/api/v1/status/tsdb"

// statusCodeLabel mirrors the fixed http_status_code label name every
// archetype's data declaration crosses its services with.
const statusCodeLabel = "http_status_code"

// inventoryState is MIG-02's one live `migrate inventory` invocation, plus
// the archetype declaration its expectations are checked against and the
// deliberately not-found probe source the negative case stands up.
type inventoryState struct {
	result      lib.Result
	inventory   migrateinventory.Inventory
	decoded     bool
	declaration seed.Declaration
	hasDecl     bool
	notFound    *httptest.Server
}

// registerInventorySteps binds MIG-02's Given/When/Then steps.
func (w *World) registerInventorySteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the seeded archetype's fixture declaration$`, w.givenSeededDeclaration)
	ctx.Step(`^a Prometheus source that returns not-found for its TSDB status endpoint$`,
		w.givenNotFoundSource)
	ctx.Step(`^the operator inventories the reference Prometheus$`, w.whenInventoryReferencePrometheus)
	ctx.Step(`^the operator inventories that source$`, w.whenInventoryNotFoundSource)
	ctx.Step(`^the inventory ranks metrics by their realized head-series cardinality$`,
		w.thenInventoryRanksByCardinality)
	ctx.Step(`^the inventory flags the high-churn label as high-churn$`, w.thenHighChurnLabelFlagged)
	ctx.Step(`^the inventory's per-metric label cardinality matches the seeded fixture$`,
		w.thenPerMetricCardinalityMatchesFixture)
	ctx.Step(`^the inventory fails with its documented source-unreachable error$`,
		w.thenInventoryFailsSourceUnreachable)
}

// givenSeededDeclaration loads the tagged archetype's seed/fixture.json data
// declaration — the same document `just migration-tier1-seed` reads — so
// later Then steps assert against declared numbers, never against the
// inventory's own reported length.
func (w *World) givenSeededDeclaration() error {
	archetype, err := w.singleArchetype("the seeded archetype's fixture declaration")
	if err != nil {
		return err
	}
	path := harnessPath(w.root, archetypeDir, archetype, "seed", "fixture.json")
	decl, err := seed.LoadDeclaration(path)
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}
	w.inventory.declaration, w.inventory.hasDecl = decl, true
	return nil
}

// singleArchetype requires the scenario to have tagged exactly one archetype.
// MIG-02 drives `cerberus migrate inventory` against the stack's ONE shared
// reference Prometheus, so — unlike MIG-16/17's "for each tagged
// archetype" loop over an independently-diffable corpus per archetype — a
// scenario naming more than one archetype here would not know which
// declaration's numbers the single live inventory is supposed to match.
func (w *World) singleArchetype(step string) (string, error) {
	if err := w.requireArchetypes(step); err != nil {
		return "", err
	}
	if len(w.archetypes) != 1 {
		return "", fmt.Errorf("%s: names %d archetypes, want exactly one", step, len(w.archetypes))
	}
	return w.archetypes[0], nil
}

// givenNotFoundSource stands up a fake Prometheus that 404s every request —
// the CLI's own documented failure contract: "a --source that 404s
// /status/tsdb exits non-zero." It is a real, separate HTTP server on a real
// loopback port — the subprocess `cerberus migrate inventory` execs reaches
// it exactly as it would a genuine unreachable Prometheus.
func (w *World) givenNotFoundSource() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusNotFound)
	}))
	w.inventory.notFound = srv
	return nil
}

// whenInventoryReferencePrometheus drives the real CLI against the live
// reference Prometheus and decodes its JSON inventory.
func (w *World) whenInventoryReferencePrometheus() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	archetype, err := w.singleArchetype("the operator inventories the reference Prometheus")
	if err != nil {
		return err
	}
	out, err := w.workPath(archetype, inventoryReportName)
	if err != nil {
		return err
	}
	res, err := w.runLive(nil, "migrate", "inventory",
		"--source", w.live.PromURL, "--top", strconv.Itoa(inventoryTopN), "--json", "--out", out)
	if err != nil {
		return err
	}
	w.inventory.result = res
	if res.ExitCode != 0 {
		return fmt.Errorf("migration harness: cerberus migrate inventory exited %d: %s",
			res.ExitCode, string(res.Stderr))
	}
	data, err := lib.ReadArtifact(out)
	if err != nil {
		return err
	}
	var inv migrateinventory.Inventory
	if err := lib.DecodeArtifact(data, &inv); err != nil {
		return err
	}
	w.inventory.inventory, w.inventory.decoded = inv, true
	return nil
}

// whenInventoryNotFoundSource drives the CLI against the not-found probe
// server the Given established, then tears the probe server down — the
// request has already completed by the time this returns, so nothing later
// in the scenario needs it alive.
func (w *World) whenInventoryNotFoundSource() error {
	if w.inventory.notFound == nil {
		return fmt.Errorf("migration harness: no not-found source established for this scenario")
	}
	sourceURL := w.inventory.notFound.URL
	res, runErr := w.runLive(nil, "migrate", "inventory",
		"--source", sourceURL, "--top", strconv.Itoa(inventoryTopN), "--json")
	w.inventory.notFound.Close()
	w.inventory.notFound = nil
	if runErr != nil {
		return runErr
	}
	w.inventory.result = res
	return nil
}

// requireDecodedInventory fails when no When step has decoded a live
// inventory yet, and returns the archetype declaration it must be checked
// against.
func (w *World) requireDecodedInventory() (seed.Declaration, error) {
	if !w.inventory.decoded {
		return seed.Declaration{}, fmt.Errorf("migration harness: no inventory was decoded for this scenario")
	}
	if !w.inventory.hasDecl {
		return seed.Declaration{}, fmt.Errorf("migration harness: no archetype fixture declaration was established for this scenario")
	}
	return w.inventory.declaration, nil
}

// thenInventoryRanksByCardinality asserts the ranking is strictly
// non-increasing by realized series count, that the fixture's own GaugeMetric
// appears with EXACTLY its declared series count (the crossJoin count plus
// the declared churn cardinality — buildChurn shares GaugeMetric's name), and
// that it outranks the fixture's OTHER two declared metrics, whose series
// counts are lower by construction. It does NOT require GaugeMetric be the
// GLOBAL top entry: this source is a real Prometheus (MIG-07 self-scrapes it
// for the very same reference Prometheus this scenario probes), and a real
// target's own built-in instrumentation (e.g.
// prometheus_http_request_duration_seconds_bucket, fanned out over method ×
// code × le) can legitimately carry more realized series than this harness's
// modest fixture — that is a genuine fact about the source, not something to
// paper over by asserting the fixture always "wins" the ranking. A ranking
// merely sorted by name, one that ranks a metric no fixture declares, or one
// that gets the fixture's own three metrics' relative order wrong, fails
// this.
func (w *World) thenInventoryRanksByCardinality() error {
	decl, err := w.requireDecodedInventory()
	if err != nil {
		return err
	}
	list := w.inventory.inventory.TopMetricsBySeries
	if len(list) == 0 {
		return fmt.Errorf("migration harness: inventory ranked no metrics by series count")
	}
	for i := 1; i < len(list); i++ {
		if list[i].Value > list[i-1].Value {
			return fmt.Errorf("migration harness: TopMetricsBySeries is not sorted descending at index %d (%d after %d)",
				i, list[i].Value, list[i-1].Value)
		}
	}
	wantSeries := int64(len(decl.Services)*len(decl.StatusCodes) + decl.ChurnCardinality)
	_, gaugeValue, ok := findNameValue(list, decl.GaugeMetric)
	if !ok {
		return fmt.Errorf("migration harness: the ranking never names the declared gauge metric %q", decl.GaugeMetric)
	}
	if gaugeValue != wantSeries {
		return fmt.Errorf("migration harness: %s ranked at %d series, want declared %d", decl.GaugeMetric, gaugeValue, wantSeries)
	}
	for _, sibling := range []string{decl.CounterMetric, decl.HistogramMetric} {
		if _, siblingValue, ok := findNameValue(list, sibling); ok && siblingValue >= gaugeValue {
			return fmt.Errorf("migration harness: %s (%d series) does not outrank its own sibling %s (%d series)",
				decl.GaugeMetric, gaugeValue, sibling, siblingValue)
		}
	}
	return nil
}

// thenHighChurnLabelFlagged asserts the archetype's declared churn label
// carries EXACTLY its declared ChurnCardinality (not merely a non-zero
// value), and that it outranks the fixture's OTHER declared dimensional
// label (statusCodeLabel), mirroring thenInventoryRanksByCardinality's
// "outranks its own siblings" check. It does NOT require the churn label be
// the GLOBAL top-ranked label: this source is a real Prometheus (MIG-07
// self-scrapes this very reference Prometheus), and a real target's own
// built-in instrumentation carries several structural labels
// (`__name__`, `le`, `handler`, `quantile`, …) whose reported "cardinality"
// reflects how many distinct internal metrics/histograms/API handlers the
// PROCESS itself exposes, not a dimensional tag an operator declared in
// their own business logic. Excluding an ever-growing list of such names by
// hand would be exactly the allow-list this project's honesty rules forbid;
// comparing the churn label only against the fixture's OWN other declared
// dimension sidesteps every such structural confounder without naming one.
func (w *World) thenHighChurnLabelFlagged() error {
	decl, err := w.requireDecodedInventory()
	if err != nil {
		return err
	}
	if decl.ChurnLabel == "" {
		return fmt.Errorf("migration harness: the archetype declares no churn_label; there is nothing to assert as high-churn")
	}
	list := w.inventory.inventory.TopLabelsByValues
	if len(list) == 0 {
		return fmt.Errorf("migration harness: inventory ranked no labels by value cardinality")
	}
	_, churnValue, ok := findNameValue(list, decl.ChurnLabel)
	if !ok {
		return fmt.Errorf("migration harness: the inventory's label-cardinality ranking never names the declared churn label %q", decl.ChurnLabel)
	}
	wantValue := int64(decl.ChurnCardinality)
	if churnValue != wantValue {
		return fmt.Errorf("migration harness: churn label %s ranked at %d, want declared %d", decl.ChurnLabel, churnValue, wantValue)
	}
	if _, siblingValue, ok := findNameValue(list, statusCodeLabel); ok && siblingValue >= churnValue {
		return fmt.Errorf("migration harness: churn label %s (%d) does not outrank the fixture's other declared dimension %s (%d)",
			decl.ChurnLabel, churnValue, statusCodeLabel, siblingValue)
	}
	return nil
}

// thenPerMetricCardinalityMatchesFixture asserts the inventory's reported
// label-value cardinality for the archetype's status-code dimension equals
// exactly len(d.StatusCodes) — the real cross-join width, not a guess.
func (w *World) thenPerMetricCardinalityMatchesFixture() error {
	decl, err := w.requireDecodedInventory()
	if err != nil {
		return err
	}
	_, value, ok := findNameValue(w.inventory.inventory.TopLabelsByValues, statusCodeLabel)
	if !ok {
		return fmt.Errorf("migration harness: the inventory's label-cardinality ranking never names %q", statusCodeLabel)
	}
	want := int64(len(decl.StatusCodes))
	if value != want {
		return fmt.Errorf("migration harness: %s cardinality = %d, want declared %d", statusCodeLabel, value, want)
	}
	return nil
}

// thenInventoryFailsSourceUnreachable asserts the CLI's real, documented
// hard-error path: a non-zero exit carrying the exact wire error
// migrateinventory's getOK emits for a non-200 response — "GET <path>: HTTP
// <code>" — not merely "some error happened."
func (w *World) thenInventoryFailsSourceUnreachable() error {
	res := w.inventory.result
	if res.ExitCode == 0 {
		return fmt.Errorf(
			"migration harness: cerberus migrate inventory exited 0 against a source that 404s its TSDB status endpoint",
		)
	}
	want := fmt.Sprintf("GET %s?limit=%d: HTTP %d", statusTSDBPath, inventoryTopN, http.StatusNotFound)
	if !bytes.Contains(res.Stderr, []byte(want)) {
		return fmt.Errorf("migration harness: stderr does not carry the documented source-unreachable error %q: %s",
			want, string(res.Stderr))
	}
	return nil
}

// findNameValue finds name's entry in a NameValue ranking, returning its
// rank index and value.
func findNameValue(list []migrateinventory.NameValue, name string) (int, int64, bool) {
	for i, nv := range list {
		if nv.Name == name {
			return i, nv.Value, true
		}
	}
	return -1, 0, false
}
