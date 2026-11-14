package tempopb

import (
	"bytes"
	"testing"
)

func TestPreallocBytesRoundTripsThroughPushBytesRequest(t *testing.T) {
	t.Parallel()

	in := PushBytesRequest{
		Traces: []PreallocBytes{{Slice: []byte("trace-one")}, {Slice: []byte{}}, {Slice: []byte{0, 1, 2}}},
		Ids:    [][]byte{[]byte("id-1"), []byte("id-2"), []byte("id-3")},
	}
	wire, err := in.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out PushBytesRequest
	if err := out.Unmarshal(wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Traces) != len(in.Traces) {
		t.Fatalf("decoded %d traces, want %d", len(out.Traces), len(in.Traces))
	}
	for i := range in.Traces {
		if !bytes.Equal(out.Traces[i].Slice, in.Traces[i].Slice) {
			t.Errorf("trace %d decoded as %q, want %q", i, out.Traces[i].Slice, in.Traces[i].Slice)
		}
	}

	// The decoded slice must not alias the wire buffer the caller may reuse.
	for i := range wire {
		wire[i] = 0xff
	}
	if !bytes.Equal(out.Traces[0].Slice, in.Traces[0].Slice) {
		t.Errorf("decoded trace changed with the wire buffer: %q", out.Traces[0].Slice)
	}

	var nilBytes *PreallocBytes
	if got := nilBytes.Size(); got != 0 {
		t.Errorf("nil Size() = %d, want 0", got)
	}
}
