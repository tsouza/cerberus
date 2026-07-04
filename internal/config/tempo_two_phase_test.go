package config

import "testing"

// TestFromEnv_TempoStructuralTwoPhase pins the toggle contract: the Tempo
// structural two-phase split is exposed, ON by default, and switchable off.
func TestFromEnv_TempoStructuralTwoPhase(t *testing.T) {
	t.Run("default on", func(t *testing.T) {
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if !cfg.TempoStructuralTwoPhase {
			t.Error("TempoStructuralTwoPhase = false, want true (default on)")
		}
	})

	t.Run("switchable off", func(t *testing.T) {
		t.Setenv(envTempoStructuralTwoPhase, "false")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.TempoStructuralTwoPhase {
			t.Error("TempoStructuralTwoPhase = true with env=false, want false")
		}
	})
}
