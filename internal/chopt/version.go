package chopt

import (
	"strconv"
	"strings"
)

// Version is a comparable major.minor ClickHouse version. Patch and build
// suffixes are intentionally dropped: ClickHouse feature availability lands at
// minor-version granularity, so the comparison the resolver makes is over
// (major, minor) only. This mirrors internal/preflight's chVersion exactly so
// the two version gates agree on what a given server string means.
type Version struct {
	Major int
	Minor int
}

// ParseVersion extracts the leading major.minor from a ClickHouse version
// string. The wire format looks like "25.8.2.1", "25.8.2.1-lts", or carries a
// build suffix; only the first two dot-separated integer fields are read, and
// any trailing non-digit run on the minor field (e.g. a "-lts" glued directly
// to it) is trimmed. Returns ok=false when the string has no leading integer
// major or minor field. It mirrors internal/preflight parseCHVersion /
// leadingInt so the auto-picker and the preflight floor parse identically.
func ParseVersion(s string) (Version, bool) {
	fields := strings.Split(strings.TrimSpace(s), ".")
	if len(fields) < 2 {
		return Version{}, false
	}
	major, ok := leadingInt(fields[0])
	if !ok {
		return Version{}, false
	}
	minor, ok := leadingInt(fields[1])
	if !ok {
		return Version{}, false
	}
	return Version{Major: major, Minor: minor}, true
}

// leadingInt parses the leading run of ASCII digits in s. Returns ok=false
// when s does not start with a digit, so a field like "lts" or an empty field
// is rejected rather than silently coerced to 0.
func leadingInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// AtLeast reports whether v is greater than or equal to min, comparing major
// first and minor as the tie-break.
func (v Version) AtLeast(min Version) bool {
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	return v.Minor >= min.Minor
}

// String renders the version as "<major>.<minor>" for boot logging and the
// resolver's diagnostic messages (the floor a feature requires is always a
// bare major.minor).
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
}
