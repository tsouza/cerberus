package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// scrapeJob is the shared job name both prometheus.yml's self-scrape job and
// otel-collector-config.yaml's prometheusreceiver use for the SAME target
// (the reference Prometheus's own /metrics) — the "equivalent collector
// prometheusreceiver over the same targets" MIG-07's PASS assertion names.
const scrapeJob = "prometheus-self"

// scrapeReferenceLabel / scrapeCollectorLabel are the label each path selects
// the shared target BY — deliberately NOT the same name. The reference
// Prometheus's own native scrape attaches the job under the classic `job`
// label. The collector's prometheusreceiver translates the identical concept
// into OTel's semantic-convention resource attribute `service.name`
// (sanitised dot→underscore into `service_name` by cerberus's read path, per
// internal/promql/resource_attributes.go) — `job` / `instance` are never
// re-aliased back. This is a REAL, nameable translation a migrating operator
// must know about (the collector path carries no literal `job` label at all),
// not a harness inconsistency to paper over: thenScrapeSeriesParity asserts
// the difference between the two paths is EXACTLY this translation and
// nothing else.
const (
	scrapeReferenceLabel = "job"
	scrapeCollectorLabel = "service_name"
)

// scrapeReferenceInstanceLabel / scrapeCollectorInstanceLabel are the other
// half of that translation: Prometheus's `instance` becomes OTel's
// `service.instance.id`. Both paths target the same compose address
// (`prometheus:9090`, see tiers/tier1-dual/prometheus.yml), so the VALUES are
// comparable too, not just the names.
const (
	scrapeReferenceInstanceLabel = "instance"
	scrapeCollectorInstanceLabel = "service_instance_id"
)

// scrapeCollectorInstanceAttr is the same attribute as
// scrapeCollectorInstanceLabel, in the DOTTED form the collector writes into
// ClickHouse's ResourceAttributes map. cerberus sanitises it to the
// underscored label above on the read path, so a direct column probe has to
// ask for the dotted key.
const scrapeCollectorInstanceAttr = "service.instance.id"

// scrapeUpMetric, scrapeDurationMetric and scrapeSamplesMetric are the three
// meta-metrics MIG-07's PASS assertion names by name. The parity assertion
// does not stop at them — it enumerates whatever the reference path actually
// produces — but a run that failed to produce even these has nothing to
// compare, so they are required to be in the enumerated set.
const (
	scrapeUpMetric       = "up"
	scrapeDurationMetric = "scrape_duration_seconds"
	scrapeSamplesMetric  = "scrape_samples_scraped"
)

// scrapeMetaPrefix is the prefix Prometheus stamps on every scrape
// meta-metric it synthesises for a target. Together with `up` it is what
// separates the meta-metrics MIG-07 compares from the target's own exposed
// metrics, which are a much larger set that says nothing about the scrape
// loop itself.
const scrapeMetaPrefix = "scrape_"

// scrapeHistogramMetric is a real classic histogram the scrape target itself
// exposes — Prometheus's own HTTP request-duration histogram — giving MIG-07
// real `_bucket` series to prove survive the collector path in a
// histogram_quantile-consumable form.
const scrapeHistogramMetric = "prometheus_http_request_duration_seconds"

// scrapeQuantilePhi is the quantile both paths are asked for. High enough
// that a bucket layout truncated at the top edge changes the answer, which a
// median would hide.
const scrapeQuantilePhi = 0.99

// scrapeRateWindow is the range both paths' rate() covers. It spans several
// scrape intervals on either path (15s on the reference, 5s on the
// collector), so neither side is reading a single-sample window.
const scrapeRateWindow = "5m"

// scrapeSettleWait bounds how long a probe waits for a real scrape cycle to
// have landed — a scenario running right after the stack comes up can
// otherwise race the first scrape.
const scrapeSettleWait = 90 * time.Second

// scrapeState is MIG-07's scenario state: the reference path's own answers,
// carried from the step that reads them to the step that diffs against them,
// so the comparison is against a live backend rather than a constant.
type scrapeState struct {
	// refUpLabels is the reference path's own `up` label set, the oracle the
	// collector path's label vocabulary is diffed against.
	refUpLabels map[string]string
}

