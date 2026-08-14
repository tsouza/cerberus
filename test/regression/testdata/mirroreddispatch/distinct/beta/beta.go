// Package beta asks a different question about the same interface, and
// says so in its name.
package beta

import "ir"

// bottomsOutAtWindow asks only whether a window exists somewhere below,
// crossing any re-keying node, because its caller resolves the column
// name separately.
//
//lint:ignore U1000 fixture
func bottomsOutAtWindow(n ir.Node) bool {
	switch v := n.(type) {
	case *ir.Window:
		return v.Outer > 0
	case *ir.Filter:
		return bottomsOutAtWindow(v.Input)
	case *ir.Project:
		return bottomsOutAtWindow(v.Input)
	}
	return false
}

//lint:ignore U1000 fixture
func unwrapWindow(n ir.Node) (*ir.Window, bool) {
	switch v := n.(type) {
	case *ir.Window:
		return v, true
	case *ir.Project:
		return unwrapWindow(v.Input)
	}
	return nil, false
}
