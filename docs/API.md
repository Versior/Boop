# Boop HTTP API v0.1

所有 JSON 接口位于 `/api/v1`。成功统一返回 `{"data":...}`；失败返回 `{"error":{"code":"...","message":"..."}}`。写接口要求 Session Cookie 和 `X-CSRF-Token`。列表使用游标 `cursor=<published_at,id>`，默认 20，最大 50；列表响应把下一游标作为 `data` 的兄弟字段返回：`{"data":[...],"next_cursor":"..."}`，最后一页为空字符串。内容写接口的 JSON 请求体上限为 512KiB（正文另有字段级上限），超限返回 413 `payload_too_large`。

## 公共与健康检查

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 进程存活，不访问外部 AI |
| GET | `/readyz` | SQLite 可读写迁移状态正常 |
| GET | `/api/v1/posts?type=&cursor=&limit=` | 已发布信息流；`type` 为 article/photo，空值含动态 |
| GET | `/api/v1/posts/{slug}` | 内容详情、点赞数、评论数和当前用户状态 |
| GET | `/api/v1/posts/{slug}/comments` | 仅已批准评论，按时间正序嵌套一级回复 |
| GET | `/api/v1/search?q=&cursor=&limit=` | SQLite FTS 关键词搜索，返回 `{"data":[...],"next_cursor":"..."}` |
| GET | `/api/v1/ai/author-status` | 作者状态缓存：text、topics、generated_at、stale、default；只读缓存，不同步调用模型 |
| GET | `/feed.xml` | RSS 2.0，最新 50 条已发布内容 |

## 搜索与 RSS

搜索规则（`GET /api/v1/search`，公开）：

- 查询词只取 Unicode 字母与数字，其余字符（引号、`*`、`(`、`)`、`-`、`^`、`:` 等 FTS5 语法字符）都是分隔符，会被丢弃；每个词各自加引号后再用 `AND` 连接，词序即输入顺序，大小写不敏感去重。因此输入永远无法注入或改变 FTS5 的查询结构，`OR`、`NEAR`、`NOT` 只会被当成普通词。
- 空白或只有标点的 `q` **不是错误**：直接返回 `{"data":[],"next_cursor":""}`，且不执行任何 `MATCH`（`MATCH ''`、`MATCH '!!!'` 在 SQLite 里是语法错误，服务端不会把它变成 500）。
- `q` 超过 100 个字符返回 400 `invalid_query`；`limit` 默认 20、最大 50，非法值返回 400 `invalid_limit`；游标非法返回 400 `invalid_cursor`。
- 只返回 `status='published'` 且 `deleted_at IS NULL` 的内容，按 `published_at DESC, id DESC` 稳定排序，游标沿用公共信息流的 `<published_at,id>` 格式（不接受评分游标），因此翻页不会重复或漏行；bm25 分数只作为服务端内部字段，不出现在响应里、也不参与排序。
- 结果字段是未来 AI 检索复用的最小集：`id`、`slug`、`type`、`title`、`excerpt`、`snippet`、`url`、`published_at`、`updated_at`。**不返回**正文、图片、标签、点赞与评论。
- `snippet` 是**纯文本**片段，取自索引里匹配最佳的那一列（`snippet(post_search, -1, …)`，即命中所在标题、正文或摘要），因此只在标题或摘要命中的结果同样能看到命中词被高亮，而不是一段与命中无关的正文开头；命中词由两个控制字符（U+0002 / U+0003）包裹。
- SSR 页面不把片段当 HTML：服务端只按这两个标记把片段切成**纯文本片段数组**，模板静态输出 `<mark>`，每段文本仍由 `html/template` 自动转义。即便存储正文里本来就有 U+0002 / U+0003，最坏结果只是多一对高亮，不可能注入 HTML 或破坏标签结构。
- `/api/v1/search` 子树永远是 JSON：未知子路径 404 `not_found`，错误方法 405 `method_not_allowed` 且带 `Allow: GET`。

RSS 规则（`GET /feed.xml`，公开）：

