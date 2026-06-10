// Corpus loader for the Tempo / TraceQL compatibility differ.
//
// The format is a lightweight TXTAR variant — the parser looks at
// single-line `-- section --` headers and treats every non-header
// line as section body. The supported sections:
//
//	-- name --                  short identifier, used in the report
//	-- query --                 the TraceQL expression (search / metrics
//	                            endpoints)
//	-- endpoint --              one of: search | search_recent | traces |
//	                            traces_v2 | tags_v1 | tags_v2 |
//	                            tag_values_v1 | tag_values_v2 |
//	                            metrics_range | metrics_instant
//	-- traceid_template --      a template like "{svc}/{idx}" (only for
//	                            `traces` / `traces_v2`)
//	-- tag_name --              attribute key whose values to enumerate
//	                            (tag_values_v1 / tag_values_v2 only)
//	-- scope --                 optional `?scope=` filter for tags_v2 (one of
//	                            resource | span | intrinsic | none); empty
//	                            omits the parameter so both backends return
//	                            the unfiltered scope set
//	-- step --                  step duration (e.g. "60s") — only for metrics_range
//	-- expected_min_traces --   integer; minimum traces both sides must return
//	-- expected_max_traces --   integer; maximum traces either side may return
//	-- expected_min_values --   integer; minimum list cardinality for tag /
//	                            tag-values endpoints
//	-- expected_max_values --   integer; maximum list cardinality for tag /
//	                            tag-values endpoints
//	-- expected_values --       newline-separated subset that must appear in
//	                            the response list (tag-names or tag-values)
//	-- expected_scopes --       newline-separated subset of scope names
//	                            (resource / span / intrinsic) that must
//	                            appear in tags_v2 responses
//	-- expected_services --     newline-separated list of `service.name` values
//	                            that should appear in `rootServiceName`
//	-- expected_root_name_re -- Go regexp; every returned trace's rootTraceName
//	                            must match
//	-- expected_min_series --   integer; minimum metrics series both sides must
//	                            return (metrics endpoints only)
//	-- expected_max_series --   integer; maximum metrics series either side may
//	                            return (metrics endpoints only)
//	-- expected_samples_per_series --   integer; minimum samples per series
//	                            (metrics endpoints only)
//	-- semantic_checks --       newline-separated list of semantic-consistency
//	                            check names; each check runs per-backend against
//	                            the parsed response (see differ.go::SemanticChecks
//	                            for the registered names)
//
// There is intentionally no opt-out marker on a per-case basis: a case
// that can't run is removed from the corpus, not flagged. Keeping the
// corpus a strict declaration of "this must diff cleanly" eliminates the
// "stub it for later" failure mode where a future contributor forgets to
// turn the case back on once the missing functionality lands.
//
// Cases are separated by the next `-- name --` header. Lines starting
// with `#` outside section bodies are comments. Inside a section body,
// `#`-prefixed lines are also stripped before the body is interpreted
// so a corpus author can group related cases with header comments
// between them without the trailing case's last section accidentally
// swallowing the comment.
//
// The section-header machinery is trivially small (~80 lines) so
// owning a TraceQL-specific loader is cheaper than carving a
// generic core.
// If we ever need a third copy, factor a small `txtar.Parser` then.

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// CorpusCase is one entry parsed out of the TXTAR file.
//
// Every field except Name + Query + Endpoint is optional; the differ
// applies whichever assertions are populated and ignores the rest. That
// keeps the corpus author free to add narrow per-case assertions without
// having to populate the whole schema.
type CorpusCase struct {
	// Name is a short identifier surfaced in the markdown report.
	Name string

	// Query is the raw TraceQL expression. It is sent to both backends
	// verbatim (URL-encoded by the differ) so a deliberate parser-only
	// query (no result expected) round-trips through the same code path
	// as a normal corpus case.
	Query string

	// Endpoint selects the HTTP path the differ hits:
	//
	//   * search           GET /api/search?q=<TraceQL>
	//   * search_recent    GET /api/search/recent (TraceQL ignored)
	//   * traces           GET /api/traces/<id>    (TraceIDTemplate populated)
	//   * traces_v2        GET /api/v2/traces/<id> (TraceIDTemplate populated;
	//                      body is the tempopb.TraceByIDResponse envelope,
	//                      not the bare v1 trace — see proto_fetch.go)
	//   * tags_v1          GET /api/search/tags
	//   * tags_v2          GET /api/v2/search/tags[?scope=<Scope>]
	//   * tag_values_v1    GET /api/search/tag/{TagName}/values
	//   * tag_values_v2    GET /api/v2/search/tag/{TagName}/values
	//   * metrics_range    GET /api/metrics/query_range (Step populated)
	//   * metrics_instant  GET /api/metrics/query
	//
	// The seeder pushes data with deterministic trace IDs derived from
	// (service, traceIdx); see TraceIDTemplate below for the format.
	Endpoint string

	// TraceIDTemplate is only consulted when Endpoint == "traces" or
	// "traces_v2".
	// Format: "<svc>/<idx>" — the differ derives the byte-identical
	// 16-byte trace ID via the same hash the seeder uses, hex-encodes
	// it, and substitutes it into the URL. Decoupling the template
	// from the binary trace ID keeps the corpus file human-editable.
	TraceIDTemplate string

	// TagName is the {name} path component for the tag_values_v1 /
	// tag_values_v2 endpoints. Required for those two endpoints, unused
	// elsewhere.
	TagName string

	// Scope is the optional `?scope=` query parameter for tags_v2. One of
	// "resource" / "span" / "intrinsic" / "none". Empty leaves the
	// parameter off the URL (Tempo defaults to returning every scope).
	Scope string

	// Step is only consulted when Endpoint == "metrics_range". The string
	// is sent verbatim as the URL's `step` parameter (Tempo accepts
	// "60s", "1m", or plain seconds). An empty Step on a metrics_range
	// case is a validation error.
	Step string

	// ExpectedMinTraces / ExpectedMaxTraces bound the cardinality both
	// backends must agree with. Zero (the default) disables the bound.
	ExpectedMinTraces int
	ExpectedMaxTraces int

	// ExpectedMinValues / ExpectedMaxValues bound the list-cardinality
	// of the tag / tag-values endpoints. Zero disables.
	ExpectedMinValues int
	ExpectedMaxValues int

	// ExpectedValues is a subset-must-be-present assertion for the
	// tag-names list (tags_v1 / tags_v2 flattened) or the tag-values list
	// (tag_values_v1 / tag_values_v2). Empty disables.
	ExpectedValues []string

	// ExpectedScopes is a subset-must-be-present assertion for the
	// tags_v2 response (the `Scopes[*].Name` strings). Empty disables.
	ExpectedScopes []string

	// ExpectedServices is a set membership assertion: every value listed
	// here must appear in at least one returned trace's rootServiceName.
	// Empty disables the assertion.
	ExpectedServices []string

	// ExpectedRootNameRE is a compiled regexp; if non-nil every returned
	// trace's rootTraceName must match. Compiled at parse time so the
	// differ runs without per-case regex cost.
	ExpectedRootNameRE *regexp.Regexp

	// ExpectedMinSeries / ExpectedMaxSeries bound the per-backend metrics
	// cardinality. Zero (the default) disables the bound. Applies to
	// metrics_range + metrics_instant endpoints only.
	ExpectedMinSeries int
	ExpectedMaxSeries int

	// ExpectedSamplesPerSeries is the minimum samples per series for
	// metrics_range cases. Zero disables the assertion. (Instant
	// responses don't carry a samples array — they have a single value
	// per series — so this is ignored for metrics_instant.)
	ExpectedSamplesPerSeries int

	// SemanticChecks is the list of per-backend semantic-consistency
	// check names. Each name is looked up in differ.go::SemanticChecks
	// at run time; an unknown name surfaces as an assertion reason so
	// a corpus author who mistypes a check learns from the report rather
	// than silently passing.
	//
	// A check may carry a colon-separated argument, e.g.
	// "groupby_labels_present:resource.service.name" — the parser stores
	// the raw "name:arg" string and the check function splits.
	SemanticChecks []string
}

