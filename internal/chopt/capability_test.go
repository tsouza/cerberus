package chopt

import "testing"

// TestCapabilityConservative: across two servers' verdicts on one canary, the
// fleet takes the one that permits less, in either order — a definitive
// refusal over a failed read over a permit — and a missing verdict (Unknown)
// leaves the other server's verdict standing.
func TestCapabilityConservative(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b, want Capability
	}{
		{CapabilityAvailable, CapabilityAvailable, CapabilityAvailable},
		{CapabilityAvailable, CapabilityForbidden, CapabilityForbidden},
		{CapabilityAvailable, CapabilityUnreachable, CapabilityUnreachable},
		{CapabilityUnreachable, CapabilityForbidden, CapabilityForbidden},
		{CapabilityUnknown, CapabilityAvailable, CapabilityAvailable},
		{CapabilityUnknown, CapabilityForbidden, CapabilityForbidden},
		{CapabilityUnknown, CapabilityUnreachable, CapabilityUnreachable},
		{CapabilityUnknown, CapabilityUnknown, CapabilityUnknown},
	}
	for _, tc := range cases {
		if got := tc.a.Conservative(tc.b); got != tc.want {
			t.Errorf("%s.Conservative(%s) = %s; want %s", tc.a, tc.b, got, tc.want)
		}
		if got := tc.b.Conservative(tc.a); got != tc.want {
			t.Errorf("%s.Conservative(%s) = %s; want %s", tc.b, tc.a, got, tc.want)
		}
	}
}
