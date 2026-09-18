//go:build chdb

// /index/stats counts the streams it SERVES, not the label sets the store
// holds — the same identity /index/volume (issue #3246) and /series group
// and dedupe on.
//
// `streams` is a scalar produced by uniqExact inside ClickHouse, so the
// served identity has to be established in the SQL; no Go-side pass can
// recover a count that was doubled there. Only an engine can show any of
// this: a stub querier hands back a scalar the SQL never computed.

package loki_test

import (
	"fmt"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
)

// TestIndexStats_ChDB_TwoStoredSpellingsCountOneStream: two streams under
// one selector, one carrying the attribute key `a.b` and the other `a_b`,
// both serving under `{a_b="1", job="api"}`. Upstream normalises at ingest,
// so they are ONE stream by the time anything counts them; cerberus
// normalises on read and must count them as one too. Entries and Bytes are
// row-level aggregates and stay at two rows' worth, which pins that the
// served-identity wrap changed only the stream key.
func TestIndexStats_ChDB_TwoStoredSpellingsCountOneStream(t *testing.T) {
	srvURL := newVolumeServer(t, normGroupingSeed(
		normGroupingRow{attrs: "'a.b','1'", bytes: 10},
		normGroupingRow{attrs: "'a_b','1'", bytes: 5},
	))

	var stats loki.IndexStats
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/stats?query=%%7Bjob%%3D%%22api%%22%%7D&start=1778760000&end=1778760600`,
		srvURL,
	), &stats)

	if stats.Streams != 1 {
		t.Errorf("`a.b` and `a_b` serve under one label set and are one stream; got streams=%d", stats.Streams)
	}
	if stats.Entries != 2 {
		t.Errorf("entries must count both rows; got %d", stats.Entries)
	}
	if stats.Bytes != 15 {
		t.Errorf("bytes must sum both rows; got %d", stats.Bytes)
	}
}

// TestIndexStats_ChDB_DistinctServedSetsStayDistinct is the control: two
// streams whose served label sets genuinely differ count as two, so the
// served-identity wrap collapses only what the rewrite makes identical.
func TestIndexStats_ChDB_DistinctServedSetsStayDistinct(t *testing.T) {
	srvURL := newVolumeServer(t, normGroupingSeed(
		normGroupingRow{attrs: "'a.b','1'", bytes: 10},
		normGroupingRow{attrs: "'a_b','2'", bytes: 5},
	))

	var stats loki.IndexStats
	getJSON(t, fmt.Sprintf(
		`%s/loki/api/v1/index/stats?query=%%7Bjob%%3D%%22api%%22%%7D&start=1778760000&end=1778760600`,
		srvURL,
	), &stats)

	if stats.Streams != 2 {
		t.Errorf("`{a_b=\"1\"}` and `{a_b=\"2\"}` are two served streams; got streams=%d", stats.Streams)
	}
}
