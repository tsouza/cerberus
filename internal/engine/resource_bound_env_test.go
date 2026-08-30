package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestResourceBoundsFromEnv_DefaultsToZeroUnset pins the "operator did not
// override" sentinel: with none of the four CERBERUS_CH_* vars
// set, every field resolves to 0 — the value applyResourceBoundOverrides
// treats as "thread nothing onto ctx, let chsql fall back to its own
// compiled-in default" (see ResourceBoundOverrides' own doc).
func TestResourceBoundsFromEnv_DefaultsToZeroUnset(t *testing.T) {
	t.Setenv(EnvRangeBucketFanoutMaxRows, "")
	t.Setenv(EnvRangeLWRFanoutMaxRows, "")
	t.Setenv(EnvRateWindowFanoutMaxRows, "")
	t.Setenv(EnvMaxEmittedSQLBytes, "")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	want := ResourceBoundOverrides{}
	if got != want {
		t.Fatalf("ResourceBoundsFromEnv() = %+v, want the zero value %+v", got, want)
	}
}

// TestResourceBoundsFromEnv_OverridesEachIndependently confirms each of the
// four env vars maps onto its own field, independent of the others —
// the exact "an operator override actually reaches the resolved config"
// contract issue #2667 exists to establish, extended to issue #2733's
// statement-size bound.
func TestResourceBoundsFromEnv_OverridesEachIndependently(t *testing.T) {
	t.Setenv(EnvRangeBucketFanoutMaxRows, "123")
	t.Setenv(EnvRangeLWRFanoutMaxRows, "456")
	t.Setenv(EnvRateWindowFanoutMaxRows, "789")
	t.Setenv(EnvMaxEmittedSQLBytes, "1048576")

	got, err := ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv() error = %v", err)
	}
	want := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows: 123,
		RangeLWRFanoutMaxRows:    456,
		RateWindowFanoutMaxRows:  789,
		MaxEmittedSQLBytes:       1048576,
	}
	if got != want {
		t.Fatalf("ResourceBoundsFromEnv() = %+v, want %+v", got, want)
	}
}

// TestResourceBoundsFromEnv_RejectsMalformedValue confirms a typo'd env var
// fails startup with a wrapped, identifiable error rather than silently
// falling back to 0 (which would silently discard the operator's actual
// intent to override).
func TestResourceBoundsFromEnv_RejectsMalformedValue(t *testing.T) {
	for _, key := range []string{
		EnvRangeBucketFanoutMaxRows, EnvRangeLWRFanoutMaxRows, EnvRateWindowFanoutMaxRows, EnvMaxEmittedSQLBytes,
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(EnvRangeBucketFanoutMaxRows, "")
			t.Setenv(EnvRangeLWRFanoutMaxRows, "")
			t.Setenv(EnvRateWindowFanoutMaxRows, "")
			t.Setenv(EnvMaxEmittedSQLBytes, "")
			t.Setenv(key, "not-a-number")

			_, err := ResourceBoundsFromEnv()
			if err == nil {
				t.Fatalf("ResourceBoundsFromEnv() with %s=not-a-number: error = nil, want an error", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name the offending var %q", err.Error(), key)
			}
		})
	}
}

// TestResourceBoundsFromEnv_RejectsNonPositiveValue confirms 0 and a
// negative value are both rejected as startup errors rather than silently
// accepted as "0 rows" or "-1 rows" — a fanout row bound (or a statement-size
// bound) that low would reject every query outright, which is never a
// legitimate operator intent (see ResourceBoundOverrides' own doc).
func TestResourceBoundsFromEnv_RejectsNonPositiveValue(t *testing.T) {
	for _, key := range []string{EnvRangeBucketFanoutMaxRows, EnvMaxEmittedSQLBytes} {
		for _, v := range []string{"0", "-1", "-1000000"} {
			t.Run(key+"="+v, func(t *testing.T) {
				t.Setenv(EnvRangeBucketFanoutMaxRows, "")
				t.Setenv(EnvRangeLWRFanoutMaxRows, "")
				t.Setenv(EnvRateWindowFanoutMaxRows, "")
				t.Setenv(EnvMaxEmittedSQLBytes, "")
				t.Setenv(key, v)

				_, err := ResourceBoundsFromEnv()
				if err == nil {
					t.Fatalf("ResourceBoundsFromEnv() with %s=%s: error = nil, want an error", key, v)
				}
			})
		}
	}
}

// TestEngine_ResourceBoundOverrides confirms the Engine method that feeds
// emitForHead / routeBExecCtx packages exactly the four Engine fields, so a
// production wiring bug (mixing up which field maps to which knob) would
// show up here rather than only via an end-to-end chDB query.
func TestEngine_ResourceBoundOverrides(t *testing.T) {
	e := &Engine{
		RangeBucketFanoutMaxRows: 111,
		RangeLWRFanoutMaxRows:    222,
		RateWindowFanoutMaxRows:  333,
		MaxEmittedSQLBytes:       444,
	}
	got := e.resourceBoundOverrides()
	want := ResourceBoundOverrides{
		RangeBucketFanoutMaxRows: 111,
		RangeLWRFanoutMaxRows:    222,
		RateWindowFanoutMaxRows:  333,
		MaxEmittedSQLBytes:       444,
	}
	if got != want {
		t.Fatalf("resourceBoundOverrides() = %+v, want %+v", got, want)
	}
}

// TestEngine_ResourceBoundOverrides_ZeroValue confirms an Engine built
// without these fields wired (e.g. Engine{} in a test) packages the zero
// ResourceBoundOverrides — the "thread nothing" sentinel.
func TestEngine_ResourceBoundOverrides_ZeroValue(t *testing.T) {
	e := &Engine{}
	got := e.resourceBoundOverrides()
	if got != (ResourceBoundOverrides{}) {
		t.Fatalf("resourceBoundOverrides() on a zero Engine = %+v, want the zero value", got)
	}
}

// TestApplyResourceBoundOverrides_ThreadsTheEmittedSQLByteBound pins the
// engine→chsql seam for issue #2733's statement-size bound: an operator value
// that reached ResourceBoundOverrides but never reached the emit context would
// be a silently inert knob, which is the exact failure mode issue #2055 found
// on MaxQuerySamples and issue #2667 was filed to prevent repeating.
//
// It asserts behaviourally, through chsql.Emit, because the ctx key chsql reads
// is unexported: a bound one byte under what the plan actually renders must
// refuse it, and a zero field must thread nothing and leave chsql on its own
// compiled-in default.
func TestApplyResourceBoundOverrides_ThreadsTheEmittedSQLByteBound(t *testing.T) {
	plan := &chplan.Project{
		Input:       &chplan.Scan{Table: "otel_metrics_gauge"},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"}},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit with no override: %v", err)
	}

	tight := applyResourceBoundOverrides(context.Background(), ResourceBoundOverrides{
		MaxEmittedSQLBytes: int64(len(sql) - 1),
	})
	if _, _, err := chsql.Emit(tight, plan); !errors.Is(err, chsql.ErrEmittedSQLTooLarge) {
		t.Errorf("Emit under a %d-byte override of %d bytes of SQL = %v, want ErrEmittedSQLTooLarge — "+
			"the override never reached the emit context", len(sql)-1, len(sql), err)
	}

	unset := applyResourceBoundOverrides(context.Background(), ResourceBoundOverrides{})
	if _, _, err := chsql.Emit(unset, plan); err != nil {
		t.Errorf("Emit with a zero MaxEmittedSQLBytes = %v, want success: a zero field must thread "+
			"nothing rather than a zero-byte ceiling", err)
	}
}
