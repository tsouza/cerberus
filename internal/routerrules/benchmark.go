package routerrules

import (
	"math"
	"math/rand"
	"sort"
)

// This file builds a SYNTHETIC, LABELED ground-truth benchmark corpus for
// measuring the router-rules catalog's DETECTION FIDELITY — quantitatively, with
// precision / recall / F1 per rule, rather than the binary fire/no-fire
// assertions of the effectiveness fixture. The corpus labels are derived from the
// same p95-watermark model the rules' thresholds resolve against, so the score
// measures how consistently the rules detect what the model says they should (and
// pins that against regressions); it is NOT a measurement of real-world rule
// effectiveness against production incidents.
//
// Every value here is FABRICATED. No production-identifying number, shape id,
// or deployment name appears: the corpus is synthesised from a seeded PRNG over
// a deliberately shaped distribution (a healthy, PromQL-dominant, range-heavy
// majority on route A plus a planted failure surface), so the benchmark is
// fully reproducible and carries zero prod grounding.
//
// The catalog ships number-free; this generator and the metrics harness are
// (test-adjacent) tooling, where concrete numbers are not only allowed but
// necessary — you cannot measure detection on a corpus that has no numbers.
//
// Ground truth is expressed at the CLASS level, because the rules fire per
// group_by class after the min_support floor. Each generated class is either a
// planted pathology (it is the textbook positive for one or more rules) or a
// healthy class (negative for every rule). benchClass.expect names the rule ids
// that SHOULD fire on the class under the benchmark's nominal config; the
// metrics harness compares that ground truth against the rules that actually
// fired.

// PathologySeverity grades how far a planted pathology sits past the detection
// boundary, so the degradation sweep can ask "do we still catch the marginal
// cases as the watermark tightens, or only the egregious ones?".
type PathologySeverity uint8

const (
	// SevHealthy is a negative class: no rule should fire on it.
	SevHealthy PathologySeverity = iota
	// SevMarginal sits just past the firing boundary — the hardest true
	// positive to keep catching as thresholds tighten.
	SevMarginal
	// SevSevere sits far past the boundary — the easy, unambiguous positive.
	SevSevere
)

func (s PathologySeverity) String() string {
	switch s {
	case SevSevere:
		return "severe"
	case SevMarginal:
		return "marginal"
	default:
		return "healthy"
	}
}

// BenchRow is one synthetic corpus row. It mirrors jsonlRow's columns exactly so
// it folds through the same corpusRow path both backends consume, with no new
// decode surface.
type BenchRow struct {
	ShapeID             string
	Language            string
	NormalizedQueryHash uint64
	NAnchors            float64
	Fanout              float64
	CumulativeD         float64
	OuterRange          float64
	Step                float64
	Route               string
	KShards             float64
	DecisionReason      string
	ReadRows            float64
	ReadBytes           float64
	QueryDurationMS     float64
	MemoryUsage         float64
	ExitStatus          string
}

func (r BenchRow) toCorpusRow() corpusRow {
	return corpusRow{
		numeric: map[string]float64{
			"n_anchors":             r.NAnchors,
			"fanout":                r.Fanout,
			"cumulative_d":          r.CumulativeD,
			"outer_range":           r.OuterRange,
			"step":                  r.Step,
			"k_shards":              r.KShards,
			"read_rows":             r.ReadRows,
			"read_bytes":            r.ReadBytes,
			"query_duration_ms":     r.QueryDurationMS,
			"memory_usage":          r.MemoryUsage,
			"normalized_query_hash": float64(r.NormalizedQueryHash),
		},
		str: map[string]string{
			"shape_id":              r.ShapeID,
			"language":              r.Language,
			"route":                 r.Route,
			"decision_reason":       r.DecisionReason,
			"exit_status":           r.ExitStatus,
			"normalized_query_hash": formatNumeric(float64(r.NormalizedQueryHash)),
		},
	}
}

// LabeledClass is one ground-truth class: its identifying group-key dimensions,
// the rule ids that SHOULD fire on it, and its planted severity.
type LabeledClass struct {
	ShapeID        string
	Language       string
	DecisionReason string
	// QueryHash distinguishes the slow-shape classes, which group_by
	// normalized_query_hash rather than shape_id.
	QueryHash uint64
	Expect    []string
	Severity  PathologySeverity
}

