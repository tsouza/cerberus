// Package restated is a fixture for the sealed-marker rules. It is never
// compiled: the rules parse it. Three implementers, so the fixture sits
// above the minimum sealed-set size.
package restated

type Shape interface{ shapeNode() }

type (
	Square   struct{}
	Circle   struct{}
	Triangle struct{}
)

func (*Square) shapeNode()   {}
func (*Circle) shapeNode()   {}
func (*Triangle) shapeNode() {}
