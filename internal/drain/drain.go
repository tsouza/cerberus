// Package drain mines log-line templates from a stream of log records
// using the Drain algorithm — Pinjia He, Jieming Zhu, Zibin Zheng and
// Michael R. Lyu, "Drain: An Online Log Parsing Approach with Fixed
// Depth Tree", ICWS 2017.
//
// This is a clean-room in-house implementation written from the
// published algorithm, NOT ported from any existing source. It exists
// so the cerberus binary can serve Loki's `/loki/api/v1/patterns`
// endpoint without linking the AGPLv3 `github.com/grafana/loki/v3/pkg/
// pattern/drain` package (and its transitive `pkg/logproto` →
// `pkg/logql/syntax` AGPL closure).
//
// Algorithm recap (paper §3):
//
//   - Preprocessing: each log message is split into tokens on
//     whitespace.
//   - Step 1 — search by length: the first tree layer buckets messages
//     by their token count, on the premise that messages from the same
//     template usually share a length.
//   - Step 2 — search by preceding tokens: a fixed-depth tree is walked
//     using the leading tokens. A token that contains a digit is treated
//     as a wildcard during the walk (numeric tokens are almost always
//     variables), which keeps the tree from exploding. A per-node child
//     cap routes overflow to the wildcard branch.
//   - Step 3 — search by token similarity: at the leaf, the message is
//     compared against each candidate log group's template token by
//     token; similarity is the fraction of non-wildcard positions that
//     match. The closest group above a threshold wins; otherwise a new
//     group is created.
//   - Update: on a match, every template position whose token differs
//     from the message becomes a wildcard, generalising the template.
package drain

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// wildcard is the placeholder token Drain stores for variable
// positions, both in the parse tree and in a cluster's template. It is
// rendered to the Loki wire form by [Cluster.String].
const wildcard = "<*>"

// wireWildcard is how a variable position is rendered in the
// `/patterns` response, matching Loki's pattern template format
// (`<_>`).
const wireWildcard = "<_>"

// Default tuning, from the paper's evaluation (depth 4, similarity
// threshold 0.4) plus a conventional 100-child cap to bound tree fan-out.
const (
	defaultDepth        = 4
	defaultSimThreshold = 0.4
	defaultMaxChildren  = 100
	// defaultSampleResolution buckets per-cluster sample counts onto a
	// coarse time grid for the `/patterns` time series, mirroring the
	// resolution Loki's pattern endpoint reports.
	defaultSampleResolution = 10 * time.Second
)

// Config tunes the miner. The zero value is not usable; build one with
// [DefaultConfig].
type Config struct {
	// Depth is the fixed tree depth (number of leading-token layers
	// walked before reaching the leaf log groups). Must be >= 2.
	Depth int
	// SimThreshold is the minimum token-similarity (in [0,1]) for a
	// message to join an existing cluster rather than spawn a new one.
	SimThreshold float64
	// MaxChildren caps the children of any internal tree node; overflow
	// is folded into the wildcard branch.
	MaxChildren int
	// SampleResolution is the time-bucket granularity for per-cluster
	// sample counts.
	SampleResolution time.Duration
}

// DefaultConfig returns the paper's recommended parameters.
func DefaultConfig() Config {
	return Config{
		Depth:            defaultDepth,
		SimThreshold:     defaultSimThreshold,
		MaxChildren:      defaultMaxChildren,
		SampleResolution: defaultSampleResolution,
	}
}

// Sample is one time-bucketed observation count for a cluster.
type Sample struct {
	// TimestampUnixSec is the bucket's start, in whole seconds since the
	// Unix epoch.
	TimestampUnixSec int64
	// Count is the number of log lines that landed in this bucket.
	Count int64
}

// Cluster is a discovered log template plus its observation counts.
type Cluster struct {
	tokens  []string
	count   int64
	samples map[int64]int64
	res     time.Duration
}

// String renders the template in Loki's wire form: tokens joined by a
// single space, with variable positions shown as `<_>`.
func (c *Cluster) String() string {
	if len(c.tokens) == 0 {
		return ""
	}
	out := make([]string, len(c.tokens))
	for i, tok := range c.tokens {
		if tok == wildcard {
			out[i] = wireWildcard
			continue
		}
		out[i] = tok
	}
	return strings.Join(out, " ")
}

// Count is the total number of log lines folded into this cluster.
func (c *Cluster) Count() int64 { return c.count }