// BenchCorpus is a generated corpus plus its class-level ground truth.
type BenchCorpus struct {
	Rows    []BenchRow
	Classes []LabeledClass
}

// AsCorpusSource exposes the in-memory rows through the CorpusSource seam the
// evaluator consumes, with no JSONL round-trip.
func (c *BenchCorpus) AsCorpusSource() CorpusSource {
	return &memCorpusSource{rows: c.Rows}
}

// BenchParams tunes the generated distribution. Zero values fall back to the
// nominal benchmark shape via withDefaults.
type BenchParams struct {
	Seed int64
	// HealthyClassesPerLang is how many healthy (negative) classes to plant per
	// language, sized so the healthy bulk dominates a realistic deployment.
	HealthyClassesPerLang int
	// HealthyClassSize is the row count per healthy class.
	HealthyClassSize int
	// PathologyPrevalence scales the row count of every planted pathology class
	// (1.0 = nominal). Below 1.0 starves marginal classes toward the
	// min_support floor, modelling a rare-pathology deployment.
	PathologyPrevalence float64
	// MinSupport is the min_rows_per_class the benchmark resolves the catalog
	// at; it must match the harness config so the ground-truth row counts are
	// sized correctly relative to the floor.
	MinSupport int
}

const (
	// benchNominalHealthyPerLang / benchNominalHealthyClassSize shape a healthy
	// majority broad enough that a per-language watermark is learned from a real
	// distribution rather than a handful of rows.
	benchNominalHealthyPerLang   = 6
	benchNominalHealthyClassSize = 30
	// benchNominalMinSupport is the firing floor the nominal benchmark uses; it
	// matches benchConfig in the harness.
	benchNominalMinSupport = 5
	// benchSeverePathologySize / benchMarginalPathologySize size planted
	// pathology classes comfortably and barely above the min-support floor.
	benchSeverePathologySize   = 10
	benchMarginalPathologySize = benchNominalMinSupport + 1
	// minPathologyClassRows is the existence floor for a marginal class. At low
	// prevalence a marginal pathology is deliberately starved below the support
	// floor (so it stops firing and the prevalence axis can move marginal recall),
	// but it must keep at least one row so the class is still present in the corpus
	// to be scored as a false negative rather than vanishing entirely.
	minPathologyClassRows = 1

	// benchMemoryHardCapBytes is the deployment query memory cap the benchmark
	// scores at (query.max_memory_bytes), and benchMemoryNearCapFraction is the
	// fraction of it route_a_memory_near_cap gates on. Their product is the
	// near-cap line the planted memory pathologies must clear. These are benchmark
	// config values an operator overrides at runtime, not catalog thresholds.
	benchMemoryHardCapBytes    = 1_073_741_824.0 // 1 GiB
	benchMemoryHardCapStr      = "1073741824"
	benchMemoryNearCapFraction = "0.8"
)

func (p BenchParams) withDefaults() BenchParams {
	if p.HealthyClassesPerLang == 0 {
		p.HealthyClassesPerLang = benchNominalHealthyPerLang
	}
	if p.HealthyClassSize == 0 {
		p.HealthyClassSize = benchNominalHealthyClassSize
	}
	if p.PathologyPrevalence == 0 {
		p.PathologyPrevalence = 1
	}
	if p.MinSupport == 0 {
		p.MinSupport = benchNominalMinSupport
	}
	return p
}

