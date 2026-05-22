package format

// ComputePromLabelCandidatesForTest exposes the cold-path powerset walk
// so benchmarks can measure the "no cache" baseline directly without
// touching the [PromLabelToOTelCandidates] memo. Test-only export —
// production callers always go through the cached entry point.
func ComputePromLabelCandidatesForTest(s string) []string {
	if s == "" {
		return []string{""}
	}
	if len(s) >= 2 && s[0] == '_' && s[1] == '_' {
		return []string{s}
	}
	return computePromLabelCandidates(s)
}
