package optcorpus

import "testing"

// TestCorpusSettingsThrowOnOverflow pins that BOTH overflow modes fail the
// statement rather than truncating it.
//
// The corpus is the population every calibration watermark is learnt from.
// Under ClickHouse's "break" mode a read that trips max_rows_to_read /
// max_bytes_to_read returns a partial scan as if it were the whole corpus, so
// the learnt watermark is fitted on a truncated sample with no error anywhere.
// "throw" makes the tripped cap visible instead.
func TestCorpusSettingsThrowOnOverflow(t *testing.T) {
	settings := corpusSettings()
	for _, mode := range []string{"timeout_overflow_mode", "read_overflow_mode"} {
		got, ok := settings[mode]
		if !ok {
			t.Fatalf("%s is not stamped at all; every cap the corpus source sets needs an explicit overflow mode", mode)
		}
		if got != "throw" {
			t.Errorf("%s = %v; want \"throw\" — \"break\" learns a watermark from a silently truncated corpus", mode, got)
		}
	}
}
