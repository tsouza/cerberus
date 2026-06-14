package surfaceparity

import (
	"fmt"
	"sort"

	promparser "github.com/prometheus/prometheus/promql/parser"
)

// Domain-aware probe operands drawn from the showcase seed
// (test/e2e/seed/cmd/seed/main.go). Using real metric names + a real
// label keeps the probe in the schema's accepted shape so a wrong arg
// type/domain doesn't produce a false wrong-reject (the prototype
// mis-flagged histogram_avg(up) by feeding a gauge to a histogram fn).
const (
	// promGauge is a real instantaneous gauge metric.
	promGauge = "up"
	// promCounter is a real monotonic-sum (counter) metric.
	promCounter = "http_server_request_duration_count"
	// promExpHist is a real native exponential-histogram metric — the
	// correct input for histogram_quantile / histogram_avg /
	// histogram_count / histogram_sum / histogram_stddev /
	// histogram_stdvar / histogram_fraction.
	promExpHist = "showcase_latency_exp_hist"
	// promLabel is a real label present on the seed metrics.
	promLabel = "job"
)

// promQLFunctionProbe synthesizes a canonical, type-and-domain-aware
// call for a PromQL function given its arg-type signature. The aim is a
// query that is well-typed for the reference parser AND fed the right
// metric family, so the only axis under test is whether cerberus lowers
// the symbol — not whether the probe is mistyped.
func promQLFunctionProbe(fn *promparser.Function) string {
	name := fn.Name
	// Histogram-family value functions need a native histogram vector.
	switch name {
	case "histogram_count", "histogram_sum", "histogram_avg",
		"histogram_stddev", "histogram_stdvar":
		return fmt.Sprintf("%s(%s)", name, promExpHist)
	case "histogram_quantile":
		return fmt.Sprintf("histogram_quantile(0.9, %s)", promExpHist)
	case "histogram_quantiles":
		// Signature: (Vector, String, Scalar, Scalar...) — the input
		// histogram vector first, then the output-label name, then the
		// requested quantiles.
		return fmt.Sprintf("histogram_quantiles(%s, \"q\", 0.5, 0.9)", promExpHist)
	case "histogram_fraction":
		return fmt.Sprintf("histogram_fraction(0, 1, %s)", promExpHist)
	}
	// label_join / label_replace have fixed, irregular signatures.
	switch name {
	case "label_join":
		return fmt.Sprintf("label_join(%s, \"dst\", \",\", \"%s\")", promGauge, promLabel)
	case "label_replace":
		return fmt.Sprintf("label_replace(%s, \"dst\", \"$1\", \"%s\", \"(.*)\")", promGauge, promLabel)
	}
	// info takes a vector + optional label-matcher set.
	if name == "info" {
		return fmt.Sprintf("info(%s)", promCounter)
	}
	// Synthesize positionally from the declared arg types. Counters are
	// fed to rate-shaped range functions; ranges become [5m] matrices.
	// We emit exactly len(ArgTypes) args (the maximal *required* form):
	// for a Variadic function the trailing arg(s) are optional, so the
	// full ArgTypes list is always within the parser's "at most N
	// argument(s)" bound — appending a synthetic extra variadic arg
	// would over-shoot it and produce a parse error (a probe-synthesis
	// false positive). Functions whose variadic genuinely takes more
	// (label_join) carry hand-written probes above.
	args := make([]string, 0, len(fn.ArgTypes))
	for i, at := range fn.ArgTypes {
		args = append(args, promArgFor(name, at, i))
	}
	if len(args) == 0 {
		return fmt.Sprintf("%s()", name)
	}
	return fmt.Sprintf("%s(%s)", name, joinArgs(args))
}

// promArgFor produces one argument literal of the requested value type.
// Matrix args use the counter metric for rate-shaped fns and the gauge
// otherwise; vector args use the gauge; scalars/strings use literals.
func promArgFor(fn string, at promparser.ValueType, idx int) string {
	switch at {
	case promparser.ValueTypeMatrix:
		base := promGauge
		if rateShaped(fn) {
			base = promCounter
		}
		return base + "[5m]"
	case promparser.ValueTypeVector:
		return promGauge
	case promparser.ValueTypeScalar:
		// A scalar in (0,1) is well-typed everywhere a scalar arg
		// appears here AND satisfies the domain constraints that
		// otherwise reject the probe: clamp/round bounds, quantile
		// levels, and the double_exponential_smoothing smoothing /
		// trend factors (which must be strictly in (0,1)).
		return "0.5"
	case promparser.ValueTypeString:
		return "\"s\""
	default:
		return "1"
	}
}

// rateShaped reports whether a range function expects a counter (so the
// probe feeds the monotonic-sum metric rather than the gauge).
func rateShaped(fn string) bool {
	switch fn {
	case "rate", "irate", "increase", "delta", "idelta", "deriv",
		"resets", "changes", "predict_linear", "double_exponential_smoothing":
		return true
	}
	return false
}

func joinArgs(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += ", "
		}
		out += a
	}
	return out
}

// promQLAggregatorProbe synthesizes a canonical aggregation expression.
// Parameterised aggregators (topk/bottomk/quantile/count_values/limitk/
// limit_ratio) carry their required scalar/string parameter.
func promQLAggregatorProbe(op string) string {
	switch op {
	case "topk", "bottomk", "limitk":
		return fmt.Sprintf("%s(3, %s)", op, promGauge)
	case "limit_ratio":
		return fmt.Sprintf("limit_ratio(0.5, %s)", promGauge)
	case "quantile":
		return fmt.Sprintf("quantile(0.9, %s)", promGauge)
	case "count_values":
		return fmt.Sprintf("count_values(\"v\", %s)", promGauge)
	default:
		return fmt.Sprintf("%s(%s)", op, promGauge)
	}
}

