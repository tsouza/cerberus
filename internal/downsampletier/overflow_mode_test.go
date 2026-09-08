package downsampletier

import "testing"

// TestCapSettingsThrowOnOverflow pins that BOTH overflow modes fail the
// statement rather than truncating it.
//
// This matters most in this package. The backfill runs `INSERT … SELECT`
// building a timeSeriesLastTwoSamplesState per bucket, so a "break" on
// max_rows_to_read / max_bytes_to_read persists a PARTIAL aggregate state and
// reports success. Verify only counts buckets, so a wrong-but-present bucket is
// invisible to it — the third state ("present but wrong") this package's
// "absent, never wrong" safety argument says cannot exist. "throw" is what
// makes that argument true.
func TestCapSettingsThrowOnOverflow(t *testing.T) {
	settings := capSettings()
	for _, mode := range []string{"timeout_overflow_mode", "read_overflow_mode"} {
		got, ok := settings[mode]
		if !ok {
			t.Fatalf("%s is not stamped at all; every cap this package sets needs an explicit overflow mode", mode)
		}
		if got != "throw" {
			t.Errorf("%s = %v; want \"throw\" — \"break\" persists a partial aggregate state as if it were complete", mode, got)
		}
	}
}
