package seed

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// Sample is one metric sample at an exact instant. The migration comparator
// keys samples by exact timestamp, so the value carried here is the same
// time.Time the ClickHouse side is written with — never a re-derived one.
type Sample struct {
	Time  time.Time
	Value float64
}

// Series is one labelled metric series. Labels carry `__name__` like any other
// label; WriteSeries sorts them into the wire order Prometheus requires.
type Series struct {
	Labels  map[string]string
	Samples []Sample
}

// promWriteTimeout bounds one remote-write POST. The fixture is written in a
// single batch, so this covers the whole reference-side metric write.
const promWriteTimeout = 30 * time.Second

// WriteSeries POSTs series to Prometheus's remote-write receiver. Per the
// remote-write spec the body is a snappy-encoded protobuf and the
// Content-Encoding / Content-Type / version headers are required.
//
// A non-2xx response is an error with the reference's own body attached: a
// half-written reference side would silently become a divergence in every
// later diff, so this fails loudly at the write.
func WriteSeries(ctx context.Context, baseURL string, series []Series) error {
	wire := make([]prompb.TimeSeries, 0, len(series))
	for _, s := range series {
		wire = append(wire, prompb.TimeSeries{
			Labels:  promLabels(s.Labels),
			Samples: promSamples(s.Samples),
		})
	}

	raw, err := proto.Marshal(&prompb.WriteRequest{Timeseries: wire})
	if err != nil {
		return fmt.Errorf("marshal write request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, promWriteTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/api/v1/write",
		bytes.NewReader(snappy.Encode(nil, raw)))
	if err != nil {
		return fmt.Errorf("new remote-write request: %w", err)
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("remote-write to %s: %w", baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
		if readErr != nil {
			return fmt.Errorf("remote-write returned %d (body unreadable: %w)", resp.StatusCode, readErr)
		}
		return fmt.Errorf("remote-write returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// promLabels converts a label map into the sorted repeated-pair shape the
// remote-write wire format requires.
func promLabels(m map[string]string) []prompb.Label {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]prompb.Label, 0, len(m))
	for _, k := range keys {
		out = append(out, prompb.Label{Name: k, Value: m[k]})
	}
	return out
}

// promSamples converts fixture samples into wire samples, keeping the exact
// millisecond timestamp the fixture declared.
func promSamples(in []Sample) []prompb.Sample {
	out := make([]prompb.Sample, 0, len(in))
	for _, s := range in {
		out = append(out, prompb.Sample{Value: s.Value, Timestamp: s.Time.UnixMilli()})
	}
	return out
}
