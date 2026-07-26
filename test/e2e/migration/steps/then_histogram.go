package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
)

// cumulativeTemporality is the OTLP AggregationTemporality enum value for
// CUMULATIVE — what PromQL's rate/increase/histogram_quantile all assume of
// the counters and histograms they read. Mirrors
// seed.otelCumulativeTemporality (unexported there), restated here because
// this file probes ClickHouse directly rather than through the seed
// package's fixture-insertion path.
const cumulativeTemporality int32 = 2

// registerHistogramFidelitySteps binds the MIG-12 metric-type/histogram
// fidelity steps: a direct ClickHouse table-routing and temporality probe
// for the counter and the classic histogram, plus the exponential-histogram
// quantile-estimator probe that is this lane's first declared
// tolerances/ entry.
func (w *World) registerHistogramFidelitySteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^a known exponential histogram is seeded into the live database$`, w.givenExpHistogramSeeded)
	ctx.Step(`^the counter lands in the sum table under cumulative, monotonic temporality$`, w.thenCounterTemporality)
	ctx.Step(`^the histogram lands in the histogram table under cumulative temporality$`, w.thenHistogramTemporality)
	ctx.Step(`^the reconstructed exponential-histogram quantile stays within the declared estimator tolerance of the true quantile$`,
		w.thenExpHistogramWithinTolerance)
}

// --- direct table-routing / temporality probes ----------------------------

// thenCounterTemporality asserts every tagged archetype's declared counter
// metric landed in otel_metrics_sum under cumulative, monotonic temporality —
// the shape rate()/increase() assume.
func (w *World) thenCounterTemporality() error {
	return w.eachArchetypeDeclaration(func(ctx context.Context, conn driver.Conn, a string, d seed.Declaration) error {
		const q = `SELECT AggregationTemporality, IsMonotonic FROM otel_metrics_sum WHERE MetricName = ? LIMIT 1`
		var temporality int32
		var monotonic bool
		if err := conn.QueryRow(ctx, q, d.CounterMetric).Scan(&temporality, &monotonic); err != nil {
			return fmt.Errorf("archetype %s: probe sum-table temporality for %q: %w", a, d.CounterMetric, err)
		}
		if temporality != cumulativeTemporality {
			return fmt.Errorf("archetype %s: metric %q landed in the sum table under temporality %d, want cumulative (%d)",
				a, d.CounterMetric, temporality, cumulativeTemporality)
		}
		if !monotonic {
			return fmt.Errorf("archetype %s: metric %q landed in the sum table as non-monotonic, want monotonic",
				a, d.CounterMetric)
		}
		return nil
	})
}

// thenHistogramTemporality asserts every tagged archetype's declared
// histogram metric landed in otel_metrics_histogram under cumulative
// temporality.
func (w *World) thenHistogramTemporality() error {
	return w.eachArchetypeDeclaration(func(ctx context.Context, conn driver.Conn, a string, d seed.Declaration) error {
		const q = `SELECT AggregationTemporality FROM otel_metrics_histogram WHERE MetricName = ? LIMIT 1`
		var temporality int32
		if err := conn.QueryRow(ctx, q, d.HistogramMetric).Scan(&temporality); err != nil {
			return fmt.Errorf("archetype %s: probe histogram-table temporality for %q: %w", a, d.HistogramMetric, err)
		}
		if temporality != cumulativeTemporality {
			return fmt.Errorf("archetype %s: metric %q landed in the histogram table under temporality %d, want cumulative (%d)",
				a, d.HistogramMetric, temporality, cumulativeTemporality)
		}
		return nil
	})
}

// eachArchetypeDeclaration dials the live ClickHouse once and runs fn over
// every tagged archetype's committed fixture declaration, so a probe never
// hardcodes a fixture-specific metric name that could drift from
// seed/fixture.json.
func (w *World) eachArchetypeDeclaration(fn func(ctx context.Context, conn driver.Conn, archetype string, d seed.Declaration) error) error {
	if err := w.requireArchetypes("the histogram-fidelity probe"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), livePollBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	for _, a := range w.archetypes {
		d, err := seed.LoadDeclaration(harnessPath(w.root, archetypeDir, a, "seed", "fixture.json"))
		if err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		if err := fn(ctx, conn, a, d); err != nil {
			return err
		}
	}
	return nil
}

// --- exponential-histogram quantile-estimator probe ------------------------

// expHistProbe is one archetype's exponential-histogram probe: the metric
// name the probe row was seeded under, the instant it was seeded at, and the
// true quantile computed independently of cerberus at seed time. got/gotOK
// are filled by the Then step once it has queried cerberus.
type expHistProbe struct {
	metric       string
	at           time.Time
	trueQuantile float64
	got          float64
	gotOK        bool
}

// expHistProbeMetricSuffix marks the probe's synthetic metric name with the
// suffix schema.Metrics.ExpHistogramSuffix routes onto the exponential-
// histogram table by default.
const expHistProbeMetricSuffix = "_quantile_probe_exp_hist"

// The probe's synthetic exponential histogram: base 2 (Scale=0), every
// observation folded into the SINGLE bucket (expHistProbeLow,
// expHistProbeHigh] so the estimator has to interpolate across the whole
// bucket width rather than land on a boundary the fixture happened to
// supply. PositiveOffset=6 with a one-element PositiveBucketCounts array
// places that bucket at (2^6, 2^7] = (64, 128] — see
// internal/chsql/histogram_quantile_native.go's positive-bucket formula,
// value = base^(PositiveOffset + pos + fraction) with pos=0.
const (
	expHistProbeLow            = 64.0
	expHistProbeHigh           = 128.0
	expHistProbeCount          = 1001
	expHistProbePositiveOffset = int32(6)
	expHistProbeQuantile       = 0.95
	expHistProbeScale          = int32(0)
	expHistProbeService        = "migration-probe"
	expHistProbeDescription    = "MIG-12 exponential-histogram quantile-estimator probe"
	expHistProbeUnit           = "s"
)

// buildExpHistogramProbe returns expHistProbeCount linearly-spaced
// observations across [expHistProbeLow, expHistProbeHigh], their sum (for
// the row's Sum column) and the TRUE rank-interpolated quantile at
// expHistProbeQuantile — computed directly from the raw values, independent
// of any histogram-bucket estimator, so it is a ground truth cerberus's own
// answer is measured against rather than assumed to agree with.
func buildExpHistogramProbe() (trueQuantile, sum float64) {
	values := make([]float64, expHistProbeCount)
	step := (expHistProbeHigh - expHistProbeLow) / float64(expHistProbeCount-1)
	for k := range values {
		values[k] = expHistProbeLow + float64(k)*step
		sum += values[k]
	}
	rank := expHistProbeQuantile * float64(expHistProbeCount-1)
	lo := int(rank)
	frac := rank - float64(lo)
	trueQuantile = values[lo]
	if lo+1 < len(values) {
		trueQuantile += frac * (values[lo+1] - values[lo])
	}
	return trueQuantile, sum
}

const insertExpHistProbeSQL = `INSERT INTO otel_metrics_exponential_histogram (
	ResourceAttributes, ServiceName, MetricName, MetricDescription, MetricUnit,
	Attributes, StartTimeUnix, TimeUnix, Count, Sum, Scale, ZeroCount,
	PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts,
	Flags, Min, Max, AggregationTemporality
)`

// insertExpHistogramProbe writes the single synthetic exponential-histogram
// row a MIG-12 probe reads back.
func insertExpHistogramProbe(ctx context.Context, conn driver.Conn, metric string, at time.Time, sum float64) error {
	batch, err := conn.PrepareBatch(ctx, insertExpHistProbeSQL)
	if err != nil {
		return fmt.Errorf("prepare exponential-histogram probe batch: %w", err)
	}
	const noFlags = uint32(0)
	const noZeroCount = uint64(0)
	const noNegativeOffset = int32(0)
	if err := batch.Append(
		map[string]string{"service.name": expHistProbeService},
		expHistProbeService, metric, expHistProbeDescription, expHistProbeUnit,
		map[string]string{},
		at, at, uint64(expHistProbeCount), sum,
		expHistProbeScale, noZeroCount,
		expHistProbePositiveOffset, []uint64{uint64(expHistProbeCount)},
		noNegativeOffset, []uint64{},
		noFlags, expHistProbeLow, expHistProbeHigh, cumulativeTemporality,
	); err != nil {
		return fmt.Errorf("append exponential-histogram probe row: %w", err)
	}
	return batch.Send()
}

// givenExpHistogramSeeded writes one synthetic exponential-histogram row per
// tagged archetype and records the true quantile the Then step will compare
// cerberus's live answer against.
func (w *World) givenExpHistogramSeeded() error {
	if err := w.requireArchetypes("the exponential-histogram probe"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), livePollBudget)
	defer cancel()
	conn, err := w.dialLiveCH(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	trueQuantile, sum := buildExpHistogramProbe()
	at := time.Now().UTC().Truncate(time.Second)
	for _, a := range w.archetypes {
		metric := promSafeName(a) + expHistProbeMetricSuffix
		if err := insertExpHistogramProbe(ctx, conn, metric, at, sum); err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		w.expHist[a] = expHistProbe{metric: metric, at: at, trueQuantile: trueQuantile}
	}
	return nil
}

// promSafeName rewrites an archetype name into a valid bare PromQL metric-
// name fragment: PromQL identifiers are `[a-zA-Z_:][a-zA-Z0-9_:]*`, and an
// archetype directory name (e.g. "three-signal") carries a hyphen that bare
// syntax rejects.
func promSafeName(archetype string) string {
	return strings.ReplaceAll(archetype, "-", "_")
}

// promInstantResponse is the Prometheus HTTP API's instant-query wire shape.
type promInstantResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Value []json.RawMessage `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// queryInstantScalar runs an instant PromQL query at instant `at` and
// returns its single result's float value, failing when the query does not
// resolve to exactly one series (a histogram_quantile over one probe metric
// name always does, so more or fewer is itself a failure worth naming).
func queryInstantScalar(ctx context.Context, baseURL, query string, at time.Time) (float64, error) {
	target := strings.TrimRight(baseURL, "/") + "/api/v1/query?" + url.Values{
		"query": {query},
		"time":  {strconv.FormatInt(at.Unix(), 10)},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, fmt.Errorf("build request for %s: %w", target, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, labelValuesResponseLimit))
	if err != nil {
		return 0, fmt.Errorf("read %s body: %w", target, err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("GET %s returned %d: %s", target, resp.StatusCode, string(body))
	}
	var out promInstantResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("decode %s body %q: %w", target, string(body), err)
	}
	if out.Status != "success" {
		return 0, fmt.Errorf("%s returned status %q, want %q", target, out.Status, "success")
	}
	if len(out.Data.Result) != 1 {
		return 0, fmt.Errorf("%s returned %d series, want exactly one", target, len(out.Data.Result))
	}
	value := out.Data.Result[0].Value
	if len(value) != 2 {
		return 0, fmt.Errorf("%s: sample carries %d fields, want two", target, len(value))
	}
	var raw string
	if err := json.Unmarshal(value[1], &raw); err != nil {
		return 0, fmt.Errorf("%s: decode sample value: %w", target, err)
	}
	return strconv.ParseFloat(raw, 64)
}

