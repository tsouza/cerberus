package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// bannerArtGlyphRows is the number of rows the wordmark spells "Cerberus" over.
const bannerArtGlyphRows = 5

// bannerArtColumns is the wordmark's rendered width; the meta line is padded to
// exactly this many columns so it ends flush with the art's right edge.
const bannerArtColumns = 42

// TestBannerArtShape pins the wordmark's geometry. Without it, blanking or
// truncating bannerArt would still leave every "does the help contain the art"
// assertion vacuously true.
func TestBannerArtShape(t *testing.T) {
	rows := 0
	for _, line := range strings.Split(bannerArt, "\n") {
		if strings.TrimSpace(line) != "" {
			rows++
		}
	}
	if rows != bannerArtGlyphRows {
		t.Errorf("bannerArt has %d glyph rows, want %d", rows, bannerArtGlyphRows)
	}
	if w := bannerArtWidth(); w != bannerArtColumns {
		t.Errorf("bannerArtWidth() = %d, want %d", w, bannerArtColumns)
	}
}

// TestRootHelpRendersBanner pins that `cerberus --help` leads with every row of
// the wordmark, followed by the build-identity line and the description — and
// that the retired jargon is gone.
func TestRootHelpRendersBanner(t *testing.T) {
	root := newRootCmd(func() error {
		t.Fatal("server RunE must not run for `--help`")
		return nil
	})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("`cerberus --help` execute: %v", err)
	}
	help := out.String()

	rows := 0
	for _, line := range strings.Split(bannerArt, "\n") {
		line = strings.TrimRight(line, " ")
		if line == "" {
			continue
		}
		rows++
		if !strings.Contains(help, line) {
			t.Errorf("help output is missing wordmark row %q; got:\n%s", line, help)
		}
	}
	if rows != bannerArtGlyphRows {
		t.Errorf("asserted %d wordmark rows, want %d", rows, bannerArtGlyphRows)
	}

	if want := bannerMetaLine(Version, buildDate()); !strings.Contains(help, want) {
		t.Errorf("help output is missing the build-identity line %q; got:\n%s", want, help)
	}
	for _, line := range strings.Split(rootDescription, "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(help, line) {
			t.Errorf("help output is missing description line %q; got:\n%s", line, help)
		}
	}
	if strings.Contains(help, "env-driven") {
		t.Errorf("help output still carries the retired jargon; got:\n%s", help)
	}
}

// TestBannerMetaLine pins the build-identity rendering across the stamped and
// unstamped builds: goreleaser's bare tag gains a "v", an already-prefixed or
// non-numeric version is left alone, and a missing build date drops the whole
// field rather than printing an empty one.
func TestBannerMetaLine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   string
		buildDate string
		want      string
	}{
		{"release build", "1.13.1", "2026-07-28", "v1.13.1 · built 2026-07-28"},
		{"already prefixed", "v1.13.1", "2026-07-28", "v1.13.1 · built 2026-07-28"},
		{"no vcs stamp", "1.13.1", "", "v1.13.1"},
		{"dev build", devVersion, "", devVersion},
		{"no ldflags at all", "", "", devVersion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bannerMetaLine(tc.version, tc.buildDate); got != tc.want {
				t.Errorf("bannerMetaLine(%q, %q) = %q, want %q", tc.version, tc.buildDate, got, tc.want)
			}
		})
	}
}

// TestRenderBannerRightAlignsMeta pins that the build-identity line ends flush
// with the wordmark's right edge, and that an unstamped build degrades to a bare
// "dev" with no dangling separator or "built " label.
func TestRenderBannerRightAlignsMeta(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   string
		buildDate string
	}{
		{"release build", "1.13.1", "2026-07-28"},
		{"unstamped build", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(renderBanner(tc.version, tc.buildDate), "\n")
			if len(lines) != bannerArtGlyphRows+1 {
				t.Fatalf("renderBanner produced %d lines, want %d", len(lines), bannerArtGlyphRows+1)
			}
			meta := lines[len(lines)-1]
			if n := utf8.RuneCountInString(meta); n != bannerArtColumns {
				t.Errorf("meta line is %d columns wide, want %d (flush with the art): %q", n, bannerArtColumns, meta)
			}
			if strings.TrimSpace(meta) != bannerMetaLine(tc.version, tc.buildDate) {
				t.Errorf("meta line = %q, want %q padded", strings.TrimSpace(meta), bannerMetaLine(tc.version, tc.buildDate))
			}
		})
	}

	unstamped := renderBanner("", "")
	if strings.Contains(unstamped, bannerBuildDatePrefix) || strings.Contains(unstamped, bannerMetaSeparator) {
		t.Errorf("unstamped build must not print an empty build-date field; got:\n%s", unstamped)
	}
}
