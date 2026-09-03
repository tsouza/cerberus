package tempo

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/schema"
)

// This file is cerberus issue #3019's completeness ratchet for the
// Tempo tag-scope enumeration: internal package tests (package tempo, not
// tempo_test) because they reference allAttrMapScopes,
// allCatalogCoveredTagScopes, and catalogScopeMode directly rather than
// through the *ForTest re-exports in export_test.go. Each test below
// checks one of the ~7 switches the issue found independently re-encoding
// the same scope vocabulary against the canonical list it should have been
// deriving from all along — see allAttrMapScopes' and
// allCatalogCoveredTagScopes' own doc comments for the full picture, and
// each function's doc for why it either got restructured (an explicit
// case per scope, panicking on anything else) or was already
// correct-by-construction and only needed a pinning test here.

// TestAttrMapScope_StringCoversEveryScope pins attrMapScope.String() over
// every canonical scope, plus the fallback shape for a value none of them
// name — proving the fallback no longer silently answers "any" (a real,
// meaningful value) for an unrecognised scope.
func TestAttrMapScope_StringCoversEveryScope(t *testing.T) {
	t.Parallel()
	if len(allAttrMapScopes) != wantAttrMapScopeCount {
		t.Fatalf("allAttrMapScopes has %d entries, want %d — update wantAttrMapScopeCount alongside "+
			"any deliberate change to the scope set", len(allAttrMapScopes), wantAttrMapScopeCount)
	}
	want := map[attrMapScope]string{
		attrMapScopeAny:             "any",
		attrMapScopeResource:        "resource",
		attrMapScopeSpan:            "span",
		attrMapScopeEvent:           "event",
		attrMapScopeLink:            "link",
		attrMapScopeInstrumentation: "instrumentation",
	}
	for _, s := range allAttrMapScopes {
		if got, want := s.String(), want[s]; got != want {
			t.Errorf("attrMapScope(%d).String() = %q, want %q", int(s), got, want)
		}
	}

	const unknownScope = attrMapScope(99)
	if got := unknownScope.String(); got == "any" || !strings.Contains(got, "99") {
		t.Errorf(`attrMapScope(99).String() = %q, want it to name the unhandled value (contain `+
			`"99"), not silently answer "any" the way an unlabelled default used to`, got)
	}
}

// TestResolveTagName_CoversEveryAttributeScope pins resolveTagName's
// traceql.AttributeScope -> attrMapScope mapping over one URL form per
// canonical attrMapScope, then cross-checks that the union of MapScope
// values it exercises is exactly allAttrMapScopes — so a scope silently
// left off both this table AND resolveTagName's switch (the "two
// independently-incomplete things agree by omission" trap) still fails
// here by name.
func TestResolveTagName_CoversEveryAttributeScope(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.ScopeAttributesColumn = "ScopeAttributes"

	cases := []struct {
		name    string
		urlName string
		want    attrMapScope
	}{
		{"resource-scoped", "resource.service.name", attrMapScopeResource},
		{"span-scoped", "span.http.method", attrMapScopeSpan},
		{"event-scoped", "event.exception.message", attrMapScopeEvent},
		{"link-scoped", "link.opentracing.ref_type", attrMapScopeLink},
		{"instrumentation-scoped", "instrumentation.name", attrMapScopeInstrumentation},
		{"auto-scope leading-dot", ".service.name", attrMapScopeAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolved, err := resolveTagName(tc.urlName, s)
			if err != nil {
				t.Fatalf("resolveTagName(%q): %v", tc.urlName, err)
			}
			if resolved.MapScope != tc.want {
				t.Errorf("resolveTagName(%q).MapScope = %v, want %v", tc.urlName, resolved.MapScope, tc.want)
			}
		})
	}

	exercised := make(map[attrMapScope]bool, len(cases))
	for _, tc := range cases {
		exercised[tc.want] = true
	}
	for _, want := range allAttrMapScopes {
		if !exercised[want] {
			t.Errorf("allAttrMapScopes contains %v with no resolveTagName case above producing it — add a row", want)
		}
	}
}

// TestBuildAttributeValuesSQL_HandlesEveryScope pins that the final,
// SQL-shape-deciding switch in buildAttributeValuesSQL (cerberus issue
// #3019) explicitly handles every canonical attrMapScope — none of them
// panics — and that a scope none of the six name panics loudly instead of
// silently rendering the attrMapScopeAny SQL shape (what an unlabelled
// `default:` used to do).
func TestBuildAttributeValuesSQL_HandlesEveryScope(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	s.ScopeAttributesColumn = "ScopeAttributes"

	for _, scope := range allAttrMapScopes {
		t.Run(scope.String(), func(t *testing.T) {
			t.Parallel()
			sqlStr, _ := buildAttributeValuesSQL(s, "some.key", scope, nil, time.Time{}, time.Time{})
			if sqlStr == "" {
				t.Errorf("buildAttributeValuesSQL(%v) produced empty SQL", scope)
			}
		})
	}

	t.Run("unrecognised scope panics rather than silently building attrMapScopeAny SQL", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected buildAttributeValuesSQL to panic for an unrecognised attrMapScope")
			}
		}()
		buildAttributeValuesSQL(s, "some.key", attrMapScope(99), nil, time.Time{}, time.Time{})
	})
}

