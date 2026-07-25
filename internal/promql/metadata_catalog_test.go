package promql

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSanitizedResourceKeyPairs pins the plan-time lookup table
// [sanitizeResourceKeysExpr] builds from: a zero-configured allowlist
// yields empty tables (the caller falls back to the per-row regex, see
// [TestSanitizeResourceKeysExpr_FallsBackToRegexWhenUnconfigured]), a
// configured allowlist is deduped and sorted for a deterministic emit, and
// every key is paired with its sanitized (dot -> underscore) spelling.
func TestSanitizedResourceKeyPairs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		allow         []string
		wantKeys      []string
		wantSanitized []string
	}{
		{
			name:          "zero_configured_keys_returns_empty_tables",
			allow:         nil,
			wantKeys:      []string{},
			wantSanitized: []string{},
		},
		{
			name:          "sorts_and_sanitizes_configured_keys",
			allow:         []string{"k8s.namespace.name", "deployment.environment.name"},
			wantKeys:      []string{"deployment.environment.name", "k8s.namespace.name"},
			wantSanitized: []string{"deployment_environment_name", "k8s_namespace_name"},
		},
		{
			name:          "dedupes_repeated_keys",
			allow:         []string{"a.b", "a.b", "a.b"},
			wantKeys:      []string{"a.b"},
			wantSanitized: []string{"a_b"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := schema.DefaultOTelMetrics()
			s.PromResourceLabels = tc.allow
			gotKeys, gotSanitized := sanitizedResourceKeyPairs(s)
			if len(gotKeys) != len(tc.wantKeys) {
				t.Fatalf("keys = %v, want %v", gotKeys, tc.wantKeys)
			}
			for i := range gotKeys {
				if gotKeys[i] != tc.wantKeys[i] {
					t.Errorf("keys[%d] = %q, want %q (full: %v)", i, gotKeys[i], tc.wantKeys[i], gotKeys)
				}
			}
			if len(gotSanitized) != len(tc.wantSanitized) {
				t.Fatalf("sanitized = %v, want %v", gotSanitized, tc.wantSanitized)
			}
			for i := range gotSanitized {
				if gotSanitized[i] != tc.wantSanitized[i] {
					t.Errorf("sanitized[%d] = %q, want %q (full: %v)", i, gotSanitized[i], tc.wantSanitized[i], gotSanitized)
				}
			}
		})
	}
}

// TestSanitizeResourceKeysExpr_FallsBackToRegexWhenUnconfigured pins the
// branch [sanitizeResourceKeysExpr] takes when no allowlist is configured:
// the key set is open, so the ONLY correct rewrite is the per-row
// `replaceRegexpAll` mirror of [format.OTelToPromLabel], never the
// constant `transform()` lookup table a closed allowlist would let it
// emit instead. Regressing this branch would either silently switch every
// default-schema deployment (no CERBERUS_PROM_RESOURCE_LABELS set) onto an
// incomplete lookup table, or vice versa.
func TestSanitizeResourceKeysExpr_FallsBackToRegexWhenUnconfigured(t *testing.T) {
	t.Parallel()

	arrayMapLambdaBody := func(t *testing.T, expr chplan.Expr) *chplan.FuncCall {
		t.Helper()
		outer, ok := expr.(*chplan.FuncCall)
		if !ok || outer.Name != "mapFromArrays" || len(outer.Args) != 2 {
			t.Fatalf("got %#v, want mapFromArrays(...)", expr)
		}
		arrayMap, ok := outer.Args[0].(*chplan.FuncCall)
		if !ok || arrayMap.Name != "arrayMap" || len(arrayMap.Args) != 2 {
			t.Fatalf("mapFromArrays arg0 = %#v, want arrayMap(...)", outer.Args[0])
		}
		lambda, ok := arrayMap.Args[0].(*chplan.Lambda)
		if !ok {
			t.Fatalf("arrayMap arg0 = %#v, want *chplan.Lambda", arrayMap.Args[0])
		}
		body, ok := lambda.Body.(*chplan.FuncCall)
		if !ok {
			t.Fatalf("lambda body = %#v, want *chplan.FuncCall", lambda.Body)
		}
		return body
	}

	// Zero-configured allowlist: the key set is open, so the rewrite MUST
	// be the per-row regex, not the lookup table.
	sOpen := schema.DefaultOTelMetrics()
	openBody := arrayMapLambdaBody(t, sanitizeResourceKeysExpr(sOpen))
	if openBody.Name != "replaceRegexpAll" {
		t.Errorf("zero-configured-key rewrite = %q, want %q (regex fallback)", openBody.Name, "replaceRegexpAll")
	}

	// A configured allowlist closes the key set: the rewrite becomes the
	// constant transform() lookup table instead.
	sClosed := schema.DefaultOTelMetrics()
	sClosed.PromResourceLabels = []string{"k8s.namespace.name"}
	closedBody := arrayMapLambdaBody(t, sanitizeResourceKeysExpr(sClosed))
	if closedBody.Name != "transform" {
		t.Errorf("configured-allowlist rewrite = %q, want %q (lookup table)", closedBody.Name, "transform")
	}
}

