// Tests in this file kill the LIVED gremlins mutants reported on
// metadata_catalog.go's resource-allowlist inversion and on
// histogram_native_mixed_or_aggregate_float_only.go's parameter guard, from
// the phase4-promql-b leg (cerberus issue #2949). See gremlins_kill_test.go
// for the shared file-header convention this file follows.
package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/schema"
)

// resourceAllowlistSchema returns a schema whose resource arm is live (a
// ResourceAttributes column plus a non-empty PromResourceLabels allowlist) so
// [configuredResourceKeysFor]'s two early returns are not what decides the
// tests below.
func resourceAllowlistSchema(allowlist []string) schema.Metrics {
	s := schema.DefaultOTelMetrics()
	s.PromResourceLabels = allowlist
	return s
}

// TestConfiguredResourceKeysFor_SpellingMatchIsEitherNotBoth kills the
// INVERT_LOGICAL mutant at metadata_catalog.go:331:45 — the `&&` of
//
//	if sanitizePromLabelChars(k) != promLabel && format.OTelToPromLabel(k) != promLabel {
//
// inside [configuredResourceKeysFor]'s allowlist loop.
//
// The guard skips a key only when NEITHER spelling reaches promLabel; a key
// that matches on EITHER spelling belongs in the result, which is the whole
// point of inverting a many-to-one sanitisation over a closed set. The `||`
// mutant skips a key unless BOTH spellings match, which silently drops every
// key the two functions spell differently.
//
// The two functions differ on a digit-initial key: `sanitizePromLabelChars`
// rewrites only non-alphanumerics, so "9a" stays "9a", while
// `format.OTelToPromLabel` additionally prefixes an underscore because a Prom
// label may not begin with a digit, giving "_9a". Asking for promLabel "_9a"
// therefore matches on the OTel spelling and NOT on the sanitised one — the
// exact single-sided match the mutant discards.
//
// The key is deliberately not spelled the same as promLabel: a key identical
// to the requested label is already reached by the dot/underscore candidate
// chain, so [configuredResourceKeysFor] drops it as `covered` further down and
// the guard under test would never decide the outcome.
func TestConfiguredResourceKeysFor_SpellingMatchIsEitherNotBoth(t *testing.T) {
	t.Parallel()

	const digitInitialKey = "9a"
	const otelSpelling = "_9a"
	s := resourceAllowlistSchema([]string{digitInitialKey})

	got := configuredResourceKeysFor(s, otelSpelling)
	if len(got) != 1 || got[0] != digitInitialKey {
		t.Fatalf("configuredResourceKeysFor(allowlist=[%q], promLabel=%q) = %v; want [%q] — the key "+
			"matches on its OTel spelling (%q) even though its sanitised spelling (%q) differs, "+
			"and either match is enough (mutant `&&`->`||` at metadata_catalog.go:331 would "+
			"require both and drop it)",
			digitInitialKey, otelSpelling, got, digitInitialKey, otelSpelling, digitInitialKey)
	}
}

// TestConfiguredResourceKeysFor_NonMatchingKeySkipsNotStops kills the
// INVERT_LOOPCTRL mutant at metadata_catalog.go:332:4 — the `continue` that
// the spelling-match guard above executes for a key that addresses a
// different label.
//
// The allowlist is walked in configured order and a key that does not match
// is simply not this label's, so the walk must go on. `break` would abandon
// every key after the first non-match, making the result depend on where the
// deployment happened to list the label. The allowlist below puts a
// non-matching key first precisely so the two differ.
//
// The matching key is "region-a" rather than the label's own spelling because
// a key identical to the requested label is dropped later as `covered`; the
// hyphen sanitises to the underscore the label carries and nothing else about
// the key changes.
func TestConfiguredResourceKeysFor_NonMatchingKeySkipsNotStops(t *testing.T) {
	t.Parallel()

	const promLabel = "region_a"
	const wanted = "region-a"
	s := resourceAllowlistSchema([]string{"unrelated", wanted})

	got := configuredResourceKeysFor(s, promLabel)
	if len(got) != 1 || got[0] != wanted {
		t.Fatalf("configuredResourceKeysFor(allowlist=[unrelated %q], promLabel=%q) = %v; want "+
			"[%q] — a non-matching key must be SKIPPED, not end the walk (mutant "+
			"`continue`->`break` at metadata_catalog.go:332 would return nothing because the "+
			"non-matching key is listed first)",
			wanted, promLabel, got, wanted)
	}
}

