// Package regression holds meta-tests that pin down past CI failures so
// they can't silently recur. Each test maps to a specific bug we already
// fixed and surfaces the regression class via repo-level file inspection
// rather than runtime behavior — cheap to run on every PR, no build tag,
// no external dependencies.
//
// When adding tests here, include a comment referencing the commit /
// PR / bug the test guards against, so future maintainers know the
// motivation if the test fails on an unrelated change.
package regression
