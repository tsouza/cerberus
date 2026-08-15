package regression

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// TestParityVocabulariesAreClosed asserts the accepted values are a closed
// set with no empty members — the property LoadParity's rejection path
// depends on.
func TestParityVocabulariesAreClosed(t *testing.T) {
	t.Parallel()

	keys := spec.ParityKeys()
	if len(keys) == 0 {
		t.Fatal("parity vocabulary is empty; LoadParity would accept any section body")
	}
	for _, key := range keys {
		values := spec.ParityValues(key)
		if len(values) == 0 {
			t.Errorf("parity key %q has no accepted values, so no fixture can ever satisfy it", key)
		}
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				t.Errorf("parity key %q accepts an empty value", key)
			}
		}
	}
}