- 响应 `Content-Type: application/rss+xml; charset=utf-8`，文档为 RSS 2.0；`channel` 带站点名称、简介、语言 `zh-CN`，有内容时带 `lastBuildDate`。
- 只包含最新 50 条已发布且未删除的内容，顺序 `published_at DESC, id DESC`；不复用公共信息流的分页查询，而是一条只读 `slug`、`type`、`title`、`body_markdown`、`excerpt`、`published_at` 的窄查询，**不读** `body_html`、关联图片、标签、点赞与评论。
- 每条 item：标题（文章标题，否则正文第一条非空行，否则按类型回退）、绝对永久链接 `/p/{slug}`、与链接相同的 `guid`（`isPermaLink="true"`）、RFC1123Z 的 `pubDate`、纯文本 `description`（上限 300 字）。
- `description` 是**真正的纯文本**且自动 XML 转义：有摘要时用摘要（摘要本身按纯文本处理），否则渲染正文——文章正文经与存储 `body_html` 相同的 goldmark + bluemonday 流水线渲染后只取可见文本（标题号、`**粗体**` 标记、链接目标、代码围栏与原始 HTML 都不会出现），动态与摄影正文本身就是纯文本，只折叠为单行。**不输出 `body_html`，也不提供 `content:encoded`**。
- 绝对链接只由 `BOOP_BASE_URL` 生成，永远不读请求 Host 或转发头。
- 其它方法返回 405 且带 `Allow: GET`；XML 路径不返回 JSON 错误信封。

## 认证

| 方法 | 路径 | 输入 | 结果 |
|---|---|---|---|
| POST | `/api/v1/auth/register` | email,password,display_name | 创建 reader 并登录；注册关闭返回 403 |
| POST | `/api/v1/auth/login` | email,password | 创建 Session，返回用户和 CSRF token |
| POST | `/api/v1/auth/logout` | 无 | 删除当前 Session 并清 Cookie |
| GET | `/api/v1/auth/me` | 无 | 当前用户和 CSRF token；游客返回 401 |
| GET | `/auth/github/start` | 可选 return_to | 保存一次性 state 后跳转 GitHub |
| GET | `/auth/github/callback` | code,state | 登录/绑定后回到同源白名单路径 |

密码 10–72 字节；邮箱最大 254 字符；登录与注册按 IP 和邮箱限速。所有认证失败使用相同外部错误信息。认证接口的 JSON 请求体上限为 64KiB，与 `BOOP_MAX_UPLOAD_MB` 上传预算无关，超限返回 413 `payload_too_large`。

GitHub 登录规则：

- `GET /auth/github/start` 生成一次性 state（32 字节随机，10 分钟有效），把它同时放进 `boop_oauth` Cookie（HttpOnly、SameSite=Lax、Path=/、Secure 跟随 `BOOP_SECURE_COOKIES`）并写入跳转 query，然后 302 到 GitHub。`return_to` 只接受同源路径（形如 `/bookmarks`）；绝对 URL、`//host`、含反斜杠或换行的值一律退回 `/`。同时挂起的登录流程总数上限为 1000，超出返回 503 `oauth_busy`；本地址超出限速时返回 429。
- `GET /auth/github/callback` 要求 query 的 `state` 存在、未过期、未使用过，且与 Cookie 常量时间相等；state 无论成败都会被消费，重放一律 403 `oauth_state_invalid`。失败码：400 `oauth_code_missing`、400 `oauth_rejected`、502 `github_unavailable`、403 `email_unverified`、403 `registration_disabled`、403 `account_disabled`、409 `oauth_conflict`。
- 已绑定的 GitHub 账号直接登录；否则仅当 GitHub 报告该邮箱 `verified`（取自 `/user/emails`）时才允许绑定已有账户或创建新账户，未验证邮箱一律 403 `email_unverified`，不写任何行。
- 绑定 GitHub 账号时，若 `oauth_accounts(provider, provider_user_id)` 已被占用，会在**同一事务内重查**实际 `user_id`：只有它等于本次解析出的账户时才幂等成功；指向别的账户时整个登录 409 `oauth_conflict` 并回滚，绝不会给未绑定该身份的用户发 Session。
- GitHub 只能创建 `reader`；新账户不带密码哈希（`password_hash` 为 NULL），因此不能用密码登录。站点关闭公开注册后，已有账户仍可用密码或 GitHub 登录，也仍可把已验证邮箱绑定到已有账户；只有“创建新账户”会被 403 拒绝。
- GitHub 登录失败返回统一状态页（带 request_id），不输出来自 GitHub 的令牌、授权码或客户端密钥。

