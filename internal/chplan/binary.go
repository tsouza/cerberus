package chplan

// BinaryOp identifies a binary operator in the IR. The emitter maps each op
// to ClickHouse SQL syntax (or, for regex match ops, a function call).
type BinaryOp string

const (
	OpEq       BinaryOp = "="
	OpNe       BinaryOp = "!="
	OpLt       BinaryOp = "<"
	OpLe       BinaryOp = "<="
	OpGt       BinaryOp = ">"
	OpGe       BinaryOp = ">="
	OpMatch    BinaryOp = "=~" // regex match (Prom/Loki style)
	OpNotMatch BinaryOp = "!~"
	OpAnd      BinaryOp = "AND"
	OpOr       BinaryOp = "OR"
	OpAdd      BinaryOp = "+"
	OpSub      BinaryOp = "-"
	OpMul      BinaryOp = "*"
	OpDiv      BinaryOp = "/"
	OpMod      BinaryOp = "%"
	OpPow      BinaryOp = "^"
	// OpAtan2 is PromQL's `atan2` arithmetic binary operator
	// (two-argument arctangent). ClickHouse has no infix spelling, so
	// the emitter renders it as the `atan2(l, r)` function call —
	// same posture as OpPow → pow(l, r).
	OpAtan2 BinaryOp = "atan2"
)

// Binary is a binary-operator expression.
type Binary struct {
	Op    BinaryOp
	Left  Expr
	Right Expr
}

func (*Binary) exprNode() {}

func (b *Binary) Equal(other Expr) bool {
	o, ok := other.(*Binary)
	if !ok {
		return false
	}
	return b.Op == o.Op && b.Left.Equal(o.Left) && b.Right.Equal(o.Right)
}
