package routerrules

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// jsonlCorpusSource reads the per-pod JSONL corpus (the default fallback sink)
// and implements aggregation, percentiles, and rule evaluation in-Go. It needs
// no ClickHouse, so the catalog is testable against a seeded fixture file. The
// JSON shape matches optcorpus.Row (column-for-column with the CH table), plus
// an optional event_time field for --since windowing.
type jsonlCorpusSource struct {
	path  string
	since float64 // event_time floor (unix seconds); 0 disables windowing
}

// NewJSONLCorpusSource builds a JSONL-backed source over a file or directory.
// sinceUnix is the event_time lower bound in unix seconds (0 = no bound).
func NewJSONLCorpusSource(path string, sinceUnix float64) CorpusSource {
	return &jsonlCorpusSource{path: path, since: sinceUnix}
}

// jsonlRow mirrors optcorpus.Row's JSON tags plus event_time. Numeric corpus
// columns are decoded as float64 so the in-Go matcher shares one comparison
// path. event_time is optional (the JSONL sink may omit it; the CH table always
// has it) and accepted as a unix-seconds number when present.
type jsonlRow struct {
	EventTime           float64 `json:"event_time"`
	ShapeID             string  `json:"shape_id"`
	Language            string  `json:"language"`
	NormalizedQueryHash uint64  `json:"normalized_query_hash"`
	NAnchors            float64 `json:"n_anchors"`
	Fanout              float64 `json:"fanout"`
	CumulativeD         float64 `json:"cumulative_d"`
	OuterRange          float64 `json:"outer_range"`
	Step                float64 `json:"step"`
	Route               string  `json:"route"`
	KShards             float64 `json:"k_shards"`
	DecisionReason      string  `json:"decision_reason"`
	ReadRows            float64 `json:"read_rows"`
	ReadBytes           float64 `json:"read_bytes"`
	QueryDurationMS     float64 `json:"query_duration_ms"`
	MemoryUsage         float64 `json:"memory_usage"`
	ExitStatus          string  `json:"exit_status"`
}

func (r jsonlRow) toCorpusRow() corpusRow {
	return corpusRow{
		eventTimeUnix: r.EventTime,
		numeric: map[string]float64{
			"n_anchors":             r.NAnchors,
			"fanout":                r.Fanout,
			"cumulative_d":          r.CumulativeD,
			"outer_range":           r.OuterRange,
			"step":                  r.Step,
			"k_shards":              r.KShards,
			"read_rows":             r.ReadRows,
			"read_bytes":            r.ReadBytes,
			"query_duration_ms":     r.QueryDurationMS,
			"memory_usage":          r.MemoryUsage,
			"normalized_query_hash": float64(r.NormalizedQueryHash),
		},
		str: map[string]string{
			"shape_id":              r.ShapeID,
			"language":              r.Language,
			"route":                 r.Route,
			"decision_reason":       r.DecisionReason,
			"exit_status":           r.ExitStatus,
			"normalized_query_hash": formatNumeric(float64(r.NormalizedQueryHash)),
		},
	}
}

