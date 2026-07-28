package main

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/config"
)

// The windowless Prom metadata scan is bounded by a lookback, and the whole
// point of deriving it here is that the bound must track the deployment's
// REAL retention. Getting the precedence wrong either under-scans (silently
// omitting metrics reference Prometheus would return) or leaves the bound
// at a fallback that has nothing to do with the data actually retained.
func TestPromMetadataLookback_Precedence(t *testing.T) {
	t.Parallel()

	const (
		explicit   = 30 * 24 * time.Hour
		globalTTL  = 60 * 24 * time.Hour
		metricsTTL = 90 * 24 * time.Hour
	)

	cases := []struct {
		name string
		cfg  config.Config
		want time.Duration
	}{
		{
			name: "explicit lookback wins over every provisioned TTL",
			cfg: config.Config{
				PromMetadataLookback: explicit,
				SchemaProvisioning:   config.SchemaProvisioning{TTL: globalTTL, TTLMetrics: metricsTTL},
			},
			want: explicit,
		},
		{
			name: "per-signal metrics TTL wins over the global TTL",
			cfg: config.Config{
				SchemaProvisioning: config.SchemaProvisioning{TTL: globalTTL, TTLMetrics: metricsTTL},
			},
			want: metricsTTL,
		},
		{
			name: "global TTL applies when no per-signal override is set",
			cfg: config.Config{
				SchemaProvisioning: config.SchemaProvisioning{TTL: globalTTL},
			},
			want: globalTTL,
		},
		{
			name: "no lookback and no TTL leaves the handler on its fallback",
			cfg:  config.Config{},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := promMetadataLookback(tc.cfg); got != tc.want {
				t.Fatalf("promMetadataLookback = %s, want %s", got, tc.want)
			}
		})
	}
}
