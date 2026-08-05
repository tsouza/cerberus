package promql

import (
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
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
//   - WireArmWireNamePinned is an `__name__=` matcher (pinned equality).
//     A pin either rewrites to the bare/stripped storage name (when its
//     value carries the caller's wire suffix) or is unconditionally
//     unsatisfiable (when it doesn't) — see ResolveName. It never
//     resolves through a synthesized name expression: pin-ness is
//     exactly what lets the predicate stay a bare-column comparison
//     ClickHouse can PREWHERE-promote and granule-prune on (see
//     internal/chsql/prewhere.go's isCheapPredicate, which treats
//     *chplan.FuncCall as not-cheap).
//   - WireArmWireNameUnpinned is an `__name__` matcher using `=~` /
//     `!=` / `!~`. No stored column holds the Prometheus wire name, so
//     it only resolves against a synthesized `concat(MetricName,
//     <suffix>)` expression — ResolveName always reports
//     DecisionWireSynthetic for it.
//   - WireArmWireBound matchers (an `le` matcher on a classic-histogram
//     selector) exist only AFTER a bucket fan-out materializes the ladder
//     as a synthetic Attributes entry; evaluated against the pre-fanout
//     row they are unconditionally unsatisfiable.
//
// See #1755 for the investigation this classifier formalizes: the package
// had three independent, inconsistent reimplementations of this split
// (splitBucketMatchers, splitRegexHistogramMatchers, and the inline loop in
// histogramQuantileMatcherPredicate) before wireArms existed, and the
// original wireArms skeleton itself carried the same #1478-class bug —
// its classification switch never inspected m.Type, so it could not tell
// a pinned `=` matcher from an unpinned `=~`/`!=`/`!~` one apart, the
// exact distinction the whole family turns on. #1756 migrates all three
// call sites onto the m.Type-aware classification below.
type WireArm int

const (
	// WireArmStorage is the default arm: a matcher that resolves directly
	// against a stored column or Attributes entry.
	WireArmStorage WireArm = iota
	// WireArmWireNamePinned is a pinned (`=`) `__name__` matcher.
	WireArmWireNamePinned
	// WireArmWireNameUnpinned is an unpinned (`=~` / `!=` / `!~`)
	// `__name__` matcher.
	WireArmWireNameUnpinned
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
	case WireArmWireNamePinned:
		return "wire-name-pinned"
	case WireArmWireNameUnpinned:
		return "wire-name-unpinned"
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
// need per-arm slices use Storage / WireName / WireNamePinned /
// WireNameUnpinned / WireBound below rather than re-deriving the
// classification.
type WireArms struct {
	Matchers []*labels.Matcher
	Arms     []WireArm
}

// filter returns the subset of w.Matchers classified into any of arms,
// preserving relative order.
func (w WireArms) filter(arms ...WireArm) []*labels.Matcher {
	out := make([]*labels.Matcher, 0, len(w.Matchers))
	for i, m := range w.Matchers {
		for _, arm := range arms {
			if w.Arms[i] == arm {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// Storage returns the matchers classified WireArmStorage, preserving order.
func (w WireArms) Storage() []*labels.Matcher { return w.filter(WireArmStorage) }

// WireName returns the matchers classified WireArmWireNamePinned or
// WireArmWireNameUnpinned (any `__name__` matcher regardless of pin-ness),
// preserving order. Callers that need to distinguish the two — anything
// that has to decide bare-column vs synthesized-expression resolution —
// use WireNamePinned / WireNameUnpinned / ResolveName instead.
func (w WireArms) WireName() []*labels.Matcher {
	return w.filter(WireArmWireNamePinned, WireArmWireNameUnpinned)
}

// WireNamePinned returns the matchers classified WireArmWireNamePinned,
// preserving order.
func (w WireArms) WireNamePinned() []*labels.Matcher { return w.filter(WireArmWireNamePinned) }

// WireNameUnpinned returns the matchers classified WireArmWireNameUnpinned,
// preserving order.
func (w WireArms) WireNameUnpinned() []*labels.Matcher { return w.filter(WireArmWireNameUnpinned) }

// WireBound returns the matchers classified WireArmWireBound, preserving
// order.
func (w WireArms) WireBound() []*labels.Matcher { return w.filter(WireArmWireBound) }

// WireNameDecision is the plan-time resolution strategy a WireArmWireName*
// matcher takes once the caller supplies the wire suffix its selector
// context targets (e.g. "_bucket" for histogram_quantile's classic-bucket
// path, and identically for splitBucketMatchers's caller).
type WireNameDecision int

const (
	// DecisionStorageBare means the matcher already carries the caller's
	// wire suffix: rewriting it to the bare storage name keeps the
	// predicate a bare-column comparison — the shape
	// internal/chsql/prewhere.go's isCheapPredicate accepts into PREWHERE
	// and CH can granule-prune on. Only ever returned for a pinned
	// matcher.
	DecisionStorageBare WireNameDecision = iota
	// DecisionWireSynthetic means the matcher can only be evaluated
	// against a synthesized concat(MetricName, suffix) expression — no
	// stored column holds the Prometheus wire name. Always returned for
	// an unpinned matcher; isCheapPredicate treats the resulting
	// *chplan.FuncCall as not-cheap, so a DecisionWireSynthetic predicate
	// never reaches PREWHERE — that's the correct, expected cost, not a
	// regression: only a pinned matcher can resolve to a granule-pruned
	// point lookup at all.
	DecisionWireSynthetic
	// DecisionUnsatisfiable means a pinned matcher names a wire spelling
	// the caller's domain doesn't expose (e.g. the bare classic-histogram
	// name itself, or a companion suffix a bucket selector didn't ask
	// for). The predicate is unconditionally false rather than falling
	// through to the OTel-CH storage row, which lives under the bare name
	// and would over-answer (#1483 MAINTAINER DECISION 1).
	DecisionUnsatisfiable
)

// ResolveName computes the WireNameDecision for the i'th matcher — which
// must be classified WireArmWireNamePinned or WireArmWireNameUnpinned,
// i.e. drawn from w.WireName() — against wireSuffix, plus the bare storage
// name a DecisionStorageBare resolves to (empty for the other two
// decisions). Calling it for a matcher classified into any other arm is a
// caller bug; ResolveName reports DecisionWireSynthetic defensively rather
// than panicking, since that's the answer with no bare-column fallout.
func (w WireArms) ResolveName(i int, wireSuffix string) (WireNameDecision, string) {
	if w.Arms[i] != WireArmWireNamePinned {
		return DecisionWireSynthetic, ""
	}
	m := w.Matchers[i]
	if !strings.HasSuffix(m.Value, wireSuffix) {
		return DecisionUnsatisfiable, ""
	}
	return DecisionStorageBare, strings.TrimSuffix(m.Value, wireSuffix)
}

// wireArms classifies each matcher in matchers into the wire domain it
// targets (see WireArm).
//
// Classification is unconditional — it does not gate on
// s.HistogramTable — because each of its consumers owns the appropriate
// gate at its own call site instead: isClassicBucketSelector guards
// splitBucketMatchers, the s.HistogramTable != "" check ahead of
// lowerRegexHistogramSelector guards splitRegexHistogramMatchers, and —
// critically — histogramQuantileMatcherPredicate's pre-migration inline
// loop classified `le` / `__name__` matchers with no gate at all. Baking a
// deployment-config gate into wireArms itself would silently change that
// third consumer's behaviour on a deployment with no histogram table
// configured, which is exactly the kind of golden-diverging regression
// #1756 rules out.
func wireArms(matchers []*labels.Matcher) WireArms {
	arms := make([]WireArm, len(matchers))
	for i, m := range matchers {
		switch {
		case m.Name == bucketBoundLabel:
			arms[i] = WireArmWireBound
		case m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual:
			arms[i] = WireArmWireNamePinned
		case m.Name == model.MetricNameLabel:
			arms[i] = WireArmWireNameUnpinned
		default:
			arms[i] = WireArmStorage
		}
	}
	return WireArms{Matchers: matchers, Arms: arms}
}
