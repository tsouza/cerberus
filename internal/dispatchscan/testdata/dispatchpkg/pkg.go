// Package dispatchpkg is the fixture the dispatchscan tests read. It
// holds one sealed interface, the classifier shapes the scan must
// recognise, the near-misses it must not, and the dispatch shapes
// SwitchArms accepts and refuses.
package dispatchpkg

// Node is sealed by an unexported niladic marker, as every IR interface
// in this repository is.
type Node interface {
	planNode()
}

type Scan struct{}

type Filter struct{ Input Node }

type Project struct{ Input Node }

func (*Scan) planNode()    {}
func (*Filter) planNode()  {}
func (*Project) planNode() {}

// isDerived is the shape the scan must find: bool result, sealed
// interface parameter, type switch on that parameter.
func isDerived(n Node) bool {
	switch v := n.(type) {
	case *Scan:
		return false
	case *Filter:
		return isDerived(v.Input)
	case nil:
		return false
	}
	return false
}

// keyedTwice returns two bools, which is still an answer and nothing
// else, so it counts.
func keyedTwice(n Node) (bool, bool) {
	switch n.(type) {
	case *Project:
		return true, true
	}
	return false, false
}

// unwrap returns a node, so it is an unwrapper rather than a
// classification: its arms legitimately differ per caller.
func unwrap(n Node) (*Filter, bool) {
	switch v := n.(type) {
	case *Filter:
		return v, true
	}
	return nil, false
}

// isEmpty takes no sealed interface at all.
func isEmpty(s string) bool {
	return s == ""
}

// switchesOnLocal switches on a local rather than on the parameter.
// Without type information the scan cannot confirm the local still holds
// the sealed value, so it under-reports here rather than recording arms
// it cannot attribute — the conservative direction.
func switchesOnLocal(n Node) bool {
	inner := innerOf(n)
	switch inner.(type) {
	case *Scan:
		return true
	}
	return false
}

// innerOf peels one wrapper, returning a node rather than an answer.
func innerOf(n Node) Node {
	if f, ok := n.(*Filter); ok {
		return f.Input
	}
	return n
}

// dispatchVocabulary is the string-switch shape SwitchArms derives.
func dispatchVocabulary(name string) int {
	switch name {
	case "rate", "irate":
		return 1
	case "sum_over_time":
		return 2
	case "rate":
		return 3
	default:
		return 0
	}
}

// twoSwitches holds more than one expression switch, which SwitchArms
// must refuse rather than guess between.
func twoSwitches(a, b string) int {
	switch a {
	case "x":
		return 1
	}
	switch b {
	case "y":
		return 2
	}
	return 0
}

// intSwitch dispatches on non-string cases, which is not a vocabulary.
func intSwitch(n int) bool {
	switch n {
	case 1:
		return true
	}
	return false
}

// noSwitch holds no expression switch at all.
func noSwitch(n int) int {
	return n + 1
}
