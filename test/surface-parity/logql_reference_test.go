package surfaceparity

import (
	"strings"
	"testing"
)

// TestLoadLogQLReferenceVerdicts_MissingFile pins the wrapping error
// message loadLogQLReferenceVerdicts produces when the pinned artifact
// is absent, distinct from a malformed-content error.
func TestLoadLogQLReferenceVerdicts_MissingFile(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := loadLogQLReferenceVerdicts()
	if err == nil {
		t.Fatal("expected an error reading a missing artifact, got nil")
	}
	if !strings.Contains(err.Error(), "read "+logqlReferenceVerdictsPath) {
		t.Errorf("error = %q, want it to name the missing path", err)
	}
}

func TestParseLogQLReferenceVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "malformed JSON",
			raw:     "not json",
			wantErr: "parse " + logqlReferenceVerdictsPath,
		},
		{
			name:    "empty verdicts map",
			raw:     `{"verdicts": {}}`,
			wantErr: "has no verdicts",
		},
		{
			name: "well-formed artifact",
			raw:  `{"verdicts": {"expr:up": "accept"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rv, err := parseLogQLReferenceVerdicts([]byte(tt.raw))
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

func TestLogQLReferenceVerdicts_Verdict(t *testing.T) {
	rv := &logqlReferenceVerdicts{Verdicts: map[string]string{
		"expr:accepted": "accept",
		"expr:rejected": "reject",
		"expr:garbled":  "maybe",
	}}

	t.Run("accept", func(t *testing.T) {
		v, err := rv.verdict("expr:accepted")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != VerdictAccept {
			t.Errorf("verdict = %q, want %q", v, VerdictAccept)
		}
	})

	t.Run("reject", func(t *testing.T) {
		v, err := rv.verdict("expr:rejected")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if v != VerdictReject {
			t.Errorf("verdict = %q, want %q", v, VerdictReject)
		}
	})

	t.Run("missing symbol", func(t *testing.T) {
		if _, err := rv.verdict("expr:unknown"); err == nil {
			t.Fatal("expected an error for a symbol absent from the artifact, got nil")
		}
	})

	t.Run("invalid verdict value", func(t *testing.T) {
		if _, err := rv.verdict("expr:garbled"); err == nil {
			t.Fatal("expected an error for an invalid verdict value, got nil")
		}
	})
}
