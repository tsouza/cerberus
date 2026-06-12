//go:build chdb

package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	chdb "github.com/chdb-io/chdb-go/chdb"
)

// session wraps a single in-process chDB session shared across every
// measurement in one bench-report run. It mirrors the plumbing the
// corpus profiler (test/perf/profile.Profiler) and the per-construct
// scaling harness use: a process-global chdb-go session, CSV for
// fire-and-forget DDL/INSERT, JSON for scalar reads.
type session struct {
	sess *chdb.Session
}

func newSession() (*session, error) {
	s, err := chdb.NewSession("")
	if err != nil {
		return nil, fmt.Errorf("open chdb session: %w", err)
	}
	return &session{sess: s}, nil
}

func (s *session) close() {
	if s.sess != nil {
		s.sess.Cleanup()
		s.sess = nil
	}
}

// exec runs a statement for its side effect (DDL / INSERT) and surfaces
// any chDB-side error.
func (s *session) exec(stmt string) error {
	res, err := s.sess.Query(stmt, "CSV")
	if err != nil {
		return err
	}
	if res != nil {
		if e := res.Error(); e != nil {
			return e
		}
	}
	return nil
}

// execAll splits a multi-statement script on `;` and runs each fragment.
func (s *session) execAll(script string) error {
	for _, stmt := range strings.Split(script, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if err := s.exec(stmt); err != nil {
			return fmt.Errorf("%w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

// scalarCount runs `SELECT count() FROM (<inner>)` and returns the row
// count. The count() wrapper makes chdb-go's driver return a single row
// while the work being measured (scan + fan-out + group-by) is identical
// to the bare SELECT.
func (s *session) scalarCount(inner string) (int64, error) {
	res, err := s.sess.Query("SELECT count() FROM ("+inner+")", "JSON")
	if err != nil {
		return 0, err
	}
	if e := res.Error(); e != nil {
		return 0, e
	}
	return parseSingleNumber(res.String())
}

// bestWall runs `SELECT count() FROM (<q>)` `iters` times and returns the
// fastest (min) wall time. min strips GC / scheduler jitter — the floor
// is the most stable single-process estimate, matching the scaling
// harness's bestOf and the orderby test's timeQuery.
func (s *session) bestWall(q string, iters int) (time.Duration, error) {
	if iters <= 0 {
		iters = 5
	}
	best := time.Hour
	wrapped := "SELECT count() FROM (" + q + ")"
	for i := 0; i < iters; i++ {
		start := time.Now()
		res, err := s.sess.Query(wrapped, "CSV")
		if err != nil {
			return 0, err
		}
		if e := res.Error(); e != nil {
			return 0, e
		}
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return best, nil
}

// explainSelectedGranules runs EXPLAIN indexes=1 over query and returns
// the leading integer of the `Granules: <selected>/<total>` line — the
// granules ClickHouse reads after PK pruning. Returns -1 if absent.
func (s *session) explainSelectedGranules(query string) (selected, total int, err error) {
	res, qerr := s.sess.Query("EXPLAIN indexes=1 "+query, "CSV")
	if qerr != nil {
		return -1, -1, qerr
	}
	if e := res.Error(); e != nil {
		return -1, -1, e
	}
	selected, total = -1, -1
	for _, line := range strings.Split(res.String(), "\n") {
		t := strings.TrimSpace(strings.Trim(line, "\""))
		if strings.HasPrefix(t, "Granules:") {
			rest := strings.TrimSpace(strings.TrimPrefix(t, "Granules:"))
			parts := strings.SplitN(rest, "/", 2)
			if n, e := strconv.Atoi(strings.TrimSpace(parts[0])); e == nil {
				selected = n
			}
			if len(parts) == 2 {
				if n, e := strconv.Atoi(strings.TrimSpace(parts[1])); e == nil {
					total = n
				}
			}
		}
	}
	return selected, total, nil
}

// parseSingleNumber pulls the single numeric value out of a chDB JSON
// result body shaped `{"data":[{"count()":N}], ...}`. Mirrors
// test/perf/profile.parseSingleCount — decoding the `data` array's first
// cell, not a textual scan (which would catch the statistics block's
// trailing numbers).
func parseSingleNumber(jsonBody string) (int64, error) {
	var parsed struct {
		Data []map[string]json.Number `json:"data"`
	}
	dec := json.NewDecoder(strings.NewReader(jsonBody))
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		return 0, fmt.Errorf("decode count json: %w", err)
	}
	if len(parsed.Data) == 0 {
		return 0, nil
	}
	for _, v := range parsed.Data[0] {
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("count not an int: %w", err)
		}
		return n, nil
	}
	return 0, nil
}

// inlineArgs textually substitutes positional `?` placeholders with their
// args (chDB's session API has no placeholder binding). String args are
// single-quoted with internal quotes doubled; time args are rendered as
// toDateTime64 literals; everything else uses its default Go format. The
// substituted SQL is semantically identical for measurement.
func inlineArgs(query string, args []any) string {
	if len(args) == 0 {
		return query
	}
	var b strings.Builder
	ai := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' && ai < len(args) {
			b.WriteString(renderArg(args[ai]))
			ai++
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

func renderArg(a any) string {
	switch v := a.(type) {
	case string:
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	case time.Time:
		return fmt.Sprintf("toDateTime64('%s', 9)", v.UTC().Format("2006-01-02 15:04:05.000000000"))
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		if v {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", v)
	}
}