## 限速（单进程内存令牌桶）

| 动作 | 键 | 突发 | 持续补充 |
|---|---|---:|---|
| 登录 | 客户端地址、邮箱 | 10 | 每 6 秒 1 次 |
| 注册 | 客户端地址、邮箱 | 5 | 每 2 分钟 1 次 |
| 评论 | 登录用户 | 5 | 每 12 秒 1 次 |
| 评论 | 客户端地址 | 30 | 每 2 秒 1 次 |
| GitHub 登录（start 与 callback 各自计数） | 客户端地址 | 10 | 每 6 秒 1 次 |
| AI（test / regenerate / assist） | 站长账号、客户端地址 | 3 | 每 20 秒 1 次 |

- 超限返回 429，响应头带 `Retry-After`（秒），响应体为 `{"error":{"code":"rate_limited","message":"请求过于频繁，请稍后再试"},"request_id":"...","retry_after":N}`。
- 一次请求按调用方给定的顺序逐个消耗键，**第一个超限的键就立即返回 429**，不再消耗或创建其后的键。因此已被封禁的地址无法用不断更换的邮箱持续扩张内存，反向地，同一邮箱换地址刷也仍会被邮箱桶拦住。
- 限额保存在进程内存，重启即清空；**同一时刻只支持一个进程**，多进程部署会使实际额度成倍。避免 key 无限增长：每次请求前检查是否距上次清理已满 1 分钟（`sweepEvery`），满 1 分钟才做一次真正的清理，清掉已回满或超过 10 分钟（`idleTTL`）未使用的桶。因此清理是“请求驱动”的，不是独立定时器。
- 邮箱键是**归一化（去空格、转小写）后的邮箱的 SHA-256**，定长且不保存邮箱明文：未验证的输入无法通过超长邮箱放大内存。
- 地址取连接对端（`RemoteAddr`），不信任可伪造的转发头；除非反向代理把真实客户端地址作为对端地址传入，否则限速按代理地址聚合。
- AI 的三个站长接口（`/api/v1/admin/ai/test`、`/api/v1/admin/ai/author-status/regenerate`、`/api/v1/admin/ai/assist`）按**站长账号 + 客户端地址**计数：先消耗账号键，账号超额时不会创建或消耗其后的地址键。公开的 `GET /api/v1/ai/author-status` 只读缓存，不消耗 AI 额度。

## 互动

| 方法 | 路径 | 输入 | 权限 |
|---|---|---|---|
| GET | `/api/v1/posts/{slug}/comments` | 无 | 公开 |
| POST | `/api/v1/posts/{id}/comments` | body,parent_id? | reader/owner |
| DELETE | `/api/v1/comments/{id}` | 无 | 作者或 owner |
| PUT | `/api/v1/posts/{id}/like` | 无 | reader/owner；幂等设为已点赞 |
| DELETE | `/api/v1/posts/{id}/like` | 无 | reader/owner；幂等取消 |
| PUT | `/api/v1/posts/{id}/bookmark` | 无 | reader/owner |
| DELETE | `/api/v1/posts/{id}/bookmark` | 无 | reader/owner |
| GET | `/api/v1/me/bookmarks?cursor=` | 无 | reader/owner |

评论与互动规则：