// TestCatalogScopeForMapScope_CoversEveryScope is the completeness test for
// the site cerberus issue #3019 proved non-exhaustive: it pins the exact
// (scope, mode) catalogScopeForMapScope returns for every canonical
// attrMapScope, and that an unrecognised one panics instead of falling
// through the old unlabelled default (which used to silently mean
// "treat as attrMapScopeAny").
func TestCatalogScopeForMapScope_CoversEveryScope(t *testing.T) {
	t.Parallel()
	want := map[attrMapScope]struct {
		scope string
		mode  catalogScopeMode
	}{
		attrMapScopeResource:        {schema.TagCatalogScopeResource, catalogScopeSingle},
		attrMapScopeSpan:            {schema.TagCatalogScopeSpan, catalogScopeSingle},
		attrMapScopeEvent:           {schema.TagCatalogScopeEvent, catalogScopeSingle},
		attrMapScopeLink:            {schema.TagCatalogScopeLink, catalogScopeSingle},
		attrMapScopeAny:             {"", catalogScopeUnion},
		attrMapScopeInstrumentation: {"", catalogScopeUncovered},
	}
	if len(want) != wantAttrMapScopeCount {
		t.Fatalf("want table has %d entries, expected one per allAttrMapScopes entry (%d)",
			len(want), wantAttrMapScopeCount)
	}
	for _, s := range allAttrMapScopes {
		gotScope, gotMode := catalogScopeForMapScope(s)
		wantEntry := want[s]
		if gotScope != wantEntry.scope || gotMode != wantEntry.mode {
			t.Errorf("catalogScopeForMapScope(%v) = (%q, %v), want (%q, %v)",
				s, gotScope, gotMode, wantEntry.scope, wantEntry.mode)
		}
	}

	t.Run("unrecognised scope panics", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected catalogScopeForMapScope to panic for an unrecognised attrMapScope")
			}
		}()
		catalogScopeForMapScope(attrMapScope(99))
	})
}

// TestCatalogScopesFor_CoversEveryCatalogScope pins catalogScopesFor over
// every scope allCatalogCoveredTagScopes lists, plus "none" (the umbrella
// that expands to all of them), and that a scope tagsCatalogEligible would
// already have rejected panics here too, rather than this function's own
// (now removed) unlabelled default silently treating it as "none".
func TestCatalogScopesFor_CoversEveryCatalogScope(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		tagScopeResource: schema.TagCatalogScopeResource,
		tagScopeSpan:     schema.TagCatalogScopeSpan,
		tagScopeEvent:    schema.TagCatalogScopeEvent,
		tagScopeLink:     schema.TagCatalogScopeLink,
	}
	if len(want) != len(allCatalogCoveredTagScopes) {
		t.Fatalf("want table has %d entries, allCatalogCoveredTagScopes has %d",
			len(want), len(allCatalogCoveredTagScopes))
	}
	for _, scope := range allCatalogCoveredTagScopes {
		got := catalogScopesFor(scope)
		if len(got) != 1 || got[0] != want[scope] {
			t.Errorf("catalogScopesFor(%q) = %v, want [%q]", scope, got, want[scope])
		}
	}

	gotNone := catalogScopesFor(tagScopeNone)
	wantNone := []string{
		schema.TagCatalogScopeResource, schema.TagCatalogScopeSpan,
		schema.TagCatalogScopeEvent, schema.TagCatalogScopeLink,
	}
	if !slices.Equal(gotNone, wantNone) {
		t.Errorf("catalogScopesFor(tagScopeNone) = %v, want %v", gotNone, wantNone)
	}

	t.Run("unrecognised scope panics", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected catalogScopesFor to panic for a scope tagsCatalogEligible would have rejected")
			}
		}()
		catalogScopesFor("bogus")
	})
}

// TestTagsCatalogEligible_CoversEveryCatalogScope pins tagsCatalogEligible
// against the SAME canonical allCatalogCoveredTagScopes list
// catalogScopesFor is checked against, so the two functions cannot drift
// apart on which scopes the catalog covers without one of these tests
// noticing. tagsCatalogEligible itself needed no restructuring — its
// `default: return false` is unambiguous (false has exactly one meaning,
// unlike a default that returns a real scope value) — this test only pins
// the contract.
func TestTagsCatalogEligible_CoversEveryCatalogScope(t *testing.T) {
	t.Parallel()
	for _, scope := range allCatalogCoveredTagScopes {
		if !tagsCatalogEligible(scope, nil, true, false) {
			t.Errorf("tagsCatalogEligible(%q, nil, windowless=true, scopeAttrsConfigured=false) = false, "+
				"want true — every catalog-covered scope must be eligible under the base conditions", scope)
		}
	}
	for _, scope := range []string{tagScopeInstrumentation, tagScopeIntrinsic, tagScopeTrace, "bogus"} {
		if tagsCatalogEligible(scope, nil, true, false) {
			t.Errorf("tagsCatalogEligible(%q, nil, windowless=true, scopeAttrsConfigured=false) = true, "+
				"want false — this scope carries no catalog arm", scope)
		}
	}
}
