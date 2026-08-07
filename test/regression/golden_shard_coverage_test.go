package regression

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// repoRootFromRegression reaches the module root from this package's directory.
// Both derivations resolve artefact paths against the process's working
// directory, exactly as `just` invokes the runner, so every exec below runs
// from here rather than from the test's own directory — a runner started in
// test/regression/ resolves no corpus at all and reports every shard set as
// covering, which is a check that cannot fail.
const repoRootFromRegression = "../.."

// goldenUpdateModule is the runner behind `just update-golden <shard>...`,
// relative to repoRootFromRegression.
const goldenUpdateModule = ".github/scripts/golden-update.mjs"

// goldenShardsLib holds the shard vocabulary and both staleness derivations.
const goldenShardsLib = "../../.github/scripts/lib/golden-shards.mjs"

// updateGoldenRecipeHeader is the recipe header that makes the shard argument
// structurally REQUIRED. `*shards` is just's variadic parameter form, which
// accepts zero arguments and hands the runner an empty string — the runner is
// what rejects it. A header without a parameter would silently restore a
// default, which is the one thing #1898's design forbids.
const updateGoldenRecipeHeader = "update-golden *shards:"

// A shard set that covers a PromQL fixture change: the fixture's own goldens,
// the routing baseline derived from its `-- query.promql --`, and the
// cardinality baseline derived from its `-- sql --`.
const promqlFixtureCoveringSet = "solver promql cardinality"

// checkGoldenShardCoverage drives the real coverage check over a synthetic
// diff, without regenerating anything or touching the working tree.
func checkGoldenShardCoverage(t *testing.T, shards, changed string) (string, int) {
	t.Helper()

	cmd := exec.Command("node", goldenUpdateModule)
	cmd.Dir = repoRootFromRegression
	cmd.Env = append(
		os.Environ(),
		"GOLDEN_UPDATE_CHECK_ONLY=1",
		"GOLDEN_SHARDS="+shards,
		"GOLDEN_UPDATE_CHANGED_FILES="+changed,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run %s: %v\n%s", goldenUpdateModule, err, out)
		}

		return string(out), exitErr.ExitCode()
	}

	return string(out), 0
}

// TestUpdateGoldenRefusesAnUncoveredShardSet pins the property that makes
// sharding `just update-golden` safe at all.
//
// The recipe used to chain all five regeneration stages unconditionally, and
// that chaining was not tidiness: it was the fix for #1573 (hit again by #1571
// and #1592), where a contributor regenerated one artefact, saw zero remaining
// churn, pushed, and got a red lane because a DIFFERENT artefact had gone
// stale. CI can only DETECT that drift — the harness refuses to rewrite a
// golden when `CI` is set, because a golden is a reviewed artifact and a lane
// that rewrites its own expectations asserts nothing — which is exactly what
// turned the omission into a trap.
//
// Naming a shard reopens that trap unless something computes the shards the
// branch's own diff implies are stale. This test drives that computation over
// synthetic diffs and asserts it fires in BOTH directions. A check that only
// ever passes is a gap, not soft coverage; a check that always fires is noise
// that gets rubber-stamped. Both halves are asserted here.
func TestUpdateGoldenRefusesAnUncoveredShardSet(t *testing.T) {
	t.Parallel()

	t.Run("fires on an under-covering set", func(t *testing.T) {
		t.Parallel()

		out, code := checkGoldenShardCoverage(t, "promql", "test/spec/promql/fixture_under_test.txtar")
		if code == 0 {
			t.Fatalf("regenerating only `promql` after a PromQL fixture changed was accepted. "+
				"That fixture's `-- query.promql --` feeds the solver decision baseline and its "+
				"`-- sql --` feeds the cardinality baseline, so both go stale — the #1096 / #1098 "+
				"miss that turned `perf-guards` red on main after merge.\n%s", out)
		}
		for _, want := range []string{
			"`cardinality` shard was not regenerated",
			"`solver` shard was not regenerated",
			"run: just update-golden " + promqlFixtureCoveringSet,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("coverage failure does not contain %q; a contributor cannot act on it "+
					"without the exact command to run.\n%s", want, out)
			}
		}
	})

	t.Run("passes a set that covers the diff", func(t *testing.T) {
		t.Parallel()

		out, code := checkGoldenShardCoverage(t,
			promqlFixtureCoveringSet, "test/spec/promql/fixture_under_test.txtar")
		if code != 0 {
			t.Fatalf("a shard set that covers the diff was rejected (exit %d). A check that "+
				"cannot be satisfied is a check nobody can run.\n%s", code, out)
		}
	})

	// The discriminating half. The solver baseline is derived from the PromQL
	// corpus alone, and the check learns that by resolving the baseline's own
	// shard ids back to the fixtures they name — not from a list written here.
	// A LogQL fixture must therefore NOT imply it. If this ever starts failing
	// because the derivation collapsed to "demand everything", the check has
	// become noise and the next contributor will stop reading it.
	t.Run("does not demand a shard the diff cannot have staled", func(t *testing.T) {
		t.Parallel()

		out, code := checkGoldenShardCoverage(t, "logql cardinality", "test/spec/logql/fixture_under_test.txtar")
		if code != 0 {
			t.Fatalf("a LogQL fixture change demanded more than `logql cardinality` (exit %d). "+
				"The solver decision baseline reads the PromQL corpus only; demanding it here "+
				"is over-coverage, and an over-firing check gets rubber-stamped.\n%s", code, out)
		}
		if strings.Contains(out, "`solver`") {
			t.Errorf("a LogQL fixture change named the `solver` shard.\n%s", out)
		}
	})

	t.Run("ignores a change no shard derives from", func(t *testing.T) {
		t.Parallel()

		out, code := checkGoldenShardCoverage(t, "promql", "docs/engine.md")
		if code != 0 {
			t.Fatalf("a documentation-only change forced extra shards (exit %d).\n%s", code, out)
		}
	})
}

