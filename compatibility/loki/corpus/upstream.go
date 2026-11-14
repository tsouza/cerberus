package corpus

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// The upstream LogQL corpus is read in place from a Go module, never copied
// into this repository. These coordinates pin it: the module, its version,
// the module's go.sum hash (the `h1:` digest `go mod download` reports and
// verifies), and the corpus directory inside it.
const (
	UpstreamModule        = "github.com/grafana/loki/v3"
	UpstreamVersion       = "v3.7.1"
	UpstreamSum           = "h1:Nw1TV8uHtiB6bkO9210kvHjewtgCyZtSippj7X42+O4="
	UpstreamQueriesSubdir = "pkg/logql/bench/queries"
)

// downloadInfo is the subset of `go mod download -json` output read here.
type downloadInfo struct {
	Dir   string
	Sum   string
	Error string
}

// UpstreamQueriesDir fetches the pinned upstream module into the Go module
// cache (a no-op when it is already there) and returns the path of its
// corpus directory. It fails when the module's hash is not [UpstreamSum],
// so a moved tag or a tampered cache cannot change the corpus silently.
//
// The download runs in a scratch directory outside any module, so neither
// the caller's go.mod nor its go.sum is consulted or modified. The answer
// is computed once per process.
func UpstreamQueriesDir() (string, error) { return upstreamQueriesDir() }

var upstreamQueriesDir = sync.OnceValues(downloadUpstreamQueries)

func downloadUpstreamQueries() (string, error) {
	scratch, err := os.MkdirTemp("", "loki-corpus-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of an empty scratch directory

	coord := UpstreamModule + "@" + UpstreamVersion
	cmd := exec.Command("go", "mod", "download", "-json", coord) //nolint:gosec // fixed arguments
	cmd.Dir = scratch
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()

	var info downloadInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return "", fmt.Errorf("go mod download %s: %v (stderr: %s)", coord, runErr, bytes.TrimSpace(stderr.Bytes()))
	}
	if info.Error != "" {
		return "", fmt.Errorf("go mod download %s: %s", coord, info.Error)
	}
	if runErr != nil {
		return "", fmt.Errorf("go mod download %s: %w (stderr: %s)", coord, runErr, bytes.TrimSpace(stderr.Bytes()))
	}
	if info.Sum != UpstreamSum {
		return "", fmt.Errorf("%s has hash %q, pinned %q", coord, info.Sum, UpstreamSum)
	}
	dir := filepath.Join(info.Dir, filepath.FromSlash(UpstreamQueriesSubdir))
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("%s carries no corpus directory: %w", coord, err)
	}
	return dir, nil
}

// opaqueIDPrefix marks an ID derived from upstream corpus text.
const opaqueIDPrefix = "upstream:"

// opaqueIDHexDigits is how many leading hex digits of the SHA-256 an
// opaque ID keeps: 64 bits, far past any collision risk for a corpus of
// a few hundred cases.
const opaqueIDHexDigits = 16

// OpaqueID derives a stable identifier from a readable identity built
// out of upstream corpus text (a description, a query). Committed
// ledgers record the opaque form, so they name an upstream case without
// carrying the upstream text; the same corpus always yields the same ID.
func OpaqueID(readable string) string {
	sum := sha256.Sum256([]byte(readable))
	return opaqueIDPrefix + hex.EncodeToString(sum[:])[:opaqueIDHexDigits]
}