// LoadCorpus opens a corpus file and parses it into CorpusCases.
// Errors carry the source path + line number for the failing entry so a
// hand-edit typo in the TXTAR shows up with actionable context.
func LoadCorpus(path string) ([]CorpusCase, error) {
	f, err := os.Open(path) //nolint:gosec // G304: corpus path is a trusted CLI argument
	if err != nil {
		return nil, fmt.Errorf("open corpus %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return parseCorpus(f, path)
}

// Section keywords recognized by the corpus parser. Kept as package-level
// constants so the parser + the header-validation helpers share a single
// source of truth and so unit tests can name them directly.
const (
	secName             = "name"
	secQuery            = "query"
	secEndpoint         = "endpoint"
	secTraceIDTemplate  = "traceid_template"
	secTagName          = "tag_name"
	secScope            = "scope"
	secStep             = "step"
	secMinTraces        = "expected_min_traces"
	secMaxTraces        = "expected_max_traces"
	secMinValues        = "expected_min_values"
	secMaxValues        = "expected_max_values"
	secValues           = "expected_values"
	secScopes           = "expected_scopes"
	secServices         = "expected_services"
	secRootNameRE       = "expected_root_name_re"
	secMinSeries        = "expected_min_series"
	secMaxSeries        = "expected_max_series"
	secSamplesPerSeries = "expected_samples_per_series"
	secSemanticChecks   = "semantic_checks"
)

// stripCommentLines returns the body with comment-only lines (`# ...`
// after trimming) removed. Comment lines between two cases are
// expected to be ignored (the corpus author wants to group related
// cases with a header comment) but they end up inside the previous
// case's last section body because the section closes only on the
// next `-- name --`. The same trick lets a section body legitimately
// contain a query that includes `#` — at the start of a line — though
// no current case does.
func stripCommentLines(raw string) string {
	var bodyLines []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	return strings.TrimSpace(strings.Join(bodyLines, "\n"))
}

// applySection routes a flushed section body to the right field on
// `cur`. The integer / regexp parsers wrap their errors with the
// section name so the caller can prefix source:line.
func applySection(cur *CorpusCase, section, body string) error {
	switch section {
	case secName:
		cur.Name = body
	case secQuery:
		cur.Query = body
	case secEndpoint:
		cur.Endpoint = body
	case secTraceIDTemplate:
		cur.TraceIDTemplate = body
	case secTagName:
		cur.TagName = body
	case secScope:
		cur.Scope = body
	case secStep:
		cur.Step = body
	case secMinTraces:
		return applyIntSection(body, "expected_min_traces", &cur.ExpectedMinTraces)
	case secMaxTraces:
		return applyIntSection(body, "expected_max_traces", &cur.ExpectedMaxTraces)
	case secMinValues:
		return applyIntSection(body, "expected_min_values", &cur.ExpectedMinValues)
	case secMaxValues:
		return applyIntSection(body, "expected_max_values", &cur.ExpectedMaxValues)
	case secValues:
		appendNonEmptyLines(body, &cur.ExpectedValues)
	case secScopes:
		appendNonEmptyLines(body, &cur.ExpectedScopes)
	case secServices:
		appendNonEmptyLines(body, &cur.ExpectedServices)
	case secRootNameRE:
		if body == "" {
			return nil
		}
		re, err := regexp.Compile(body)
		if err != nil {
			return fmt.Errorf("expected_root_name_re: compile %q: %w", body, err)
		}
		cur.ExpectedRootNameRE = re
	case secMinSeries:
		return applyIntSection(body, "expected_min_series", &cur.ExpectedMinSeries)
	case secMaxSeries:
		return applyIntSection(body, "expected_max_series", &cur.ExpectedMaxSeries)
	case secSamplesPerSeries:
		return applyIntSection(body, "expected_samples_per_series", &cur.ExpectedSamplesPerSeries)
	case secSemanticChecks:
		appendNonEmptyLines(body, &cur.SemanticChecks)
	}
	return nil
}

// appendNonEmptyLines splits `body` on newlines and appends every
// trimmed non-empty line to `*dst`. Pulled out so the multi-line
// subset assertions (expected_services / expected_values /
// expected_scopes / semantic_checks) share one parse path instead of
// four near-duplicates.
func appendNonEmptyLines(body string, dst *[]string) {
	if body == "" {
		return
	}
	for _, line := range strings.Split(body, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			*dst = append(*dst, s)
		}
	}
}

func applyIntSection(body, name string, dst *int) error {
	if body == "" {
		return nil
	}
	n, err := strconv.Atoi(body)
	if err != nil {
		return fmt.Errorf("%s: parse %q: %w", name, body, err)
	}
	*dst = n
	return nil
}

// validateCase checks the assembled case for required-field violations.
// Returns the case (with defaulted endpoint) ready to append.
func validateCase(cur CorpusCase, ord int) (CorpusCase, error) {
	if cur.Name == "" {
		return cur, fmt.Errorf("case missing -- name -- (case #%d)", ord)
	}
	if cur.Endpoint == "" {
		cur.Endpoint = "search"
	}
	if !isTagEndpoint(cur.Endpoint) && cur.Query == "" && cur.Endpoint != "search_recent" {
		return cur, fmt.Errorf("case %q missing -- query -- (search_recent and the four tag endpoints are the only kinds that may omit it)", cur.Name)
	}
	switch cur.Endpoint {
	case "search", "search_recent", "traces", "traces_v2",
		"tags_v1", "tags_v2", "tag_values_v1", "tag_values_v2",
		"metrics_range", "metrics_instant":
	default:
		return cur, fmt.Errorf("case %q: unknown endpoint %q (want search | search_recent | traces | traces_v2 | tags_v1 | tags_v2 | tag_values_v1 | tag_values_v2 | metrics_range | metrics_instant)", cur.Name, cur.Endpoint)
	}
	if (cur.Endpoint == "traces" || cur.Endpoint == "traces_v2") && cur.TraceIDTemplate == "" {
		return cur, fmt.Errorf("case %q: endpoint=%s requires -- traceid_template --", cur.Name, cur.Endpoint)
	}
	if (cur.Endpoint == "tag_values_v1" || cur.Endpoint == "tag_values_v2") && cur.TagName == "" {
		return cur, fmt.Errorf("case %q: endpoint=%s requires -- tag_name --", cur.Name, cur.Endpoint)
	}
	if cur.Scope != "" && cur.Endpoint != "tags_v2" {
		return cur, fmt.Errorf("case %q: -- scope -- is only valid for endpoint=tags_v2 (got %s)", cur.Name, cur.Endpoint)
	}
	if cur.Endpoint == "metrics_range" && cur.Step == "" {
		return cur, fmt.Errorf("case %q: endpoint=metrics_range requires -- step -- (e.g. \"60s\")", cur.Name)
	}
	return cur, nil
}

// isTagEndpoint reports whether the given endpoint is one of the four
// tag / tag-values endpoints, which all have no TraceQL query slot.
func isTagEndpoint(ep string) bool {
	switch ep {
	case "tags_v1", "tags_v2", "tag_values_v1", "tag_values_v2":
		return true
	}
	return false
}

// isKnownSection reports whether the given header name is a recognized
// body section (anything other than `name`, which is the case
// boundary).
func isKnownSection(name string) bool {
	switch name {
	case secQuery, secEndpoint, secTraceIDTemplate, secTagName, secScope,
		secStep,
		secMinTraces, secMaxTraces, secMinValues, secMaxValues,
		secValues, secScopes, secServices, secRootNameRE,
		secMinSeries, secMaxSeries, secSamplesPerSeries, secSemanticChecks:
		return true
	}
	return false
}

// parseCorpus is the unit-testable inner driver.
func parseCorpus(r io.Reader, source string) ([]CorpusCase, error) {
	var (
		out      []CorpusCase
		current  CorpusCase
		section  string
		buf      strings.Builder
		haveOpen bool
		lineNum  int
	)

	flushSection := func() error {
		body := stripCommentLines(buf.String())
		buf.Reset()
		return applySection(&current, section, body)
	}

	flushCase := func() error {
		if !haveOpen {
			return nil
		}
		v, err := validateCase(current, len(out)+1)
		if err != nil {
			return err
		}
		out = append(out, v)
		current = CorpusCase{}
		haveOpen = false
		return nil
	}

	handleHeader := func(name string) error {
		if haveOpen {
			if err := flushSection(); err != nil {
				return err
			}
		}
		switch {
		case name == secName:
			// New case starts. The `name` section is the canonical
			// case boundary (the first non-comment header).
			if err := flushCase(); err != nil {
				return err
			}
			haveOpen = true
			section = secName
		case isKnownSection(name):
			if !haveOpen {
				return fmt.Errorf("section %q outside a case (start with -- name --)", name)
			}
			section = name
		default:
			return fmt.Errorf("unknown section %q", name)
		}
		return nil
	}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "-- ") && strings.HasSuffix(trimmed, " --") {
			name := strings.TrimSpace(trimmed[3 : len(trimmed)-3])
			if err := handleHeader(name); err != nil {
				return nil, fmt.Errorf("%s:%d: %w", source, lineNum, err)
			}
			continue
		}

		if !haveOpen {
			// Skip leading comments / blank lines until the first `-- name --`.
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			return nil, fmt.Errorf("%s:%d: content before first '-- name --' header", source, lineNum)
		}

		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read corpus %q: %w", source, err)
	}

	// Final flushes.
	if haveOpen {
		if err := flushSection(); err != nil {
			return nil, fmt.Errorf("%s: trailing section: %w", source, err)
		}
		if err := flushCase(); err != nil {
			return nil, err
		}
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("%s: corpus is empty", source)
	}
	return out, nil
}