// TestUpdateGoldenHasNoDefaultShard pins the other half of the design: there is
// no way to invoke the recipe without saying what is being regenerated.
//
// A default would quietly reintroduce the 14-minute whole-corpus run for anyone
// who typed the old command out of habit, and — worse — a default of "nothing"
// would let a contributor regenerate none of it while believing they had.
func TestUpdateGoldenHasNoDefaultShard(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile(justfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", justfilePath, err)
	}
	if !strings.Contains(string(buf), updateGoldenRecipeHeader) {
		t.Errorf("%s has no %q recipe header. The shard argument must stay a recipe parameter; "+
			"a bare `update-golden:` header restores a default nobody stated.",
			justfilePath, updateGoldenRecipeHeader)
	}

	out, code := checkGoldenShardCoverage(t, "", "")
	if code == 0 {
		t.Fatalf("bare `just update-golden` was accepted. It must be an error that prints the "+
			"shard vocabulary.\n%s", out)
	}
	for _, want := range []string{"requires at least one shard", "shards:", "all"} {
		if !strings.Contains(out, want) {
			t.Errorf("the empty-shard error does not contain %q, so it does not teach the "+
				"vocabulary it rejects.\n%s", want, out)
		}
	}

	out, code = checkGoldenShardCoverage(t, "promql cardinaltiy", "")
	if code == 0 {
		t.Fatalf("a misspelled shard was accepted and would have silently regenerated less "+
			"than the caller asked for.\n%s", out)
	}
	if !strings.Contains(out, "unknown shard") {
		t.Errorf("a misspelled shard was rejected without saying so.\n%s", out)
	}
}

// TestGoldenShardRelationIsDerived pins that the staleness relation is computed
// from the artefacts rather than restated as a table of fixture names.
//
// This is the repo's derive-not-declare rule applied to the one place it is
// easiest to violate: it is far simpler to write `promql -> [solver,
// cardinality]` than to resolve a baseline's shard ids back to the corpus they
// came from. The declared form drifts the moment a baseline is repointed, and
// its drift is silent — which is the failure mode the chaining existed to
// prevent, moved one level up.
func TestGoldenShardRelationIsDerived(t *testing.T) {
	t.Parallel()

	buf, err := os.ReadFile(goldenShardsLib)
	if err != nil {
		t.Fatalf("read %s: %v", goldenShardsLib, err)
	}
	src := string(buf)

	for _, want := range []struct{ token, why string }{
		{"corpusRootFromShardTree", "the per-fixture baselines must name their own corpus"},
		{"go list", "the package closure must come from the import graph"},
		{"-deps", "the package closure must come from the import graph"},
	} {
		if !strings.Contains(src, want.token) {
			t.Errorf("%s no longer contains %q: %s. If the derivation was replaced by a "+
				"hand-written map of fixture paths to shards, that map drifts silently the first "+
				"time a baseline is repointed.", goldenShardsLib, want.token, want.why)
		}
	}

	// The relation must not be spelled out as a literal edge between two shard
	// names. Anything of that shape is a declaration wearing a derivation's
	// clothes.
	for _, banned := range []string{
		"'promql': ['solver'",
		"promql: ['solver'",
		"promql: ['cardinality'",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("%s declares the shard-to-shard staleness edge %q by hand. Derive it from "+
				"the artefacts instead.", goldenShardsLib, banned)
		}
	}
}
