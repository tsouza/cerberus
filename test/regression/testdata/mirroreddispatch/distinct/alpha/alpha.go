// Package alpha classifies the same interface as beta, under a name that
// states its own question. Nothing here is flagged.
package alpha

import "ir"

// exposesAnchorColumn asks whether the anchor column is readable off the
// output scope — the question its own caller needs.
//
//lint:ignore U1000 fixture
func exposesAnchorColumn(n ir.Node) bool {
	switch v := n.(type) {
	case *ir.Window:
		return v.Outer > 0
	case *ir.NativeWindow:
		return true
	case *ir.Filter:
		return exposesAnchorColumn(v.Input)
	}
	return false
}

// unwrapWindow shares its name with beta's, but returns a node rather
// than only a bool: an unwrapper's arms legitimately differ per caller,
// so it is out of the rule's scope.
//
//lint:ignore U1000 fixture
func unwrapWindow(n ir.Node) (*ir.Window, bool) {
	switch v := n.(type) {
	case *ir.Window:
		return v, true
	case *ir.Filter:
		return unwrapWindow(v.Input)
	}
	return nil, false
}

// isLeaf and the isLeaf in this same file's sibling below share a name
// within ONE package, which is not a cross-package mirror.
//
//lint:ignore U1000 fixture
func isLeaf(n ir.Node) bool {
	switch n.(type) {
	case *ir.Scan:
		return true
	}
	return false
}
