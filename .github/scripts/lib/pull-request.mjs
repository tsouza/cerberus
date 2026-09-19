// Live pull-request metadata. GitHub's pull_request event payload can be
// stale (and API edits do not reliably emit edited); every metadata gate must
// therefore read the current PR object before judging it.

export async function fetchPullRequest({ apiUrl, repository, number, token, fetchImpl = fetch }) {
  if (!repository || !number || !token) {
    throw new Error('live pull-request metadata requires repository, number, and token');
  }
  const response = await fetchImpl(`${apiUrl}/repos/${repository}/pulls/${number}`, {
    headers: { accept: 'application/vnd.github+json', authorization: `Bearer ${token}` },
  });
  if (!response.ok) {
    throw new Error(`GitHub pull-request metadata request failed: HTTP ${response.status}`);
  }
  const payload = await response.json();
  if (typeof payload.title !== 'string' || typeof payload.body !== 'string') {
    throw new Error('GitHub pull-request metadata response omitted title or body');
  }
  return { title: payload.title, body: payload.body };
}
