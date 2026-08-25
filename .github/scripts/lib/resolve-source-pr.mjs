// resolve-source-pr.mjs — resolve the pull request that produced a given
// commit, for the ONE place cerberus needs it: release-preflight.mjs's
// green-check guard (tsouza/cerberus#2394).
//
// The problem this exists to solve: `release.yml`'s `preflight` job needs
// check-runs to exist on the commit it is about to tag. For a squash-merged
// PR, that commit (`pushedSha`, the new SHA `git log` shows on `main`) is
// byte-identical in TREE content to the PR's tip commit (`pull.head.sha`,
// the SHA the PR's `pull_request:`-triggered workflows actually ran
// against) — but it is a DIFFERENT SHA, and GitHub check-runs attach to a
// SHA, not a tree hash. Historically the only way to give `preflight`
// check-runs on `pushedSha` was to re-run every lane's `push:` trigger on
// it, even though the PR already proved the identical tree green. This
// module lets a caller instead credit a check-run posted on the PR's tip
// SHA as evidence for the merge commit, since the PR's tip SHA is where
// that validation actually happened.
//
// FAIL CLOSED, not fail open — this is release-pipeline logic (see #2361):
// `selectSourcePR` returns a resolved PR ONLY for an EXACT, UNAMBIGUOUS
// match — a single pull request that is MERGED and whose `merge_commit_sha`
// equals the target SHA exactly. `merge_commit_sha` is GitHub's own record
// of "this SHA is what merging this PR produced on its base branch", set
// identically for squash / merge-commit / rebase merge strategies, so it is
// the one field that proves the relationship rather than merely suggesting
// it. Everything short of that — no match, more than one match, an open PR,
// a merged PR whose `merge_commit_sha` differs (e.g. a PR that merely
// CONTAINS this commit in its ancestry, which `commits/{sha}/pulls` also
// returns) — resolves to `null`. A caller that gets `null` must behave
// EXACTLY as it did before this module existed: read check-runs from the
// target SHA alone. There is no "probably the right PR" tier.
//
// Why `commits/{sha}/pulls` needs this filter at all: GitHub's "list pull
// requests associated with a commit" endpoint is NOT scoped to "the PR that
// introduced this commit" — it returns every pull request whose branch
// contains the commit, which includes any PR later opened against `main`
// after this commit landed (their history contains it via the base branch).
// An unfiltered read would treat an unrelated, unmerged, in-flight PR as the
// "source" of a commit it has nothing to do with. The `merged_at` +
// `merge_commit_sha` filter is precise: only the one PR whose merge
// produced this EXACT sha survives it.
//
// Imports only node: builtins (Node ships `fetch`), matching the house style
// in release-preflight.mjs. Exercised by
// `node --test .github/scripts/lib/resolve-source-pr.test.mjs`, wired into
// ci.yml as "resolve-source-pr self-test (fail-closed PR-match guard)" — the
// real test surface; this module carries no test scaffold of its own.

// selectSourcePR — pure. `pulls` is the raw array returned by
// `GET /repos/{owner}/{repo}/commits/{sha}/pulls`; `sha` is the commit being
// resolved. Returns `{ number, headSha, headRef, baseRef }` for the single
// exact match, or `null` for none / more than one (ambiguous is a refusal,
// not a guess).
export function selectSourcePR(pulls, sha) {
  const candidates = (pulls ?? []).filter(
    (p) => p && p.merged_at != null && p.merge_commit_sha === sha && p.head?.sha,
  );
  if (candidates.length !== 1) return null;
  const pr = candidates[0];
  return {
    number: pr.number,
    headSha: pr.head.sha,
    headRef: pr.head.ref ?? null,
    baseRef: pr.base?.ref ?? null,
  };
}

// resolveSourcePR — network wrapper. Paginates `commits/{sha}/pulls` (a
// commit is realistically associated with a handful of PRs at most, but the
// pagination loop costs nothing and matches the other list-* helpers in
// release-preflight.mjs) and applies `selectSourcePR`. Throws on a
// non-2xx response or a transport failure — the caller decides what
// "could not resolve" means for its own gate; this module never silently
// swallows a real error into a `null` that looks identical to "no PR found".
export async function resolveSourcePR({ repo, sha, apiBase = 'https://api.github.com', token, fetchImpl = fetch }) {
  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: 'application/vnd.github+json',
    'X-GitHub-Api-Version': '2022-11-28',
  };
  const out = [];
  let page = 1;
  for (;;) {
    const res = await fetchImpl(`${apiBase}/repos/${repo}/commits/${sha}/pulls?per_page=100&page=${page}`, {
      headers,
    });
    if (!res.ok) {
      throw new Error(`GET commits/${sha}/pulls -> ${res.status} ${res.statusText}`);
    }
    const data = await res.json();
    out.push(...data);
    if (data.length < 100) break;
    page += 1;
  }
  return selectSourcePR(out, sha);
}
