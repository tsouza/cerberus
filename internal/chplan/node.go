package chplan

// Node is a vertex in the cerberus query plan tree.
//
// All concrete operators (Scan, Filter, Project, Aggregate, RangeWindow,
// Limit, ...) implement Node. Optimizer rules rewrite Node trees; the chsql
// emitter walks them to produce ClickHouse SQL.
type Node interface {
	// planNode is a sealing marker so external packages can't accidentally
	// satisfy this interface.
	planNode()

	// Children returns the direct input children of this node, in
	// left-to-right order, for visitor-style traversal.
	Children() []Node

	// Equal reports structural equality with another Node, recursively. Used
	// by optimizer rule tests to compare before/after plans.
	Equal(Node) bool
}

// Walk visits n and every node reachable from it in depth-first pre-order.
// The visit function returns false to skip a node's children.
func Walk(n Node, visit func(Node) bool) {
	if n == nil || !visit(n) {
		return
	}
	for _, c := range n.Children() {
		Walk(c, visit)
	}
}
