// Package spec — round-trip extension.
//
// This file defines the (optional) `seed:` / `expected_rows:` section
// contract that TXTAR fixtures can use to opt into a semantic
// assertion layer on top of today's text-equality `sql:` golden.
//
// When BOTH sections are present, the `chdb` build-tagged runner
// (see runner_chdb.go) opens an in-process chDB session, applies the
// `seed:` DDL+INSERTs, executes the emitted `sql:` after binding the
// `args:` placeholders, and asserts the resulting rows match
// `expected_rows:`. Without the `chdb` build tag the sections are
// ignored — fixtures still parse cleanly.
//
// Section contract:
//
//   - `seed:` — one or more semicolon-terminated CH statements (DDL +
//     INSERTs) that populate the session. Authors should include
//     deterministic ORDER BY-able data; the runner does not sort
//     rows on its own.
//
//   - `expected_rows:` — JSON array of arrays. Each inner array is one
//     row, with values positionally matching the SELECT projection of
//     the emitted `sql:`. Map(String,String) columns appear as JSON
//     objects ({"job":"api"}) — the runner round-trips Map columns
//     through `toJSONString(...)` server-side and decodes them on the
//     Go side (per the chDB driver probe, native Map scan panics in
//     parquet-go inside chdb-go v1.11.0).
//
// We chose JSON over TSV for `expected_rows:` because Map columns
// embed nested structure that TSV cannot represent without an
// escaping convention, and because reflect.DeepEqual on the decoded
// shape (slice of map[string]any) handles map-key ordering for free.
package spec

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// RoundTripSections is the parsed shape of the optional `seed:` /
// `expected_rows:` / `sql:` / `args:` sections attached to a fixture.
//
// When `Seed` or `ExpectedRows` is empty, the fixture has not opted
// into round-trip execution. Callers should check IsRoundTrip().
type RoundTripSections struct {
	// Seed is the CH DDL+INSERT script that populates the in-process
	// chDB session. The runner splits on semicolons and runs each
	// statement individually so chdb-go's single-statement Exec
	// works.
	Seed string

	// ExpectedRows is the parsed JSON content of `expected_rows:`.
	// Each row is a slice of `any` mirroring the emitted SQL's
	// SELECT projection. Map columns are unmarshalled as
	// map[string]any.
	ExpectedRows [][]any

	// SQL is the raw `sql:` section (after normalization). The
	// runner rewrites Map column references to wrap them in
	// `toJSONString(...)` before binding args.
	SQL string

	// Args is the bound []any matching the `?` placeholders in SQL,
	// parsed from the `args:` text format the emitter writes.
	Args []any
}

// IsRoundTrip reports whether the fixture opted into the
// seed+expected_rows assertion path.
func (r *RoundTripSections) IsRoundTrip() bool {
	return strings.TrimSpace(r.Seed) != "" && len(r.ExpectedRows) > 0
}

// LoadRoundTrip extracts `seed:` / `expected_rows:` / `sql:` / `args:`
// from a fixture. Missing sections produce an empty/false result;
// invalid JSON or args returns an error.
func LoadRoundTrip(c *Case) (*RoundTripSections, error) {
	out := &RoundTripSections{}

	if v, ok := c.Section("seed"); ok {
		out.Seed = v
	}
	if v, ok := c.Section("expected_rows"); ok {
		v = strings.TrimSpace(v)
		if v != "" {
			if err := json.Unmarshal([]byte(v), &out.ExpectedRows); err != nil {
				return nil, fmt.Errorf("expected_rows JSON: %w", err)
			}
		}
	}
	if v, ok := c.Section("sql"); ok {
		out.SQL = strings.TrimSpace(v)
	}
	if v, ok := c.Section("args"); ok {
		args, err := parseArgs(v)
		if err != nil {
			return nil, fmt.Errorf("args: %w", err)
		}
		out.Args = args
	}
	return out, nil
}

// argsLineRe matches one line of the args section, e.g.:
//
//	[0] string = "temperature"
//	[1] float64 = 100
//	[2] int64 = 9
//
// The format is what chsql_test.formatArgs writes via fmt.Fprintf
// with the verb `%T = %#v`. Re-parsing it back to a []any covers the
// three numeric kinds (int64, float64) plus strings; that matches
// every fixture we ship today.
var argsLineRe = regexp.MustCompile(`^\[(\d+)\]\s+(\S+)\s*=\s*(.*)$`)

func parseArgs(s string) ([]any, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "(none)" {
		return nil, nil
	}
	lines := strings.Split(s, "\n")
	args := make([]any, 0, len(lines))
	for _, raw := range lines {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		m := argsLineRe.FindStringSubmatch(raw)
		if m == nil {
			return nil, fmt.Errorf("malformed args line %q", raw)
		}
		idx, _ := strconv.Atoi(m[1])
		if idx != len(args) {
			return nil, fmt.Errorf("args line %q out of sequence (want index %d)", raw, len(args))
		}
		typ := m[2]
		val := strings.TrimSpace(m[3])
		v, err := parseArgValue(typ, val)
		if err != nil {
			return nil, fmt.Errorf("args[%d] (%s): %w", idx, typ, err)
		}
		args = append(args, v)
	}
	return args, nil
}

// parseArgValue decodes one Go literal as produced by `%#v`. It only
// needs to handle the types the emitter currently produces.
func parseArgValue(typ, raw string) (any, error) {
	switch typ {
	case "string":
		// %#v wraps strings in double quotes; strconv.Unquote
		// undoes it (handling embedded escapes).
		return strconv.Unquote(raw)
	case "int", "int64":
		return strconv.ParseInt(raw, 10, 64)
	case "uint", "uint64":
		return strconv.ParseUint(raw, 10, 64)
	case "float64":
		return strconv.ParseFloat(raw, 64)
	case "bool":
		return strconv.ParseBool(raw)
	default:
		return nil, fmt.Errorf("unsupported type %q", typ)
	}
}
