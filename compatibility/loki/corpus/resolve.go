package corpus

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// The template placeholders a definition query may carry.
const (
	placeholderSelector   = "${SELECTOR}"
	placeholderLabelName  = "${LABEL_NAME}"
	placeholderLabelValue = "${LABEL_VALUE}"
	placeholderRange      = "${RANGE}"
)

// Direction is the log-query direction of an expanded case.
type Direction int

// The two directions. Their String forms are the wire names the Loki HTTP
// API accepts for `direction`.
const (
	Forward Direction = iota
	Backward
)

func (d Direction) String() string {
	if d == Backward {
		return "BACKWARD"
	}
	return "FORWARD"
}

// Case is one fully resolved query execution.
type Case struct {
	Query       string
	Start       time.Time
	End         time.Time
	Direction   Direction
	Step        time.Duration // metric cases only; zero for log cases
	Source      string        // `<suite>/<file>:<line>` of the definition
	Description string
	Tags        []string
}

// Kind classifies the case's resolved query; see [QueryKind].
func (c Case) Kind() string { return QueryKind(c.Query) }

// Resolver expands definitions against one dataset. It draws every random
// choice — the stream a `${SELECTOR}` picks, the label a `${LABEL_NAME}`
// picks, the window offset — from a single seeded source, so expanding the
// same definitions in the same order against the same metadata and seed
// yields the same cases on every run.
type Resolver struct {
	md  *Metadata
	rnd *rand.Rand
}

// NewResolver returns a resolver over md seeded with seed.
func NewResolver(md *Metadata, seed int64) *Resolver {
	return &Resolver{md: md, rnd: rand.New(rand.NewSource(seed))} //nolint:gosec // deterministic corpus expansion, not security
}

// Expand resolves def's placeholders and window, then fans it out by
// direction: a metric definition yields one forward case carrying the
// step; a log definition yields one case per direction its `directions:`
// names. instant selects the instant-query range bound for `${RANGE}`.
//
// The random draws happen in a fixed order — selector, label, window — so
// adding a placeholder to one definition never perturbs another's draws
// beyond the ones it consumes itself.
func (r *Resolver) Expand(def Definition, instant bool) ([]Case, error) {
	query, err := r.resolveQuery(def.Query, def.Requires, instant)
	if err != nil {
		return nil, err
	}
	length, err := def.length()
	if err != nil {
		return nil, err
	}
	step, err := def.step()
	if err != nil {
		return nil, err
	}
	start, end, err := r.window(length)
	if err != nil {
		return nil, err
	}

	base := Case{
		Query:       query,
		Start:       start,
		End:         end,
		Source:      def.Source,
		Description: def.Description,
		Tags:        def.Tags,
	}
	if def.Kind == KindMetric {
		base.Direction = Forward
		base.Step = step
		return []Case{base}, nil
	}

	var dirs []Direction
	switch def.Directions {
	case DirectionsForward:
		dirs = []Direction{Forward}
	case DirectionsBackward:
		dirs = []Direction{Backward}
	case DirectionsBoth:
		dirs = []Direction{Forward, Backward}
	default:
		return nil, fmt.Errorf("log query %q has no directions; Load defaults them to both", def.Description)
	}
	cases := make([]Case, 0, len(dirs))
	for _, dir := range dirs {
		c := base
		c.Direction = dir
		cases = append(cases, c)
	}
	return cases, nil
}

// resolveQuery substitutes the placeholders in query. `${SELECTOR}` is
// resolved first because both `${LABEL_*}` and `${RANGE}` are derived from
// the stream it picks.
func (r *Resolver) resolveQuery(query string, req Requirements, instant bool) (string, error) {
	var selector string
	if strings.Contains(query, placeholderSelector) {
		sel, err := r.pickSelector(req, strings.Contains(query, placeholderRange), instant)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", placeholderSelector, err)
		}
		selector = sel
		query = strings.ReplaceAll(query, placeholderSelector, selector)
	}

	if strings.Contains(query, placeholderLabelName) || strings.Contains(query, placeholderLabelValue) {
		if selector == "" {
			return "", fmt.Errorf("%s and %s are derived from %s, which the query does not use",
				placeholderLabelName, placeholderLabelValue, placeholderSelector)
		}
		name, value, err := r.pickLabel(selector, req.Labels)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", placeholderLabelName, err)
		}
		query = strings.ReplaceAll(query, placeholderLabelName, name)
		query = strings.ReplaceAll(query, placeholderLabelValue, value)
	}

	if strings.Contains(query, placeholderRange) {
		rng, err := r.streamRange(selector, instant)
		if err != nil {
			return "", fmt.Errorf("resolve %s: %w", placeholderRange, err)
		}
		query = strings.ReplaceAll(query, placeholderRange, rng)
	}
	return query, nil
}

// requirementIndex pairs one requirement dimension with the metadata index
// that answers it.
type requirementIndex struct {
	what   string
	values []string
	index  map[string][]string
}

// pickSelector draws one of the streams [Resolver.candidates] returns.
func (r *Resolver) pickSelector(req Requirements, needsRange, instant bool) (string, error) {
	candidates, err := r.candidates(req, needsRange, instant)
	if err != nil {
		return "", err
	}
	return candidates[r.rnd.Intn(len(candidates))], nil
}

