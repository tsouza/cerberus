package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Entry is one log line at an exact instant, plus the per-record structured
// metadata the reference and the ClickHouse side must both carry.
type Entry struct {
	Time               time.Time
	Line               string
	StructuredMetadata map[string]string
}

// Stream is one labelled log stream. Labels are the stream identity — the same
// keys the ClickHouse side writes into ResourceAttributes, so a LogQL matcher
// selects the same records on both sides without a naming bridge.
type Stream struct {
	Labels  map[string]string
	Entries []Entry
}

// lokiPushTimeout bounds one /loki/api/v1/push POST.
const lokiPushTimeout = 30 * time.Second

// errBodyLimit caps how much of a failing backend's response body is read back
// into an error message.
const errBodyLimit = 4096

// PushStreams POSTs streams to Loki's push API in the documented JSON shape:
// each value is [<unix nanos as string>, <line>, <structured metadata object>].
// A non-2xx response is an error carrying Loki's own body.
func PushStreams(ctx context.Context, baseURL string, streams []Stream) error {
	body, err := json.Marshal(lokiPushBody(streams))
	if err != nil {
		return fmt.Errorf("marshal loki push: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, lokiPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/loki/api/v1/push", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new loki push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("loki push to %s: %w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		if readErr != nil {
			return fmt.Errorf("loki push returned %d (body unreadable: %w)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("loki push returned %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// lokiPushValue is one [ts, line, metadata] triple. It marshals as a
// heterogeneous JSON array, which is the shape Loki's push API defines.
type lokiPushValue []any

type lokiPushStream struct {
	Stream map[string]string `json:"stream"`
	Values []lokiPushValue   `json:"values"`
}

type lokiPushRequest struct {
	Streams []lokiPushStream `json:"streams"`
}

func lokiPushBody(streams []Stream) lokiPushRequest {
	out := lokiPushRequest{Streams: make([]lokiPushStream, 0, len(streams))}
	for _, s := range streams {
		ls := lokiPushStream{Stream: s.Labels, Values: make([]lokiPushValue, 0, len(s.Entries))}
		for _, e := range s.Entries {
			v := lokiPushValue{strconv.FormatInt(e.Time.UnixNano(), 10), e.Line}
			if len(e.StructuredMetadata) > 0 {
				v = append(v, e.StructuredMetadata)
			}
			ls.Values = append(ls.Values, v)
		}
		out.Streams = append(out.Streams, ls)
	}
	return out
}
