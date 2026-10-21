package schema

import "testing"

func TestParseMetricsTimestampPrecision(t *testing.T) {
	cases := []struct {
		in      string
		want    MetricsTimestampPrecision
		wantErr bool
	}{
		{"", MetricsTimestampNanos, false},
		{"nanos", MetricsTimestampNanos, false},
		{"seconds", MetricsTimestampSeconds, false},
		{"Seconds", MetricsTimestampNanos, true},
		{"millis", MetricsTimestampNanos, true},
	}
	for _, c := range cases {
		got, err := ParseMetricsTimestampPrecision(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("ParseMetricsTimestampPrecision(%q) = (%v, %v), want (%v, error=%t)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

// TestMetricsTimestampPrecisionStringRoundTrips pins that each precision's
// String is the spelling ParseMetricsTimestampPrecision reads back, so the
// configuration default and every log line name a value the knob accepts.
func TestMetricsTimestampPrecisionStringRoundTrips(t *testing.T) {
	for _, p := range []MetricsTimestampPrecision{MetricsTimestampNanos, MetricsTimestampSeconds} {
		got, err := ParseMetricsTimestampPrecision(p.String())
		if err != nil || got != p {
			t.Errorf("ParseMetricsTimestampPrecision(%q) = (%v, %v), want %v", p.String(), got, err, p)
		}
	}
}

// TestMetricsTimestampPrecisionOfType pins the classification of every
// timestamp type a deployed metrics table can carry: DateTime64(9) is
// nanos whatever its timezone, every coarser DateTime64 scale and DateTime
// is seconds whatever its timezone, and anything else is not a timestamp.
func TestMetricsTimestampPrecisionOfType(t *testing.T) {
	cases := []struct {
		typ  string
		want MetricsTimestampPrecision
		ok   bool
	}{
		{"DateTime64(9)", MetricsTimestampNanos, true},
		{"DateTime64(9, 'UTC')", MetricsTimestampNanos, true},
		{"DateTime64(9,'America/Sao_Paulo')", MetricsTimestampNanos, true},
		{" DateTime64(9) ", MetricsTimestampNanos, true},
		{"DateTime64(0)", MetricsTimestampSeconds, true},
		{"DateTime64(3)", MetricsTimestampSeconds, true},
		{"DateTime64(6, 'Europe/Lisbon')", MetricsTimestampSeconds, true},
		{"DateTime", MetricsTimestampSeconds, true},
		{"DateTime('UTC')", MetricsTimestampSeconds, true},
		{"DateTime('America/Sao_Paulo')", MetricsTimestampSeconds, true},
		{"DateTime64(10)", MetricsTimestampNanos, false},
		{"DateTime64(-1)", MetricsTimestampNanos, false},
		{"DateTime64", MetricsTimestampNanos, false},
		{"DateTime64()", MetricsTimestampNanos, false},
		{"DateTime64(9, UTC)", MetricsTimestampNanos, false},
		{"DateTime64(9, 'UTC', 1)", MetricsTimestampNanos, false},
		{"DateTime('')", MetricsTimestampNanos, false},
		{"DateTime(UTC)", MetricsTimestampNanos, false},
		{"DateTime('UTC'", MetricsTimestampNanos, false},
		{"Nullable(DateTime64(9))", MetricsTimestampNanos, false},
		{"Array(DateTime64(9))", MetricsTimestampNanos, false},
		{"UInt64", MetricsTimestampNanos, false},
		{"Date", MetricsTimestampNanos, false},
		{"", MetricsTimestampNanos, false},
	}
	for _, c := range cases {
		got, ok := MetricsTimestampPrecisionOfType(c.typ)
		if got != c.want || ok != c.ok {
			t.Errorf("MetricsTimestampPrecisionOfType(%q) = (%v, %t), want (%v, %t)", c.typ, got, ok, c.want, c.ok)
		}
	}
}

// TestMetricsTimestampPrecisionColumnTypeClassifiesBack pins that the type
// provisioning creates for each precision is the type the read path
// classifies as that precision.
func TestMetricsTimestampPrecisionColumnTypeClassifiesBack(t *testing.T) {
	for _, p := range []MetricsTimestampPrecision{MetricsTimestampNanos, MetricsTimestampSeconds} {
		got, ok := MetricsTimestampPrecisionOfType(p.ColumnType())
		if !ok || got != p {
			t.Errorf("MetricsTimestampPrecisionOfType(%q) = (%v, %t), want %v", p.ColumnType(), got, ok, p)
		}
	}
}
