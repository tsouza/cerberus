package spec

// Statement is one emitted SQL statement and its positional args.
type Statement struct {
	SQL  string
	Args []any
}
