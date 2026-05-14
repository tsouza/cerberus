package chplan

// Scan reads rows from a single ClickHouse table. If Columns is empty, the
// emitter projects `*` and downstream Project nodes can narrow it; otherwise
// only the listed columns are read.
//
// Database is optional; when non-empty the emitter renders the table
// reference as `<Database>`.`<Table>` (both parts backtick-quoted) — used
// for synthetic single-row sources like `system.one` that the no-arg
// date functions scan. Empty Database emits just the bare table name,
// matching the original behaviour for every user-facing table.
type Scan struct {
	Database string
	Table    string
	Columns  []string
}

func (*Scan) planNode() {}

func (*Scan) Children() []Node { return nil }

func (s *Scan) Equal(other Node) bool {
	o, ok := other.(*Scan)
	if !ok || s.Database != o.Database || s.Table != o.Table || len(s.Columns) != len(o.Columns) {
		return false
	}
	for i := range s.Columns {
		if s.Columns[i] != o.Columns[i] {
			return false
		}
	}
	return true
}