// Healthy-distribution magnitude anchors. These set the BODY of each healthy
// per-language distribution; planted pathologies are placed deliberately above
// (or, for the overshard/regret cases, below) the watermark these bodies imply.
// They are ordinary corpus data values, not catalog thresholds.
const (
	healthyMemBase    = 20_000_000.0 // ~20 MB healthy peak memory
	healthyMemSpread  = 60_000_000.0
	healthyDurBase    = 40.0 // ms
	healthyDurSpread  = 200.0
	healthyReadBase   = 50_000.0 // rows scanned
	healthyReadSpread = 400_000.0
	healthyDBase      = 30.0 // cumulative_d spine lookback
	healthyDSpread    = 90.0
	healthyFanBase    = 4.0
	healthyFanSpread  = 8.0

	// Pathology magnitudes, placed past the p95 tail the healthy body implies.
	// Severe sits far past; marginal sits just past.
	//
	// route_a_memory_near_cap now gates on a fraction of the CONFIGURED cap
	// (benchMemoryHardCapBytes), not the corpus p95, so both near-cap pathology
	// magnitudes must clear benchMemoryNearCapThreshold. Severe sits near the cap
	// itself; marginal sits just over the near-cap line.
	sevMemSevere = 0.93 * benchMemoryHardCapBytes // ~93% of the cap
	sevMemMarg   = 0.83 * benchMemoryHardCapBytes // just over the 0.8 near-cap line
	sevDurSevere = 9_000.0
	sevDurMarg   = 280.0
	// sevDurMargSlow is the marginal slow-shape duration: it sits in the band
	// between the p95 (~886 ms) and p99 (~9 s) duration watermarks, so the slow
	// rule catches it at the nominal 0.95 watermark but loses it as the watermark
	// tightens to 0.99 — the percentile-gated marginal-recall sensitivity the
	// degradation sweep asserts (route_a_memory_near_cap no longer provides it now
	// that it is cap-relative, not p95-relative).
	sevDurMargSlow = 1_500.0
	sevReadSevere  = 8_000_000.0
	sevReadMarg    = 520_000.0
	sevDSevere     = 400.0
	sevDMarg       = 130.0
	// Fan-out positives must clear the route-B fanout floor (= routeBFloorFanout,
	// the p95 of the constant-fan-out route-B seed). Severe is far past; marginal
	// is just past, to probe retention as the floor moves.
	sevFanSevere = 120.0
	sevFanMarg   = 72.0

	// Route-B regret class: finishes fast and low-fanout on route B, so it sits
	// BELOW both the slow watermark and the route-B fanout floor.
	regretFanout = 2.0
	regretDur    = 30.0
	regretShards = 6.0

	// Route-B floor seed: route-B healthy rows establish the fan-out floor the
	// shard rules gate on. Their fan-out is a single high constant (route B is
	// where big, properly-sharded fan-outs land), so it defines the p95 floor AND
	// every seed row sits AT it — and the overshard rule's strict `fanout < floor`
	// is false for them, so a well-sharded route-B class is not mistaken for
	// regret. The genuine overshard class plants its own far-lower fan-out.
	routeBFloorFanout    = 70.0
	routeBFloorDurBase   = 600.0
	routeBFloorDurSpread = 400.0
)

// languages are the three heads, weighted PromQL-dominant to match a realistic
// query mix (the healthy generator plants more promql classes).
var benchLanguages = []string{"promql", "logql", "traceql"}

// GenerateBenchCorpus builds a deterministic labeled corpus from p. The same
// seed yields byte-identical rows, so the metrics are reproducible.
func GenerateBenchCorpus(p BenchParams) *BenchCorpus {
	p = p.withDefaults()
	rng := rand.New(rand.NewSource(p.Seed)) //nolint:gosec // deterministic fixture PRNG, not security.
	bc := &BenchCorpus{}

	plantHealthy(bc, rng, p)
	plantRouteBFloor(bc, rng, p)
	plantPathologies(bc, rng, p)

	// Deterministic ordering so a chdb re-insert and the in-Go scan see the same
	// row sequence (quantileExact is order-independent, but stability avoids
	// surprises in any order-sensitive downstream).
	sortBenchRows(bc.Rows)
	sort.Slice(bc.Classes, func(i, j int) bool {
		return classID(bc.Classes[i]) < classID(bc.Classes[j])
	})
	return bc
}

