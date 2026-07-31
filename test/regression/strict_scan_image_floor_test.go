package regression

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// strictScanSourcePath is the strict-scan lane's test source, which pins the
// image the testcontainer actually starts.
const strictScanSourcePath = "../spec/strictscan_integration_test.go"

// strictScanImageVar is the Justfile variable the strict-scan recipe pre-pulls.
const strictScanImageVar = "CH_STRICT_SCAN_IMAGE"

// TestStrictScanImageClearsChoptFloors is the ratchet that keeps the strict-scan
// lane able to EXECUTE every shape the emitter can produce.
//
// The lane runs the spec corpus's emitted SQL against a real ClickHouse. When
// the pinned server predates a feature's floor, the lane does not go red — it
// degrades: the server rejects the query before a row decodes, the case is
// classified out of the matrix decoder's scope, and that shape silently stops
// being strict-scanned. That is exactly how the native timeSeries*ToGrid family
// (25.9 floor) sat unexercised behind a 25.8 pin while the lane reported green.
//
// Both sides are derived, not restated: the floor is the maximum MinVersion in
// internal/chopt's feature registry, and the version is parsed from the image
// tag the Justfile pre-pulls. Adding a feature with a higher floor fails here
// until the pin moves.
func TestStrictScanImageClearsChoptFloors(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	justfile := string(buf)

	pinned := justVariableList(t, justfile, strictScanImageVar)
	if len(pinned) != 1 {
		t.Fatalf("%s pins %d images (%v); the strict-scan lane starts exactly one server", strictScanImageVar, len(pinned), pinned)
	}
	image := pinned[0]

	// The Go const is what testcontainers actually starts; the Justfile var is
	// only what gets pre-pulled. If they drift, the ratchet would be measuring
	// a server the lane never boots.
	src, err := os.ReadFile(strictScanSourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", strictScanSourcePath, err)
	}
	constRE := regexp.MustCompile(`strictScanCHImage\s*=\s*"([^"]*)"`)
	m := constRE.FindStringSubmatch(string(src))
	if m == nil {
		t.Fatalf("%s declares no strictScanCHImage const — the ratchet is scanning for a shape that no longer exists", strictScanSourcePath)
	}
	if m[1] != image {
		t.Fatalf("strictScanCHImage = %q but %s = %q; the lane would start a server the pre-pull and this ratchet never saw", m[1], strictScanImageVar, image)
	}

	_, tag, ok := strings.Cut(image, ":")
	if !ok {
		t.Fatalf("%s = %q carries no :tag, so no version can be derived from it", strictScanImageVar, image)
	}
	imageVersion, ok := chopt.ParseVersion(tag)
	if !ok {
		t.Fatalf("cannot parse a ClickHouse version out of the %s tag %q", strictScanImageVar, tag)
	}

	var highest chopt.Version
	var highestID string
	for _, f := range chopt.Registry() {
		if !f.MinVersion.AtLeast(highest) || f.MinVersion == highest {
			continue
		}
		highest, highestID = f.MinVersion, f.ID
	}
	if highestID == "" {
		t.Fatalf("every chopt feature floors at %s — the registry is empty or the ratchet reads the wrong field", chopt.AlwaysAvailable)
	}

	if !imageVersion.AtLeast(highest) {
		t.Fatalf("%s is %s (ClickHouse %s) but chopt feature %q floors at %s. "+
			"The strict-scan lane cannot execute that feature's SQL on this server, and an unrunnable shape is "+
			"classified out of scope rather than failing — raise the pin (and strictScanCHImage) to a tag at or above %s.",
			strictScanImageVar, image, imageVersion, highestID, highest, highest)
	}
}
