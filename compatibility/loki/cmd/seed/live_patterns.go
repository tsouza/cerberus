package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	livePatternsService         = "cerberus-patterns-live"
	livePatternsEntriesPerLevel = 40
	livePatternsEntryInterval   = time.Second
	livePatternsFixtureAge      = 2 * time.Minute
	livePatternsPollInterval    = 250 * time.Millisecond
	livePatternsProbeTimeout    = 30 * time.Second
	livePatternsMetadataVersion = 1
)

var livePatternsLevels = []string{"ERROR", "INFO", "WARN"}

type livePatternsMetadata struct {
	Version        int            `json:"version"`
	Selector       string         `json:"selector"`
	Start          time.Time      `json:"start"`
	End            time.Time      `json:"end"`
	CreatedAt      time.Time      `json:"created_at"`
	EntriesByLevel map[string]int `json:"entries_by_level"`
}

type livePatternsFixture struct {
	stream   stream
	metadata livePatternsMetadata
}

func buildLivePatternsFixture(now time.Time) livePatternsFixture {
	now = now.UTC().Truncate(time.Second)
	start := now.Add(-livePatternsFixtureAge)
	entryCount := len(livePatternsLevels) * livePatternsEntriesPerLevel
	entries := make([]entry, 0, entryCount)
	counts := make(map[string]int, len(livePatternsLevels))
	for i := 0; i < entryCount; i++ {
		level := livePatternsLevels[i%len(livePatternsLevels)]
		entries = append(entries, entry{
			ts:    start.Add(time.Duration(i) * livePatternsEntryInterval),
			level: level,
			line:  "live patterns fixture processed request",
		})
		counts[strings.ToLower(level)]++
	}

	config := serviceConfig{
		Name:        livePatternsService,
		ServiceName: livePatternsService,
		Format:      "unstructured",
		Cluster:     "live-patterns",
		Namespace:   "compatibility",
		Pod:         "live-patterns-0",
		Container:   "seeder",
	}
	return livePatternsFixture{
		stream: stream{
			config: config,
			labels: map[string]string{
				"cluster":      config.Cluster,
				"namespace":    config.Namespace,
				"service":      config.Name,
				"service_name": config.ServiceName,
				"pod":          config.Pod,
				"container":    config.Container,
				"env":          "production",
				"region":       "us-east-1",
				"datacenter":   "dc1",
			},
			entries: entries,
		},
		metadata: livePatternsMetadata{
			Version:        livePatternsMetadataVersion,
			Selector:       `{service_name="` + livePatternsService + `"}`,
			Start:          start,
			End:            now,
			CreatedAt:      now,
			EntriesByLevel: counts,
		},
	}
}

func writeLivePatternsMetadata(path string, metadata livePatternsMetadata) error {
	if err := validateLivePatternsMetadata(metadata); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".live-patterns-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename metadata into place: %w", err)
	}
	return nil
}

func validateLivePatternsMetadata(metadata livePatternsMetadata) error {
	if metadata.Version != livePatternsMetadataVersion {
		return fmt.Errorf("version=%d, want %d", metadata.Version, livePatternsMetadataVersion)
	}
	if metadata.Selector == "" {
		return errors.New("selector is empty")
	}
	if !metadata.Start.Before(metadata.End) {
		return fmt.Errorf("invalid window: start=%s end=%s", metadata.Start, metadata.End)
	}
	if metadata.CreatedAt.IsZero() {
		return errors.New("created_at is zero")
	}
	if len(metadata.EntriesByLevel) == 0 {
		return errors.New("entries_by_level is empty")
	}
	for level, count := range metadata.EntriesByLevel {
		if level == "" || count <= 0 {
			return fmt.Errorf("invalid level volume %q=%d", level, count)
		}
	}
	return nil
}

func waitLivePatternsBoth(ctx context.Context, lokiURL, cerberusURL string, metadata livePatternsMetadata, logger *slog.Logger) error {
	for _, target := range []struct {
		name string
		url  string
	}{
		{name: "loki", url: lokiURL},
		{name: "cerberus", url: cerberusURL},
	} {
		if err := waitLivePatternsNonEmpty(ctx, target.url, metadata); err != nil {
			return fmt.Errorf("%s: %w", target.name, err)
		}
		logger.Info("live /patterns fixture visible", "target", target.name)
	}
	return nil
}

func waitLivePatternsNonEmpty(ctx context.Context, baseURL string, metadata livePatternsMetadata) error {
	deadline := time.Now().Add(livePatternsProbeTimeout)
	var lastErr error
	for {
		lastErr = probeLivePatterns(ctx, baseURL, metadata)
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for non-empty /patterns: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(livePatternsPollInterval):
		}
	}
}

func probeLivePatterns(ctx context.Context, baseURL string, metadata livePatternsMetadata) error {
	params := url.Values{}
	params.Set("query", metadata.Selector)
	params.Set("start", strconv.FormatInt(metadata.Start.UnixNano(), 10))
	params.Set("end", strconv.FormatInt(metadata.End.UnixNano(), 10))
	params.Set("step", "10s")
	endpoint := strings.TrimRight(baseURL, "/") + "/loki/api/v1/patterns?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Status string            `json:"status"`
		Data   []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if envelope.Status != "success" {
		return fmt.Errorf("status field=%q", envelope.Status)
	}
	if len(envelope.Data) == 0 {
		return errors.New("data is empty")
	}
	return nil
}