// plantHealthy fills the negative majority: route-A, exit ok, every numeric
// feature drawn from the body of its per-language distribution (strictly below
// the p95 tail). No rule should fire on any of these.
func plantHealthy(bc *BenchCorpus, rng *rand.Rand, p BenchParams) {
	for _, lang := range benchLanguages {
		n := p.HealthyClassesPerLang
		if lang != "promql" {
			n = max(1, n/2) // promql-dominant mix
		}
		for c := 0; c < n; c++ {
			shape := lang + ":healthy_" + itoaLocal(c)
			hash := uint64(1000 + len(bc.Classes))
			for i := 0; i < p.HealthyClassSize; i++ {
				bc.Rows = append(bc.Rows, BenchRow{
					ShapeID:             shape,
					Language:            lang,
					NormalizedQueryHash: hash,
					NAnchors:            jitter(rng, 1, 5),
					Fanout:              jitter(rng, healthyFanBase, healthyFanSpread),
					CumulativeD:         jitter(rng, healthyDBase, healthyDSpread),
					OuterRange:          jitter(rng, 300, 3600),
					Step:                jitter(rng, 15, 60),
					Route:               "A",
					KShards:             0,
					DecisionReason:      "below-threshold",
					ReadRows:            jitter(rng, healthyReadBase, healthyReadSpread),
					ReadBytes:           jitter(rng, 1_000_000, 40_000_000),
					QueryDurationMS:     jitter(rng, healthyDurBase, healthyDurSpread),
					MemoryUsage:         jitter(rng, healthyMemBase, healthyMemSpread),
					ExitStatus:          "ok",
				})
			}
			bc.Classes = append(bc.Classes, LabeledClass{
				ShapeID: shape, Language: lang, DecisionReason: "below-threshold",
				QueryHash: hash, Expect: nil, Severity: SevHealthy,
			})
		}
	}
}

// plantRouteBFloor seeds healthy route-B classes whose high fan-out establishes
// the route-B fanout floor the shard rules learn. These are negative classes
// (exit ok, finishing reasonably), but with fan-out characteristic of route B.
func plantRouteBFloor(bc *BenchCorpus, rng *rand.Rand, p BenchParams) {
	for _, lang := range []string{"promql", "logql"} {
		shape := lang + ":routeb_healthy"
		hash := uint64(2000 + len(bc.Classes))
		for i := 0; i < p.HealthyClassSize; i++ {
			bc.Rows = append(bc.Rows, BenchRow{
				ShapeID:             shape,
				Language:            lang,
				NormalizedQueryHash: hash,
				Fanout:              routeBFloorFanout,
				CumulativeD:         jitter(rng, healthyDBase, healthyDSpread),
				Route:               "B",
				KShards:             jitter(rng, 4, 8),
				DecisionReason:      "sliceable",
				ReadRows:            jitter(rng, healthyReadBase, healthyReadSpread),
				ReadBytes:           jitter(rng, 1_000_000, 40_000_000),
				QueryDurationMS:     jitter(rng, routeBFloorDurBase, routeBFloorDurSpread),
				MemoryUsage:         jitter(rng, healthyMemBase, healthyMemSpread),
				ExitStatus:          "ok",
			})
		}
		bc.Classes = append(bc.Classes, LabeledClass{
			ShapeID: shape, Language: lang, DecisionReason: "sliceable",
			QueryHash: hash, Expect: nil, Severity: SevHealthy,
		})
	}
}

// pathologySpec declares one planted-pathology class: how to fill a row, and
// which rules should fire on it. Each spec is emitted at both severe and
// marginal magnitudes (where a magnitude axis exists) so the sweep can measure
// marginal-case retention separately.
func plantPathologies(bc *BenchCorpus, rng *rand.Rand, p BenchParams) {
	specs := pathologySpecs()
	for _, sp := range specs {
		for _, sev := range sp.severities {
			size := pathologySize(sev, p)
			if size <= 0 {
				continue
			}
			shape := sp.shape
			if len(sp.severities) > 1 {
				shape = sp.shape + "_" + sev.String()
			}
			hash := sp.hashBase + uint64(sev)
			for i := 0; i < size; i++ {
				row := sp.fill(rng, sev)
				row.ShapeID = shape
				row.Language = sp.lang
				row.NormalizedQueryHash = hash
				bc.Rows = append(bc.Rows, row)
			}
			bc.Classes = append(bc.Classes, LabeledClass{
				ShapeID:        shape,
				Language:       sp.lang,
				DecisionReason: sp.fill(rng, sev).DecisionReason,
				QueryHash:      hash,
				Expect:         sp.expect(sev),
				Severity:       sev,
			})
		}
	}
}

