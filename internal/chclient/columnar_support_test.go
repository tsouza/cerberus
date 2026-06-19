package chclient

import (
	"context"
	"sort"
	"testing"

	chproto "github.com/ClickHouse/ch-go/proto"
)

// TestProfileEventAccumulator pins the ProfileEvents bridge: ch-go streams
// events in batches, so observe must SUM by name across batches, and attrs
// must render only non-zero totals as `ch.profile_event.<Name>` integers.
func TestProfileEventAccumulator(t *testing.T) {
	t.Parallel()

	var a profileEventAccumulator
	// Two batches, same names accumulate; a zero-valued event is dropped.
	_ = a.observe(context.Background(), []chproto.ProfileEvent{
		{Name: "SelectedRows", Value: 100},
		{Name: "RowsReadByPrewhereReaders", Value: 40},
		{Name: "QueryConditionCacheHits", Value: 0},
	})
	_ = a.observe(context.Background(), []chproto.ProfileEvent{
		{Name: "SelectedRows", Value: 50}, // accumulates to 150
	})

	got := map[string]int64{}
	for _, kv := range a.attrs() {
		got[string(kv.Key)] = kv.Value.AsInt64()
	}

	want := map[string]int64{
		"ch.profile_event.SelectedRows":              150,
		"ch.profile_event.RowsReadByPrewhereReaders": 40,
	}
	if len(got) != len(want) {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Fatalf("attr count = %d (%v), want %d", len(got), keys, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	if _, ok := got["ch.profile_event.QueryConditionCacheHits"]; ok {
		t.Error("zero-valued ProfileEvent should be dropped, not stamped")
	}
}

// TestProfileEventAccumulator_Empty: no events observed -> no attrs, and stamp
// on a nil span is a no-op (does not panic).
func TestProfileEventAccumulator_Empty(t *testing.T) {
	t.Parallel()

	var a profileEventAccumulator
	if got := a.attrs(); len(got) != 0 {
		t.Fatalf("attrs() on empty accumulator = %v, want none", got)
	}
	a.stamp(nil) // must not panic
}
