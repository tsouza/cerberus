package main

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/promql"
)

// TestBoot_RetiredClassicMergeNameWarnsAndStarts pins the boot path for the
// retired classic bucket-merge ceiling name: the configuration still loads,
// the bounds still resolve (the old value ignored, the new one or the default
// applied), and exactly one warning naming the replacement is logged while the
// old name is set. The new name alone logs nothing.
func TestBoot_RetiredClassicMergeNameWarnsAndStarts(t *testing.T) {
	const pinned int64 = 1234
	for _, tc := range []struct {
		name        string
		retired     string
		current     string
		wantCeiling int64
		wantWarning bool
	}{
		{"retired alone", "50000000", "", promql.DefaultResourceBounds().ClassicBucketMergeMaxEntries, true},
		{"both set", "50000000", "1234", pinned, true},
		{"current alone", "", "1234", pinned, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, promql.EnvClassicBucketMergeMaxCostUnitsRetired, tc.retired)
			setOrUnset(t, promql.EnvClassicBucketMergeMaxEntries, tc.current)

			cfg, err := config.FromEnv()
			if err != nil {
				t.Fatalf("config.FromEnv: %v", err)
			}
			_, promBounds, err := resolveBoundOverrides(cfg)
			if err != nil {
				t.Fatalf("resolveBoundOverrides: %v", err)
			}
			if promBounds.ClassicBucketMergeMaxEntries != tc.wantCeiling {
				t.Fatalf("ClassicBucketMergeMaxEntries = %d, want %d", promBounds.ClassicBucketMergeMaxEntries, tc.wantCeiling)
			}

			var buf bytes.Buffer
			warnRetiredSettings(slog.New(slog.NewTextHandler(&buf, nil)), cfg)
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if !tc.wantWarning {
				if buf.Len() != 0 {
					t.Fatalf("logged %q, want nothing", buf.String())
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want one warning:\n%s", len(lines), buf.String())
			}
			for _, want := range []string{"level=WARN", promql.EnvClassicBucketMergeMaxCostUnitsRetired, promql.EnvClassicBucketMergeMaxEntries, "ignored"} {
				if !strings.Contains(lines[0], want) {
					t.Fatalf("warning %q does not mention %q", lines[0], want)
				}
			}
		})
	}
}

// setOrUnset sets key to v for the test, or unsets it when v is empty, and
// restores the previous value afterwards either way.
func setOrUnset(t *testing.T, key, v string) {
	t.Helper()
	t.Setenv(key, v)
	if v == "" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unsetenv %s: %v", key, err)
		}
	}
}
