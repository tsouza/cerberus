package chopt

// MinSupportedMajor and MinSupportedMinor name the minimum supported
// ClickHouse release line: the oldest LTS line upstream ClickHouse still
// supports. A server below it is unsupported: cerberus still starts against
// it, and reports it as unsupported.
const (
	MinSupportedMajor = 26
	MinSupportedMinor = 3
)

// MinSupportedVersion returns the minimum supported ClickHouse version,
// MinSupportedMajor.MinSupportedMinor.
func MinSupportedVersion() Version {
	return Version{Major: MinSupportedMajor, Minor: MinSupportedMinor}
}

// Unsupported reports whether v lies below the minimum supported version.
func (v Version) Unsupported() bool {
	return v.Less(MinSupportedVersion())
}