- 评论正文为纯文本（不解析 Markdown），去除首尾空白后长度需为 1–2000 字，否则 400 `invalid_body`。
- 只允许一级回复（最大深度 2，`docs/PRODUCT.md` §5.3）。`parent_id` 必须是同一文章下、本身没有父级、且当前公开可见的评论；否则 400 `invalid_parent`（跨文章、未知 id、二级回复、以及回复待审核/已拒绝/已删除的父评论都是这一个错误码）。
- 只能评论已发布且未删除的内容；草稿、归档与已删除内容一律 404。
- `comments.enabled=false` 时写入返回 403 `comments_disabled`；已有公开评论仍可读取。`comments.moderation_enabled=true` 时读者的新评论为 `pending`，站长自己的评论始终 `approved`；关闭审核时均为 `approved`。
- 公开列表只返回 `approved` 且未删除的评论，按创建时间正序，每个顶层评论带 `replies`。父评论被删除或被拒绝时，它的回复不会出现在公开列表中（即使回复本身仍是 `approved`）。
- `DELETE /api/v1/comments/{id}` 是软删除：作者或站长可删，其它账号 403 `forbidden`，未知或已删除的评论 404，重复删除仍是 404。
- 评论写接口的 JSON 请求体上限为 64KiB（正文另有 2000 字上限），超限返回 413 `payload_too_large`。
- 评论响应字段：`id`、`post_id`、`parent_id`、`body`、`status`、`created_at`、`author{id,display_name,avatar_url}`、`mine`、`replies[]`；审核队列额外带 `post_title`/`post_slug`。绝不返回作者邮箱或角色。
- 点赞与收藏都是幂等的：重复 PUT/DELETE 返回同样的最终状态。响应为 `{"post_id":N,"liked":bool,"like_count":N}` 与 `{"post_id":N,"bookmarked":bool,"bookmark_count":N}`；`like_count` 是该内容的公开点赞数，`bookmark_count` 是**当前用户**的收藏总数（收藏是私有的，不公开他人数据）。
- 只能对已发布未删除内容点赞或收藏，否则 404。
- `GET /api/v1/me/bookmarks` 用 `cursor=<created_at>,<post_id>` 稳定分页，默认 20、最大 50，返回 `{"data":[...],"next_cursor":"..."}`；只列出仍公开的内容，按收藏时间倒序。
- 内容详情与信息流（SSR 与 JSON）都带当前登录用户的 `liked`、`bookmarked`；游客一律为 `false`。

## 站长内容管理

| 方法 | 路径 | 输入/说明 |
|---|---|---|
| POST | `/api/v1/admin/posts` | type,status,title,body,excerpt,asset_ids,tags,location,captured_at |
| PATCH | `/api/v1/admin/posts/{id}` | 上述字段的部分更新，带 `updated_at` 乐观锁 |
| DELETE | `/api/v1/admin/posts/{id}` | 软删除 |
| POST | `/api/v1/admin/uploads` | multipart 单文件，字段名 `file`；jpg/png/webp/gif，默认最大 10MB |
| GET | `/api/v1/admin/comments?status=pending` | 审核队列；`status` 为 pending/approved/rejected，默认 pending，`limit` 默认 20、最大 50 |
| POST | `/api/v1/admin/comments/{id}/approve` | 批准；重复调用幂等，未知或已删除评论 404 |
| POST | `/api/v1/admin/comments/{id}/reject` | 拒绝；幂等同上 |
| DELETE | `/api/v1/admin/comments/{id}` | 管理删除（软删除）；重复删除 404 |

上传与图片服务：

