// Package spec — round-trip execution options.
//
// [RunRoundTrip] is head-agnostic: it seeds a chDB session, executes a
// fixture's recorded SQL and pins the rows against `expected_rows:`. One
// thing about that contract is NOT head-agnostic, though — which SQL a
// fixture's `-- sql --` section actually holds.
//
// A fixture records the SQL its own lowering test produced, and for some
// heads that is an EARLIER pipeline stage than the one production sends
// to ClickHouse. The head that knows this owns the knowledge of how to
// close the gap; the shared runner only needs to be told that a gap
// exists. This file is that seam: a lane passes a [SQLReconstructor] and
// the runner applies it, without the runner ever naming a query language.
package spec

// SQLReconstructor rewrites the SQL and args a fixture recorded into the
// form production actually executes for that fixture's query.
//
// The three-valued result mirrors the reconstruction contract the heads
// already use:
//
//   - (sql, args, true, nil)   — reconstructed; execute this instead.
//   - ("", nil, false, nil)    — this fixture needs no reconstruction;
//     the runner executes the recorded SQL untouched.
//   - ("", nil, false, err)    — the fixture is faulty and the run fails.
type SQLReconstructor func(c *Case) (sqlText string, args []any, ok bool, err error)

// RoundTripOption customises a [RunRoundTrip] execution.
type RoundTripOption func(*roundTripConfig)

// roundTripConfig is the resolved option set for one round-trip run.
type roundTripConfig struct {
	// reconstruct, when non-nil, is consulted before execution and may
	// replace the fixture's recorded SQL/args. nil means "execute what
	// the fixture recorded", which is what every lane that captures its
	// SQL at the stage production sends wants.
	reconstruct SQLReconstructor
}

// WithSQLReconstructor makes [RunRoundTrip] consult fn before executing a
// fixture, so a lane whose fixtures are captured before a pipeline stage
// that production always applies executes the post-stage SQL instead.
func WithSQLReconstructor(fn SQLReconstructor) RoundTripOption {
	return func(cfg *roundTripConfig) { cfg.reconstruct = fn }
}

// newRoundTripConfig folds opts into a resolved config.
func newRoundTripConfig(opts []RoundTripOption) roundTripConfig {
	var cfg roundTripConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}
