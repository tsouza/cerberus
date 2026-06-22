package config

import "testing"

// TestFromEnv_EnabledHeads_DefaultAllThree confirms the toggle defaults to
// all three heads when CERBERUS_ENABLED_HEADS is unset — full backward
// compatibility, an unset value behaves exactly as today.
func TestFromEnv_EnabledHeads_DefaultAllThree(t *testing.T) {
	t.Setenv("CERBERUS_ENABLED_HEADS", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	for _, h := range []Head{HeadProm, HeadLoki, HeadTempo} {
		if !cfg.HeadEnabled(h) {
			t.Errorf("HeadEnabled(%q) = false; want true (default is all three)", h)
		}
	}
}

// TestFromEnv_EnabledHeads_Subset confirms a comma-separated subset enables
// exactly the named heads and disables the rest.
func TestFromEnv_EnabledHeads_Subset(t *testing.T) {
	cases := []struct {
		val  string
		want map[Head]bool
	}{
		{"prom", map[Head]bool{HeadProm: true, HeadLoki: false, HeadTempo: false}},
		{"loki", map[Head]bool{HeadProm: false, HeadLoki: true, HeadTempo: false}},
		{"tempo", map[Head]bool{HeadProm: false, HeadLoki: false, HeadTempo: true}},
		{"prom,loki", map[Head]bool{HeadProm: true, HeadLoki: true, HeadTempo: false}},
		// Case-insensitive + whitespace-tolerant, since the Helm chart may
		// render either casing or padded list values.
		{" PROM , Tempo ", map[Head]bool{HeadProm: true, HeadLoki: false, HeadTempo: true}},
		{"tempo,loki,prom", map[Head]bool{HeadProm: true, HeadLoki: true, HeadTempo: true}},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_ENABLED_HEADS", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			for h, want := range tc.want {
				if got := cfg.HeadEnabled(h); got != want {
					t.Errorf("HeadEnabled(%q) = %v; want %v", h, got, want)
				}
			}
		})
	}
}

// TestFromEnv_EnabledHeads_Invalid confirms an unknown head token or an
// effectively-empty explicit list fails fast at startup — a misconfiguration
// must trip the process, never silently serve a wrong (or empty) set.
func TestFromEnv_EnabledHeads_Invalid(t *testing.T) {
	for _, val := range []string{"promql", "traces", "prom,bogus", ",", " , "} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("CERBERUS_ENABLED_HEADS", val)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("FromEnv(%q): want error, got nil", val)
			}
		})
	}
}
