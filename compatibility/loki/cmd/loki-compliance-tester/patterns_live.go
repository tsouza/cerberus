package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	livePatternsSource          = "cerberus/patterns-live"
	livePatternsMetadataVersion = 1
	livePatternsMetadataMaxAge  = 15 * time.Minute
	livePatternsWindowMaxSpan   = 10 * time.Minute
	livePatternsStep            = 10 * time.Second
	livePatternsBodyLimit       = 16 << 20
)

type livePatternsMetadata struct {
	Version        int            `json:"version"`
	Selector       string         `json:"selector"`
	Start          time.Time      `json:"start"`
	End            time.Time      `json:"end"`
	CreatedAt      time.Time      `json:"created_at"`
	EntriesByLevel map[string]int `json:"entries_by_level"`
}

type patternWire struct {
	Level   string            `json:"level"`
	Samples []json.RawMessage `json:"samples"`
}

type patternsWire struct {
	Status string
	Data   []patternWire
}

type patternsObservation struct {
	envelopeErr string
	encodingErr string
	levels      []string
	volume      map[string]int64
}

type livePatternsAxis struct {
	kind        string
	description string
}

func livePatternsAxes() []livePatternsAxis {
	return []livePatternsAxis{
		{kind: "patterns_envelope", description: "live /patterns success envelope and non-empty data"},
		{kind: "patterns_levels", description: "live /patterns level vocabulary and coverage"},
		{kind: "patterns_samples", description: "live /patterns sample tuple encoding"},
		{kind: "patterns_volume", description: "live /patterns per-level volume bound"},
	}
}

