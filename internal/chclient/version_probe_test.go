package chclient

import "testing"

func TestParseServerVersion(t *testing.T) {
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
		{"25", 0, 0, false},
		{"lts.8", 0, 0, false},
		{"25.lts", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseServerVersion(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("parseServerVersion(%q) ok = %v; want %v", tc.in, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Major != tc.wantMajor || got.Minor != tc.wantMinor {
				t.Errorf("parseServerVersion(%q) = %d.%d; want %d.%d", tc.in, got.Major, got.Minor, tc.wantMajor, tc.wantMinor)
			}
		})
	}
}

func TestServerVersionAtLeast(t *testing.T) {
	cases := []struct {
		v     ServerVersion
		major int
		minor int
		want  bool
	}{
		{ServerVersion{25, 8}, 25, 3, true},
		{ServerVersion{25, 3}, 25, 3, true},
		{ServerVersion{25, 2}, 25, 3, false},
		{ServerVersion{26, 0}, 25, 9, true},
		{ServerVersion{24, 8}, 25, 0, false},
		{ServerVersion{25, 6}, 24, 8, true},
	}
	for _, tc := range cases {
		if got := tc.v.AtLeast(tc.major, tc.minor); got != tc.want {
			t.Errorf("%s.AtLeast(%d,%d) = %v; want %v", tc.v, tc.major, tc.minor, got, tc.want)
		}
	}
}

func TestServerVersionString(t *testing.T) {
	if got := (ServerVersion{25, 6}).String(); got != "25.6" {
		t.Errorf("String() = %q; want %q", got, "25.6")
	}
}
