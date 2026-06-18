package chopt

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{"25.8.2.1", 25, 8, true},
		{"25.8.2.1-lts", 25, 8, true},
		{"24.8", 24, 8, true},
		{"25.3.0.0", 25, 3, true},
		{" 25.6.1 ", 25, 6, true},
		{"25", 0, 0, false},         // only one field
		{"lts.8.2", 0, 0, false},    // non-numeric major
		{"25.lts", 0, 0, false},     // non-numeric minor
		{"", 0, 0, false},           // empty
		{"25.6-rc1.2", 25, 6, true}, // trailing non-digit on minor trimmed
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseVersion(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseVersion(%q) ok = %v; want %v", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Major != tc.wantMajor || got.Minor != tc.wantMinor {
				t.Errorf("ParseVersion(%q) = %d.%d; want %d.%d", tc.in, got.Major, got.Minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		v    Version
		min  Version
		want bool
	}{
		{Version{25, 8}, Version{25, 3}, true},
		{Version{25, 3}, Version{25, 3}, true},
		{Version{25, 2}, Version{25, 3}, false},
		{Version{26, 0}, Version{25, 9}, true},
		{Version{24, 8}, Version{25, 0}, false},
		{Version{25, 6}, Version{24, 8}, true},
	}
	for _, tc := range cases {
		if got := tc.v.AtLeast(tc.min); got != tc.want {
			t.Errorf("%s.AtLeast(%s) = %v; want %v", tc.v, tc.min, got, tc.want)
		}
	}
}

func TestVersionString(t *testing.T) {
	if got := (Version{25, 8}).String(); got != "25.8" {
		t.Errorf("String() = %q; want %q", got, "25.8")
	}
}