func readLivePatternsMetadata(path string, now time.Time) (livePatternsMetadata, error) {
	payload, err := os.ReadFile(path) //nolint:gosec // operator-supplied handshake path; offline harness input
	if err != nil {
		return livePatternsMetadata{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var metadata livePatternsMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return livePatternsMetadata{}, fmt.Errorf("decode: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return livePatternsMetadata{}, errors.New("metadata contains trailing JSON")
	}
	if err := validateLivePatternsMetadata(metadata, now.UTC()); err != nil {
		return livePatternsMetadata{}, err
	}
	return metadata, nil
}

func validateLivePatternsMetadata(metadata livePatternsMetadata, now time.Time) error {
	if metadata.Version != livePatternsMetadataVersion {
		return fmt.Errorf("version=%d, want %d", metadata.Version, livePatternsMetadataVersion)
	}
	if metadata.Selector == "" {
		return errors.New("selector is empty")
	}
	if !metadata.Start.Before(metadata.End) {
		return fmt.Errorf("invalid window: start=%s end=%s", metadata.Start, metadata.End)
	}
	if span := metadata.End.Sub(metadata.Start); span > livePatternsWindowMaxSpan {
		return fmt.Errorf("window span=%s exceeds %s", span, livePatternsWindowMaxSpan)
	}
	if metadata.CreatedAt.Before(metadata.End) {
		return fmt.Errorf("created_at=%s precedes window end=%s", metadata.CreatedAt, metadata.End)
	}
	if metadata.CreatedAt.After(now) {
		return fmt.Errorf("created_at=%s is in the future relative to %s", metadata.CreatedAt, now)
	}
	if age := now.Sub(metadata.CreatedAt); age > livePatternsMetadataMaxAge {
		return fmt.Errorf("metadata age=%s exceeds %s", age, livePatternsMetadataMaxAge)
	}
	if len(metadata.EntriesByLevel) == 0 {
		return errors.New("entries_by_level is empty")
	}
	for level, count := range metadata.EntriesByLevel {
		if level == "" || level != strings.ToLower(level) || count <= 0 {
			return fmt.Errorf("invalid level volume %q=%d", level, count)
		}
	}
	return nil
}

func compareLivePatterns(c *http.Client, f flags, metadata livePatternsMetadata) []Result {
	type fetched struct {
		observation patternsObservation
		err         error
	}
	out := make([]fetched, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for idx, addr := range []string{f.addr1, f.addr2} {
		idx, addr := idx, addr
		go func() {
			defer wg.Done()
			wire, err := fetchLivePatterns(c, addr, metadata)
			if err != nil {
				out[idx].err = err
				return
			}
			out[idx].observation = observeLivePatterns(wire, metadata)
		}()
	}
	wg.Wait()

	results := make([]Result, 0, len(livePatternsAxes()))
	for _, axis := range livePatternsAxes() {
		result := Result{TestCase: TestCase{
			Query:       metadata.Selector,
			Source:      livePatternsSource,
			Description: axis.description,
			Kind:        axis.kind,
			Direction:   "n/a",
			Start:       metadata.Start.Format(time.RFC3339Nano),
			End:         metadata.End.Format(time.RFC3339Nano),
			Step:        livePatternsStep.String(),
		}}
		switch {
		case out[0].err != nil:
			result.UnexpectedFailure = fmt.Sprintf("reference (-addr-1) failed: %v", out[0].err)
		case out[1].err != nil:
			result.UnexpectedFailure = fmt.Sprintf("test endpoint (-addr-2) failed: %v", out[1].err)
		default:
			result.Diff = compareLivePatternsAxis(axis.kind, out[0].observation, out[1].observation, metadata)
		}
		results = append(results, result)
	}
	return results
}

func fetchLivePatterns(c *http.Client, baseURL string, metadata livePatternsMetadata) (patternsWire, error) {
	params := url.Values{}
	params.Set("query", metadata.Selector)
	params.Set("start", strconv.FormatInt(metadata.Start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(metadata.End.UnixNano(), 10))
	params.Set("step", livePatternsStep.String())
	endpoint := strings.TrimRight(baseURL, "/") + "/loki/api/v1/patterns?" + params.Encode()
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return patternsWire{}, fmt.Errorf("new request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return patternsWire{}, fmt.Errorf("http call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, livePatternsBodyLimit))
	if err != nil {
		return patternsWire{}, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return patternsWire{}, fmt.Errorf("status=%d body=%s", resp.StatusCode, errorBodySnippet(string(body)))
	}
	return decodePatternsWire(body), nil
}

func decodePatternsWire(body []byte) patternsWire {
	var raw struct {
		Status json.RawMessage `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return patternsWire{Status: "decode error: " + err.Error()}
	}
	var status string
	if len(raw.Status) == 0 {
		status = "missing"
	} else if err := json.Unmarshal(raw.Status, &status); err != nil {
		status = "invalid: " + err.Error()
	}
	if len(raw.Data) == 0 || bytes.Equal(raw.Data, []byte("null")) {
		return patternsWire{Status: status}
	}
	var data []patternWire
	if err := json.Unmarshal(raw.Data, &data); err != nil {
		return patternsWire{Status: status, Data: []patternWire{{Level: "__data_decode_error__" + err.Error()}}}
	}
	return patternsWire{Status: status, Data: data}
}

func observeLivePatterns(wire patternsWire, metadata livePatternsMetadata) patternsObservation {
	observation := patternsObservation{volume: make(map[string]int64)}
	if wire.Status != "success" {
		observation.envelopeErr = fmt.Sprintf("status field=%q", wire.Status)
	} else if len(wire.Data) == 0 {
		observation.envelopeErr = "data is missing, null, or empty"
	}
	levelSet := make(map[string]struct{})
	for patternIndex, pattern := range wire.Data {
		levelSet[pattern.Level] = struct{}{}
		for sampleIndex, raw := range pattern.Samples {
			var tuple []int64
			if err := json.Unmarshal(raw, &tuple); err != nil {
				observation.encodingErr = fmt.Sprintf("pattern[%d].samples[%d] is not an integer tuple: %v", patternIndex, sampleIndex, err)
				continue
			}
			if len(tuple) != 2 {
				observation.encodingErr = fmt.Sprintf("pattern[%d].samples[%d] has %d elements, want 2", patternIndex, sampleIndex, len(tuple))
				continue
			}
			ts, count := tuple[0], tuple[1]
			if ts < metadata.Start.Unix() || ts > metadata.End.Unix() {
				observation.encodingErr = fmt.Sprintf("pattern[%d].samples[%d] timestamp=%d outside [%d,%d]", patternIndex, sampleIndex, ts, metadata.Start.Unix(), metadata.End.Unix())
			}
			if count <= 0 {
				observation.encodingErr = fmt.Sprintf("pattern[%d].samples[%d] count=%d, want positive", patternIndex, sampleIndex, count)
				continue
			}
			observation.volume[pattern.Level] += count
		}
	}
	observation.levels = make([]string, 0, len(levelSet))
	for level := range levelSet {
		observation.levels = append(observation.levels, level)
	}
	sort.Strings(observation.levels)
	return observation
}

func compareLivePatternsAxis(kind string, reference, test patternsObservation, metadata livePatternsMetadata) string {
	switch kind {
	case "patterns_envelope":
		return sideErrors("envelope", reference.envelopeErr, test.envelopeErr)
	case "patterns_levels":
		expected := make([]string, 0, len(metadata.EntriesByLevel))
		for level := range metadata.EntriesByLevel {
			expected = append(expected, level)
		}
		sort.Strings(expected)
		return sideSliceDiff("levels", expected, reference.levels, test.levels)
	case "patterns_samples":
		return sideErrors("sample encoding", reference.encodingErr, test.encodingErr)
	case "patterns_volume":
		for _, side := range []struct {
			name   string
			volume map[string]int64
		}{
			{name: "reference", volume: reference.volume},
			{name: "test endpoint", volume: test.volume},
		} {
			for level, seeded := range metadata.EntriesByLevel {
				got := side.volume[level]
				if got <= 0 || got > int64(seeded) {
					return fmt.Sprintf("%s level=%q volume=%d outside (0,%d]", side.name, level, got, seeded)
				}
			}
		}
		return ""
	default:
		return "unknown live patterns axis " + kind
	}
}

func sideErrors(axis, reference, test string) string {
	switch {
	case reference != "" && test != "":
		return fmt.Sprintf("%s: reference=%s; test endpoint=%s", axis, reference, test)
	case reference != "":
		return fmt.Sprintf("%s: reference=%s", axis, reference)
	case test != "":
		return fmt.Sprintf("%s: test endpoint=%s", axis, test)
	default:
		return ""
	}
}

func sideSliceDiff(axis string, expected, reference, test []string) string {
	if slices.Equal(reference, expected) && slices.Equal(test, expected) {
		return ""
	}
	return fmt.Sprintf("%s: expected=%v reference=%v test endpoint=%v", axis, expected, reference, test)
}
