package shadow

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Query is a single corpus entry.
type Query struct {
	// Name is a short identifier for reporting (file:line or explicit `-- name --`).
	Name string
	// Expr is the PromQL expression to evaluate.
	Expr string
	// ExpectedStrategy optionally overrides the CLI-level strategy for this query.
	// Empty string means "use the CLI strategy".
	ExpectedStrategy string
}

// LoadCorpus reads a corpus file. Format is a lightweight TXTAR variant:
//
//	-- query --
//	rate(http_requests_total[5m])
//	-- expected_strategy --
//	prefer-native
//	-- query --
//	up
//
// Lines starting with `#` outside section bodies are comments and ignored.
// Blank lines between sections are ignored. The first `-- query --` marker
// starts the first entry.
func LoadCorpus(path string) ([]Query, error) {
	f, err := os.Open(path) //nolint:gosec // G304: corpus path is a trusted CLI argument
	if err != nil {
		return nil, fmt.Errorf("open corpus %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return parseCorpus(f, path)
}

func parseCorpus(r io.Reader, source string) ([]Query, error) {
	const (
		secQuery    = "query"
		secStrategy = "expected_strategy"
	)

	var (
		out      []Query
		current  Query
		section  string
		buf      strings.Builder
		haveOpen bool
		lineNum  int
	)

	flushSection := func() {
		body := strings.TrimSpace(buf.String())
		buf.Reset()
		switch section {
		case secQuery:
			current.Expr = body
		case secStrategy:
			current.ExpectedStrategy = body
		}
	}

	flushQuery := func() {
		if !haveOpen {
			return
		}
		if current.Name == "" {
			current.Name = fmt.Sprintf("%s:q%d", source, len(out)+1)
		}
		out = append(out, current)
		current = Query{}
		haveOpen = false
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
			name := strings.TrimSpace(trimmed[3 : len(trimmed)-3])
			// First header inside a query closes the previous section.
			if haveOpen {
				flushSection()
			}
			switch name {
			case secQuery:
				// New query starts.
				flushQuery()
				haveOpen = true
				section = secQuery
			case secStrategy:
				if !haveOpen {
					return nil, fmt.Errorf("%s:%d: expected_strategy outside a query block", source, lineNum)
				}
				section = secStrategy
			default:
				return nil, fmt.Errorf("%s:%d: unknown section %q", source, lineNum, name)
			}
			continue
		}

		if !haveOpen {
			// Skip leading comments / blank lines until the first `-- query --`.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			return nil, fmt.Errorf("%s:%d: content before first '-- query --' header", source, lineNum)
		}

		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read corpus %q: %w", source, err)
	}

	// Final flushes.
	if haveOpen {
		flushSection()
		flushQuery()
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s: corpus is empty", source)
	}
	for i, q := range out {
		if q.Expr == "" {
			return nil, fmt.Errorf("%s: query #%d has empty expression", source, i+1)
		}
	}

	return out, nil
}
