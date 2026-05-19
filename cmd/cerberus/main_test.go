package main

import "testing"

// TestIsVersionFlag pins the argv shapes recognized by the
// `--version` pre-flight. The cerberus container in
// compatibility/prometheus/docker-compose.yml uses this exact path as
// its docker healthcheck because the distroless runtime image has no
// shell / wget / curl; a regression here would silently re-break the
// compatibility lane's "container ... is unhealthy" failure mode
// (see PR #297 + follow-up).
func TestIsVersionFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty argv (nil)", nil, false},
		{"empty argv (slice)", []string{}, false},
		{"binary only — server mode", []string{"cerberus"}, false},
		{"long form --version", []string{"cerberus", "--version"}, true},
		{"short form -v", []string{"cerberus", "-v"}, true},
		{"subcommand-style version", []string{"cerberus", "version"}, true},
		{"unrelated flag falls through", []string{"cerberus", "--help"}, false},
		{"unrelated subcommand falls through", []string{"cerberus", "serve"}, false},
		{"version with trailing junk still recognized", []string{"cerberus", "--version", "extra"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isVersionFlag(tc.args); got != tc.want {
				t.Fatalf("isVersionFlag(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
