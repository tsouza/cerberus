package telemetry

import "testing"

func TestSanitizeForLog(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no control chars is unchanged", `sum(rate(up{job="api"}[5m]))`, `sum(rate(up{job="api"}[5m]))`},
		{
			"a forged log line via embedded newline is neutralised",
			"up\nlevel=ERROR msg=\"fake entry\"",
			`up\nlevel=ERROR msg="fake entry"`,
		},
		{"carriage return", "a\rb", `a\rb`},
		{"tab", "a\tb", `a\tb`},
		{"multiple control chars in one value", "a\nb\rc\td", `a\nb\rc\td`},
		{
			"a non-named control char falls back to a hex escape",
			"a\x1bb", // ESC — the ANSI escape-sequence introducer
			`a\x1bb`,
		},
		{"a C1 control char is also escaped", "a\u0085b", `a\x85b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeForLog(tc.in); got != tc.want {
				t.Errorf("SanitizeForLog(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeForLog_FastPathIdentity pins the no-control-chars fast path:
// a string that never triggers strings.ContainsFunc must come back byte-for-
// byte identical (no builder round-trip), not merely equal in content — a
// mutation that swapped the early return for an always-rebuild would still
// pass a plain string-equality check but would regress the allocation-free
// path this function exists to keep cheap on the hot request path.
func TestSanitizeForLog_FastPathIdentity(t *testing.T) {
	const clean = `sum(rate(up{job="api"}[5m]))`
	got := SanitizeForLog(clean)
	if got != clean {
		t.Fatalf("SanitizeForLog(%q) = %q, want unchanged", clean, got)
	}
}
