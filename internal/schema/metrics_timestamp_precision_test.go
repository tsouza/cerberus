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
		{"Seconds", "", true},
		{"millis", "", true},
	}
	for _, c := range cases {
		got, err := ParseMetricsTimestampPrecision(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("ParseMetricsTimestampPrecision(%q) = (%q, %v), want (%q, error=%t)", c.in, got, err, c.want, c.wantErr)
		}
	}
}
