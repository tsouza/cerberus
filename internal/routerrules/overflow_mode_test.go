package routerrules

import "testing"

// TestCHCorpusSettingsThrowOnOverflow pins that BOTH overflow modes fail the
// statement rather than truncating it.
//
// This source feeds watermark fitting. Under ClickHouse's "break" mode a read
// that trips max_rows_to_read / max_bytes_to_read returns the rows it managed
// to read as if that were the whole population, so a watermark would be fitted
// on a silently truncated sample and look perfectly healthy. "throw" turns that
// into a visible failure.
func TestCHCorpusSettingsThrowOnOverflow(t *testing.T) {
	settings := chCorpusSettings()
	for _, mode := range []string{"timeout_overflow_mode", "read_overflow_mode"} {
		got, ok := settings[mode]
		if !ok {
			t.Fatalf("%s is not stamped at all; every cap this source sets needs an explicit overflow mode", mode)
		}
		if got != "throw" {
			t.Errorf("%s = %v; want \"throw\" — \"break\" fits a watermark on a silently truncated population", mode, got)
		}
	}
}
