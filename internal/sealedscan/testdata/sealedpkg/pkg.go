// Package sealedpkg is a fixture for the sealedscan tests. It is not
// built as part of cerberus: it lives under testdata precisely so the
// scanner can be pointed at a known shape, including the shapes it must
// REFUSE to treat as sealing markers.
package sealedpkg

// Shape is sealed by shapeNode(): three implementers.
type Shape interface {
	shapeNode()
	Area() int
}

// Colour is sealed by colourNode(): two implementers, the floor.
type Colour interface{ colourNode() }

// Lonely has a single implementer and so is below the floor.
type Lonely interface{ lonelyNode() }

// Exported is not a sealing marker: the method is exported.
type Exported interface{ ExportedNode() }

// Parametrised is not a sealing marker: the method takes an argument.
type Parametrised interface{ paramNode(int) }

type (
	Square   struct{}
	Circle   struct{}
	Triangle struct{}
	Red      struct{}
	Blue     struct{}
	Only     struct{}
	Busy     struct{}
)

func (*Square) shapeNode()   {}
func (*Circle) shapeNode()   {}
func (*Triangle) shapeNode() {}
func (*Square) Area() int    { return 0 }
func (*Circle) Area() int    { return 0 }
func (*Triangle) Area() int  { return 0 }

func (*Red) colourNode()  {}
func (*Blue) colourNode() {}

func (*Only) lonelyNode() {}

// busyNode has a body, so it is ordinary behaviour rather than a
// sealing marker even though it is unexported and niladic.
func (*Busy) busyNode() { _ = 1 }

// --- the embedded-base sealing pattern, as internal/logql/lsyntax uses
// it: the marker is declared ONCE on a shared base struct, and the
// members embed the base to acquire it by method promotion. Counting
// declarations alone would derive a set of one here. ---

// Sound is sealed by soundNode(), supplied via soundBase.
type Sound interface{ soundNode() }

type soundBase struct{}

func (soundBase) soundNode() {}

type (
	Bark struct {
		soundBase
		Loud bool
	}
	Meow struct{ soundBase }
	Moo  struct{ *soundBase }
)
