package promql

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// targetInfoMetric is the default info metric the experimental PromQL
// `info()` function joins against when no `{__name__=…}` selector is
// given — mirroring prometheus/promql/info.go::targetInfo.
const targetInfoMetric = "target_info"

// infoIdentifyingLabels is the hard-coded identifying-label set PromQL's
// `info()` joins on (prometheus/promql/info.go::identifyingLabels). The
// experimental function does not yet auto-discover identifying labels per
// info metric; it always keys on instance + job.
var infoIdentifyingLabels = []string{"instance", "job"}

// lowerInfo lowers the experimental PromQL function
//
//	info(v instant-vector, [data-label-selector])
//
// into an InfoJoin: the input vector `v` (arg 0) is enriched with the
// data labels of the matching `target_info`-style info series, joined on
// the identifying labels instance + job. The sample values of `v` are
// carried through unchanged — info() only adds labels.
//
// The optional second argument is a label selector (`{…}`, a
// parser.VectorSelector carrying only label matchers). Its `__name__`
// matcher (if any) selects which info metric to join against (default
// target_info); its non-`__name__` matchers both constrain which info
// series participate AND restrict which data labels are copied onto the
// output (by label name).
//
// Reference semantics: prometheus/promql/info.go::evalInfo. Conflict
// resolution (base labels win), the newest-info-wins tie-break, and the
// "return base unenriched when no info matches" branch are all handled in
// the emitter (internal/chsql/info_join.go).
func lowerInfo(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) < 1 || len(c.Args) > 2 {
		return nil, fmt.Errorf("promql: info expects 1 or 2 arguments, got %d", len(c.Args))
	}

	// Resolve the info-metric name matcher + data-label selector from the
	// optional second argument.
	infoName := targetInfoMetric
	var dataLabelMatchers []*labels.Matcher
	var dataLabelFilter []string
	if len(c.Args) == 2 {
		sel, ok := c.Args[1].(*parser.VectorSelector)
		if !ok {
			return nil, fmt.Errorf("promql: info second argument must be a label selector, got %T", c.Args[1])
		}
		name, dms, err := splitInfoSelector(sel)
		if err != nil {
			return nil, err
		}
		if name != "" {
			infoName = name
		}
		dataLabelMatchers = dms
		dataLabelFilter = matcherNames(dms)
	}

	// Lower the input vector (arg 0) with the surrounding instant/range
	// LWR pipeline — its sample values are the ones info() preserves.
	base, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	// Build + lower the info-metric selector. The info side is an ordinary
	// VectorSelector — `target_info{<data-label-matchers>}` — so it picks
	// up the same LWR collapse + PREWHERE promotion a normal selector
	// would. The data-label matchers constrain which info series
	// participate (matching evalInfo's infoLabelMatchers), and their names
	// drive the emitter's data-label filter.
	infoMatchers := make([]*labels.Matcher, 0, len(dataLabelMatchers)+1)
	nameMatcher, err := labels.NewMatcher(labels.MatchEqual, model.MetricNameLabel, infoName)
	if err != nil {
		return nil, fmt.Errorf("promql: info metric name matcher: %w", err)
	}
	infoMatchers = append(infoMatchers, nameMatcher)
	infoMatchers = append(infoMatchers, dataLabelMatchers...)

	infoSel := &parser.VectorSelector{
		Name:          infoName,
		LabelMatchers: infoMatchers,
	}
	info, err := lowerVectorSelector(infoSel, s, ctx)
	if err != nil {
		return nil, err
	}

	return &chplan.InfoJoin{
		Base:              base,
		Info:              info,
		IdentifyingLabels: append([]string(nil), infoIdentifyingLabels...),
		DataLabelFilter:   dataLabelFilter,
		MetricNameColumn:  s.MetricNameColumn,
		AttributesColumn:  s.AttributesColumn,
		TimestampColumn:   s.TimestampColumn,
		ValueColumn:       s.ValueColumn,
	}, nil
}

// splitInfoSelector separates the `{…}` data-label selector into its
// `__name__` value (selecting which info metric to join against) and the
// remaining non-`__name__` matchers (the data-label filter).
//
// The supported `__name__` shape is a single positive equality matcher —
// `{__name__="target_info"}` — which is the realistic Grafana shape. A
// regex / negated / multiple `__name__` matcher is rejected with a clear
// error rather than silently mis-answering: the reference's
// effectiveInfoNameMatchers synthesis (negative-only → `.+_info`, multi-
// metric fan-out) is outside this lowering's parity envelope.
func splitInfoSelector(sel *parser.VectorSelector) (string, []*labels.Matcher, error) {
	var name string
	var nameMatcherCount int
	data := make([]*labels.Matcher, 0, len(sel.LabelMatchers))
	for _, m := range sel.LabelMatchers {
		if m.Name == model.MetricNameLabel {
			nameMatcherCount++
			if m.Type != labels.MatchEqual {
				return "", nil, fmt.Errorf(
					"promql: info selector __name__ matcher must be an equality match (got %s), "+
						"regex / negated info-metric selection is unsupported", m.Type)
			}
			name = m.Value
			continue
		}
		data = append(data, m)
	}
	if nameMatcherCount > 1 {
		return "", nil, fmt.Errorf("promql: info selector accepts at most one __name__ matcher, got %d", nameMatcherCount)
	}
	return name, data, nil
}

// matcherNames returns the distinct label names referenced by ms, in
// first-seen order. Used to drive the emitter's data-label filter from
// the `{…}` selector's non-`__name__` matchers.
func matcherNames(ms []*labels.Matcher) []string {
	if len(ms) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ms))
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if _, ok := seen[m.Name]; ok {
			continue
		}
		seen[m.Name] = struct{}{}
		out = append(out, m.Name)
	}
	return out
}