// promAggregators is the aggregation-op set with the reference posture.
// limitk / limit_ratio are experimental (parser.ItemType.IsExperimental-
// Aggregator) — the reference gates them off by default exactly like
// experimental functions.
var promAggregators = []struct {
	op           string
	experimental bool
}{
	{"sum", false},
	{"avg", false},
	{"count", false},
	{"min", false},
	{"max", false},
	{"group", false},
	{"stddev", false},
	{"stdvar", false},
	{"topk", false},
	{"bottomk", false},
	{"count_values", false},
	{"quantile", false},
	{"limitk", true},
	{"limit_ratio", true},
}

// promBinaryOps is the binary-operator set. Each probe applies the op
// between two real vectors (or, for atan2, the trig binary op). All are
// reference-accepted (no experimental binary ops in v3.x).
var promBinaryOps = []struct {
	sym   string
	probe string
}{
	{"add", promGauge + " + " + promGauge},
	{"sub", promGauge + " - " + promGauge},
	{"mul", promGauge + " * " + promGauge},
	{"div", promGauge + " / " + promGauge},
	{"mod", promGauge + " % " + promGauge},
	{"pow", promGauge + " ^ " + promGauge},
	{"eql", promGauge + " == " + promGauge},
	{"neq", promGauge + " != " + promGauge},
	{"gtr", promGauge + " > " + promGauge},
	{"lss", promGauge + " < " + promGauge},
	{"gte", promGauge + " >= " + promGauge},
	{"lte", promGauge + " <= " + promGauge},
	{"and", promGauge + " and " + promGauge},
	{"or", promGauge + " or " + promGauge},
	{"unless", promGauge + " unless " + promGauge},
	{"atan2", promGauge + " atan2 " + promGauge},
}

// promModifiers is the @ / offset modifier set. Both are core PromQL
// (reference-accepted).
var promModifiers = []struct {
	sym   string
	probe string
}{
	{"offset", promGauge + " offset 5m"},
	{"at", promGauge + " @ 0"},
	{"at_start", promGauge + " @ start()"},
	{"at_end", promGauge + " @ end()"},
}

// referenceImplementedExperimentalFns is the set of parser-experimental
// PromQL functions the REFERENCE engine actually IMPLEMENTS (and returns
// data for) once EnableExperimentalFunctions is set — the posture every
// Grafana/Prometheus deployment that opts in sees. The parser's
// `Experimental` flag alone models only the default-OFF parse gate, not
// whether a result exists; for these symbols the reference verdict is
// ACCEPT, so a cerberus implementation lands parity-accept rather than a
// spurious wrong-accept. Membership is added the moment cerberus grows a
// real lowering+emit for the symbol (mirrored by a live showcase panel +
// chDB parity fixture).
var referenceImplementedExperimentalFns = map[string]bool{
	// histogram_quantiles(<vector>, "<label>", phi...) — variadic
	// multi-quantile sibling of histogram_quantile. Implemented in
	// internal/promql/histogram_quantile.go (per-phi kernel + q-label
	// injection + UNION ALL); pinned by the histogram_quantiles_*
	// fixtures under test/spec/promql and the showcase-promql panel.
	"histogram_quantiles": true,
}

// probePromQL enumerates the PromQL parser symbol table, synthesizes a
// domain-aware probe per symbol, runs the cerberus verdict, models the
// reference verdict from the parser's experimental flags, and
// classifies each.
func probePromQL() ([]Entry, error) {
	var entries []Entry

	// Functions — parser.Functions is the authoritative map; its
	// Experimental flag is the reference oracle.
	names := make([]string, 0, len(promparser.Functions))
	for name := range promparser.Functions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fn := promparser.Functions[name]
		probe := promQLFunctionProbe(fn)
		cv, cerr := cerberusVerdictPromQL(probe)
		ref := VerdictAccept
		if fn.Experimental && !referenceImplementedExperimentalFns[name] {
			ref = VerdictReject
		}
		entries = append(entries, Entry{
			Head:          "promql",
			Symbol:        "fn:" + name,
			Kind:          "function",
			Probe:         probe,
			Cerberus:      cv,
			Reference:     ref,
			Class:         classify(cv, ref),
			CerberusError: cerr,
		})
	}

	// Aggregators.
	for _, a := range promAggregators {
		probe := promQLAggregatorProbe(a.op)
		cv, cerr := cerberusVerdictPromQL(probe)
		ref := VerdictAccept
		if a.experimental {
			ref = VerdictReject
		}
		entries = append(entries, Entry{
			Head:          "promql",
			Symbol:        "agg:" + a.op,
			Kind:          "aggregator",
			Probe:         probe,
			Cerberus:      cv,
			Reference:     ref,
			Class:         classify(cv, ref),
			CerberusError: cerr,
		})
	}

	// Binary operators.
	for _, b := range promBinaryOps {
		cv, cerr := cerberusVerdictPromQL(b.probe)
		ref := VerdictAccept
		entries = append(entries, Entry{
			Head:          "promql",
			Symbol:        "op:" + b.sym,
			Kind:          "binary-op",
			Probe:         b.probe,
			Cerberus:      cv,
			Reference:     ref,
			Class:         classify(cv, ref),
			CerberusError: cerr,
		})
	}

	// Modifiers.
	for _, m := range promModifiers {
		cv, cerr := cerberusVerdictPromQL(m.probe)
		ref := VerdictAccept
		entries = append(entries, Entry{
			Head:          "promql",
			Symbol:        "mod:" + m.sym,
			Kind:          "modifier",
			Probe:         m.probe,
			Cerberus:      cv,
			Reference:     ref,
			Class:         classify(cv, ref),
			CerberusError: cerr,
		})
	}

	return entries, nil
}