// pathologySize scales a planted class by prevalence and severity. The two
// severities are clamped differently ON PURPOSE so the prevalence axis can
// actually move a metric:
//
//   - SEVERE classes keep a floor of min_support+1, so a severe pathology always
//     clears the firing floor at every prevalence. That is what makes the
//     SevereRecall=1.0 contract hold across the whole sweep grid (a severe class
//     is, by construction, never lost merely because the deployment is rare).
//   - MARGINAL classes get only a one-row existence floor, so prevalence < 1
//     genuinely starves them below the support floor and they stop firing. That
//     is the modelled behaviour — a rare marginal pathology has too few rows to
//     learn from — and it is what gives the prevalence curve a metric to degrade
//     (marginal recall), instead of the old clamp that pinned every prevalence
//     to an identical, inert corpus.
func pathologySize(sev PathologySeverity, p BenchParams) int {
	base := benchSeverePathologySize
	floor := p.MinSupport + 1
	if sev == SevMarginal {
		base = p.MinSupport + 1
		floor = minPathologyClassRows
	}
	scaled := int(math.Round(float64(base) * p.PathologyPrevalence))
	if scaled < floor {
		scaled = floor
	}
	return scaled
}

// pathologySpec is the declarative shape of one planted pathology family. The
// expected-rule set is a function of severity, because a self-relative tail
// detector (geometry, fan-out) fires on a class only when THAT class crosses
// its own watermark: a marginal-on-memory OOM is not necessarily in the
// geometry tail, so its label must not claim the geometry rule.
type pathologySpec struct {
	shape      string
	lang       string
	hashBase   uint64
	expect     func(sev PathologySeverity) []string
	severities []PathologySeverity
	fill       func(rng *rand.Rand, sev PathologySeverity) BenchRow
}

// always returns an expect-func that yields the same rule set at every severity.
func always(rules ...string) func(PathologySeverity) []string {
	return func(PathologySeverity) []string { return rules }
}

// pathologySpecs enumerates one planted family per detectable signal, each
// labeled with exactly the rules that should fire on it. The expected rule sets
// encode the catalog's intended semantics (e.g. an OOM on route A fires the
// route-A OOM rule AND the route-agnostic failure-cluster + heavy-geometry
// generalizers when its geometry is in the tail). It is split into the
// hard-failure / rejection families and the self-relative tail detectors so each
// half stays readable.
func pathologySpecs() []pathologySpec {
	return append(pathologyFailureSpecs(), pathologyTailSpecs()...)
}

