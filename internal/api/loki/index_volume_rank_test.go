package loki

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestRankIndexVolumeRows_LabelsModeTieBreaksOnTheBareName pins the
// labels-mode comparator: upstream accumulates `labelVolumes[l.Name]` and
// MapToVolumeResponse sorts equal volumes by that bare NAME, so `a` ranks
// before `a0`. Ranking on the `{a0=""}` framing the wire row carries
// inverts it — `"{a0=\"\"}" < "{a=\"\"}"` because '0' (0x30) sorts before
// '=' (0x3D) — which is exactly the framing artefact that makes a
// String()-based comparison wrong for this mode. Series mode keeps the
// rendered-labels comparator upstream uses there.
func TestRankIndexVolumeRows_LabelsModeTieBreaksOnTheBareName(t *testing.T) {
	t.Parallel()

	rows := []chclient.IndexVolumeRow{
		{Labels: map[string]string{"a0": ""}, Bytes: 10},
		{Labels: map[string]string{"a": ""}, Bytes: 10},
	}
	ranked := rankIndexVolumeRows(rows, 1, aggregateByLabels)
	if len(ranked) != 1 {
		t.Fatalf("limit 1 kept %d rows", len(ranked))
	}
	if _, ok := ranked[0].metric["a"]; !ok {
		t.Errorf("labels-mode tie at 10 bytes kept %v; upstream keeps the bare name that sorts first, `a`", ranked[0].metric)
	}

	// The same two rows in series mode rank on the rendered label set, so
	// the two modes must disagree on this pair — the assertion that keeps
	// this test from passing under a comparator that ignores the mode.
	series := rankIndexVolumeRows(rows, 1, aggregateBySeries)
	if _, ok := series[0].metric["a0"]; !ok {
		t.Errorf("series-mode tie kept %v; the rendered `{a0=\"\"}` sorts before `{a=\"\"}` under upstream's Labels.String() comparator", series[0].metric)
	}
}

// TestRankIndexVolumeRows_VolumeOutranksName pins the primary key in both
// modes: a larger volume wins regardless of how the names collate.
func TestRankIndexVolumeRows_VolumeOutranksName(t *testing.T) {
	t.Parallel()

	rows := []chclient.IndexVolumeRow{
		{Labels: map[string]string{"a": ""}, Bytes: 5},
		{Labels: map[string]string{"z": ""}, Bytes: 50},
	}
	for _, mode := range []string{aggregateByLabels, aggregateBySeries} {
		ranked := rankIndexVolumeRows(rows, 1, mode)
		if _, ok := ranked[0].metric["z"]; !ok {
			t.Errorf("%s mode: the 50-byte row lost to the 5-byte one: %v", mode, ranked[0].metric)
		}
	}
}