// thenExpHistogramWithinTolerance queries cerberus's live
// histogram_quantile over each archetype's seeded probe row and asserts the
// absolute difference from the true quantile stays within the declared,
// derived tolerances.ExpHistogramQuantileEpsilon — the estimator-epsilon
// comparison mode (docs/migration-testing.md section 5, mode 2).
func (w *World) thenExpHistogramWithinTolerance() error {
	if len(w.expHist) == 0 {
		return fmt.Errorf("no exponential-histogram probe has been seeded; the scenario must seed one first")
	}
	if err := w.requireArchetypes("the exponential-histogram quantile assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		probe, ok := w.expHist[a]
		if !ok {
			return fmt.Errorf("archetype %s has no seeded exponential-histogram probe", a)
		}
		query := fmt.Sprintf("histogram_quantile(%v, %s)", expHistProbeQuantile, probe.metric)

		deadline := time.Now().Add(livePollBudget)
		var got float64
		var lastErr error
		for {
			ctx, cancel := context.WithTimeout(context.Background(), labelValuesFetchTimeout)
			got, lastErr = queryInstantScalar(ctx, w.live.CerberusURL, query, probe.at)
			cancel()
			if lastErr == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("archetype %s: cerberus never answered the exponential-histogram probe query %q: %w",
					a, query, lastErr)
			}
			time.Sleep(livePollInterval)
		}

		probe.got, probe.gotOK = got, true
		w.expHist[a] = probe

		delta := math.Abs(got - probe.trueQuantile)
		if delta > tolerances.ExpHistogramQuantileEpsilon {
			return fmt.Errorf(
				"archetype %s: %s returned %v, want within %v of the true quantile %v (observed |diff| = %v)",
				a, query, got, tolerances.ExpHistogramQuantileEpsilon, probe.trueQuantile, delta,
			)
		}
	}
	return nil
}