// stream decodes the JSONL corpus row-by-row, applying the --since window, and
// invokes fn per row. Decode is streaming so a large corpus never loads wholly
// into memory.
func (s *jsonlCorpusSource) stream(fn func(corpusRow) error) error {
	files, err := s.files()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := s.streamFile(f, fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *jsonlCorpusSource) streamFile(path string, fn func(corpusRow) error) error {
	f, err := os.Open(path) //nolint:gosec // operator-supplied corpus path; offline CLI.
	if err != nil {
		return fmt.Errorf("routerrules: open corpus %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, corpusScanInitial), corpusScanMax)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var jr jsonlRow
		if err := json.Unmarshal([]byte(line), &jr); err != nil {
			return fmt.Errorf("routerrules: decode corpus line in %q: %w", path, err)
		}
		if s.since > 0 && jr.EventTime > 0 && jr.EventTime < s.since {
			continue
		}
		if err := fn(jr.toCorpusRow()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return fmt.Errorf("routerrules: scan corpus %q: %w", path, err)
	}
	return nil
}

func (s *jsonlCorpusSource) files() ([]string, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return nil, fmt.Errorf("routerrules: stat corpus path %q: %w", s.path, err)
	}
	if !info.IsDir() {
		return []string{s.path}, nil
	}
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return nil, fmt.Errorf("routerrules: read corpus dir %q: %w", s.path, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".json") {
			out = append(out, filepath.Join(s.path, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Aggregate resolves a corpus param in-Go.
func (s *jsonlCorpusSource) Aggregate(_ context.Context, spec AggSpec) (Value, error) {
	switch {
	case spec.CountRatio:
		return s.countRatio(spec)
	case spec.Percentile != nil:
		return s.percentile(spec)
	case spec.Agg != "":
		return s.agg(spec)
	default:
		return Value{}, fmt.Errorf("routerrules: jsonl aggregate: empty AggSpec for column %q", spec.Column)
	}
}

func (s *jsonlCorpusSource) percentile(spec AggSpec) (Value, error) {
	buckets := map[string][]float64{}
	all := []float64{}
	err := s.stream(func(r corpusRow) error {
		if !matchScope(r, spec.Scope) {
			return nil
		}
		v := r.numericValue(spec.Column)
		if len(spec.PartitionBy) > 0 {
			key := r.enumValue(spec.PartitionBy[0])
			buckets[key] = append(buckets[key], v)
		} else {
			all = append(all, v)
		}
		return nil
	})
	if err != nil {
		return Value{}, err
	}
	if len(spec.PartitionBy) > 0 {
		part := make(map[string]float64, len(buckets))
		for k, vs := range buckets {
			part[k] = quantileExact(vs, *spec.Percentile)
		}
		return Value{Partition: part, PartitionCol: spec.PartitionBy[0]}, nil
	}
	return Value{Scalar: quantileExact(all, *spec.Percentile)}, nil
}

func (s *jsonlCorpusSource) agg(spec AggSpec) (Value, error) {
	buckets := map[string][]float64{}
	all := []float64{}
	err := s.stream(func(r corpusRow) error {
		if !matchScope(r, spec.Scope) {
			return nil
		}
		v := r.numericValue(spec.Column)
		if len(spec.PartitionBy) > 0 {
			key := r.enumValue(spec.PartitionBy[0])
			buckets[key] = append(buckets[key], v)
		} else {
			all = append(all, v)
		}
		return nil
	})
	if err != nil {
		return Value{}, err
	}
	if len(spec.PartitionBy) > 0 {
		part := make(map[string]float64, len(buckets))
		for k, vs := range buckets {
			part[k] = aggregate(spec.Agg, vs)
		}
		return Value{Partition: part, PartitionCol: spec.PartitionBy[0]}, nil
	}
	return Value{Scalar: aggregate(spec.Agg, all)}, nil
}

func (s *jsonlCorpusSource) countRatio(spec AggSpec) (Value, error) {
	var num, den float64
	err := s.stream(func(r corpusRow) error {
		if matchScope(r, spec.NumScope) {
			num++
		}
		if matchScope(r, spec.DenScope) {
			den++
		}
		return nil
	})
	if err != nil {
		return Value{}, err
	}
	if den == 0 {
		return Value{Scalar: 0}, nil
	}
	return Value{Scalar: num / den}, nil
}

// EvalRule folds the corpus in-Go: it groups matched rows by the rule's
// group_by columns, counting support and accumulating evidence aggregates.
func (s *jsonlCorpusSource) EvalRule(_ context.Context, q RuleQuery) ([]GroupResult, error) {
	type acc struct {
		key      []string
		support  int64
		evidence []evidenceAcc
	}
	groups := map[string]*acc{}
	var order []string

	err := s.stream(func(r corpusRow) error {
		ok, err := q.Condition.match(r, q.Env)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		key := make([]string, len(q.GroupBy))
		for i, col := range q.GroupBy {
			key[i] = r.groupValue(col)
		}
		gk := strings.Join(key, "\x00")
		a := groups[gk]
		if a == nil {
			a = &acc{key: key, evidence: make([]evidenceAcc, len(q.Evidence))}
			for i := range a.evidence {
				a.evidence[i].fn = q.Evidence[i].fn
			}
			groups[gk] = a
			order = append(order, gk)
		}
		a.support++
		for i, ev := range q.Evidence {
			a.evidence[i].add(r.numericValue(ev.column))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(order)
	out := make([]GroupResult, 0, len(order))
	for _, gk := range order {
		a := groups[gk]
		ev := make([]float64, len(a.evidence))
		for i := range a.evidence {
			ev[i] = a.evidence[i].result()
		}
		out = append(out, GroupResult{GroupKey: a.key, Support: a.support, Evidence: ev})
	}
	return out, nil
}

// matchScope reports whether a row satisfies an enum-equality scope filter.
func matchScope(r corpusRow, scope Scope) bool {
	for col, want := range scope {
		if r.enumValue(col) != want {
			return false
		}
	}
	return true
}

// evidenceAcc accumulates one evidence aggregate over matched rows.
type evidenceAcc struct {
	fn    AggFunc
	count int64
	sum   float64
	sumsq float64
	max   float64
	min   float64
	seen  bool
}

func (e *evidenceAcc) add(v float64) {
	e.count++
	e.sum += v
	e.sumsq += v * v
	if !e.seen || v > e.max {
		e.max = v
	}
	if !e.seen || v < e.min {
		e.min = v
	}
	e.seen = true
}

func (e *evidenceAcc) result() float64 {
	return aggregateAcc(e.fn, e.count, e.sum, e.sumsq, e.max, e.min)
}

func aggregateAcc(fn AggFunc, count int64, sum, sumsq, max, min float64) float64 {
	if count == 0 {
		return 0
	}
	switch fn {
	case AggMax:
		return max
	case AggMin:
		return min
	case AggAvg:
		return sum / float64(count)
	case AggStdDev:
		mean := sum / float64(count)
		variance := sumsq/float64(count) - mean*mean
		if variance < 0 {
			variance = 0
		}
		return math.Sqrt(variance)
	default:
		return 0
	}
}

// aggregate computes a scalar aggregate over a value slice (param resolution).
func aggregate(fn AggFunc, vs []float64) float64 {
	if len(vs) == 0 {
		return 0
	}
	var sum, sumsq, mx, mn float64
	mx, mn = vs[0], vs[0]
	for _, v := range vs {
		sum += v
		sumsq += v * v
		if v > mx {
			mx = v
		}
		if v < mn {
			mn = v
		}
	}
	return aggregateAcc(fn, int64(len(vs)), sum, sumsq, mx, mn)
}

// quantileExact computes the exact quantile of vs at fraction p in [0,1] with
// the SAME rank formula ClickHouse's quantileExact uses, so the JSONL path and
// the CH path agree byte-for-byte at the cutoff. ClickHouse's QuantileExact
// sorts the values and returns the element at index n = level * size, clamped to
// the last element — nearest-rank, no interpolation. An empty slice yields 0.
func quantileExact(vs []float64, p float64) float64 {
	size := len(vs)
	if size == 0 {
		return 0
	}
	sorted := make([]float64, size)
	copy(sorted, vs)
	sort.Float64s(sorted)
	// Mirror ClickHouse QuantileExact::get: size_t n = level * size; clamp to
	// [0, size-1]; return the nth order statistic.
	idx := int(p * float64(size))
	if idx >= size {
		idx = size - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

const (
	// corpusScanInitial / corpusScanMax bound the JSONL line scanner. A corpus
	// row is small, but a pathological long line shouldn't crash the scan; the
	// max is generous headroom over any real row.
	corpusScanInitial = 64 << 10
	corpusScanMax     = 4 << 20
)
