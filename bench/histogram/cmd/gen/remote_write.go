package main

// Minimal Prometheus remote-write v1 client.
//
// The wire format is a snappy-compressed `prometheus.WriteRequest` protobuf.
// Rather than pull the whole `github.com/prometheus/prometheus` module (and its
// large transitive graph) into this benchmark tool, we hand-encode the three
// tiny messages remote-write needs. The schema (stable since remote-write 1.0):
//
//	message Sample      { double value = 1; int64 timestamp = 2; }
//	message Label       { string name  = 1; string value = 2; }
//	message TimeSeries  { repeated Label labels = 1; repeated Sample samples = 2; }
//	message WriteRequest{ repeated TimeSeries timeseries = 1; }
//
// Both Prometheus (`/api/v1/write`) and Grafana Mimir (`/api/v1/push`) accept
// this exact body.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/golang/snappy"
)

// promSample is one (timestamp, value) point. TimestampMs is Unix milliseconds.
type promSample struct {
	valueField     float64
	timestampMsFld int64
}

// promSeries is a labelled series with its samples. Labels must include
// `__name__` and be sorted by name before encoding (Prometheus requires it).
type promSeries struct {
	labels  [][2]string
	samples []promSample
}

// --- protobuf wire helpers -------------------------------------------------

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// appendTag writes a protobuf field tag (field number + wire type).
func appendTag(b []byte, field int, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

// appendLenDelim writes a length-delimited (wire type 2) field.
func appendLenDelim(b []byte, field int, payload []byte) []byte {
	b = appendTag(b, field, 2)
	b = appendVarint(b, uint64(len(payload)))
	return append(b, payload...)
}

// appendString writes a string field (wire type 2).
func appendString(b []byte, field int, s string) []byte {
	b = appendTag(b, field, 2)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

// appendFixed64 writes a 64-bit fixed field (wire type 1), used for doubles.
func appendFixed64(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, 1)
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(b, tmp[:]...)
}

// appendVarintField writes a varint field (wire type 0), used for int64.
func appendVarintField(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, 0)
	return appendVarint(b, v)
}

func encodeLabel(name, value string) []byte {
	var b []byte
	b = appendString(b, 1, name)
	b = appendString(b, 2, value)
	return b
}

func encodeSample(s promSample) []byte {
	var b []byte
	b = appendFixed64(b, 1, math.Float64bits(s.valueField))
	// timestamp is a signed int64; two's-complement bits fit the varint.
	b = appendVarintField(b, 2, uint64(s.timestampMsFld))
	return b
}

func encodeTimeSeries(ts promSeries) []byte {
	var b []byte
	for _, l := range ts.labels {
		b = appendLenDelim(b, 1, encodeLabel(l[0], l[1]))
	}
	for _, s := range ts.samples {
		b = appendLenDelim(b, 2, encodeSample(s))
	}
	return b
}

// encodeWriteRequest builds the full WriteRequest and snappy-compresses it,
// returning the ready-to-POST body.
func encodeWriteRequest(series []promSeries) []byte {
	var raw []byte
	for _, ts := range series {
		raw = appendLenDelim(raw, 1, encodeTimeSeries(ts))
	}
	return snappy.Encode(nil, raw)
}

// postRemoteWrite POSTs a snappy-encoded WriteRequest to a remote-write
// endpoint. Extra headers (e.g. Mimir's X-Scope-OrgID) are applied verbatim.
func postRemoteWrite(ctx context.Context, url string, body []byte, extraHeaders map[string]string) error {
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodPost,
		strings.TrimRight(url, "/"), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	req.Header.Set("User-Agent", "cerberus-hist-bench-gen/1")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("remote_write %s returned %d: %s", url, resp.StatusCode, string(snippet))
	}
	return nil
}
