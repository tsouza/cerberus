package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

func (e *emitter) emitExpr(x chplan.Expr) error {
	switch v := x.(type) {
	case *chplan.ColumnRef:
		writeIdent(&e.b, v.Name)
		return nil
	case *chplan.LitString:
		return e.bindArg(v.V)
	case *chplan.LitInt:
		return e.bindArg(v.V)
	case *chplan.LitFloat:
		return e.bindArg(v.V)
	case *chplan.LitBool:
		return e.bindArg(v.V)
	case *chplan.Binary:
		return e.emitBinary(v)
	case *chplan.FuncCall:
		return e.emitFunc(v)
	case *chplan.MapAccess:
		return e.emitMapAccess(v)
	default:
		return fmt.Errorf("%w: expr %T", ErrUnsupported, x)
	}
}

func (e *emitter) bindArg(v any) error {
	e.b.WriteByte('?')
	e.args = append(e.args, v)
	return nil
}

func (e *emitter) emitBinary(b *chplan.Binary) error {
	// Regex match ops lower to CH function calls (match / NOT match).
	switch b.Op {
	case chplan.OpMatch, chplan.OpNotMatch:
		if b.Op == chplan.OpNotMatch {
			e.b.WriteString("NOT ")
		}
		e.b.WriteString("match(")
		if err := e.emitExpr(b.Left); err != nil {
			return err
		}
		e.b.WriteString(", ")
		if err := e.emitExpr(b.Right); err != nil {
			return err
		}
		e.b.WriteByte(')')
		return nil
	}

	e.b.WriteByte('(')
	if err := e.emitExpr(b.Left); err != nil {
		return err
	}
	fmt.Fprintf(&e.b, " %s ", b.Op)
	if err := e.emitExpr(b.Right); err != nil {
		return err
	}
	e.b.WriteByte(')')
	return nil
}

func (e *emitter) emitMapAccess(m *chplan.MapAccess) error {
	if err := e.emitExpr(m.Map); err != nil {
		return err
	}
	e.b.WriteByte('[')
	if err := e.emitExpr(m.Key); err != nil {
		return err
	}
	e.b.WriteByte(']')
	return nil
}

func (e *emitter) emitFunc(f *chplan.FuncCall) error {
	e.b.WriteString(f.Name)
	e.b.WriteByte('(')
	for i, a := range f.Args {
		if i > 0 {
			e.b.WriteString(", ")
		}
		if err := e.emitExpr(a); err != nil {
			return err
		}
	}
	e.b.WriteByte(')')
	return nil
}