// candidates narrows the dataset's selectors to the streams that satisfy
// every requirement, in `all_selectors` order. With needsRange, a stream
// without a positive range bound for the lane is not eligible, since
// `${RANGE}` could not then be resolved for it.
func (r *Resolver) candidates(req Requirements, needsRange, instant bool) ([]string, error) {
	candidates := r.md.AllSelectors
	if req.LogFormat != "" {
		candidates = Intersect(candidates, r.md.ByFormat[LogFormat(req.LogFormat)])
		if len(candidates) == 0 {
			return nil, fmt.Errorf("no stream has log format %q", req.LogFormat)
		}
	}
	for _, dim := range []requirementIndex{
		{"unwrappable field", req.UnwrappableFields, r.md.ByUnwrappableField},
		{"detected field", req.DetectedFields, r.md.ByDetectedField},
		{"structured metadata key", req.StructuredMetadata, r.md.ByStructuredMetadata},
		{"label", req.Labels, r.md.ByLabelKey},
		{"keyword", req.Keywords, r.md.ByKeyword},
	} {
		for _, v := range dim.values {
			candidates = Intersect(candidates, dim.index[v])
			if len(candidates) == 0 {
				return nil, fmt.Errorf("no stream has %s %q", dim.what, v)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("the dataset has no streams")
	}

	if needsRange {
		eligible := make([]string, 0, len(candidates))
		for _, sel := range candidates {
			if r.rangeFor(sel, instant) > 0 {
				eligible = append(eligible, sel)
			}
		}
		if len(eligible) == 0 {
			return nil, fmt.Errorf("no candidate stream has a positive %s range bound", laneName(instant))
		}
		candidates = eligible
	}
	return candidates, nil
}

// pickLabel draws one equality matcher of selector as the `${LABEL_NAME}` /
// `${LABEL_VALUE}` pair, preferring the labels the definition requires
// when the selector has any of them.
func (r *Resolver) pickLabel(selector string, required []string) (name, value string, err error) {
	matchers, err := selectorMatchers(selector)
	if err != nil {
		return "", "", err
	}
	var all, preferred []*labels.Matcher
	for _, m := range matchers {
		if m.Type != labels.MatchEqual {
			continue
		}
		all = append(all, m)
		if slices.Contains(required, m.Name) {
			preferred = append(preferred, m)
		}
	}
	pool := all
	if len(preferred) > 0 {
		pool = preferred
	}
	if len(pool) == 0 {
		return "", "", fmt.Errorf("selector %s has no equality matcher", selector)
	}
	m := pool[r.rnd.Intn(len(pool))]
	return m.Name, m.Value, nil
}

// selectorMatchers parses a bare stream selector with cerberus's LogQL
// parser and returns its matchers in written order.
func selectorMatchers(selector string) ([]*labels.Matcher, error) {
	expr, err := lsyntax.ParseExpr(selector)
	if err != nil {
		return nil, fmt.Errorf("parse selector %s: %w", selector, err)
	}
	sel, ok := expr.(*lsyntax.MatchersExpr)
	if !ok {
		return nil, fmt.Errorf("%s is not a bare stream selector", selector)
	}
	return sel.Matchers(), nil
}

// streamRange renders the lane's range bound of the stream selector picked.
func (r *Resolver) streamRange(selector string, instant bool) (string, error) {
	if selector == "" {
		return "", fmt.Errorf("%s is derived from %s, which the query does not use", placeholderRange, placeholderSelector)
	}
	if _, ok := r.md.RangesBySelector[selector]; !ok {
		return "", fmt.Errorf("the dataset metadata has no range bounds for %s", selector)
	}
	d := r.rangeFor(selector, instant)
	if d <= 0 {
		return "", fmt.Errorf("stream %s has no positive %s range bound", selector, laneName(instant))
	}
	return logqlDuration(d), nil
}

// rangeFor is the stream's range bound for the lane, zero when unknown.
func (r *Resolver) rangeFor(selector string, instant bool) time.Duration {
	ranges := r.md.RangesBySelector[selector]
	switch {
	case ranges == nil:
		return 0
	case instant:
		return ranges.MinInstantRange
	default:
		return ranges.MinRange
	}
}

// window draws a [start, end) of the given length inside the dataset span.
func (r *Resolver) window(length time.Duration) (start, end time.Time, err error) {
	span := r.md.TimeRange.End.Sub(r.md.TimeRange.Start)
	if span < length {
		return time.Time{}, time.Time{}, fmt.Errorf("window %v is longer than the dataset span %v", length, span)
	}
	var offset time.Duration
	if slack := span - length; slack > 0 {
		offset = time.Duration(r.rnd.Int63n(int64(slack)))
	}
	start = r.md.TimeRange.Start.Add(offset)
	return start, start.Add(length), nil
}

// logqlDuration renders d as a LogQL range literal. Go's Duration.String
// always spells the seconds field, so a whole number of minutes such as
// "5m0s" drops its zero seconds to read "5m".
func logqlDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	return s
}

// Intersect returns the elements of a that b also carries, in a's order.
// It tests membership, so neither list needs to be sorted.
func Intersect(a, b []string) []string {
	inB := make(map[string]struct{}, len(b))
	for _, s := range b {
		inB[s] = struct{}{}
	}
	out := make([]string, 0, min(len(a), len(b)))
	for _, s := range a {
		if _, ok := inB[s]; ok {
			out = append(out, s)
		}
	}
	return out
}

func laneName(instant bool) string {
	if instant {
		return "instant"
	}
	return "range"
}
