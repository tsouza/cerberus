package chopt

import (
	"strconv"
	"strings"
)

// Version is a comparable ClickHouse build version: major.minor.patch.build,
// the four dot-separated integers `SELECT version()` reports (for example
// "26.3.17.56").
//
// Feature availability lands at minor-version granularity, so every registry
// floor (Feature.MinVersion) is a bare major.minor with Patch and Build left at
// zero, and a floor comparison is decided by (major, minor) alone. Known
// wrong-result defects are the exception: ClickHouse backports their fixes onto
// maintained release lines at a specific patch release, so the boundary of a
// Feature.UnsafeBuilds range needs all four components. The comparison is
// lexicographic over (Major, Minor, Patch, Build).
//
// internal/preflight parses the server version through ParseVersion too, so the
// auto-picker, the defect gate and the preflight floor read a server string
// identically.
type Version struct {
	Major int
	Minor int
	Patch int
	Build int
}

// versionFields is the number of dot-separated integer fields a full
// ClickHouse build version carries (major.minor.patch.build).
const versionFields = 4

// requiredVersionFields is how many leading fields a string must carry to parse
// at all: a version without major.minor has no comparable meaning.
const requiredVersionFields = 2

// ParseVersion extracts a Version from a ClickHouse version string. The wire
// format looks like "25.8.2.1", "25.8.2.1-lts", or carries a build suffix. The
// major and minor fields are required; patch and build are read when present
// and left at zero otherwise, so a bare "24.8" parses to the floor 24.8.0.0.
// A trailing non-digit run on a field (e.g. a "-lts" glued to the build) is
// trimmed and ends the numeric version, and reading also stops at the first
// optional field that does not start with a digit. Returns ok=false when the
// string has no leading integer major or minor field.
func ParseVersion(s string) (Version, bool) {
	fields := strings.Split(strings.TrimSpace(s), ".")
	if len(fields) < requiredVersionFields {
		return Version{}, false
	}
	var parts [versionFields]int
	for i := 0; i < versionFields && i < len(fields); i++ {
		n, ok := leadingInt(fields[i])
		if !ok {
			if i < requiredVersionFields {
				return Version{}, false
			}
			break
		}
		parts[i] = n
		if !isAllDigits(strings.TrimSpace(fields[i])) {
			// A suffix glued to this field ("6-rc1", "1-lts") ends the
			// numeric version; whatever follows is not a build component.
			break
		}
	}
	return Version{Major: parts[0], Minor: parts[1], Patch: parts[2], Build: parts[3]}, true
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

// isAllDigits reports whether s is a non-empty run of ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Compare returns -1, 0 or +1 as v sorts before, equal to, or after other,
// lexicographically over (Major, Minor, Patch, Build).
func (v Version) Compare(other Version) int {
	a := [versionFields]int{v.Major, v.Minor, v.Patch, v.Build}
	b := [versionFields]int{other.Major, other.Minor, other.Patch, other.Build}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// AtLeast reports whether v is greater than or equal to min.
func (v Version) AtLeast(min Version) bool {
	return v.Compare(min) >= 0
}

// String renders the version as "<major>.<minor>" when it carries no patch or
// build component — every registry floor, and so every "needs ClickHouse >=X"
// message — and as the full "<major>.<minor>.<patch>.<build>" otherwise, so a
// probed server is reported at the precision its defect gates are decided at.
func (v Version) String() string {
	short := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
	if v.Patch == 0 && v.Build == 0 {
		return short
	}
	return short + "." + strconv.Itoa(v.Patch) + "." + strconv.Itoa(v.Build)
}