// Samples returns the per-bucket observation counts, ascending by
// timestamp.
func (c *Cluster) Samples() []Sample {
	out := make([]Sample, 0, len(c.samples))
	for ts, n := range c.samples {
		out = append(out, Sample{TimestampUnixSec: ts, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimestampUnixSec < out[j].TimestampUnixSec })
	return out
}

// observe records one log line at tsNano against this cluster.
func (c *Cluster) observe(tsNano int64) {
	c.count++
	bucket := time.Unix(0, tsNano).Truncate(c.res).Unix()
	if c.samples == nil {
		c.samples = map[int64]int64{}
	}
	c.samples[bucket]++
}

// node is one parse-tree node. Internal nodes route by token via
// children; leaf nodes carry the log groups in clusters.
type node struct {
	children map[string]*node
	clusters []*Cluster
}

func newNode() *node { return &node{children: map[string]*node{}} }

// Miner is a Drain parse tree. It is not safe for concurrent use; the
// cerberus `/patterns` handler builds a fresh miner per request.
type Miner struct {
	cfg  Config
	root *node
	all  []*Cluster
}

// New builds an empty miner with cfg.
func New(cfg Config) *Miner {
	if cfg.Depth < 2 {
		cfg.Depth = defaultDepth
	}
	if cfg.MaxChildren < 1 {
		cfg.MaxChildren = defaultMaxChildren
	}
	if cfg.SampleResolution <= 0 {
		cfg.SampleResolution = defaultSampleResolution
	}
	return &Miner{cfg: cfg, root: newNode()}
}

// Train folds one log line (observed at tsNano nanoseconds since the
// epoch) into the tree, creating or updating a cluster. Empty lines are
// ignored.
func (m *Miner) Train(line string, tsNano int64) {
	tokens := strings.Fields(line)
	if len(tokens) == 0 {
		return
	}
	if cl := m.treeSearch(tokens); cl != nil {
		m.updateTemplate(cl, tokens)
		cl.observe(tsNano)
		return
	}
	cl := &Cluster{tokens: append([]string(nil), tokens...), res: m.cfg.SampleResolution}
	m.addCluster(cl)
	cl.observe(tsNano)
	m.all = append(m.all, cl)
}

// Clusters returns every discovered cluster in creation order. The
// caller is responsible for any wire-shape sorting.
func (m *Miner) Clusters() []*Cluster { return m.all }

// treeSearch walks the fixed-depth tree for tokens and returns the
// best-matching cluster at the leaf, or nil if no path or no group
// clears the similarity threshold.
func (m *Miner) treeSearch(tokens []string) *Cluster {
	lenNode, ok := m.root.children[strconv.Itoa(len(tokens))]
	if !ok {
		return nil
	}
	parent := lenNode
	depth := 1
	for _, tok := range tokens {
		if depth >= m.cfg.Depth || depth > len(tokens) {
			break
		}
		if child, ok := parent.children[tok]; ok {
			parent = child
		} else if child, ok := parent.children[wildcard]; ok {
			parent = child
		} else {
			return nil
		}
		depth++
	}
	return m.fastMatch(parent.clusters, tokens)
}

// addCluster inserts cl into the tree, creating the length and
// leading-token nodes along the way. Numeric leading tokens collapse to
// the wildcard branch, as does any node that exceeds the child cap.
func (m *Miner) addCluster(cl *Cluster) {
	tokens := cl.tokens
	lenKey := strconv.Itoa(len(tokens))
	lenNode, ok := m.root.children[lenKey]
	if !ok {
		lenNode = newNode()
		m.root.children[lenKey] = lenNode
	}
	parent := lenNode
	depth := 1
	for _, tok := range tokens {
		if depth >= m.cfg.Depth || depth > len(tokens) {
			break
		}
		parent = m.descendForInsert(parent, tok)
		depth++
	}
	parent.clusters = append(parent.clusters, cl)
}

// descendForInsert returns the child of parent that tok routes to when
// inserting, creating it if necessary.
func (m *Miner) descendForInsert(parent *node, tok string) *node {
	if hasNumber(tok) {
		return getOrCreate(parent, wildcard)
	}
	if child, ok := parent.children[tok]; ok {
		return child
	}
	// New literal child only if there is room; otherwise overflow into
	// the wildcard branch to bound fan-out.
	if len(parent.children) >= m.cfg.MaxChildren {
		return getOrCreate(parent, wildcard)
	}
	return getOrCreate(parent, tok)
}

func getOrCreate(parent *node, key string) *node {
	if child, ok := parent.children[key]; ok {
		return child
	}
	child := newNode()
	parent.children[key] = child
	return child
}

// fastMatch picks the candidate cluster with the highest token
// similarity (tie-broken by wildcard count) that clears the threshold.
func (m *Miner) fastMatch(candidates []*Cluster, tokens []string) *Cluster {
	var best *Cluster
	bestSim := -1.0
	bestParams := -1
	for _, cl := range candidates {
		sim, params := seqSimilarity(cl.tokens, tokens)
		if sim > bestSim || (sim == bestSim && params > bestParams) {
			bestSim, bestParams, best = sim, params, cl
		}
	}
	if best != nil && bestSim >= m.cfg.SimThreshold {
		return best
	}
	return nil
}

// updateTemplate generalises cl's template against a newly matched line:
// any non-wildcard position whose token differs becomes a wildcard.
func (m *Miner) updateTemplate(cl *Cluster, tokens []string) {
	for i := range cl.tokens {
		if i >= len(tokens) {
			break
		}
		if cl.tokens[i] != wildcard && cl.tokens[i] != tokens[i] {
			cl.tokens[i] = wildcard
		}
	}
}

// seqSimilarity returns the fraction of non-wildcard template positions
// that equal the message token, and the number of wildcard positions.
// template and tokens are assumed equal length (same length-node leaf).
func seqSimilarity(template, tokens []string) (sim float64, params int) {
	if len(template) == 0 {
		return 1, 0
	}
	matched := 0
	for i, tmpl := range template {
		if tmpl == wildcard {
			params++
			continue
		}
		if i < len(tokens) && tmpl == tokens[i] {
			matched++
		}
	}
	return float64(matched) / float64(len(template)), params
}

// hasNumber reports whether tok contains a decimal digit — Drain's
// heuristic for "this token is probably a variable".
func hasNumber(tok string) bool {
	for _, r := range tok {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}
