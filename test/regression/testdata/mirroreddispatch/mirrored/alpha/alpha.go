// Package alpha holds the original classifier.
package alpha

import "ir"

// isMatrixShape is the complete answer: it knows about the native window
// kind, and it stops at a Project that re-keys the rows.
//
//lint:ignore U1000 fixture
func isMatrixShape(n ir.Node) bool { // WANT
	switch v := n.(type) {
	case *ir.Window:
		return v.Outer > 0
	case *ir.NativeWindow:
		return true
	case *ir.Filter:
		return isMatrixShape(v.Input)
	}
	return false
}
