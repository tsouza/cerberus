//go:build chdb

package perf

// stripTrailingSemi drops a single trailing ';' / trailing whitespace so an
// emitted statement can be embedded as a subquery. Shared by the structural
// + nested-set cycle guards. (Previously defined in
// structural_recursive_scaling_chdb_test.go, which was folded into the
// generic test/perf/scaling harness; kept here so the cycle guards that
// still consume it stay self-contained.)
func stripTrailingSemi(s string) string {
	for len(s) > 0 && (s[len(s)-1] == ';' || s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