// TestConfiguredResourceKeysFor_ExcludedKeySkipsNotStops kills the
// INVERT_LOOPCTRL mutant at metadata_catalog.go:339:4 — the `continue` taken
// for a key that a dedicated top-level column already backs.
//
// "service.name" is excluded because [excludedResourceKeys] lists it whenever
// ServiceNameColumn is configured: the dedicated column owns that key and the
// resource arm must not duplicate it. But exclusion is a property of THAT key
// alone, so a sibling allowlist entry that sanitises to the same Prom label
// and is NOT itself excluded still belongs in the result.
//
// "service-name" is that sibling: it sanitises to "service_name" like
// "service.name" does, and it is absent from the excluded set (which holds
// only the dotted "service.name" and its Prom spelling "service_name"). With
// the excluded key listed first, `break` returns nothing where `continue`
// returns the sibling.
func TestConfiguredResourceKeysFor_ExcludedKeySkipsNotStops(t *testing.T) {
	t.Parallel()

	const promLabel = "service_name"
	const excludedKey = "service.name"
	const siblingKey = "service-name"

	s := resourceAllowlistSchema([]string{excludedKey, siblingKey})
	if s.ServiceNameColumn == "" {
		t.Fatalf("positive control: the default schema configures no ServiceNameColumn, so %q is "+
			"not excluded and this test would not reach the guard it pins", excludedKey)
	}

	got := configuredResourceKeysFor(s, promLabel)
	if len(got) != 1 || got[0] != siblingKey {
		t.Fatalf("configuredResourceKeysFor(allowlist=[%q %q], promLabel=%q) = %v; want [%q] — a "+
			"key excluded by its dedicated column must be SKIPPED, not end the walk (mutant "+
			"`continue`->`break` at metadata_catalog.go:339 would return nothing because the "+
			"excluded key is listed first)",
			excludedKey, siblingKey, promLabel, got, siblingKey)
	}
}

// TestSanitizedResourceKeyPairs_DuplicateSkipsNotStops kills the
// INVERT_LOOPCTRL mutant at metadata_catalog.go:358:4 — the `continue` taken
// for a repeated allowlist entry inside [sanitizedResourceKeyPairs].
//
// The loop de-duplicates a configured allowlist that may legitimately repeat
// a key; a repeat carries no information and the remaining entries still do.
// `break` would truncate the lookup table at the first repeat, so a
// deployment that listed a key twice would lose every key after it — and the
// table is what [sanitizeMapKeysExpr] emits in place of a per-key regex, so
// the loss is silent and total for the dropped keys.
func TestSanitizedResourceKeyPairs_DuplicateSkipsNotStops(t *testing.T) {
	t.Parallel()

	s := resourceAllowlistSchema([]string{"alpha", "alpha", "beta"})

	keys, sanitized := sanitizedResourceKeyPairs(s)
	if len(keys) != 2 || keys[0] != "alpha" || keys[1] != "beta" {
		t.Fatalf("sanitizedResourceKeyPairs(allowlist=[alpha alpha beta]) keys = %v; want "+
			"[alpha beta] — a repeated key must be SKIPPED, not end the walk (mutant "+
			"`continue`->`break` at metadata_catalog.go:358 would drop beta)", keys)
	}
	if len(sanitized) != len(keys) {
		t.Fatalf("sanitizedResourceKeyPairs returned %d keys and %d sanitised spellings; the two "+
			"slices are a pairing and must have equal length", len(keys), len(sanitized))
	}
}

// mixedOrFloatOnlyAggQuery builds `<agg>((latency_exp_hist) or
// (other_metric))` — an aggregation whose input is the mixed exp-histogram /
// float set-op shape [floatOnlyAggOverMixedExpHistogramSetOp] answers.
func mixedOrFloatOnlyAggQuery(agg string) string {
	return agg + `((latency_exp_hist) or (other_metric))`
}

