package ast

import (
	"strings"
)

// Attribute is a reference to a span/resource/trace field or an intrinsic.
// All four fields are exported because lowering reads them directly and
// compares Attribute values for the zero value, so the struct is kept
// comparable (no slice/map fields).
type Attribute struct {
	Name      string
	Scope     AttributeScope
	Parent    bool
	Intrinsic Intrinsic
}

func (Attribute) isFieldExpression() {}

// impliedType returns the static type an intrinsic resolves to; ordinary
// scoped attributes are query-time typed (TypeAttribute).
func (a Attribute) impliedType() StaticType {
	switch a.Intrinsic {
	case IntrinsicDuration, IntrinsicEventTimeSinceStart, IntrinsicTraceDuration:
		return TypeDuration
	case IntrinsicChildCount, IntrinsicNestedSetLeft, IntrinsicNestedSetRight, IntrinsicNestedSetParent:
		return TypeInt
	case IntrinsicName, IntrinsicStatusMessage, IntrinsicEventName,
		IntrinsicLinkTraceID, IntrinsicLinkSpanID, IntrinsicTraceRootService,
		IntrinsicTraceRootSpan, IntrinsicTraceID, IntrinsicSpanID, IntrinsicParentID:
		return TypeString
	case IntrinsicStatus:
		return TypeStatus
	case IntrinsicKind:
		return TypeKind
	case IntrinsicParent:
		return TypeNil
	default:
		return TypeAttribute
	}
}

// NewAttribute builds an unscoped attribute, resolving the name to an
// intrinsic when it names one.
func NewAttribute(name string) Attribute {
	return Attribute{Name: name, Intrinsic: intrinsicFromString(name)}
}

// NewScopedAttribute builds a scoped (or parent-qualified) attribute. An
// intrinsic is only resolved for the bare unscoped, non-parent form, where
// a name like `duration` is a built-in rather than a user attribute.
func NewScopedAttribute(scope AttributeScope, parent bool, name string) Attribute {
	intrinsic := IntrinsicNone
	if scope == AttributeScopeNone && !parent {
		intrinsic = intrinsicFromString(name)
	}
	return Attribute{Scope: scope, Parent: parent, Name: name, Intrinsic: intrinsic}
}

// NewIntrinsic builds an attribute that refers to the given intrinsic.
func NewIntrinsic(n Intrinsic) Attribute {
	return Attribute{Scope: AttributeScopeNone, Name: n.String(), Intrinsic: n}
}

func (a Attribute) String() string {
	var scopes []string
	if a.Parent {
		scopes = append(scopes, "parent")
	}
	if a.Scope != AttributeScopeNone {
		scopes = append(scopes, a.Scope.String())
	}

	name := a.Name
	if a.Intrinsic != IntrinsicNone {
		name = a.Intrinsic.String()
	}

	prefix := ""
	if len(scopes) > 0 {
		prefix = strings.Join(scopes, ".") + "."
	}
	// A top-level user attribute is written `.name`; a top-level intrinsic
	// is written bare.
	if prefix == "" && a.Intrinsic == IntrinsicNone && len(name) > 0 {
		prefix = "."
	}

	if containsNonAttributeRune(name) {
		name = "\"" + name + "\""
	}
	return prefix + name
}

// containsNonAttributeRune reports whether s holds any rune that cannot
// appear in a bare attribute name and therefore forces quoting. It is the
// exact inverse of the lexer's isAttributeRune, so the disallowed set (the
// structural punctuation plus whitespace) is defined in one place only.
func containsNonAttributeRune(s string) bool {
	for _, r := range s {
		if !isAttributeRune(r) {
			return true
		}
	}
	return false
}
