package routerrules

import _ "embed"

// embeddedCatalog is the shipped generic catalog, compiled into the binary so
// the CLI works with no external file. An operator can override it at runtime
// via --catalog <path> to test a newer ruleset. This file is the single audit
// target for the no-numbers invariant.
//
//go:embed catalog/router_rules.yaml
var embeddedCatalog []byte

// EmbeddedCatalog returns the raw bytes of the shipped catalog. The no-numbers
// guard test loads these bytes directly so the reviewer-facing proof runs
// against exactly what ships.
func EmbeddedCatalog() []byte {
	out := make([]byte, len(embeddedCatalog))
	copy(out, embeddedCatalog)
	return out
}
