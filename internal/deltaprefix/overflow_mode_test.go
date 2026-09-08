package deltaprefix

import "testing"

// TestCapSettingsThrowOnOverflow pins that BOTH overflow modes fail the
// statement rather than truncating it.
//
// ClickHouse's "break" mode stops reading the instant a cap is hit and returns
// the PARTIAL result as if it were complete. This package's statements include
// the `INSERT … SELECT` backfill, so a "break" on max_rows_to_read /
// max_bytes_to_read would silently PERSIST a partial prefix of the source
// history and report success — wrong data written, with nothing to tell the
// operator the cap was ever reached. withCaps' own doc says these caps exist so
// a runaway backfill "fails loudly under ClickHouse's own enforcement"; only
// "throw" actually delivers that.
func TestCapSettingsThrowOnOverflow(t *testing.T) {
	settings := capSettings()
	for _, mode := range []string{"timeout_overflow_mode", "read_overflow_mode"} {
		got, ok := settings[mode]
		if !ok {
			t.Fatalf("%s is not stamped at all; every cap this package sets needs an explicit overflow mode", mode)
		}
		if got != "throw" {
			t.Errorf("%s = %v; want \"throw\" — \"break\" returns a silently truncated read as if it were complete", mode, got)
		}
	}
}