// pathologyFailureSpecs are the families driven by a non-ok exit_status (OOM,
// timeout, sample-budget, breaker, rejected) plus the route-B-still-failing
// guardrail.
func pathologyFailureSpecs() []pathologySpec {
	return []pathologySpec{
		// Route-A OOM. oom_on_route_a + failure_cluster_by_reason fire on any
		// route-A OOM regardless of geometry; heavy_shape_geometry_failing fires
		// only when cumulative_d is in the per-language tail, so it is expected at
		// SEVERE (d in tail) but NOT at MARGINAL (d in the healthy body).
		{
			shape: "prom:oom_heavy", lang: "promql", hashBase: 30000,
			expect: func(sev PathologySeverity) []string {
				base := []string{"oom_on_route_a", "failure_cluster_by_reason"}
				if sev == SevSevere {
					return append(base, "heavy_shape_geometry_failing")
				}
				return base
			},
			severities: []PathologySeverity{SevSevere, SevMarginal},
			fill: func(rng *rand.Rand, sev PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "oom", DecisionReason: "high-cardinality",
					MemoryUsage: pick(sev, sevMemSevere, sevMemMarg),
					CumulativeD: pick(sev, sevDSevere, sevDMarg),
					NAnchors:    jitter(rng, 5, 20), Fanout: jitter(rng, 20, 40),
					ReadRows: jitter(rng, 1_000_000, 4_000_000),
				}
			},
		},
		// Route-A timeout. route_a_timeout_should_shard + failure_cluster_by_reason
		// fire on any route-A timeout. The SEVERE variant is also in the geometry
		// tail (heavy_shape_geometry_failing) AND in the duration tail at 9s
		// (route_a_slow_hot_shape, which gates only on route-A duration). The
		// MARGINAL variant clears neither tail, so it expects only the two
		// status-driven rules.
		{
			shape: "log:timeout_heavy", lang: "logql", hashBase: 31000,
			expect: func(sev PathologySeverity) []string {
				base := []string{"route_a_timeout_should_shard", "failure_cluster_by_reason"}
				if sev == SevSevere {
					return append(base, "heavy_shape_geometry_failing", "route_a_slow_hot_shape")
				}
				return base
			},
			severities: []PathologySeverity{SevSevere, SevMarginal},
			fill: func(rng *rand.Rand, sev PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "timeout", DecisionReason: "high-cardinality",
					QueryDurationMS: pick(sev, sevDurSevere, sevDurMarg),
					CumulativeD:     pick(sev, sevDSevere, sevDMarg),
					NAnchors:        jitter(rng, 5, 20), Fanout: jitter(rng, 20, 40),
					MemoryUsage: jitter(rng, healthyMemBase, healthyMemSpread),
				}
			},
		},
		// Route-A sample-budget: route_a_hit_sample_budget +
		// cerberus_side_rejection_pressure.
		{
			shape: "prom:topk_budget", lang: "promql", hashBase: 32000,
			expect:     always("route_a_hit_sample_budget", "cerberus_side_rejection_pressure"),
			severities: []PathologySeverity{SevSevere},
			fill: func(rng *rand.Rand, _ PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "sample_budget", DecisionReason: "scalar-heavy",
					ReadRows: jitter(rng, 2_000_000, 6_000_000), Fanout: jitter(rng, 10, 30),
				}
			},
		},
		// Cerberus-side breaker: cerberus_side_rejection_pressure only.
		{
			shape: "trc:breaker", lang: "traceql", hashBase: 33000,
			expect:     always("cerberus_side_rejection_pressure"),
			severities: []PathologySeverity{SevSevere},
			fill: func(rng *rand.Rand, _ PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "breaker", DecisionReason: "scalar-heavy",
					ReadRows: jitter(rng, 100_000, 500_000),
				}
			},
		},
		// Explicit rejected: cerberus_side_rejection_pressure only.
		{
			shape: "prom:rejected", lang: "promql", hashBase: 34000,
			expect:     always("cerberus_side_rejection_pressure"),
			severities: []PathologySeverity{SevSevere},
			fill: func(rng *rand.Rand, _ PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "rejected", DecisionReason: "scalar-heavy",
					ReadRows: jitter(rng, 100_000, 500_000),
				}
			},
		},
		// Route-B still failing: route_b_still_failing +
		// failure_cluster_by_reason + heavy_shape_geometry_failing.
		{
			shape: "prom:routeb_fail", lang: "promql", hashBase: 35000,
			expect:     always("route_b_still_failing", "failure_cluster_by_reason", "heavy_shape_geometry_failing"),
			severities: []PathologySeverity{SevSevere},
			fill: func(rng *rand.Rand, _ PathologySeverity) BenchRow {
				return BenchRow{
					Route: "B", ExitStatus: "oom", DecisionReason: "not-sliceable",
					KShards: jitter(rng, 8, 32), CumulativeD: sevDSevere,
					MemoryUsage: pick(SevSevere, sevMemSevere, sevMemMarg),
					NAnchors:    jitter(rng, 5, 20), Fanout: jitter(rng, 20, 40),
				}
			},
		},
	}
}

