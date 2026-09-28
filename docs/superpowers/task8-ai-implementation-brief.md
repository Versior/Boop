# Task 8 implementation brief — AI author status and writing assistant

## Role split

Pi implements this task. Codex/Jarvis owns this brief, verifies the result, performs an independent standards/spec review, and sends any required fixes back to Pi.

## Product boundary

- Keep the approved Boop/X-inspired visual language: plain borders, compact spacing, restrained blue accent, no gradients, glass, neon, chat dashboard, floating robot, or X branding/assets.
- AI is embedded in two places only: an author-status card in the home right rail (on mobile before the quick composer), and a compact owner-only assistant inside the existing article composer.
- Generation is request-driven. There is no scheduler, worker, queue, Redis, or second process.
- Only the owner publishes. Readers never gain write permissions.
- Preserve all current APIs and behavior. Follow `docs/PRODUCT.md`, `docs/API.md`, `docs/DATABASE.md`, and the Task 8 section of `docs/superpowers/plans/2026-09-28-boop-mvp.md`.

## Implementation constraints

- Reuse current settings, encrypted `ai.api_key`, `ai_cache`, auth/CSRF helpers, JSON helpers, logger, and token-bucket code.
- Go stdlib HTTP only; do not add an AI SDK, interface/factory, background service, migration, Node tooling, or speculative provider abstraction.
- Load AI settings and decrypt the API key at call time so admin changes take effect without restart.
- OpenAI-compatible `POST {base_url}/chat/completions`, safely joining the path whether or not the configured URL has a trailing slash.
- Explicit timeout no longer than 20 seconds, response cap 1 MiB, input/prompt cap, and a process-wide semaphore of 2 AI calls suitable for 1 core / 512 MiB.
- Errors and logs must never contain API keys, prompts, model output, or raw upstream bodies. Non-2xx and malformed replies return short stable errors.
- Model content must be one strict JSON object: reject unknown fields and trailing JSON.

## Author status

- Cache key: `author_status`; store a small JSON value such as `{text,topics}` in the existing `ai_cache` table. Plain text only. Bound text to 280 runes and topics to at most 5 short values.
- Source is a bounded snapshot of recent published, non-deleted posts (maximum 20 and about 24 KiB total). Track the newest published content `updated_at` as `source_updated_at`.
- On a home-page visit or `GET /api/v1/ai/author-status`:
  - Fresh cached value: return immediately.
  - Expired cache but no published content newer than `source_updated_at`: reuse it without an upstream call.
  - Expired cache with newer content: return the old value immediately and trigger one background refresh.
  - No cache: return a handwritten fallback immediately; if AI is configured and there is published content, trigger one background refresh.
- The upstream call must never delay home rendering. Detach the refresh from the request context but give it a timeout. Use one process-local single-flight guard; concurrent visits must not duplicate generation.
- On refresh failure retain the old value/fallback, store only a safe short `last_error`, and log no sensitive data.
- `POST /api/v1/admin/ai/author-status/regenerate` is owner-only, CSRF protected, AI-rate-limited, and performs an explicit refresh. If one is already running, return a stable conflict/busy response rather than spawning another call.
- `GET /api/v1/ai/author-status` is public and returns the effective status plus metadata needed by the UI (`text`, optional `topics`, `generated_at`, `stale`, `default`). It must not itself synchronously call the model.

## Writing assistant

- `POST /api/v1/admin/ai/assist` is owner-only, CSRF protected, AI-rate-limited, strict JSON, and limited to `summary`, `tags`, or `seo`.
- Accept bounded title/body/excerpt/tags input. Reject empty or excessive content before an upstream call.
- Strict result shapes and bounds: summary <= 300 runes; tags deduplicated case-insensitively, maximum 8, each <= 30 runes; SEO title <= 160 runes and description <= 300 runes.
- Suggestions never overwrite draft fields automatically.
- In the existing quick composer, show a compact sparkle/“AI 助手” control only for owner + article mode. Keep it inline, not a modal. Offer 摘要/标签/SEO actions and render the suggestion with an explicit “采用” action for summary and tags. SEO may be displayed with copy controls rather than extending the post persistence contract in this task.
- `POST /api/v1/admin/ai/test` is owner-only, CSRF protected and AI-rate-limited. It makes the smallest useful connectivity call and returns only a safe success/model indication.

## Server/UI wiring

- Register the four routes and preserve JSON 404/405 fallbacks:
  - `GET /api/v1/ai/author-status`
  - `POST /api/v1/admin/ai/test`
  - `POST /api/v1/admin/ai/author-status/regenerate`
  - `POST /api/v1/admin/ai/assist`
- Add the existing reserved AI limiter to `limiters`; key expensive owner calls by authenticated user and address in the existing style.
- Add the home-only author status data to the shared view with a zero/hidden state for other pages. Do not add database work to every page.
- Desktop: render the card above other future right-rail content, below search. Mobile: render the same status card before the quick composer. Use one shared template definition if possible.
- Add a small inline sparkle SVG symbol consistent with the existing stroke icons. Keep keyboard focus, live status, error text, and button disabled/loading states accessible.
- The home page and cached status must remain usable when AI is disabled, unconfigured, slow, invalid, or down.

## Tests and docs

- Add focused tests for: fresh/stale/no-cache/no-new-content states; concurrent single-flight; failed refresh preserving old data; timeout; response cap; non-2xx redaction; invalid/trailing/unknown JSON; disabled/unconfigured behavior; owner authorization/CSRF/rate limit; assistant bounds; public route; UI position/visibility; and no automatic composer overwrite.
- Update `docs/API.md`, `docs/DATABASE.md`, and `docs/PRODUCT.md` so implemented routes/cache behavior are no longer marked pending.
- Run: `gofmt -w .`, `go test ./...`, `go vet ./...`, `node --check web/static/app.js`, and `git diff --check`.
- Build with caches on G: `GOMODCACHE=G:\Go\pkg\mod`, `GOCACHE=G:\Go\cache`, output `G:\CodexCache\Boop\boop-task8-pi.exe`.
- Commit only after all checks pass with exact message: `feat: add resilient ai status and writing tools`.
- Do not push.

## Final report

Report the commit SHA, changed files, exact verification commands/results, binary path/size/SHA256, and any deliberate simplifications. Do not claim success if any gate failed.
