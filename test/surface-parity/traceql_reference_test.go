package surfaceparity

import (
	"strings"
	"testing"
)

// TestLoadTraceQLReferenceVerdicts_MissingFile pins the wrapping error
// message loadTraceQLReferenceVerdicts produces when the pinned
// artifact is absent, distinct from a malformed-content error.
func TestLoadTraceQLReferenceVerdicts_MissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := loadTraceQLReferenceVerdicts()
	if err == nil {
		t.Fatal("expected an error reading a missing artifact, got nil")
	}
	if !strings.Contains(err.Error(), "read "+traceqlReferenceVerdictsPath) {
		t.Errorf("error = %q, want it to name the missing path", err)
	}
}

func TestParseTraceQLReferenceVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "malformed JSON",
			raw:     "not json",
			wantErr: "parse " + traceqlReferenceVerdictsPath,
		},
		{
			name:    "empty verdicts map",
			raw:     `{"verdicts": {}}`,
			wantErr: "has no verdicts",
		},
		{
			name: "well-formed artifact",
			raw:  `{"verdicts": {"intrinsic:duration": "accept"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rv, err := parseTraceQLReferenceVerdicts([]byte(tt.raw))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if len(rv.Verdicts) != 1 {
					t.Errorf("Verdicts = %v, want 1 entry", rv.Verdicts)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestTraceQLReferenceVerdicts_Verdict(t *testing.T) {
	rv := &traceqlReferenceVerdicts{Verdicts: map[string]string{
		"intrinsic:accepted": "accept",
		"intrinsic:rejected": "reject",
		"intrinsic:garbled":  "maybe",
	}}

	t.Run("accept", func(t *testing.T) {
		v, err := rv.verdict("intrinsic:accepted")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != VerdictAccept {
			t.Errorf("verdict = %q, want %q", v, VerdictAccept)
		}
	})

	t.Run("reject", func(t *testing.T) {
		v, err := rv.verdict("intrinsic:rejected")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != VerdictReject {
			t.Errorf("verdict = %q, want %q", v, VerdictReject)
		}
	})

	t.Run("missing symbol", func(t *testing.T) {
		if _, err := rv.verdict("intrinsic:unknown"); err == nil {
			t.Fatal("expected an error for a symbol absent from the artifact, got nil")
		}
	})

	t.Run("invalid verdict value", func(t *testing.T) {
		if _, err := rv.verdict("intrinsic:garbled"); err == nil {
			t.Fatal("expected an error for an invalid verdict value, got nil")
		}
	})
}
