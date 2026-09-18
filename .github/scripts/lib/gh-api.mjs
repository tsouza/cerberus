// gh-api.mjs — the one GitHub REST client the scripts share.
//
// Every script that reads the GitHub API used to carry its own copy of the
// same three things: the header block (`Accept`, `Authorization`,
// `X-GitHub-Api-Version`), a "fetch and parse JSON, throw on non-2xx" helper,
// and a page loop over `per_page=100`. Eleven copies, and they disagreed on
// the one behaviour that matters to a caller — whether a 404 is `null` or an
// error — silently, per file. This module is the single copy; each caller
// states its 404 behaviour explicitly instead of inheriting whichever copy it
// happened to be pasted from.
//
// Exports:
//   GITHUB_API_VERSION      the REST API version every request pins.
//   DEFAULT_API_BASE        `https://api.github.com`; callers read
//                           GITHUB_API_URL first and fall back to this.
//   GITHUB_PER_PAGE         the largest page size GitHub serves (100).
//   HTTP_NOT_FOUND / HTTP_NO_CONTENT
//   NOT_FOUND_NULL / NOT_FOUND_THROW
//                           the two `notFound` policies (see ghJSON).
//   ghHeaders(token, { accept, userAgent })
//                           the request headers for a bearer token.
//   ghJSON(url, { token, headers, what, notFound, init, fetchImpl })
//                           one request, parsed. `notFound` is REQUIRED to be
//                           one of the two policies: NOT_FOUND_NULL returns
//                           null on 404 (a lookup that may legitimately miss),
//                           NOT_FOUND_THROW treats 404 like any other non-2xx
//                           (a resource that must exist). Any other non-2xx
//                           throws `<what>: HTTP <status> <text> for <url>`.
//                           A 204 resolves to null. `init` is passed to fetch
//                           (method, body) with the headers merged in;
//                           `fetchImpl` is injectable for tests.
//   withPage(url, page, perPage)
//                           `url` with `per_page` and `page` appended.
//   ghPaginate({ url, token, headers, what, pick, perPage, maxPages, fetchImpl })
//                           every item across every page. `pick` pulls the
//                           item array out of a page body (the check-run,
//                           check-suite, workflow-run and combined-status
//                           endpoints each wrap theirs under a different
//                           key); a short page ends the walk. With `maxPages`
//                           set, a walk that never shortens throws rather
//                           than returning a silent prefix.

export const GITHUB_API_VERSION = '2022-11-28';
export const DEFAULT_API_BASE = 'https://api.github.com';
export const GITHUB_PER_PAGE = 100;
export const HTTP_NOT_FOUND = 404;
export const HTTP_NO_CONTENT = 204;
export const NOT_FOUND_NULL = 'null';
export const NOT_FOUND_THROW = 'throw';

const DEFAULT_ACCEPT = 'application/vnd.github+json';

export function ghHeaders(token, { accept = DEFAULT_ACCEPT, userAgent } = {}) {
  const headers = {
    Accept: accept,
    Authorization: `Bearer ${token}`,
    'X-GitHub-Api-Version': GITHUB_API_VERSION,
  };
  if (userAgent) headers['User-Agent'] = userAgent;
  return headers;
}

export async function ghJSON(
  url,
  { token, headers, what, notFound, init = {}, fetchImpl = globalThis.fetch } = {},
) {
  if (notFound !== NOT_FOUND_NULL && notFound !== NOT_FOUND_THROW) {
    throw new Error(
      `ghJSON(${url}): notFound must be ${NOT_FOUND_NULL} or ${NOT_FOUND_THROW}; ` +
        `a caller states what a missing resource means to it`,
    );
  }
  const method = init.method ?? 'GET';
  const res = await fetchImpl(url, {
    ...init,
    headers: { ...(token ? ghHeaders(token) : {}), ...(headers ?? {}), ...(init.headers ?? {}) },
  });
  if (res.status === HTTP_NOT_FOUND && notFound === NOT_FOUND_NULL) return null;
  if (!res.ok) {
    throw new Error(`${what ?? method}: HTTP ${res.status} ${res.statusText} for ${url}`);
  }
  if (res.status === HTTP_NO_CONTENT) return null;
  return res.json();
}

export function withPage(url, page, perPage = GITHUB_PER_PAGE) {
  const join = url.includes('?') ? '&' : '?';
  return `${url}${join}per_page=${perPage}&page=${page}`;
}

export async function ghPaginate({
  url,
  token,
  headers,
  what,
  pick = (body) => body,
  perPage = GITHUB_PER_PAGE,
  maxPages,
  fetchImpl = globalThis.fetch,
}) {
  const items = [];
  for (let page = 1; maxPages === undefined || page <= maxPages; page++) {
    const body = await ghJSON(withPage(url, page, perPage), {
      token,
      headers,
      what,
      notFound: NOT_FOUND_THROW,
      fetchImpl,
    });
    const batch = pick(body) ?? [];
    items.push(...batch);
    if (batch.length < perPage) return items;
  }
  throw new Error(
    `${what ?? url}: still returning full pages after ${maxPages} of them ` +
      `(${maxPages * perPage} items) — the result would be a silent prefix`,
  );
}