- 文件类型由字节推断（`http.DetectContentType`），不信任客户端 MIME：扩展名与内容不符返回 400 `invalid_filename`，内容不是 jpg/png/webp/gif 返回 415 `unsupported_media_type`。
- `BOOP_MAX_UPLOAD_MB`（默认 10MB）是**单文件上限**：文件本身超过该上限返回 413 `payload_too_large`，恰好等于上限的文件可以上传。请求体总上限是该上限加上固定的 multipart 封装预算（64KiB，用于 boundary 与分段头），请求体超出同样返回 413 `payload_too_large`。
- 必须恰好一个文件字段，字段名必须是 `file`，否则返回 400 `invalid_body`；其它非文件表单字段会被忽略。缺少文件、多个文件或非 multipart 请求同样返回 400 `invalid_body`。
- 新文件返回 201；相同字节且属于同一站长时返回 200 且 `reused=true`，复用既有 asset 与文件。响应字段：`id`、`url`、`storage_key`、`mime_type`、`size_bytes`、`width`、`height`、`original_name`、`reused`。
- 客户端文件名只作为 `original_name` 元数据保存，绝不进入路径；`storage_key` 由服务端生成为 `YYYY/MM/<32 位随机十六进制>.<ext>`。
- `asset_ids` 必须是当前站长上传过的图片资源：不存在、不属于自己或不是图片 MIME 均返回 400 `invalid_asset`；发布 `photo` 至少需要一张这样的图片，否则返回 400 `invalid_assets`。
- `GET /uploads/{storage_key}` 公开只读、无需登录；键形状不符、目录穿越或文件不存在均返回 404，响应带 `Content-Type`、`nosniff` 与 `Cache-Control: public, max-age=31536000, immutable`。

