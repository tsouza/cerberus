package lib

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// DecodeArtifact strictly decodes an artifact a `cerberus migrate` command
// emitted into a typed value. Unknown fields are rejected: a scenario asserting
// on a struct that has drifted from the artifact's real shape would otherwise
// read zero values and pass while checking nothing.
func DecodeArtifact[T any](data []byte, into *T) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("migration harness: decode artifact: %w", err)
	}
	return nil
}

// ReadArtifact reads a file a `cerberus migrate --out` run wrote and returns its
// bytes, naming the path when it is unreadable.
func ReadArtifact(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // harness-authored artifact path inside the scenario workspace
	if err != nil {
		return nil, fmt.Errorf("migration harness: read artifact %s: %w", path, err)
	}
	return data, nil
}
