# Task 9 implementation brief — search and RSS

## Role split

Pi implements this task. Codex/Jarvis owns the constraints, performs independent verification and two-axis review, and sends required fixes back to Pi.

## Product and visual boundary

- Keep Boop's approved X/Twitter-inspired visual language: compact information density, hairline borders, clear type hierarchy, restrained blue focus/accent, no gradients, glass, oversized hero, card grid, or copied X branding/assets.
- `/search` should feel like an X search/results view inside the existing three-column shell: a prominent but compact search row at the top of the center column, query/result count context, and one continuous result stream separated by borders.
- On mobile the search control remains at the top of the center column. Keep the existing fixed bottom navigation and 44px minimum interactive targets.
- Search and RSS are public. Do not add semantic search, embeddings, vector tables, AI calls, a search service, a provider abstraction, or a new dependency.

## Search domain

- Create concrete `internal/search.Result`, `Page`, and `Options` values plus a concrete SQLite query function. This is the future v0.2 retrieval seam; do not add an interface for one implementation.
- Query the existing FTS5 `post_search` table and join `posts`. Return only `status='published'` and `deleted_at IS NULL`.
- Normalize the query into bounded Unicode letter/number tokens, safely quote every token for FTS5, and join them deterministically. Empty or punctuation-only input returns an empty page without executing a malformed `MATCH` expression. Cap the public query at 100 runes and reject overlong values with stable `invalid_query`.
- Use a useful FTS snippet with deterministic highlight markers, but keep `Result.Snippet` plain/safe for API and later AI retrieval. Perform HTML escaping before converting only trusted internal markers to `<mark>` in the SSR view; never trust FTS/source text as HTML.
- Stable pagination should use the existing `<published_at>,<id>` cursor and order matching results by `published_at DESC, id DESC`. Reuse `content.EncodeCursor` / `DecodeCursor`; do not invent a score cursor. Default limit 20, maximum 50. Ranking can remain an internal numeric field for future use but must not destabilize pagination.
- Result fields should be the minimum reusable retrieval shape: post id/slug/type/title/excerpt/snippet/published_at/updated_at and optional rank. Do not load full post bodies, assets, tags, reactions, or comments for the search stream.

## HTTP and X-style SSR

- Add `GET /api/v1/search?q=&cursor=&limit=` with the standard `{data,next_cursor}` envelope. Unknown route and wrong method responses under `/api/v1/search` must stay JSON with correct 404/405 and `Allow: GET`.
- Add `GET /search?q=&cursor=` SSR and register `search.html` in the template set.
- Search page behavior:
  - Blank query: show a quiet prompt, not an error.
  - Punctuation-only query: show the same zero-result state and never leak SQLite syntax errors.
  - Query with matches: show compact rows with type chip, title or first useful line, escaped/highlighted snippet, relative/site-timezone date and link to `/p/{slug}`.
  - No matches: show a concise X-like empty state that includes the escaped query.
  - Pagination uses a normal same-origin “加载更多” link preserving `q` with `url.Values`, never string concatenation.
- Make the left “搜索” nav item active on `/search`; use one explicit page filter value such as `search` and update base nav condition. Do not add a second mobile navigation item.
- The right-rail search form remains. On the search page it should reflect the current query if feasible without adding JS state; the center search form is authoritative.
- Add the RSS discovery link to `<head>`: `<link rel="alternate" type="application/rss+xml" ... href="/feed.xml">`.

## RSS 2.0

- Add `GET /feed.xml`, newest 50 published/non-deleted posts, ordered `published_at DESC, id DESC`.
- Generate valid RSS 2.0 with Go `encoding/xml` or an equally small stdlib approach. Use absolute links built from configured `BOOP_BASE_URL`; never use request Host or forwarded headers.
- Channel: configured site name/description, absolute site link, language `zh-CN`, lastBuildDate when items exist.
- Item: deterministic title (article title, otherwise bounded first non-empty line with a type fallback), absolute permalink, GUID using the same permalink, RFC1123Z pubDate, and plain-text bounded description from excerpt/body. XML escaping must be automatic; do not emit stored `body_html` or add `content:encoded` in v0.1.
- Return `Content-Type: application/rss+xml; charset=utf-8`. Wrong methods should return 405 with `Allow: GET`; no JSON envelope is required for the XML path.
- No database migration: reuse `posts`, existing indexes and the existing FTS table.

## Testing and docs

- Focused domain tests: FTS escaping, quote/operator injection, punctuation-only and blank query, Unicode/CJK, max query/limit/cursor errors, stable pagination without duplicates, and draft/archived/deleted exclusion.
- Server tests: SSR escaping/highlight safety, active nav, empty/no-result states, query-preserving pagination, JSON envelope/fallbacks, RSS XML validity, absolute links from `BOOP_BASE_URL`, XML escaping, newest-50 bound, draft/deleted exclusion, and content type/method behavior.
- Update `docs/API.md`, `docs/PRODUCT.md`, and `docs/DATABASE.md` with the implemented search/RSS semantics. Do not describe semantic/vector search as implemented.
- Run `gofmt -w .`, `go test ./... -count=1`, `go vet ./...`, `node --check web/static/app.js`, and `git diff --check`.
- Build with caches on G: `GOMODCACHE=G:\Go\pkg\mod`, `GOCACHE=G:\Go\cache`, output `G:\CodexCache\Boop\boop-task9-pi.exe`.
- Commit only after every gate passes with exact message: `feat: add search and rss`.
- Do not push.

## Final report

Report commit SHA, changed files, exact verification results, binary path/size/SHA256, and deliberate simplifications. Never claim a gate passed unless its command exited successfully.
