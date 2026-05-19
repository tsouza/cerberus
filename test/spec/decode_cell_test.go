//go:build chdb

package spec

import (
	"reflect"
	"testing"
	"time"
)

func TestDecodeCell_RawStringsPreservesBracePrefixedPayloads(t *testing.T) {
	t.Parallel()

	const jsonObject = `{"foo":"bar"}`
	const jsonArray = `[1,2,3]`

	cases := []struct {
		name     string
		in       any
		raw      bool
		want     any
		wantKind reflect.Kind
	}{
		{"object_decoded", jsonObject, false, map[string]any{"foo": "bar"}, reflect.Map},
		{"object_raw", jsonObject, true, jsonObject, reflect.String},
		{"array_decoded", jsonArray, false, []any{float64(1), float64(2), float64(3)}, reflect.Slice},
		{"array_raw", jsonArray, true, jsonArray, reflect.String},
		{"plain_string_decoded", "hello", false, "hello", reflect.String},
		{"plain_string_raw", "hello", true, "hello", reflect.String},
		{"bytes_object_decoded", []byte(jsonObject), false, map[string]any{"foo": "bar"}, reflect.Map},
		{"bytes_object_raw", []byte(jsonObject), true, jsonObject, reflect.String},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeCell(tc.in, tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("decodeCell(%#v, raw=%v): got %#v (%T), want %#v (%T)",
					tc.in, tc.raw, got, got, tc.want, tc.want)
			}
			if reflect.ValueOf(got).Kind() != tc.wantKind {
				t.Fatalf("decodeCell(%#v, raw=%v): kind %s, want %s",
					tc.in, tc.raw, reflect.ValueOf(got).Kind(), tc.wantKind)
			}
		})
	}
}

func TestDecodeCell_NilTimeUnchangedByRawFlag(t *testing.T) {
	t.Parallel()

	if got := decodeCell(nil, true); got != nil {
		t.Fatalf("nil + raw: got %#v, want nil", got)
	}
	if got := decodeCell(nil, false); got != nil {
		t.Fatalf("nil + decoded: got %#v, want nil", got)
	}

	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	const want = "2026-01-02T03:04:05Z"
	if got := decodeCell(ts, true); got != want {
		t.Fatalf("time + raw: got %#v, want %q", got, want)
	}
	if got := decodeCell(ts, false); got != want {
		t.Fatalf("time + decoded: got %#v, want %q", got, want)
	}
}
