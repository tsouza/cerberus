// Package beta holds the copy, made by copying and then drifted: it never
// learned about the native window kind.
package beta

import "ir"

//lint:ignore U1000 fixture
func isMatrixShape(n ir.Node) bool { // WANT
	switch v := n.(type) {
	case *ir.Window:
		return v.Outer > 0
	case *ir.Filter:
		return isMatrixShape(v.Input)
	}
	return false
}

// isCheap shares no name with anything in alpha, and classifies the same
// interface. Different questions are allowed to differ.
//
//lint:ignore U1000 fixture
func isCheap(n ir.Node) bool {
	switch n.(type) {
	case *ir.Scan:
		return true
	case *ir.Project:
		return false
	}
	return false
}
