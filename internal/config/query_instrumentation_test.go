package config

import "testing"

// The log-comment-shape instrumentation flag ships DARK (default false) and
// accepts the full strconv bool vocabulary, mirroring the existing bool-flag
// tests (e.g. TestFromEnv_OTLP_Insecure_*).

func TestFromEnv_LogCommentShape_DefaultsOff(t *testing.T) {
	t.Setenv("CERBERUS_LOG_COMMENT_SHAPE", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.LogCommentShape {
		t.Errorf("LogCommentShape default = true; want false")
	}
}

func TestFromEnv_LogCommentShape_Garbage(t *testing.T) {
	t.Setenv("CERBERUS_LOG_COMMENT_SHAPE", "yarp")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv: want error for invalid bool, got nil")
	}
}

func TestFromEnv_LogCommentShape_BoolVocabulary(t *testing.T) {
	for _, tc := range boolVocabulary() {
		t.Run(tc.val, func(t *testing.T) {
			t.Setenv("CERBERUS_LOG_COMMENT_SHAPE", tc.val)
			cfg, err := FromEnv()
			if err != nil {
				t.Fatalf("FromEnv: %v", err)
			}
			if cfg.LogCommentShape != tc.want {
				t.Errorf("LogCommentShape(%q) = %v; want %v",
					tc.val, cfg.LogCommentShape, tc.want)
			}
		})
	}
}

// boolVocabulary is the strconv-bool vocabulary cerberus accepts for its
// bool flags, mirroring TestFromEnv_OTLP_InsecureBoolVocabulary.
func boolVocabulary() []struct {
	val  string
	want bool
} {
	return []struct {
		val  string
		want bool
	}{
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"t", true},
		{"false", false},
		{"0", false},
		{"f", false},
	}
}
