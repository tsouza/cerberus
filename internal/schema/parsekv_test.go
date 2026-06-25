package schema

import "testing"

// TestParseKVList_Empty pins that unset / whitespace-only input returns nil
// (no settings), the source of the byte-identical default DDL.
func TestParseKVList_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		got, err := ParseKVList(in)
		if err != nil {
			t.Errorf("ParseKVList(%q): unexpected error %v", in, err)
		}
		if got != nil {
			t.Errorf("ParseKVList(%q) = %+v, want nil", in, got)
		}
	}
}

// TestParseKVList_OrderedTypeInferred pins ordered parsing with per-value type
// inference: a bare integer -> int64, a bare float -> float64, true/false ->
// bool, everything else -> string. The order is preserved end-to-end so the
// rendered DDL is deterministic.
func TestParseKVList_OrderedTypeInferred(t *testing.T) {
	got, err := ParseKVList("storage_policy=s3_tiered, min_bytes_for_wide_part=0, ratio=0.5, flag=true")
	if err != nil {
		t.Fatalf("ParseKVList: %v", err)
	}
	want := []KV{
		{Key: "storage_policy", Value: "s3_tiered"},
		{Key: "min_bytes_for_wide_part", Value: int64(0)},
		{Key: "ratio", Value: 0.5},
		{Key: "flag", Value: true},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestParseKVList_FailFastNoEquals pins the fail-fast: a token with no `=` is
// an error, not a silent drop — a malformed setting must surface at startup.
func TestParseKVList_FailFastNoEquals(t *testing.T) {
	for _, in := range []string{
		"storage_policy",               // single token, no '='
		"storage_policy=s3, malformed", // second token malformed
		"=value",                       // empty key
		" = value",                     // whitespace key
	} {
		if _, err := ParseKVList(in); err == nil {
			t.Errorf("ParseKVList(%q): want fail-fast error, got nil", in)
		}
	}
}

// TestParseKVList_SkipsEmptyTokens pins that empty comma tokens (e.g. a
// trailing comma) are skipped, mirroring envCSVList, without erroring.
func TestParseKVList_SkipsEmptyTokens(t *testing.T) {
	got, err := ParseKVList("a=1,,b=2,")
	if err != nil {
		t.Fatalf("ParseKVList: %v", err)
	}
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" {
		t.Errorf("got %+v, want [a=1 b=2]", got)
	}
}
