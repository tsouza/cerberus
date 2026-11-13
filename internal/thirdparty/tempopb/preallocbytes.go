package tempopb

// PreallocBytes is the custom type tempo.pb.go generates for the repeated
// bytes field of PushBytesRequest. Tempo's own implementation backs it with
// its ingester's byte-slice pool; cerberus never ingests, so this one copies
// into a fresh slice. The wire encoding is the same raw bytes either way.
type PreallocBytes struct {
	Slice []byte
}

// Unmarshal copies dAtA, which the caller may reuse.
func (r *PreallocBytes) Unmarshal(dAtA []byte) error {
	r.Slice = append([]byte(nil), dAtA...)
	return nil
}

// MarshalTo writes the bytes into dAtA and returns their length.
func (r *PreallocBytes) MarshalTo(dAtA []byte) (int, error) {
	return copy(dAtA, r.Slice), nil
}

// Size is the encoded length.
func (r *PreallocBytes) Size() int {
	if r == nil {
		return 0
	}
	return len(r.Slice)
}