// TestFloatOnlyAggOverMixedExpHistogramSetOp_ParamlessOpsAccepted kills TWO
// mutants on histogram_native_mixed_or_aggregate_float_only.go:91,
//
//	if agg.Op != parser.QUANTILE && agg.Param != nil {
//
// the guard that rejects an aggregation carrying a parameter it has no
// meaning for:
//
//   - 91:44 CONDITIONALS_NEGATION, `agg.Param != nil` -> `agg.Param == nil`.
//   - 91:31 INVERT_LOGICAL, the `&&` -> `||`.
//
// `min` is one of the histogram-dropping ops [expHistogramAggDropsHistogramSamples]
// admits, it is not QUANTILE, and the parser gives it a nil Param. Under the
// original both conjuncts are needed and `nil` Param means the guard does not
// fire, so the shape is recognised. Under EITHER mutant the guard fires — the
// negation makes `Param == nil` true, and the `||` needs only
// `Op != QUANTILE` — and a perfectly ordinary `min` over the mixed set-op is
// refused.
func TestFloatOnlyAggOverMixedExpHistogramSetOp_ParamlessOpsAccepted(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	q := mixedOrFloatOnlyAggQuery("min")
	expr := mustParse(t, q)

	agg, b, ok := floatOnlyAggOverMixedExpHistogramSetOp(expr, s, lowerCtx{})
	if !ok {
		t.Fatalf("floatOnlyAggOverMixedExpHistogramSetOp(%q) = ok false; want true — `min` carries "+
			"no parameter, so the QUANTILE/Param guard must not fire (mutants "+
			"`!=`->`==` at :91:44 and `&&`->`||` at :91:31 both make it fire)", q)
	}
	if agg == nil || agg.Op != parser.MIN {
		t.Fatalf("floatOnlyAggOverMixedExpHistogramSetOp(%q) returned agg %#v; want the MIN "+
			"aggregation itself", q, agg)
	}
	if b == nil {
		t.Fatalf("floatOnlyAggOverMixedExpHistogramSetOp(%q) returned a nil set-op; want the "+
			"recognised `or` binary expression", q)
	}
}

// TestFloatOnlyAggOverMixedExpHistogramSetOp_QuantileKeepsItsParam kills the
// CONDITIONALS_NEGATION mutant at
// histogram_native_mixed_or_aggregate_float_only.go:91:12 — `agg.Op !=
// parser.QUANTILE` -> `agg.Op == parser.QUANTILE`.
//
// QUANTILE is the one admitted op for which a parameter is REQUIRED, which is
// exactly why it is named as the exception: the guard means "a parameter is
// an error unless this is quantile". The parser always gives `quantile` a
// non-nil Param, so the mutated guard `Op == QUANTILE && Param != nil` fires
// on every quantile there is, and the op the exception was written for
// becomes the only one refused.
func TestFloatOnlyAggOverMixedExpHistogramSetOp_QuantileKeepsItsParam(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	q := `quantile(0.9, (latency_exp_hist) or (other_metric))`
	expr := mustParse(t, q)

	agg, _, ok := floatOnlyAggOverMixedExpHistogramSetOp(expr, s, lowerCtx{})
	if !ok {
		t.Fatalf("floatOnlyAggOverMixedExpHistogramSetOp(%q) = ok false; want true — QUANTILE is "+
			"the op the Param exception exists for (mutant `!=`->`==` at :91:12 rejects every "+
			"quantile instead)", q)
	}
	if agg == nil || agg.Op != parser.QUANTILE {
		t.Fatalf("floatOnlyAggOverMixedExpHistogramSetOp(%q) returned agg %#v; want the QUANTILE "+
			"aggregation itself", q, agg)
	}
	if agg.Param == nil {
		t.Fatalf("floatOnlyAggOverMixedExpHistogramSetOp(%q) returned a QUANTILE aggregation with "+
			"a nil Param; the phi argument is what the exception admits", q)
	}
}
