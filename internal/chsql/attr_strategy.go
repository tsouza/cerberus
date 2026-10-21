package chsql

import "github.com/tsouza/cerberus/internal/chplan"

// AttrStrategy, AttrStrategies, AttrStrategyMap and AttrStrategyJSON are
// aliases of the chplan types of the same name — declared there, not here,
// because internal/logql's Lang.EmitSchemaStrategies() returns them inside
// chplan.SchemaStrategies, the shared Lang contract engine.schemaStrategier
// reads, and
// logql is architecturally forbidden from depending on chsql
// (.go-arch-lint.yml); chplan is the one package both logql and chsql
// already depend on. See chplan.AttrStrategy's own doc for the full
// design (the missing-vs-empty semantics decision, the per-signal scoping
// rationale, cerberus issue #2777). Aliasing here means every existing
// chsql.AttrStrategy* reference keeps compiling unchanged.
type (
	AttrStrategy   = chplan.AttrStrategy
	AttrStrategies = chplan.AttrStrategies
)

const (
	AttrStrategyMap  = chplan.AttrStrategyMap
	AttrStrategyJSON = chplan.AttrStrategyJSON
)
