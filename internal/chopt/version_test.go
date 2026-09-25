package chopt

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in     string
		want   Version
		wantOK bool
	}{
		{"25.8.2.1", Version{Major: 25, Minor: 8, Patch: 2, Build: 1}, true},
		{"25.8.2.1-lts", Version{Major: 25, Minor: 8, Patch: 2, Build: 1, Vendor: true}, true},
		{"26.3.17.56", Version{Major: 26, Minor: 3, Patch: 17, Build: 56}, true},
		{"24.8", Version{Major: 24, Minor: 8}, true},
		{"25.3.0.0", Version{Major: 25, Minor: 3}, true},
		{" 25.6.1 ", Version{Major: 25, Minor: 6, Patch: 1}, true},
		{"25.8.lts", Version{Major: 25, Minor: 8, Vendor: true}, true},                            // non-numeric patch ends the version
		{"25.6-rc1.2", Version{Major: 25, Minor: 6, Vendor: true}, true},                          // suffix on minor ends the version
		{"26.3.17.56.99", Version{Major: 26, Minor: 3, Patch: 17, Build: 56, Vendor: true}, true}, // only four fields are read
		{"25", Version{}, false},      // only one field
		{"lts.8.2", Version{}, false}, // non-numeric major
		{"25.lts", Version{}, false},  // non-numeric minor
		{"", Version{}, false},        // empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ParseVersion(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ParseVersion(%q) ok = %v; want %v", tc.in, ok, tc.wantOK)
			}
			if ok && got != tc.want {
				t.Errorf("ParseVersion(%q) = %+v; want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	v := func(s string) Version {
		t.Helper()
		out, ok := ParseVersion(s)
		if !ok {
			t.Fatalf("ParseVersion(%q) failed", s)
		}
		return out
	}
	cases := []struct {
		v, min string
		want   bool
	}{
		{"25.8", "25.3", true},
		{"25.3", "25.3", true},
		{"25.2", "25.3", false},
		{"26.0", "25.9", true},
		{"24.8", "25.0", false},
		{"25.6", "24.8", true},
		// A floor carries no patch/build, so every build of the floor's minor
		// meets it.
		{"25.3.1.2703", "25.3", true},
		{"25.2.99.99", "25.3", false},
		// Patch and build decide a backport boundary.
		{"26.3.12.3", "26.3.13.31", false},
		{"26.3.13.31", "26.3.13.31", true},
		{"26.3.17.4", "26.3.17.56", false},
		{"26.3.17.110", "26.3.17.56", true},
		{"26.4.1.1141", "26.3.33.24", true},
	}
	for _, tc := range cases {
		if got := v(tc.v).AtLeast(v(tc.min)); got != tc.want {
			t.Errorf("%s.AtLeast(%s) = %v; want %v", tc.v, tc.min, got, tc.want)
		}
	}
}

func TestVersionString(t *testing.T) {
	cases := []struct {
		v    Version
		want string
	}{
		{Version{Major: 25, Minor: 8}, "25.8"},
		{Version{Major: 26, Minor: 3, Patch: 17, Build: 56}, "26.3.17.56"},
		{Version{Major: 25, Minor: 6, Patch: 1}, "25.6.1.0"},
	}
	for _, tc := range cases {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("%+v.String() = %q; want %q", tc.v, got, tc.want)
		}
	}
}

func TestBuildRangeContains(t *testing.T) {
	r := BuildRange{
		From:  Version{Major: 26, Minor: 3},
		Until: Version{Major: 26, Minor: 3, Patch: 13, Build: 31},
	}
	cases := []struct {
		v    Version
		want bool
	}{
		{Version{Major: 26, Minor: 2, Patch: 19, Build: 43}, false},
		{Version{Major: 26, Minor: 3, Patch: 1, Build: 896}, true},
		{Version{Major: 26, Minor: 3, Patch: 12, Build: 3}, true},
		{Version{Major: 26, Minor: 3, Patch: 13, Build: 31}, false},
		{Version{Major: 26, Minor: 4, Patch: 1, Build: 1141}, false},
	}
	for _, tc := range cases {
		if got := r.Contains(tc.v); got != tc.want {
			t.Errorf("Contains(%s) = %v; want %v", tc.v, got, tc.want)
		}
	}
}

func TestLowestVersion(t *testing.T) {
	old := Version{Major: 25, Minor: 3, Patch: 14, Build: 14}
	fresh := Version{Major: 26, Minor: 6, Patch: 1, Build: 1193}
	vendorOld := Version{Major: 25, Minor: 3, Patch: 14, Build: 14, Vendor: true}
	cases := []struct {
		name   string
		in     []Version
		want   Version
		wantOK bool
	}{
		{"empty", nil, Version{}, false},
		{"single", []Version{fresh}, fresh, true},
		{"older last", []Version{fresh, old}, old, true},
		{"older first", []Version{old, fresh}, old, true},
		{"equal builds prefer vendor", []Version{old, vendorOld, fresh}, vendorOld, true},
		{"vendor kept over a later equal upstream", []Version{vendorOld, old}, vendorOld, true},
	}
	for _, tc := range cases {
		got, ok := LowestVersion(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: LowestVersion(%v) = %v, %v; want %v, %v", tc.name, tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
