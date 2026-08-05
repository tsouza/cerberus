package promql

import (
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/schema"
)

// WireArm names the wire-visible domain a matcher targets, as opposed to a
// plain OTel-CH storage column/attribute. A classic histogram is stored as
// ONE row per observation under the bare metric name, with no `le`
// attribute at all — its Prometheus wire surface (the `<base>_bucket` /
// `<base>_count` / `<base>_sum` triple, and the `le` ladder label the
// `_bucket` series exposes) exists ONLY as a cerberus-side rewrite, never
// as literal storage. A matcher on each domain needs a different
// resolution strategy:
//
//   - WireArmStorage matchers resolve directly against the stored row
//     (matcherToExpr's ordinary Attributes/column path).
//   - WireArmWireName matchers (an `__name__` matcher) resolve against a
//     synthesized name expression — the bare/stripped name for a pinned
//     equality match, or a rewritten `concat(MetricName, <suffix>)` for an
//     unpinned match — never a literal stored column.
//   - WireArmWireBound matchers (an `le` matcher on a classic-histogram
//     selector) exist only AFTER a bucket fan-out materializes the ladder
//     as a synthetic Attributes entry; evaluated against the pre-fanout
//     row they are unconditionally unsatisfiable.
//
// See #1755 for the investigation this classifier formalizes: the package
// had three independent, inconsistent reimplementations of this split
// (splitBucketMatchers, splitRegexHistogramMatchers, and the inline loop in
// histogramQuantileMatcherPredicate) before wireArms existed, and the third
// — histogram_quantile.go's — never classified `le` at all, which is the
// direct root cause of #1478.
type WireArm int

const (
	// WireArmStorage is the default arm: a matcher that resolves directly
	// against a stored column or Attributes entry.
	WireArmStorage WireArm = iota
	// WireArmWireName is an `__name__` matcher — the Prometheus wire
	// surface's synthetic suffixed name space, never a literal stored
	// column.
	WireArmWireName
	// WireArmWireBound is an `le` matcher on a classic-histogram selector
	// — the synthetic bucket-ladder label, satisfiable only after a
	// fan-out materializes it.
	WireArmWireBound
)

// String renders arm for diagnostics/test failure messages.
func (arm WireArm) String() string {
	switch arm {
	case WireArmStorage:
		return "storage"
	case WireArmWireName:
		return "wire-name"
	case WireArmWireBound:
		return "wire-bound"
	default:
		return "unknown"
	}
}

// bucketBoundLabel is the synthetic ladder-bound label name Prometheus's
// classic-histogram wire convention uses. Named as a constant (rather than
// a bare "le" literal at each call site) so a future non-Prometheus wire
// convention has exactly one place to override it.
const bucketBoundLabel = "le"

// WireArms is the per-matcher classification wireArms produces: Matchers[i]
// was classified into Arms[i], in the ORIGINAL matcher order. Callers that
// need per-arm slices use Storage / WireName / WireBound below rather than
// re-deriving the classification.
type WireArms struct {
	Matchers []*labels.Matcher
	Arms     []WireArm
}

// filter returns the subset of w.Matchers classified into arm, preserving
// relative order.
func (w WireArms) filter(arm WireArm) []*labels.Matcher {
	out := make([]*labels.Matcher, 0, len(w.Matchers))
	for i, m := range w.Matchers {
		if w.Arms[i] == arm {
			out = append(out, m)
		}
	}
	return out
}

// Storage returns the matchers classified WireArmStorage, preserving order.
func (w WireArms) Storage() []*labels.Matcher { return w.filter(WireArmStorage) }

// WireName returns the matchers classified WireArmWireName (the `__name__`
// matchers), preserving order.
func (w WireArms) WireName() []*labels.Matcher { return w.filter(WireArmWireName) }

// WireBound returns the matchers classified WireArmWireBound (the `le`
// matchers), preserving order.
func (w WireArms) WireBound() []*labels.Matcher { return w.filter(WireArmWireBound) }

// wireArms classifies each matcher in matchers into the wire domain it
// targets (see WireArm), given the histogram-relevant configuration on s.
//
// This is deliberately a SKELETON, not yet wired into any consumer:
// histogramQuantileMatcherPredicate, splitBucketMatchers, and
// splitRegexHistogramMatchers each still carry their own inline or
// bespoke split. Migrating them onto wireArms is tracked as a follow-up
// (see the issue referenced from #1755) so it can land alongside — not
// underneath — whichever PR fixes #1478/#1483/#1692 on
// histogram_quantile.go, rather than racing it.
//
// s.HistogramTable == "" means the deployment has no classic-histogram
// routing configured at all, in which case the wire-name / wire-bound
// distinction is meaningless (there is no synthetic surface to route
// around) and every matcher classifies WireArmStorage — mirroring the
// existing schema gate in isClassicBucketSelector.
func wireArms(matchers []*labels.Matcher, s schema.Metrics) WireArms {
	arms := make([]WireArm, len(matchers))
	if s.HistogramTable == "" {
		return WireArms{Matchers: matchers, Arms: arms}
	}
	for i, m := range matchers {
		switch m.Name {
		case bucketBoundLabel:
			arms[i] = WireArmWireBound
		case model.MetricNameLabel:
			arms[i] = WireArmWireName
		default:
			arms[i] = WireArmStorage
		}
	}
	return WireArms{Matchers: matchers, Arms: arms}
}
