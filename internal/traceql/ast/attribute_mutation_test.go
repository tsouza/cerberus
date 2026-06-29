package ast

import "testing"

// Mutation-coverage tests for attribute.go: intrinsic resolution rules and the
// per-intrinsic implied type.

// TestNewScopedAttributeIntrinsicResolution pins the guard that only the bare,
// unscoped, non-parent form resolves an intrinsic. The guard is
// `scope == AttributeScopeNone && !parent`:
//   - negating the first clause makes the scoped form resolve an intrinsic;
//   - turning `&&` into `||` makes the scoped OR parent form resolve one.
//
// Each row below distinguishes one of those mutations.
func TestNewScopedAttributeIntrinsicResolution(t *testing.T) {
	t.Parallel()

	// Bare unscoped non-parent: "duration" IS the intrinsic.
	if got := NewScopedAttribute(AttributeScopeNone, false, "duration").Intrinsic; got != IntrinsicDuration {
		t.Errorf("unscoped duration intrinsic = %v; want IntrinsicDuration", got)
	}
	// Scoped: must NOT resolve (kills `scope == None` negation and `&&`→`||`).
	if got := NewScopedAttribute(AttributeScopeSpan, false, "duration").Intrinsic; got != IntrinsicNone {
		t.Errorf("span.duration intrinsic = %v; want IntrinsicNone", got)
	}
	if got := NewScopedAttribute(AttributeScopeResource, false, "name").Intrinsic; got != IntrinsicNone {
		t.Errorf("resource.name intrinsic = %v; want IntrinsicNone", got)
	}
	// Parent-qualified unscoped: must NOT resolve (kills the `!parent` clause
	// and `&&`→`||`).
	if got := NewScopedAttribute(AttributeScopeNone, true, "duration").Intrinsic; got != IntrinsicNone {
		t.Errorf("parent.duration intrinsic = %v; want IntrinsicNone", got)
	}
	// Scope/parent fields are preserved verbatim.
	a := NewScopedAttribute(AttributeScopeResource, true, "service.name")
	if a.Scope != AttributeScopeResource || !a.Parent || a.Name != "service.name" {
		t.Errorf("NewScopedAttribute fields = %+v", a)
	}
}

// TestNewAttributeResolvesIntrinsic pins the unscoped constructor.
func TestNewAttributeResolvesIntrinsic(t *testing.T) {
	t.Parallel()
	if got := NewAttribute("status").Intrinsic; got != IntrinsicStatus {
		t.Errorf("NewAttribute(status).Intrinsic = %v; want IntrinsicStatus", got)
	}
	if got := NewAttribute("http.method").Intrinsic; got != IntrinsicNone {
		t.Errorf("NewAttribute(http.method).Intrinsic = %v; want IntrinsicNone", got)
	}
}

// TestNewIntrinsic pins the intrinsic-named constructor.
func TestNewIntrinsic(t *testing.T) {
	t.Parallel()
	a := NewIntrinsic(IntrinsicKind)
	if a.Intrinsic != IntrinsicKind {
		t.Errorf("Intrinsic = %v; want IntrinsicKind", a.Intrinsic)
	}
	if a.Name != "kind" {
		t.Errorf("Name = %q; want kind", a.Name)
	}
	if a.Scope != AttributeScopeNone {
		t.Errorf("Scope = %v; want none", a.Scope)
	}
}

// TestAttributeString pins the surface rendering of an attribute reference:
// a top-level user attribute is dotted (`.name`), a top-level intrinsic is
// bare, and scope/parent qualifiers prefix the name. These distinguish the
// `len(scopes) > 0` and `prefix == "" && Intrinsic == None && len(name) > 0`
// branches that drive the prefix.
func TestAttributeString(t *testing.T) {
	t.Parallel()
	cases := []struct {
		attr Attribute
		want string
	}{
		{NewAttribute("http.method"), ".http.method"},
		{NewIntrinsic(IntrinsicDuration), "duration"},
		{NewScopedAttribute(AttributeScopeSpan, false, "foo"), "span.foo"},
		{NewScopedAttribute(AttributeScopeResource, false, "service.name"), "resource.service.name"},
		{NewScopedAttribute(AttributeScopeNone, true, "foo"), "parent.foo"},
		{NewScopedAttribute(AttributeScopeResource, true, "x"), "parent.resource.x"},
		// Empty bare name renders empty (no spurious "." prefix). Pins the
		// `len(name) > 0` boundary: a `>= 0` mutant would prepend ".".
		{NewAttribute(""), ""},
	}
	for _, c := range cases {
		if got := c.attr.String(); got != c.want {
			t.Errorf("Attribute.String() = %q; want %q", got, c.want)
		}
	}
}

// TestAttributeImpliedType pins the static type each intrinsic resolves to.
// A wrong branch in the impliedType switch mis-types one of these.
func TestAttributeImpliedType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   Intrinsic
		want StaticType
	}{
		{IntrinsicDuration, TypeDuration},
		{IntrinsicEventTimeSinceStart, TypeDuration},
		{IntrinsicTraceDuration, TypeDuration},
		{IntrinsicChildCount, TypeInt},
		{IntrinsicNestedSetLeft, TypeInt},
		{IntrinsicNestedSetRight, TypeInt},
		{IntrinsicNestedSetParent, TypeInt},
		{IntrinsicName, TypeString},
		{IntrinsicStatusMessage, TypeString},
		{IntrinsicEventName, TypeString},
		{IntrinsicLinkTraceID, TypeString},
		{IntrinsicLinkSpanID, TypeString},
		{IntrinsicTraceRootService, TypeString},
		{IntrinsicTraceRootSpan, TypeString},
		{IntrinsicTraceID, TypeString},
		{IntrinsicSpanID, TypeString},
		{IntrinsicParentID, TypeString},
		{IntrinsicStatus, TypeStatus},
		{IntrinsicKind, TypeKind},
		{IntrinsicParent, TypeNil},
	}
	for _, c := range cases {
		if got := NewIntrinsic(c.in).impliedType(); got != c.want {
			t.Errorf("impliedType(%v) = %v; want %v", c.in, got, c.want)
		}
	}
	// An ordinary scoped attribute is query-time typed.
	if got := NewScopedAttribute(AttributeScopeSpan, false, "http.method").impliedType(); got != TypeAttribute {
		t.Errorf("impliedType(span.http.method) = %v; want TypeAttribute", got)
	}
}
