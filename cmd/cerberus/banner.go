package main

import (
	"runtime/debug"
	"strings"
	"time"
	"unicode/utf8"
)

// bannerArt is the ASCII wordmark printed at the top of the root help. A raw
// literal keeps the backslashes verbatim; the whitespace-only final row is the
// gap the build-identity line sits in.
const bannerArt = `   ___          _
  / __\___ _ __| |__   ___ _ __ _   _ ___
 / /  / _ \ '__| '_ \ / _ \ '__| | | / __|
/ /__|  __/ |  | |_) |  __/ |  | |_| \__ \
\____/\___|_|  |_.__/ \___|_|   \__,_|___/
`

// bannerMetaSeparator joins the two build-identity fields on the banner's meta
// line.
const bannerMetaSeparator = " · "

// bannerBuildDatePrefix labels the build-date field on the banner meta line.
const bannerBuildDatePrefix = "built "

// devVersion is the main.Version fallback for a build with no goreleaser ldflag.
const devVersion = "dev"

// vcsTimeSetting is the runtime/debug build setting carrying the commit
// timestamp — the same embedded build info buildRevision reads vcs.revision from.
const vcsTimeSetting = "vcs.time"

// buildDateLayout renders the vcs.time stamp as a bare calendar date.
const buildDateLayout = "2006-01-02"

// rootDescription is the human-facing blurb under the wordmark: what cerberus
// is, in the register of the project's one-line pitch, hard-wrapped to fit an
// 80-column terminal. The subcommand list below it carries the rest.
const rootDescription = "Drop-in Prometheus / Loki / Tempo HTTP gateway for ClickHouse.\n" +
	"Translate PromQL, LogQL, and TraceQL into optimized ClickHouse SQL —\n" +
	"keep Grafana, swap the backend.\n" +
	"\n" +
	"Run with no subcommand to start the server."

// rootLong renders the root help body: the wordmark, the build-identity line
// flush under it, then the description.
func rootLong() string {
	return renderBanner(Version, buildDate()) + "\n\n" + rootDescription
}

// renderBanner returns the wordmark with the build-identity line right-aligned
// under it. An empty version degrades to "dev" and an empty buildDate drops the
// build-date field entirely, so an unstamped dev build never prints "v · built ".
func renderBanner(version, buildDate string) string {
	meta := bannerMetaLine(version, buildDate)
	var b strings.Builder
	b.WriteString(strings.TrimRight(bannerArt, "\n"))
	b.WriteByte('\n')
	if pad := bannerArtWidth() - utf8.RuneCountInString(meta); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	b.WriteString(meta)
	return b.String()
}

// bannerMetaLine renders the build-identity fields, omitting the build date when
// the binary carries no VCS timestamp.
func bannerMetaLine(version, buildDate string) string {
	meta := bannerVersionLabel(version)
	if buildDate != "" {
		meta += bannerMetaSeparator + bannerBuildDatePrefix + buildDate
	}
	return meta
}

// bannerVersionLabel renders main.Version for display. goreleaser injects the
// bare tag ("1.13.1"), which reads as a version only with a "v" prefix; anything
// that doesn't start with a digit ("dev", an already-prefixed "v1.13.1") is
// printed as-is.
func bannerVersionLabel(version string) string {
	if version == "" {
		return devVersion
	}
	if c := version[0]; c >= '0' && c <= '9' {
		return "v" + version
	}
	return version
}

// bannerArtWidth is the column count of the widest wordmark row, ignoring
// trailing padding. The meta line is right-aligned to it so the version sits
// flush with the wordmark's right edge.
func bannerArtWidth() int {
	width := 0
	for _, line := range strings.Split(bannerArt, "\n") {
		if n := utf8.RuneCountInString(strings.TrimRight(line, " ")); n > width {
			width = n
		}
	}
	return width
}

// buildDate returns the binary's build date as YYYY-MM-DD, read from the same
// embedded build info as buildRevision. It is empty when the build carries no
// VCS timestamp (a `go test` binary, or -buildvcs=false), which is what lets the
// banner drop the field instead of printing a blank one.
func buildDate() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key != vcsTimeSetting {
			continue
		}
		t, err := time.Parse(time.RFC3339, s.Value)
		if err != nil {
			return ""
		}
		return t.Format(buildDateLayout)
	}
	return ""
}