// registerScrapeParitySteps binds MIG-07's Given/When/Then steps.
func (w *World) registerScrapeParitySteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^scrape meta-metrics are produced under both the reference path and the collector path$`,
		w.thenScrapeMetaMetricsBothPaths)
	ctx.Step(`^every meta-metric present under the reference path is also present under the collector path$`,
		w.thenScrapeSeriesParity)
	ctx.Step(`^the labels the collector path drops are exactly the ones the named OTel translation replaces$`,
		w.thenScrapeLabelTranslationIsTheOnlyDifference)
	ctx.Step(`^the classic histogram from the scrape target yields through cerberus the quantile the reference itself yields$`,
		w.thenScrapeHistogramQuantileConsumable)
}

// promInstantSeries is one series from an instant-query result, typed just
// enough for MIG-07's assertions: the label set and the scalar value.
type promInstantSeries struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// promInstantEnvelope is the /api/v1/query response shape MIG-07 decodes.
type promInstantEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string              `json:"resultType"`
		Result     []promInstantSeries `json:"result"`
	} `json:"data"`
}

// instantQuery issues an instant /api/v1/query against base for promQuery,
// polling up to scrapeSettleWait for a non-empty result — both scrape paths
// need at least one real scrape cycle to have landed before a probe issued
// right after the stack comes up would see anything.
func instantQuery(base, promQuery string) (promInstantEnvelope, error) {
	reqURL := base + "/api/v1/query?query=" + url.QueryEscape(promQuery)
	deadline := time.Now().Add(scrapeSettleWait)
	var last promInstantEnvelope
	for {
		env, err := doInstantQuery(reqURL)
		if err == nil && env.Status == "success" && len(env.Data.Result) > 0 {
			return env, nil
		}
		if err != nil {
			last = promInstantEnvelope{}
		} else {
			last = env
		}
		if time.Now().After(deadline) {
			if err != nil {
				return promInstantEnvelope{}, fmt.Errorf(
					"migration harness: instant query %q against %s never succeeded within %s: %w",
					promQuery, base, scrapeSettleWait, err,
				)
			}
			return last, fmt.Errorf(
				"migration harness: instant query %q against %s returned zero series after %s (status %q)",
				promQuery, base, scrapeSettleWait, last.Status,
			)
		}
		time.Sleep(bridgePollInterval)
	}
}

func doInstantQuery(reqURL string) (promInstantEnvelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), faultQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: build the instant-query request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return promInstantEnvelope{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: read the instant-query response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: instant query returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	var env promInstantEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return promInstantEnvelope{}, fmt.Errorf("migration harness: decode the instant-query response: %w (body: %s)", err, string(body))
	}
	return env, nil
}

// scrapeSelector renders `<label>="<job>"`, the selector each path uses to
// address the shared target under its own real label.
func scrapeSelector(label string) string {
	return fmt.Sprintf(`{%s=%q}`, label, scrapeJob)
}

// scrapeUpHealthy is the `up` value Prometheus's own scrape-health convention
// uses for "the target answered the last scrape".
const scrapeUpHealthy = 1.0

// thenScrapeMetaMetricsBothPaths asserts the reference Prometheus's own
// self-scrape produces `up` / `scrape_duration_seconds` /
// `scrape_samples_scraped` for the shared target directly, AND that cerberus
// — reading the SAME target scraped by the collector's prometheusreceiver —
// produces the same three, both paths reporting the target healthy. Both
// sides are asserted, not just one: MIG-07's PASS bullet is explicit that
// scrape meta-metrics must be "produced under the collector path", which only
// means something checked against the reference path actually having them.
//
// It also captures the reference path's `up` label set, which the two
// following steps diff against. Reading it from the live reference rather
// than restating it here is what keeps those steps oracle-based.
func (w *World) thenScrapeMetaMetricsBothPaths() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	refSelector := scrapeSelector(scrapeReferenceLabel)
	collectorSelector := scrapeSelector(scrapeCollectorLabel)
	for _, metric := range []string{scrapeUpMetric, scrapeDurationMetric, scrapeSamplesMetric} {
		ref, err := instantQuery(w.live.PromURL, metric+refSelector)
		if err != nil {
			return fmt.Errorf("migration harness: reference path: %w", err)
		}
		collector, err := instantQuery(w.live.CerberusURL, metric+collectorSelector)
		if err != nil {
			return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
		}
		if metric != scrapeUpMetric {
			continue
		}
		refUp, err := instantSampleValue(ref)
		if err != nil {
			return fmt.Errorf("migration harness: reference path: %w", err)
		}
		gotUp, err := instantSampleValue(collector)
		if err != nil {
			return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
		}
		if refUp != scrapeUpHealthy {
			return fmt.Errorf("migration harness: reference path reports up=%v for %s, want healthy", refUp, scrapeJob)
		}
		if gotUp != scrapeUpHealthy {
			return fmt.Errorf("migration harness: collector path (through cerberus) reports up=%v for %s, want healthy", gotUp, scrapeJob)
		}
		w.scrape.refUpLabels = ref.Data.Result[0].Metric
	}
	return nil
}

// instantSampleValue parses the scalar of a single-series instant-query
// result. Prometheus wires a sample value as a STRING, so every numeric
// assertion in this file goes through a real parse — comparing the raw string
// against a literal would accept `+Inf` and reject a correct `1.0`.
func instantSampleValue(env promInstantEnvelope) (float64, error) {
	if len(env.Data.Result) == 0 {
		return 0, fmt.Errorf("query returned zero series")
	}
	raw, ok := env.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("result value is not a string scalar: %#v", env.Data.Result[0].Value[1])
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("result value %q does not parse as a number: %w", raw, err)
	}
	return v, nil
}

// metricNameLabel is the reserved label the metric name is enumerated under.
const metricNameLabel = "__name__"

// labelValuesLimit caps a metadata response body. The shared target's whole
// metric namespace is a few hundred short names, comfortably inside it.
const labelValuesLimit = 1 << 20

// fetchMatchedLabelValues GETs `/api/v1/label/<label>/values?match[]=<sel>`
// and returns the values, sorted. Both the reference Prometheus and cerberus
// implement it, which is what makes it usable as a per-path enumerator.
func fetchMatchedLabelValues(baseURL, label, selector string) ([]string, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/label/" + url.PathEscape(label) + "/values" +
		"?match[]=" + url.QueryEscape(selector)
	ctx, cancel := context.WithTimeout(context.Background(), labelValuesFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, labelValuesLimit))
	if err != nil {
		return nil, fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	var out labelValuesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s body %q: %w", target, string(body), err)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("%s returned status %q, want %q", target, out.Status, "success")
	}
	values := append([]string{}, out.Data...)
	sort.Strings(values)
	return values, nil
}

// scrapeMetaNames keeps only the scrape meta-metrics out of a path's whole
// metric-name enumeration. The target's own exposed metrics are a much larger
// set whose membership says nothing about the scrape loop.
func scrapeMetaNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == scrapeUpMetric || strings.HasPrefix(n, scrapeMetaPrefix) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// thenScrapeSeriesParity enumerates the metric NAMES each path produces for
// the shared target and requires every scrape meta-metric the reference path
// synthesises to be present under the collector path too, naming every one
// that is not.
//
// The enumeration is the point: checking three names this file already knows
// about would only re-assert what the previous step established. What the
// reference Prometheus decides to synthesise for a target is version- and
// configuration-dependent, so the reference path's own output is the oracle,
// and a meta-metric the collector's prometheusreceiver silently stops
// emitting fails here the day it happens.
func (w *World) thenScrapeSeriesParity() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	refNames, err := fetchMatchedLabelValues(w.live.PromURL, metricNameLabel, scrapeSelector(scrapeReferenceLabel))
	if err != nil {
		return fmt.Errorf("migration harness: enumerate the reference path's metric names: %w", err)
	}
	refMeta := scrapeMetaNames(refNames)
	if len(refMeta) == 0 {
		return fmt.Errorf("migration harness: the reference path enumerated %d metric names for %s but not one scrape meta-metric, so there is nothing to compare",
			len(refNames), scrapeJob)
	}
	for _, want := range []string{scrapeUpMetric, scrapeDurationMetric, scrapeSamplesMetric} {
		if !slices.Contains(refMeta, want) {
			return fmt.Errorf("migration harness: the reference path does not produce %s for %s; its meta-metrics are %v",
				want, scrapeJob, refMeta)
		}
	}

	collectorNames, err := fetchMatchedLabelValues(w.live.CerberusURL, metricNameLabel, scrapeSelector(scrapeCollectorLabel))
	if err != nil {
		return fmt.Errorf("migration harness: enumerate the collector path's metric names through cerberus: %w", err)
	}
	var missing []string
	for _, name := range refMeta {
		if !slices.Contains(collectorNames, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("migration harness: %v are produced under the reference path for %s but absent under the collector path; the collector path enumerates %v",
			missing, scrapeJob, scrapeMetaNames(collectorNames))
	}
	return nil
}

// thenScrapeLabelTranslationIsTheOnlyDifference asserts the ONE difference
// between the two paths' label vocabularies is the OTel semantic-convention
// translation named at the top of this file, and that the translation carries
// the VALUES across: the collector path's `service_name` must be what the
// reference path called `job`, and its `service_instance_id` what the
// reference path called `instance`.
//
// This is the "listed with the relabel/honor_labels reason" half of MIG-07's
// PASS bullet, discharged by naming the reason rather than by querying both
// paths under a label only one of them has. A reference-path label that is
// NOT part of the translation and does not survive to the collector path is a
// silent loss of series identity, and is listed and failed.
//
// The collector path additionally carries the prometheusreceiver's own
// scrape-target resource attributes (the target's address and scheme, whose
// attribute names track the OTel semantic conventions the collector release
// implements). Those are ADDITIVE context, not a loss, so they are not pinned
// by name here — docs/migration-testing.md section 6 records that limit.
func (w *World) thenScrapeLabelTranslationIsTheOnlyDifference() error {
	if len(w.scrape.refUpLabels) == 0 {
		return fmt.Errorf("migration harness: the reference path's label set was never captured; the meta-metric step must run first")
	}
	collector, err := instantQuery(w.live.CerberusURL, scrapeUpMetric+scrapeSelector(scrapeCollectorLabel))
	if err != nil {
		return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
	}
	got := collector.Data.Result[0].Metric

	// translated maps each reference-path label the OTel convention renames to
	// the name it arrives under on the collector path. Every OTHER
	// reference-path label must survive unchanged.
	translated := map[string]string{
		scrapeReferenceLabel:         scrapeCollectorLabel,
		scrapeReferenceInstanceLabel: scrapeCollectorInstanceLabel,
	}
	var lost []string
	for name, refValue := range w.scrape.refUpLabels {
		want := name
		if renamed, ok := translated[name]; ok {
			want = renamed
		}
		gotValue, present := got[want]
		if !present {
			lost = append(lost, fmt.Sprintf("%s (expected under %s)", name, want))
			continue
		}
		if gotValue != refValue {
			return fmt.Errorf("migration harness: the reference path's %s=%q arrives on the collector path as %s=%q — the two paths are not describing the same target",
				name, refValue, want, gotValue)
		}
	}
	if len(lost) > 0 {
		sort.Strings(lost)
		// A missing `service_name` has two very different causes and the label
		// set alone cannot tell them apart, so ask ClickHouse which one it is
		// before reporting. cerberus resolves that label from the dedicated
		// ServiceName COLUMN and, following Prometheus semantics, omits a
		// label whose value is empty — so an empty column and a dropped
		// projection look identical from the query side.
		//
		//   ServiceName empty     -> the receiver never set service.name for
		//                            this scrape, and the target identity
		//                            really does live in server_address /
		//                            server_port / url_scheme. The declared
		//                            translation is wrong; fix the harness.
		//   ServiceName populated -> the collector DID set it and cerberus's
		//                            read path is dropping it. That is a
		//                            product bug and must stay red.
		if slices.Contains(lost, fmt.Sprintf("%s (expected under %s)", scrapeReferenceLabel, scrapeCollectorLabel)) {
			probeCtx, cancel := context.WithTimeout(context.Background(), scrapeSettleWait)
			landed, probeErr := w.scrapeServiceNameColumn(probeCtx, w.scrape.refUpLabels[scrapeReferenceInstanceLabel])
			cancel()
			switch {
			case probeErr != nil:
				return fmt.Errorf("migration harness: %s is absent under the collector path and the ServiceName column could not be probed to say why: %w",
					scrapeCollectorLabel, probeErr)
			case landed != "":
				return fmt.Errorf("migration harness: ClickHouse holds ServiceName=%q for the collector path's %s, but cerberus did not project it as %s — the read path is dropping a populated service name",
					landed, scrapeUpMetric, scrapeCollectorLabel)
			}
		}
		// Report the collector path's label VALUES, not just its names. The
		// first live run showed `service_instance_id` present while
		// `service_name` was absent entirely, and the names alone cannot say
		// why: either the receiver never sets `service.name` on this metric —
		// in which case the declared job->service_name translation is simply
		// wrong — or it does set it and cerberus's read path drops it, which
		// is a real gap. The values distinguish the two. Relaxing this
		// assertion to match the observed set without knowing which would
		// bury the second case.
		return fmt.Errorf("migration harness: %v are present on the reference path's %s but absent under the collector path; the only difference the OTel translation accounts for is %s->%s and %s->%s. Collector-path labels (name=value): %v",
			lost, scrapeUpMetric,
			scrapeReferenceLabel, scrapeCollectorLabel,
			scrapeReferenceInstanceLabel, scrapeCollectorInstanceLabel,
			got)
	}
	return nil
}

// scrapeBucketQuery renders `sum by (le) (rate(<hist>_bucket{<sel>}[<win>]))`
// — the bucket-rate vector histogram_quantile consumes, and the vector whose
// `le` labels ARE the surviving bucket layout.
func scrapeBucketQuery(label string) string {
	return fmt.Sprintf("sum by (le) (rate(%s%s%s[%s]))",
		scrapeHistogramMetric, histogramBucketSuffix, scrapeSelector(label), scrapeRateWindow)
}

// scrapeQuantileQuery wraps scrapeBucketQuery in the quantile call.
func scrapeQuantileQuery(label string) string {
	return fmt.Sprintf("histogram_quantile(%v, %s)", scrapeQuantilePhi, scrapeBucketQuery(label))
}

// bucketBounds reads the `le` label off every series of a bucket-rate result
// and returns the sorted bucket edges. Parsing to float rather than comparing
// the raw strings is deliberate: the layout is the set of BOUNDARIES, and how
// a backend chooses to render `1` or `+Inf` is a formatting convention, not
// part of it.
func bucketBounds(env promInstantEnvelope) ([]float64, error) {
	out := make([]float64, 0, len(env.Data.Result))
	for _, s := range env.Data.Result {
		raw, ok := s.Metric["le"]
		if !ok {
			return nil, fmt.Errorf("a bucket series carries no le label: %v", s.Metric)
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("bucket bound %q does not parse as a number: %w", raw, err)
		}
		out = append(out, v)
	}
	sort.Float64s(out)
	return out, nil
}

// narrowestBucketWidth is the epsilon the two paths' quantiles are compared
// within, derived from the bucket layout both paths were observed to carry
// rather than guessed: the narrowest gap between adjacent finite bucket
// edges, counting the implicit zero floor as the first edge.
//
// That is the resolution histogram_quantile itself has. Two backends
// interpolating within the SAME bucket cannot differ by more than its width;
// two backends that disagree by more have landed in different buckets, which
// is the failure this assertion exists to catch. A layout that failed to
// round-trip produces NaN, an infinity or a value orders of magnitude away,
// all of which are far outside it.
func narrowestBucketWidth(bounds []float64) (float64, error) {
	prev := 0.0
	width := math.Inf(1)
	for _, b := range bounds {
		if math.IsInf(b, 1) {
			continue
		}
		if b <= prev {
			return 0, fmt.Errorf("bucket edges %v are not strictly increasing above zero", bounds)
		}
		if b-prev < width {
			width = b - prev
		}
		prev = b
	}
	if math.IsInf(width, 1) {
		return 0, fmt.Errorf("bucket edges %v carry no finite bound, so no resolution can be derived from them", bounds)
	}
	return width, nil
}

// thenScrapeHistogramQuantileConsumable asserts a real classic histogram from
// the scrape target — Prometheus's own request-duration histogram — survives
// the collector path in a form histogram_quantile can consume through
// cerberus, and that the answer AGREES with the reference Prometheus's own
// histogram_quantile over the same target.
//
// Rejecting only the literal string "NaN" would let +Inf, 0, and an
// arbitrarily wrong finite quantile through, so the value is parsed and
// checked for finiteness and positivity, then diffed against the reference's
// own answer within the resolution the shared bucket layout provides (see
// narrowestBucketWidth).
func (w *World) thenScrapeHistogramQuantileConsumable() error {
	if !w.liveSet {
		return fmt.Errorf("migration harness: the tier-1 stack has not been established; the scenario must establish it first")
	}
	refBuckets, err := instantQuery(w.live.PromURL, scrapeBucketQuery(scrapeReferenceLabel))
	if err != nil {
		return fmt.Errorf("migration harness: reference path bucket rates: %w", err)
	}
	collectorBuckets, err := instantQuery(w.live.CerberusURL, scrapeBucketQuery(scrapeCollectorLabel))
	if err != nil {
		return fmt.Errorf("migration harness: collector path bucket rates (through cerberus): %w", err)
	}
	refBounds, err := bucketBounds(refBuckets)
	if err != nil {
		return fmt.Errorf("migration harness: reference path: %w", err)
	}
	collectorBounds, err := bucketBounds(collectorBuckets)
	if err != nil {
		return fmt.Errorf("migration harness: collector path (through cerberus): %w", err)
	}
	if !equalFloat64s(refBounds, collectorBounds) {
		return fmt.Errorf("migration harness: the reference path exposes bucket edges %v for %s but the collector path exposes %v — the classic bucket layout did not survive",
			refBounds, scrapeHistogramMetric, collectorBounds)
	}
	epsilon, err := narrowestBucketWidth(refBounds)
	if err != nil {
		return fmt.Errorf("migration harness: %w", err)
	}

	refEnv, err := instantQuery(w.live.PromURL, scrapeQuantileQuery(scrapeReferenceLabel))
	if err != nil {
		return fmt.Errorf("migration harness: reference path histogram_quantile: %w", err)
	}
	collectorEnv, err := instantQuery(w.live.CerberusURL, scrapeQuantileQuery(scrapeCollectorLabel))
	if err != nil {
		return fmt.Errorf("migration harness: histogram_quantile through cerberus over the collector-scraped histogram: %w", err)
	}
	refQ, err := instantSampleValue(refEnv)
	if err != nil {
		return fmt.Errorf("migration harness: reference path histogram_quantile: %w", err)
	}
	gotQ, err := instantSampleValue(collectorEnv)
	if err != nil {
		return fmt.Errorf("migration harness: histogram_quantile through cerberus: %w", err)
	}
	for path, v := range map[string]float64{"reference": refQ, "collector (through cerberus)": gotQ} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			return fmt.Errorf("migration harness: the %s path's histogram_quantile over %s is %v — a surviving bucket layout yields a finite, positive duration",
				path, scrapeHistogramMetric, v)
		}
	}
	if math.Abs(refQ-gotQ) > epsilon {
		return fmt.Errorf("migration harness: histogram_quantile over %s is %v on the reference path and %v through cerberus, apart by more than the layout's own resolution %v — the two paths are reading different bucket layouts",
			scrapeHistogramMetric, refQ, gotQ, epsilon)
	}
	return nil
}

// scrapeServiceNameColumn reads the ServiceName column ClickHouse holds for
// the collector path's `up` row. It is the tie-breaker for a missing
// `service_name` label: cerberus omits an empty label the way Prometheus does,
// so the query side cannot distinguish "the receiver never set service.name"
// from "cerberus dropped a populated one". The column can.
//
// It reads the row the collector wrote, identified by the metric name and the
// scrape target's own instance value, so it cannot pick up the fixture's own
// seeded series.
func (w *World) scrapeServiceNameColumn(ctx context.Context, instance string) (string, error) {
	conn, err := w.bridgeCHConn(ctx)
	if err != nil {
		return "", fmt.Errorf("dial clickhouse: %w", err)
	}
	defer func() { _ = conn.Close() }()

	const q = `SELECT ServiceName FROM otel_metrics_gauge
		WHERE MetricName = ? AND ResourceAttributes[?] = ?
		ORDER BY TimeUnix DESC LIMIT 1`
	var got string
	if err := conn.QueryRow(
		ctx, q,
		scrapeUpMetric, scrapeCollectorInstanceAttr, instance,
	).Scan(&got); err != nil {
		return "", fmt.Errorf("read ServiceName for %s: %w", scrapeUpMetric, err)
	}
	return got, nil
}
