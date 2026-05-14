//go:build chdb

package chclienttest_test

import (
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
)

// Compile-time guard: *chclienttest.Client must satisfy every handler's
// Querier interface. If any handler grows a method (or one drifts to a
// different signature) this file fails to build and the divergence is
// caught at PR time, not at test-run time.
var (
	_ prom.Querier  = (*chclienttest.Client)(nil)
	_ loki.Querier  = (*chclienttest.Client)(nil)
	_ tempo.Querier = (*chclienttest.Client)(nil)
)