// pathologyTailSpecs are the self-relative tail detectors: a class flagged for
// sitting in the top (or, for overshard regret, the bottom) of its own
// distribution rather than for a hard failure.
func pathologyTailSpecs() []pathologySpec {
	return []pathologySpec{
		// Route-B overshard regret: route_b_overshard_low_fanout only.
		{
			shape: "prom:overshard", lang: "promql", hashBase: 36000,
			expect:     always("route_b_overshard_low_fanout"),
			severities: []PathologySeverity{SevSevere},
			fill: func(rng *rand.Rand, _ PathologySeverity) BenchRow {
				return BenchRow{
					Route: "B", ExitStatus: "ok", DecisionReason: "sliceable",
					Fanout: regretFanout, QueryDurationMS: regretDur, KShards: regretShards,
					CumulativeD: jitter(rng, healthyDBase, healthyDSpread),
					MemoryUsage: jitter(rng, healthyMemBase, healthyMemSpread),
					ReadRows:    jitter(rng, healthyReadBase, healthyReadSpread),
				}
			},
		},
		// High-fanout route-A: route_a_high_fanout_should_shard only.
		{
			shape: "prom:hot_fanout", lang: "promql", hashBase: 37000,
			expect:     always("route_a_high_fanout_should_shard"),
			severities: []PathologySeverity{SevSevere, SevMarginal},
			fill: func(rng *rand.Rand, sev PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "ok", DecisionReason: "below-threshold",
					Fanout:          pick(sev, sevFanSevere, sevFanMarg),
					CumulativeD:     jitter(rng, healthyDBase, healthyDSpread),
					MemoryUsage:     jitter(rng, healthyMemBase, healthyMemSpread),
					QueryDurationMS: jitter(rng, healthyDurBase, healthyDurSpread),
					ReadRows:        jitter(rng, healthyReadBase, healthyReadSpread),
				}
			},
		},
		// Memory near cap (healthy but in the per-language memory tail):
		// route_a_memory_near_cap only.
		{
			shape: "prom:mem_near_cap", lang: "promql", hashBase: 38000,
			expect:     always("route_a_memory_near_cap"),
			severities: []PathologySeverity{SevSevere, SevMarginal},
			fill: func(rng *rand.Rand, sev PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "ok", DecisionReason: "below-threshold",
					MemoryUsage:     pick(sev, sevMemSevere, sevMemMarg),
					Fanout:          jitter(rng, healthyFanBase, healthyFanSpread),
					CumulativeD:     jitter(rng, healthyDBase, healthyDSpread),
					QueryDurationMS: jitter(rng, healthyDurBase, healthyDurSpread),
					ReadRows:        jitter(rng, healthyReadBase, healthyReadSpread),
				}
			},
		},
		// Slow hot shape (route A, in the per-language duration tail):
		// route_a_slow_hot_shape only. Grouped by normalized_query_hash.
		{
			shape: "prom:slow_hot", lang: "promql", hashBase: 39000,
			expect:     always("route_a_slow_hot_shape"),
			severities: []PathologySeverity{SevSevere, SevMarginal},
			fill: func(rng *rand.Rand, sev PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "ok", DecisionReason: "below-threshold",
					QueryDurationMS: pick(sev, sevDurSevere, sevDurMargSlow),
					MemoryUsage:     jitter(rng, healthyMemBase, healthyMemSpread),
					Fanout:          jitter(rng, healthyFanBase, healthyFanSpread),
					CumulativeD:     jitter(rng, healthyDBase, healthyDSpread),
					ReadRows:        jitter(rng, healthyReadBase, healthyReadSpread),
				}
			},
		},
		// Read-amplification (experimental rule): read_amplification_hot_shape.
		{
			shape: "prom:read_amp", lang: "promql", hashBase: 40000,
			expect:     always("read_amplification_hot_shape"),
			severities: []PathologySeverity{SevSevere},
			fill: func(rng *rand.Rand, _ PathologySeverity) BenchRow {
				return BenchRow{
					Route: "A", ExitStatus: "ok", DecisionReason: "below-threshold",
					ReadRows:        sevReadSevere,
					ReadBytes:       jitter(rng, 100_000_000, 400_000_000),
					MemoryUsage:     jitter(rng, healthyMemBase, healthyMemSpread),
					Fanout:          jitter(rng, healthyFanBase, healthyFanSpread),
					CumulativeD:     jitter(rng, healthyDBase, healthyDSpread),
					QueryDurationMS: jitter(rng, healthyDurBase, healthyDurSpread),
				}
			},
		},
	}
}

// pick chooses the severe or marginal magnitude for a severity.
func pick(sev PathologySeverity, severe, marginal float64) float64 {
	if sev == SevMarginal {
		return marginal
	}
	return severe
}

// jitter draws a value in [base, base+spread) deterministically from rng,
// rounded to an integer (corpus columns are integer-typed in the CH DDL).
func jitter(rng *rand.Rand, base, spread float64) float64 {
	return math.Round(base + rng.Float64()*spread)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// classID is a stable identifier for a labeled class across its group_by
// dimensions, used to order classes and to look one up from a fired finding.
func classID(c LabeledClass) string {
	return c.Language + "|" + c.ShapeID + "|" + c.DecisionReason + "|" + formatNumeric(float64(c.QueryHash))
}

func sortBenchRows(rows []BenchRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.ShapeID != b.ShapeID {
			return a.ShapeID < b.ShapeID
		}
		return a.NormalizedQueryHash < b.NormalizedQueryHash
	})
}

func itoaLocal(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
