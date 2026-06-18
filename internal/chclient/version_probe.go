package chclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// ServerVersion is a comparable major.minor ClickHouse version as read by the
// runtime probe. Patch and build suffixes are dropped: ClickHouse feature
// availability lands at minor-version granularity, so the auto-picker compares
// over (major, minor) only. The shape mirrors internal/chopt.Version and
// internal/preflight's chVersion so all three version gates agree.
type ServerVersion struct {
	Major int
	Minor int
}

// String renders the version as "<major>.<minor>" for boot logging.
func (v ServerVersion) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
}

// AtLeast reports whether v is greater than or equal to min, comparing major
// first and minor as the tie-break. It is the serverAtLeast predicate the
// optimization resolver consumes.
func (v ServerVersion) AtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// ProbeVersion issues SELECT version() once and parses the result into a
// comparable major.minor ServerVersion. It is the single runtime version probe
// the optimization auto-picker resolves against: cmd/cerberus calls it after
// building the client, then hands the result to chopt.Resolve.
//
// The probe runs ONCE. A rolling ClickHouse upgrade that crosses a feature
// floor needs a cerberus restart/reconnect to re-probe and re-resolve; that is
// the documented v1 behaviour (see docs/clickhouse-optimizations.md).
//
// It reuses the breaker-guarded QueryStrings read surface, so a probe against
// an unreachable server fails like any other read rather than hanging.
func (c *Client) ProbeVersion(ctx context.Context) (ServerVersion, error) {
	rows, err := c.QueryStrings(ctx, "SELECT version()")
	if err != nil {
		return ServerVersion{}, fmt.Errorf("probe clickhouse version: %w", err)
	}
	if len(rows) == 0 {
		return ServerVersion{}, fmt.Errorf("probe clickhouse version: empty result")
	}
	v, ok := parseServerVersion(rows[0])
	if !ok {
		return ServerVersion{}, fmt.Errorf("probe clickhouse version: unparseable %q", strings.TrimSpace(rows[0]))
	}
	return v, nil
}

// parseServerVersion extracts the leading major.minor from a ClickHouse
// version string ("25.8.2.1", "25.8.2.1-lts", ...). Only the first two
// dot-separated integer fields are read, and any trailing non-digit run on the
// minor field is trimmed. Returns ok=false when there is no leading integer
// major or minor. Mirrors internal/preflight parseCHVersion / leadingInt.
func parseServerVersion(s string) (ServerVersion, bool) {
	fields := strings.Split(strings.TrimSpace(s), ".")
	if len(fields) < 2 {
		return ServerVersion{}, false
	}
	major, ok := versionLeadingInt(fields[0])
	if !ok {
		return ServerVersion{}, false
	}
	minor, ok := versionLeadingInt(fields[1])
	if !ok {
		return ServerVersion{}, false
	}
	return ServerVersion{Major: major, Minor: minor}, true
}

// versionLeadingInt parses the leading run of ASCII digits in s, rejecting a
// field that does not start with a digit.
func versionLeadingInt(s string) (int, bool) {
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
