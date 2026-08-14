// Package ir stands in for chplan: a sealed node interface with several
// kinds, closed by an unexported niladic marker.
package ir

// Node is the sealed interface the classifiers below switch over.
type Node interface {
	planNode()
}

type Scan struct{}

type Filter struct{ Input Node }

type Project struct{ Input Node }

type Window struct{ Outer int }

type NativeWindow struct{}

func (*Scan) planNode()         {}
func (*Filter) planNode()       {}
func (*Project) planNode()      {}
func (*Window) planNode()       {}
func (*NativeWindow) planNode() {}
