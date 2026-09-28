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
| GET | `/api/v1/search?q=&cursor=` | SQLite FTS 搜索 |
| GET | `/api/v1/ai/author-status` | 返回缓存状态、generated_at、stale 标志 |
| GET | `/feed.xml` | RSS 2.0 |

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

## 限速（单进程内存令牌桶）

| 动作 | 键 | 突发 | 持续补充 |
|---|---|---:|---|
| 登录 | 客户端地址、邮箱 | 10 | 每 6 秒 1 次 |
| 注册 | 客户端地址、邮箱 | 5 | 每 2 分钟 1 次 |
| 评论 | 登录用户 | 5 | 每 12 秒 1 次 |
| 评论 | 客户端地址 | 30 | 每 2 秒 1 次 |

- 超限返回 429，响应头带 `Retry-After`（秒），响应体为 `{"error":{"code":"rate_limited","message":"请求过于频繁，请稍后再试"},"request_id":"...","retry_after":N}`。
- 一次请求按调用方给定的顺序逐个消耗键，**第一个超限的键就立即返回 429**，不再消耗或创建其后的键。因此已被封禁的地址无法用不断更换的邮箱持续扩张内存，反向地，同一邮箱换地址刷也仍会被邮箱桶拦住。
- 限额保存在进程内存，重启即清空；**同一时刻只支持一个进程**，多进程部署会使实际额度成倍。避免 key 无限增长：每次请求前检查是否距上次清理已满 1 分钟（`sweepEvery`），满 1 分钟才做一次真正的清理，清掉已回满或超过 10 分钟（`idleTTL`）未使用的桶。因此清理是“请求驱动”的，不是独立定时器。
- 邮箱键是**归一化（去空格、转小写）后的邮箱的 SHA-256**，定长且不保存邮箱明文：未验证的输入无法通过超长邮箱放大内存。
- 地址取连接对端（`RemoteAddr`），不信任可伪造的转发头；除非反向代理把真实客户端地址作为对端地址传入，否则限速按代理地址聚合。
- AI 接口（Task 8 未实现）预留额度：突发 3、每 20 秒 1 次；目前没有任何路由使用它。

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
| PATCH | `/api/v1/admin/settings` | 白名单字段更新；空密钥表示保持不变，显式 `clear_secret=true` 才删除 |
| POST | `/api/v1/admin/ai/test` | 最小请求验证配置，不回显密钥 |
| POST | `/api/v1/admin/ai/author-status/regenerate` | 手动刷新作者状态，单飞 |
| POST | `/api/v1/admin/ai/assist` | action=summary/tags/seo，输入草稿内容 |
| POST | `/api/v1/admin/ai/chat` | v0.2 站内问答，SSE 输出 |

## HTML 页面

- `GET /` 首页 SSR。
- `GET /p/{slug}` 内容详情 SSR。
- `GET /search?q=` 搜索结果 SSR。
- `GET /login`、`GET /register`。
- `GET /bookmarks` 登录用户收藏；游客重定向到 `/login`。
- `GET /admin`（Task 7）、`GET /admin/comments`、`GET /admin/settings`（Task 7）站长页面；`/admin/comments` 的游客重定向到 `/login`，普通读者得到 403 HTML 页。
- 未知页面返回带 request_id 的统一 404；API 永远不返回 HTML 错误页。