// TestConfiguredResourceKeysFor pins [configuredResourceKeysFor]'s
// inversion of the sanitisation rewrite over the CONFIGURED (closed)
// resource-label allowlist: the zero-configured-allowlist fallback (no
// closed set to invert), several colliding keys that reach the same
// sanitized Prom label through separators the dot/underscore candidate
// chain never rewrites (so [attributeLookup] alone would silently miss
// them), and the dedicated-column exclusion that must win even when a
// configured key would otherwise collide onto an excluded label.
func TestConfiguredResourceKeysFor(t *testing.T) {
	t.Parallel()

	t.Run("zero_configured_allowlist_returns_nil", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.PromResourceLabels = nil
		if got := configuredResourceKeysFor(s, "k8s_namespace_name"); got != nil {
			t.Errorf("got %v, want nil (no closed key set to invert)", got)
		}
	})

	t.Run("no_resource_attributes_column_returns_nil", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.PromResourceLabels = []string{"k8s.namespace.name"}
		s.ResourceAttributesColumn = ""
		if got := configuredResourceKeysFor(s, "k8s_namespace_name"); got != nil {
			t.Errorf("got %v, want nil (resource arm disabled)", got)
		}
	})

	// Three keys that all sanitize to `k8s_namespace_name` via DIFFERENT
	// separators (hyphen, colon, space) that [attributeLookupKeys]'s
	// dot<->underscore candidate chain never produces from the label
	// itself — each is reachable ONLY through this inversion. The
	// allowlist's own order decides which arm a downstream coalesce tries
	// first on a genuine collision, so the result must preserve it exactly.
	t.Run("colliding_keys_preserve_allowlist_order", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.PromResourceLabels = []string{
			"k8s-namespace-name",
			"k8s:namespace:name",
			"k8s namespace name",
		}
		want := []string{"k8s-namespace-name", "k8s:namespace:name", "k8s namespace name"}
		got := configuredResourceKeysFor(s, "k8s_namespace_name")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v (allowlist order preserved)", got, want)
		}
	})

	// A key reachable via the ordinary dot/underscore candidate chain is
	// skipped — it would only duplicate the arm [attributeLookup] already
	// emits, not a genuinely unreachable collision.
	t.Run("dot_reachable_key_is_skipped_as_already_covered", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.PromResourceLabels = []string{"k8s.namespace.name"}
		if got := configuredResourceKeysFor(s, "k8s_namespace_name"); len(got) != 0 {
			t.Errorf("got %v, want empty (dot form already reached by the candidate chain)", got)
		}
	})

	// A dedicated-column-backed key (service.name -> ServiceName) must
	// never be surfaced here even when allowlisted: the dedicated path
	// already owns it, and letting it through would double-promote the
	// key exactly the way [DedicatedResourceLabelExcluded] exists to
	// prevent.
	t.Run("dedicated_column_key_never_promoted", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics() // ServiceNameColumn is set.
		s.PromResourceLabels = []string{"service.name"}
		if got := configuredResourceKeysFor(s, "service_name"); len(got) != 0 {
			t.Errorf("got %v, want empty (service.name is dedicated-column-backed)", got)
		}
	})
}

// TestDedicatedLabelColumns pins the configured-columns list a catalog
// Project must additionally carry alongside Attributes: empty when the
// schema has no dedicated top-level label column, and the ServiceName
// column when the schema configures one.
func TestDedicatedLabelColumns(t *testing.T) {
	t.Parallel()

	t.Run("no_dedicated_columns_configured", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		s.ServiceNameColumn = ""
		if got := dedicatedLabelColumns(s); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("service_name_column_configured", func(t *testing.T) {
		t.Parallel()
		s := schema.DefaultOTelMetrics()
		want := []string{s.ServiceNameColumn}
		if got := dedicatedLabelColumns(s); !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

// TestCatalogLabelValuesExpr pins [catalogLabelValuesExpr]'s two
// resolution paths: `__name__` reads the dedicated MetricName column
// directly (BYPASSING [rawLabelValueExpr] entirely, since MetricName is
// not attribute-map-backed), while every other label MUST resolve
// identically to [rawLabelValueExpr] — the matcher path's own
// resolution — because the doc contract on both functions requires
// listing and matching to agree by construction: any divergence would
// let /label/<name>/values advertise a value no selector on that label
// can actually match, or vice versa.
func TestCatalogLabelValuesExpr(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()

	t.Run("metric_name_reads_dedicated_column", func(t *testing.T) {
		t.Parallel()
		got, ok := catalogLabelValuesExpr(s, "__name__").(*chplan.ColumnRef)
		if !ok || got.Name != s.MetricNameColumn {
			t.Errorf("got %#v, want ColumnRef{Name: %q}", catalogLabelValuesExpr(s, "__name__"), s.MetricNameColumn)
		}
	})

	t.Run("dedicated_column_label_matches_matcher_resolution", func(t *testing.T) {
		t.Parallel()
		got := catalogLabelValuesExpr(s, "service_name")
		want := rawLabelValueExpr(s, "service_name")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("catalogLabelValuesExpr(service_name) = %#v,\nwant rawLabelValueExpr(service_name) = %#v", got, want)
		}
	})

	t.Run("generic_label_matches_matcher_resolution", func(t *testing.T) {
		t.Parallel()
		got := catalogLabelValuesExpr(s, "k8s_namespace_name")
		want := rawLabelValueExpr(s, "k8s_namespace_name")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("catalogLabelValuesExpr(k8s_namespace_name) = %#v,\nwant rawLabelValueExpr(k8s_namespace_name) = %#v", got, want)
		}
	})
}
