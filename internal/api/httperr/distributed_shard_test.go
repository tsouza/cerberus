package httperr_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/httperr"
	"github.com/tsouza/cerberus/internal/chclient"
)

// TestIsDistributedShardErr pins the shared classifier every head's 503
// path routes through: both chclient partial-shard-failure sentinels match,
// through their concrete wrapper types and through further fmt.Errorf
// wrapping (the engine's stage markers), and nothing else does — a timeout,
// a cancellation, a circuit-open, and nil all stay out of the class.
func TestIsDistributedShardErr(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare ErrShardUnavailable", chclient.ErrShardUnavailable, true},
		{"bare ErrStaleReplicaFallbackDenied", chclient.ErrStaleReplicaFallbackDenied, true},
		{"concrete ShardUnavailableError", &chclient.ShardUnavailableError{Cause: errors.New("code 279")}, true},
		{"concrete StaleReplicaFallbackDeniedError", &chclient.StaleReplicaFallbackDeniedError{Cause: errors.New("code 369")}, true},
		{"wrapped by an engine stage marker", fmt.Errorf("engine: execute: %w", &chclient.ShardUnavailableError{}), true},
		{"query timeout is not a shard failure", chclient.ErrQueryTimeout, false},
		{"circuit open is not a shard failure", chclient.ErrCircuitOpen, false},
		{"context deadline is not a shard failure", context.DeadlineExceeded, false},
		{"context canceled is not a shard failure", context.Canceled, false},
		{"unrelated error", errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := httperr.IsDistributedShardErr(tc.err); got != tc.want {
				t.Errorf("IsDistributedShardErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestDistributedShardUnavailableMessage pins that the text names the
// specific rejection for each sentinel, that both texts share the class
// prefix the heads' dashboards key on, and that an unrelated error gets the
// bare class name rather than either specific attribution.
func TestDistributedShardUnavailableMessage(t *testing.T) {
	t.Parallel()

	const classPrefix = "distributed shard unavailable"

	unreachable := httperr.DistributedShardUnavailableMessage(fmt.Errorf("wrapped: %w", chclient.ErrShardUnavailable))
	stale := httperr.DistributedShardUnavailableMessage(fmt.Errorf("wrapped: %w", chclient.ErrStaleReplicaFallbackDenied))
	other := httperr.DistributedShardUnavailableMessage(errors.New("boom"))

	for name, msg := range map[string]string{"unreachable": unreachable, "stale": stale, "other": other} {
		if !strings.HasPrefix(msg, classPrefix) {
			t.Errorf("%s message %q does not carry the class prefix %q", name, msg, classPrefix)
		}
	}
	if !strings.Contains(unreachable, "could not reach any replica") {
		t.Errorf("unreachable-shard message does not name the rejection: %q", unreachable)
	}
	if !strings.Contains(stale, "stale") {
		t.Errorf("stale-replica message does not name the rejection: %q", stale)
	}
	if unreachable == stale {
		t.Errorf("the two rejections must not render the same text: %q", unreachable)
	}
	if other != classPrefix {
		t.Errorf("an unrelated error must get the bare class name %q, got %q", classPrefix, other)
	}
}
