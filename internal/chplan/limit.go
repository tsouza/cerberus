package chplan

// Limit caps the row count flowing out of its input — a SQL LIMIT clause.
// Negative or zero Count is treated as "no limit" by the emitter.
type Limit struct {
	Input Node
	Count int64
}

func (*Limit) planNode() {}

func (l *Limit) Children() []Node { return []Node{l.Input} }

func (l *Limit) Equal(other Node) bool {
	o, ok := other.(*Limit)
	if !ok {
		return false
	}
	return l.Count == o.Count && l.Input.Equal(o.Input)
}