## 设置与 AI

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/admin/settings` | 返回非密钥设置及密钥是否已配置 |
| PATCH | `/api/v1/admin/settings` | 白名单字段更新；空密钥表示保持不变，显式 `clear_secret` 才删除 |
| POST | `/api/v1/admin/ai/test` | 最小请求验证配置，成功只返回 ok 与配置的模型名，不回显密钥或模型输出 |
| POST | `/api/v1/admin/ai/author-status/regenerate` | 手动刷新作者状态；同一时刻只能有一个刷新在跑，重复请求 409 `ai_busy` |
| POST | `/api/v1/admin/ai/assist` | action=summary/tags/seo，输入草稿内容；只返回建议，不写入草稿 |
| POST | `/api/v1/admin/ai/chat` | v0.2 站内问答，SSE 输出（未实现） |

设置接口规则：

- 两个接口都只允许 owner：游客 401 `unauthorized`，读者 403 `forbidden`。
- `GET` 返回 `site_name`、`site_description`、`site_avatar_url`、`site_timezone`、`page_size`、`registration_enabled`、`comments_enabled`、`comments_moderation_enabled`、`ai_enabled`、`ai_base_url`、`ai_chat_model`、`ai_embedding_model`、`ai_author_status_ttl_hours`，以及 `github_client_id_set`、`github_client_secret_set`、`ai_api_key_set`、`master_key` 四个布尔标志。**任何密钥明文都不会出现在响应里**，页面只知道某个密钥是否已配置。
- `PATCH` 只接受上述字段加上三个密钥字段（`github_client_id`、`github_client_secret`、`ai_api_key`）和 `clear_secret`；上传目录、单文件大小与允许的 MIME 类型只能由环境变量配置，请求里出现即 400 `invalid_body`。
- `site_avatar_url` 允许为空（表示不配置站点头像），非空时必须是**无凭据的 http/https 绝对 URL**，且不超过 2048 个字符；相对路径、其它协议、带 `user:pass@` 的值一律 400 `invalid_settings`。
- 密钥字段为空字符串表示**保持不变**（不会清空）；删除必须显式列出密钥名，例如 `{"clear_secret":["github.client_secret"]}`，删除不存在的密钥是幂等的；`clear_secret` 里的未知名返回 400 `invalid_settings`。
- 值校验沿用 `internal/settings` 的类型与范围规则：非法值返回 400 `invalid_settings`，且**整次更新失败**——校验先于事务，不会出现部分字段已写入的状态。
- 密钥用 `BOOP_MASTER_KEY` 的 AES-256-GCM 加密后存入 `secret_settings`，每次写入生成新的 nonce；**密文同时以它自己的 settings key 作为 GCM additional data 绑定**，因此把某条密文换到另一个 key（或另一个 setting）上都无法解密，只会得到认证失败，绝不会返回明文。
- 写入或替换密钥需要 `BOOP_MASTER_KEY`，未配置时返回 409 `master_key_required`（非密钥字段仍可修改）；**删除密钥只是删一行，不需要解密，因此在没有 `BOOP_MASTER_KEY` 时也能幂等清除**。同一次请求里既有新密钥又有 `clear_secret` 时按“写入”处理，整单 409。
- 设置请求体上限为 64KiB，超限返回 413 `payload_too_large`。
- 设置表损坏时 `GET` 与设置页返回 500，而不是用默认值渲染表单（避免把默认值保存回去覆盖真实设置）。

AI 接口规则：

- 三个站长接口都只允许 owner 并要求 CSRF：游客 401 `unauthorized`，读者 403 `forbidden`，缺少 `X-CSRF-Token` 403 `csrf_invalid`，超限 429。
- `GET /api/v1/ai/author-status` 是公开接口，返回 `{"data":{"text":"...","topics":["..."],"generated_at":"...","stale":false,"default":false}}`。`text` 是不超过 280 字的纯文本；`topics` 最多 5 个、每个不超过 24 字，可以为空；`default=true` 表示这是手写兑底文案（还没有生成结果）；`stale=true` 表示缓存已过期（可能还有内容更新待处理）。该接口只读缓存，**不在请求内调用模型**。
- 缓存是 `ai_cache` 中唯一一行 `cache_key='author_status'`，值形如 `{"text":"...","topics":["..."]}`；`source_updated_at` 是生成时最新已发布内容的 `updated_at`（与当前值按时间比较，不按字符串比较），`expires_at` 由 `ai.author_status_ttl_hours`（默认 168 小时）决定。
- 访问首页或该接口时的读取顺序：缓存仍新鲜则直接返回；缓存过期但**没有**比 `source_updated_at` 更新的已发布内容（内容没变，或删除/归档让这个最大值回退）时复用旧值且不调用模型；缓存过期且最新内容**严格更新**时立即返回旧值并在后台单飞刷新；完全没有缓存时立即返回手写兑底文案，并在有已发布内容时后台刷新。时间戳无法解析时不能证明内容没变，按“可能更新”保守刷新。
- 刷新永不阻塞首页渲染：它从请求上下文分离，但有 30 秒上限；全进程同一时刻只允许一个刷新（单飞），并发访问不会重复生成。
- `POST /api/v1/admin/ai/author-status/regenerate` 是站长显式刷新，成功返回与公开接口相同的 `data`。已有刷新在跑时返回 409 `ai_busy`，不会启动第二个调用；站点从未发布内容时返回 409 `ai_no_content`。
- 刷新失败保留旧值（或兑底文案），只在 `ai_cache.last_error` 写入一个稳定短码（`timeout`、`upstream`、`invalid_reply` 等）：已有可渲染值时**不覆盖** `value_json`、`source_updated_at`、`generated_at` 与 `expires_at`，只写短码；尚无任何可渲染值时写入一行最小失败占位行（`generated_at` 为空），它只带短码与固定的 5 分钟退避，因此首次失败也能被观察到、也仍然渲染手写兑底文案，且退避期内不会每次访问都打上游。日志只记录短码与上游状态码，**绝不记录 API Key、提示词、模型输出或上游响应体**；非 2xx、超时与响应超限都返回短而稳定的错误码。
- `POST /api/v1/admin/ai/assist` 只接受 `action=summary|tags|seo` 与 `title`、`body`、`excerpt`、`tags` 四个草稿字段，未知字段一律 400 `invalid_body`（因此请求无法指向任何已保存内容）。标题、正文、摘要与标签全为空，或总字节数超过 16KiB，会在调用模型之前返回 400 `invalid_body`；未知 `action` 返回 400 `invalid_action`。
- 助手响应分别为 `{"data":{"action":"summary","summary":"..."}}`、`{"data":{"action":"tags","tags":["..."]}}` 与 `{"data":{"action":"seo","seo_title":"...","seo_description":"..."}}`；上限是摘要 300 字、标签 8 个且每个 30 字（大小写不敏感去重）、SEO 标题 160 字、描述 300 字。**建议不会写入草稿**：只有站长在前端点“采用”时才会填入摘要或标签，SEO 只提供复制。
- 首页快捷发布器里的 AI 助手控件只在站点启用 AI、Base URL 与对话模型非空，且 `ai.api_key` **能真正解密且非空**时渲染（与 GitHub 登录入口同一条规则）：缺少 `BOOP_MASTER_KEY` 或密文损坏时隐藏控件并写脱敏告警，而不是渲染一个必然失败的按钮；设置页仍会把 `ai_api_key_set` 报为已配置。
- `POST /api/v1/admin/ai/test` 只做一次最小对话请求，成功返回 `{"data":{"ok":true,"model":"<配置的模型名>"}}`，不回显模型输出或任何密钥。
- 模型回复必须是**一个**严格 JSON 对象：未知字段与尾随 JSON 一律按无法解析处理。失败码映射：未启用 409 `ai_disabled`，缺少 Base URL、模型或 API Key（含缺少或换错 `BOOP_MASTER_KEY`、密文损坏）409 `ai_unconfigured`，超时 504 `ai_timeout`，非 2xx 502 `ai_upstream_error`，无法解析（含响应体超过 1MiB）502 `ai_invalid_reply`。AI 不可用时首页与缓存状态仍然可用。
- AI 调用边界：单次 20 秒超时、单个响应最多 1MiB、提示词最多 48KiB，且全进程同时最多 2 个上游调用（适配 1 核 / 512MiB）。设置与密钥在每次调用时重新读取并解密，改动无需重启。

## HTML 页面

- `GET /` 首页 SSR。
- `GET /p/{slug}` 内容详情 SSR。
- `GET /search?q=&cursor=` 搜索结果 SSR：空查询是提示态，无结果（含只有标点的查询）是带转义查询词的空态，翻页用同源“加载更多”链接并保留 `q`；左栏“搜索”项在 `/search` 高亮，移动端底部导航不变。有结果时文案是“本页 N 条”，因为服务端没有 COUNT 查询、`len` 只是本页数量，不冒充总数。
- 每个页面的 `<head>` 都带 `<link rel="alternate" type="application/rss+xml" href="/feed.xml">` 发现链接。
- `GET /login`、`GET /register`。
- `GET /bookmarks` 登录用户收藏；游客重定向到 `/login`。
- `GET /login` 与 `GET /register` 在 GitHub 客户端 ID 与 Secret **都能解密且非空**时显示同一个 `/auth/github/start` 登录入口（注册页在关闭公开注册后仍然显示，供已绑定的账号登录）；未配置 `BOOP_MASTER_KEY`、缺少任一半或密文损坏（换了主密钥）时隐藏入口并写脱敏告警，`/auth/github/start` 也随之安全失败，不会发出任何 Session。该判断每次都实际解密，不做缓存。
- `GET /admin` 站长管理入口，303 跳转到 `/admin/settings`；游客重定向到 `/login`，普通读者得到 403 HTML 页。
- `GET /admin/comments`、`GET /admin/settings` 站长页面；两者的游客都重定向到 `/login`，普通读者得到 403 HTML 页。
- 未知页面返回带 request_id 的统一 404；API 永远不返回 HTML 错误页。

品牌与头像：

- `site_name`、`site_description`、`site_avatar_url` 注入所有页面的外壳：标题后缀、桌面与移动品牌名、品牌 `aria-label`、搜索框标签与占位符、`meta description`，以及首页可见文案（`sr-only` 标题与底部说明）。页面外壳不再硬编码任何品牌名。
- 渲染头像的优先级是 `site_avatar_url` → 站长账号 `users.avatar_url` → 内置 SVG，用于快捷发布、信息流卡片、收藏卡片与内容详情；`site_avatar_url` 同时作为页面 `<link rel="icon">`，为空时不输出。CSP 的 `img-src` 为 `'self' data: http: https:`，否则配置的绝对头像地址会被浏览器直接拦掉；脚本、样式与连接仍然是同源（`script-src 'self'`、`connect-src 'self'`）。
- 展示路径读取设置失败时回退到默认值并写一条告警（页面仍然可用）；而设置表单和写入路径在设置损坏时返回 500 / 4xx，避免把默认值写回去覆盖真实设置。
- 首页、内容详情、收藏页在已经读过设置时复用同一次读取（只注入外壳），不会为品牌再查一次库。

