package local

import (
	"time"

	"github.com/prometheus/prometheus/promql"
)

// Options configures a local PromQL evaluator. Zero-valued fields fall back to
// safe defaults appropriate for an in-process oracle.
type Options struct {
	// MaxSamples caps the number of samples a single query may load. Defaults
	// to 50_000_000, matching Prometheus's default in production setups.
	MaxSamples int
	// Timeout is the per-query wall-clock budget. Defaults to 2 minutes.
	Timeout time.Duration
	// LookbackDelta is the time since the last sample after which a series is
	// considered stale. Defaults to 5 minutes (Prometheus's default).
	LookbackDelta time.Duration
}

// Engine is a thin wrapper around promql.Engine that exposes the cerberus
// shadow-mode oracle surface: Instant and Range query evaluation against a
// caller-supplied SampleStore.
type Engine struct {
	engine *promql.Engine
}

// NewEngine constructs a local PromQL evaluator with the provided options.
// The wrapped promql.Engine has @ modifier and negative offsets enabled to
// mirror modern Prometheus defaults.
func NewEngine(opts Options) *Engine {
	if opts.MaxSamples == 0 {
		opts.MaxSamples = 50_000_000
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Minute
	}
	if opts.LookbackDelta == 0 {
		opts.LookbackDelta = 5 * time.Minute
	}
	return &Engine{
		engine: promql.NewEngine(promql.EngineOpts{
			MaxSamples:           opts.MaxSamples,
			Timeout:              opts.Timeout,
			LookbackDelta:        opts.LookbackDelta,
			EnableAtModifier:     true,
			EnableNegativeOffset: true,
		}),
	}
}
