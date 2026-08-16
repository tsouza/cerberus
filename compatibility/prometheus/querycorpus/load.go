// Package querycorpus loads the conflict-resistant PromQL compatibility corpus.
package querycorpus

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	headerFilename   = "header.yml"
	manifestFilename = "manifest.txt"
	fragmentsDirname = "fragments"
)

var fragmentNameRE = regexp.MustCompile(`^([0-9]{3})-[a-z0-9]+(?:-[a-z0-9]+)*\.yml$`)

// Case is one upstream promql-compliance-tester test case.
type Case struct {
	Query       string   `yaml:"query"`
	VariantArgs []string `yaml:"variant_args,omitempty"`
	ShouldFail  bool     `yaml:"should_fail,omitempty"`
}

type document struct {
	TestCases []Case `yaml:"test_cases"`
}

// Load validates and assembles dir into the single YAML document consumed by
// promql-compliance-tester. The manifest is both the complete fragment roster
// and its canonical order; filesystem enumeration is never load-bearing.
func Load(dir string) ([]byte, []Case, error) {
	header, err := os.ReadFile(filepath.Join(dir, headerFilename))
	if err != nil {
		return nil, nil, fmt.Errorf("read corpus header: %w", err)
	}
	if !bytes.HasSuffix(header, []byte("\ntest_cases:\n")) || bytes.Count(header, []byte("\ntest_cases:\n")) != 1 {
		return nil, nil, errors.New("corpus header must end with exactly one test_cases mapping key")
	}

	manifest, err := loadManifest(filepath.Join(dir, manifestFilename))
	if err != nil {
		return nil, nil, err
	}
	entries, err := os.ReadDir(filepath.Join(dir, fragmentsDirname))
	if err != nil {
		return nil, nil, fmt.Errorf("read corpus fragments: %w", err)
	}
	if err := validateRoster(manifest, entries); err != nil {
		return nil, nil, err
	}

	assembled := append([]byte(nil), header...)
	for _, name := range manifest {
		fragment, readErr := os.ReadFile(filepath.Join(dir, fragmentsDirname, name))
		if readErr != nil {
			return nil, nil, fmt.Errorf("read corpus fragment %q: %w", name, readErr)
		}
		if len(bytes.TrimSpace(fragment)) == 0 {
			return nil, nil, fmt.Errorf("corpus fragment %q is empty", name)
		}
		if !bytes.HasSuffix(fragment, []byte("\n")) {
			return nil, nil, fmt.Errorf("corpus fragment %q must end with a newline", name)
		}
		assembled = append(assembled, fragment...)
	}

	cases, err := decodeCases(assembled)
	if err != nil {
		return nil, nil, err
	}
	return assembled, cases, nil
}

func loadManifest(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read corpus manifest: %w", err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		return nil, errors.New("corpus manifest must end with a newline")
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil, errors.New("corpus manifest is empty")
	}
	for i, name := range lines {
		match := fragmentNameRE.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("corpus manifest entry %q is not NNN-family.yml", name)
		}
		index, _ := strconv.Atoi(match[1])
		if index != i {
			return nil, fmt.Errorf("corpus manifest entry %q has index %03d, want %03d", name, index, i)
		}
		if i > 0 && lines[i-1] >= name {
			return nil, fmt.Errorf("corpus manifest is not in unique canonical filename order at %q", name)
		}
	}
	return lines, nil
}

func validateRoster(manifest []string, entries []os.DirEntry) error {
	found := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return fmt.Errorf("unexpected directory in corpus fragments: %q", entry.Name())
		}
		if fragmentNameRE.FindStringSubmatch(entry.Name()) == nil {
			return fmt.Errorf("unexpected file in corpus fragments: %q", entry.Name())
		}
		found = append(found, entry.Name())
	}
	sort.Strings(found)
	if len(found) != len(manifest) {
		return fmt.Errorf("corpus fragment roster mismatch: manifest=%v filesystem=%v", manifest, found)
	}
	for i := range manifest {
		if found[i] != manifest[i] {
			return fmt.Errorf("corpus fragment roster mismatch: manifest=%v filesystem=%v", manifest, found)
		}
	}
	return nil
}

func decodeCases(raw []byte) ([]Case, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var decoded document
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decode assembled corpus: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("assembled corpus contains more than one YAML document")
		}
		return nil, fmt.Errorf("decode assembled corpus trailer: %w", err)
	}
	if len(decoded.TestCases) == 0 {
		return nil, errors.New("assembled corpus contains no test cases")
	}
	seen := make(map[string]int, len(decoded.TestCases))
	for i, testCase := range decoded.TestCases {
		if strings.TrimSpace(testCase.Query) == "" {
			return nil, fmt.Errorf("assembled corpus test case %d has an empty query", i)
		}
		identity := testCase.Query + "\x00" + strings.Join(testCase.VariantArgs, "\x00") + "\x00" + strconv.FormatBool(testCase.ShouldFail)
		if previous, ok := seen[identity]; ok {
			return nil, fmt.Errorf("assembled corpus duplicates test case %d at index %d: %q", previous, i, testCase.Query)
		}
		seen[identity] = i
	}
	return decoded.TestCases, nil
}
