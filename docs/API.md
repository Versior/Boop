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
| GET | `/robots.txt` | 抓取规则与 sitemap 声明，绝对地址只由 `BOOP_BASE_URL` 生成 |
| GET | `/sitemap.xml` | 首页 + **每一条**已发布内容，逐条带 `lastmod` |
| GET | `/static/*` | 嵌入的 CSS/JS/SVG；带内容版本号，一年不可变缓存 + gzip 协商 |

## 搜索与 RSS

搜索规则（`GET /api/v1/search`，公开）：

- 查询词只取 Unicode 字母与数字，其余字符（引号、`*`、`(`、`)`、`-`、`^`、`:` 等 FTS5 语法字符）都是分隔符，会被丢弃；每个词各自加引号后再用 `AND` 连接，词序即输入顺序，大小写不敏感去重。因此输入永远无法注入或改变 FTS5 的查询结构，`OR`、`NEAR`、`NOT` 只会被当成普通词。
- 空白或只有标点的 `q` **不是错误**：直接返回 `{"data":[],"next_cursor":""}`，且不执行任何 `MATCH`（`MATCH ''`、`MATCH '!!!'` 在 SQLite 里是语法错误，服务端不会把它变成 500）。
- **检索有两条路径，由查询词的字符集决定。** FTS5 的 `unicode61` 分词器把一整段连续的 CJK（汉字、假名、谚文）当成**一个 token**，所以它匹配不到出现在「一段待评论的动态」里的「评论」——中文本来就不分词写。因此含 CJK 的查询改走**子串检索**：在 `posts` 的 `title` / `excerpt` / `body_markdown` 三列上按 `LIKE '%词%'` 匹配，每个词都必须出现（与 FTS 路径的 `AND` 等价）。中国用户绝大多数查询都含中文，所以这是主要的检索路径，不是兜底。
- 不含 CJK 的查询仍走 FTS5 索引，语义没有变：`Google` 能命中，`oogle` 不能。**子串检索的代价说清楚**：它要读已发布行（实测计划为 `SEARCH p USING INDEX idx_posts_feed (status=?)`，不退化成临时 B 树排序），没有 bm25 分数，混排查询里的拉丁词也按子串匹配；命中稀疏到走不完一页时，成本随语料增长（实测数字见 `docs/DATABASE.md`）。语料规模大到这一步不再划算时，修法是给 FTS 索引加 CJK 二元组（bigram）分词。
- 两条路径共用同一套游标、排序与页大小，`next_cursor` 的语义与翻页行为完全一致：翻页不会重复或漏行。相邻的中文不拆词，整串当一个词查（「今天评论」找的是这个字面串，不会拆成「今天」+「评论」）；要用两个字面串做「与」，在它们之间加空格或标点即可。
- `q` 超过 100 个字符返回 400 `invalid_query`；`limit` 默认 20、最大 50，非法值返回 400 `invalid_limit`；游标非法返回 400 `invalid_cursor`。
- 只返回 `status='published'` 且 `deleted_at IS NULL` 的内容，按 `published_at DESC, id DESC` 稳定排序，游标沿用公共信息流的 `<published_at,id>` 格式（不接受评分游标），因此翻页不会重复或漏行；bm25 分数只作为服务端内部字段，不出现在响应里、也不参与排序。
- 结果字段是未来 AI 检索复用的最小集：`id`、`slug`、`type`、`title`、`excerpt`、`snippet`、`url`、`published_at`、`updated_at`。**不返回**正文、图片、标签、点赞与评论。`slug` 是存储的原文标识符（`Slugify` 保留 CJK），`url` 是 `/p/{slug}` 并把 slug 按**单段路径**百分号编码后的地址——字段名说的是地址，地址就必须是转义后的 URI，而这份 JSON 不经过 `html/template`，没有任何一层会替你转义。`/feed.xml` 与 `/sitemap.xml` 用同一条规则（那里是绝对地址）。
- `snippet` 是**纯文本**片段，取自匹配最佳的那一列（标题、正文或摘要），因此只在标题或摘要命中的结果同样能看到命中词被高亮，而不是一段与命中无关的正文开头；命中词由两个控制字符（U+0002 / U+0003）包裹。FTS 路径用 `snippet(post_search, -1, …)`，子串路径在应用层按**同样的列优先级**取列并插入**同样的标记**，窗口为命中前后共 80 个字符，被截断的一侧以 `…` 收尾，两条路径的输出随后都被同一个 240 字符上限收口。
- SSR 页面不把片段当 HTML：服务端只按这两个标记把片段切成**纯文本片段数组**，模板静态输出 `<mark>`，每段文本仍由 `html/template` 自动转义。即便存储正文里本来就有 U+0002 / U+0003，最坏结果只是多一对高亮，不可能注入 HTML 或破坏标签结构。
- `/api/v1/search` 子树永远是 JSON：未知子路径 404 `not_found`，错误方法 405 `method_not_allowed` 且带 `Allow: GET`。

RSS 规则（`GET /feed.xml`，公开）：

- 响应 `Content-Type: application/rss+xml; charset=utf-8`，文档为 RSS 2.0；`channel` 带站点名称、简介、语言 `zh-CN`，有内容时带 `lastBuildDate`。
- 只包含最新 50 条已发布且未删除的内容，顺序 `published_at DESC, id DESC`；不复用公共信息流的分页查询，而是一条只读 `slug`、`type`、`title`、`body_markdown`、`excerpt`、`published_at` 的窄查询，**不读** `body_html`、关联图片、标签、点赞与评论。物化顺序与接口契约不受影响：已发布行一定有非空 `published_at`，查询直接读这一列并直接按它排序（不套 `COALESCE`），这样排序能直接由 `idx_posts_feed` 满足，不再对已发布行做临时 B-tree 排序。
- 每条 item：标题（文章标题，否则正文第一条非空行，否则按类型回退）、绝对永久链接 `/p/{slug}`、与链接相同的 `guid`（`isPermaLink="true"`）、RFC1123Z 的 `pubDate`、纯文本 `description`（上限 300 字）。
- `description` 是**真正的纯文本**且自动 XML 转义：有摘要时用摘要（摘要本身按纯文本处理），否则渲染正文——文章正文经与存储 `body_html` 相同的 goldmark + bluemonday 流水线渲染后只取可见文本（标题号、`**粗体**` 标记、链接目标、代码围栏与原始 HTML 都不会出现），动态与摄影正文本身就是纯文本，只折叠为单行。**不输出 `body_html`，也不提供 `content:encoded`**。
- 绝对链接只由 `BOOP_BASE_URL` 生成，永远不读请求 Host 或转发头。
- 其它方法返回 405 且带 `Allow: GET`；XML 路径不返回 JSON 错误信封。

## 抓取与发现文档

`GET /robots.txt` 与 `GET /sitemap.xml` 是给爬虫的纯文本与 XML 文档，都不返回 JSON 信封。

`robots.txt` 规则：

- 输出 `User-agent: *`、`Allow: /` 和一组 `Disallow`：`/admin`、`/api/`、`/auth/`、`/search`、`/bookmarks`、`/login`、`/register`。规则按最长前缀匹配，所以一条 `Allow: /` 就让其余路径都可抓，只有这几条留在外面。
- `/search` 被排除，是因为它的结果集随查询串无限展开，且每次抓取都要跑一次全文检索；`/bookmarks` 是逐访客的页面，对爬虫没有意义。
- `Sitemap:` 必须是绝对地址，只由 `BOOP_BASE_URL` 生成，**永不**读请求 Host 或 `X-Forwarded-*`：否则任何能发请求的人都能让本进程向所有爬虫宣告别人的源站。

`sitemap.xml` 规则：

- 文档为 sitemap 0.9，`<urlset>` 带 `xmlns`，内容是**首页 + 每一条已发布且未删除的内容**，契约与公共信息流一致（`status='published' AND deleted_at IS NULL`）。只列首页而没有内容的 sitemap 比没有更糟：信任它的爬虫永远不会知道内容存在。
- 查询是一条只读 `slug, updated_at` 的窄查询，按 `published_at DESC, id DESC` 排序——与 RSS 用同一个免 `COALESCE` 的顺序，直接由 `idx_posts_feed` 满足，不产生临时 B-tree。单份 sitemap 最多 50000 条 URL（协议上限），所以这是一个有界查询。
- 每条内容带自己的 `lastmod`（取 `updated_at`，UTC 的 W3C datetime），因此编辑过的内容会被重新抓取，而不必让整份文件看起来全新。
- 首页条目**不带** `lastmod`：首页只渲染最新一页内容，没有任何单个存储时间戳能描述它何时变化，随便填一个都是过度或不足声明。
- `/?type=article` 与 `/?type=photo` 这两个筛选视图**不**单独列入：它们是首页的子集，而且从首页一次点击就能到达，列进去只会给爬虫增加与首页高度重复的 URL。
- 时间戳无法解析时**省略** `lastmod` 元素，而不是输出空元素——空元素会让文档非法。
- `<loc>` 里的 slug 已按**单段路径**百分号编码（`url.PathEscape`）。slug 存的是原文，`Slugify` 保留 CJK，所以中文标题本来就是中文路径段；而 `encoding/xml` 只转义五个 XML 实体、其余原样输出，因此这里必须自己编码——协议要求 `<loc>` 是转义后的 URI。编码结果与详情页自己的 `<link rel="canonical">` **逐字节相同**（两者都走请求路径的转义形式），爬虫顺着 `<loc>` 抓到的页面声明的正是同一个地址。ASCII slug 不受影响（同一规则：`/p/plain-slug` 保持原样）。`/feed.xml` 的 `<link>` 与 `isPermaLink="true"` 的 `<guid>` 走同一个构造函数。
- 两个路径的非 GET 请求都被本进程回答为 405 并带 `Allow: GET`；但不带 Origin/Referer 的写请求会先被同源守卫拦成 403 `origin_required`，这与 `/feed.xml` 的处理一致。

## 静态资源

- `web/static` 在首次请求时一次性编译成 bundle：原始字节、gzip 副本（仅当压缩后确实更小）与**每种表示各自的**强 ETag，用 `sync.OnceValue` 记忆；编译是嵌入字节的纯函数，因此记忆化不影响正确性。
- 响应带 `Cache-Control: public, max-age=31536000, immutable` 和 `Vary: Accept-Encoding`。两种表示的 ETag 必须不同（`"<sha256>"` 与 `"<sha256>-gzip"`）：共用一个标签会让共享缓存把 gzip 字节发给只要明文的客户端，因为缓存正是拿 ETag 当重验证键的。
- `embed.FS` 没有修改时间，所以不输出 `Last-Modified`，ETag 是唯一校验器。它是**实际发出的字节**的 sha256，这也是「不可变」这个声明能成立的前提。`http.ServeContent` 免费提供 `Range` 与 `If-None-Match`。
- 带 `Range` 的请求**一律回落到明文表示**：字节范围作用于「被选中的表示」，切一段 gzip 流会让客户端拿到无法解压的字节；而 `Accept-Encoding` 只是偏好而非要求，所以对这一次请求放弃压缩是安全答案。明文响应由 `ServeContent` 给出 `Content-Length` 与 `Content-Range`。
- 压缩响应显式写出自己的 `Content-Length`（压缩后字节数）：`ServeContent` 对有 `Content-Encoding` 的响应不填长度，否则这一条会退化成 chunked。
- 每个 `/static` URL 都带内容派生的版本号（12 位十六进制，覆盖所有资源的文件名与摘要），一年缓存因此安全：发版改版本号，URL 跟着变，回访者不会被钉在旧字节上。新增模板里的静态引用必须带上这个 query。
- `gzip;q=0` 与 `*;q=0` 是明确拒绝而不是缺省，会被尊重；`q` 缺失或无法解析按 RFC 9110 视为 1。

## 认证

| 方法 | 路径 | 输入 | 结果 |
|---|---|---|---|
| POST | `/api/v1/auth/register` | email,password,display_name | 创建 reader 并登录；注册关闭返回 403 |
| POST | `/api/v1/auth/login` | email,password | 创建 Session，返回用户和 CSRF token |
| POST | `/api/v1/auth/logout` | 无 | 删除当前 Session 并清 Cookie |
| GET | `/api/v1/auth/me` | 无 | 当前用户和 CSRF token；游客返回 401 |
| GET | `/auth/github/start` | 可选 return_to | 保存一次性 state 后跳转 GitHub |
| GET | `/auth/github/callback` | code,state | 登录/绑定后回到同源白名单路径 |

密码 10–72 字节，**该长度规则只在创建密码时生效**（注册、站长初始化、改密）；登录只校验已存密码，不套用长度规则，因此早于该规则创建的账号仍能登录，也不会在请求发出前就被浏览器拦掉。邮箱最大 254 字符；登录与注册按 IP 和邮箱限速。所有认证失败使用相同外部错误信息。认证接口的 JSON 请求体上限为 64KiB，与 `BOOP_MAX_UPLOAD_MB` 上传预算无关，超限返回 413 `payload_too_large`。

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
| PATCH | `/api/v1/admin/posts/{id}` | 上述字段的部分更新；**`updated_at` 是必填**，见下 |
| DELETE | `/api/v1/admin/posts/{id}` | 软删除 |
| POST | `/api/v1/admin/uploads` | multipart 单文件，字段名 `file`；jpg/png/webp/gif，默认最大 10MB |
| GET | `/api/v1/admin/comments?status=pending` | 审核队列；`status` 为 pending/approved/rejected，默认 pending，`limit` 默认 20、最大 50 |
| POST | `/api/v1/admin/comments/{id}/approve` | 批准；重复调用幂等，未知或已删除评论 404 |
| POST | `/api/v1/admin/comments/{id}/reject` | 拒绝；幂等同上 |
| DELETE | `/api/v1/admin/comments/{id}` | 管理删除（软删除）；重复删除 404 |

内容写入规则（`PATCH /api/v1/admin/posts/{id}`）：

- **`updated_at` 是必填字段**，它同时是乐观锁的版本号。缺字段、或值不能按 `RFC3339Nano` 解析（即 `RFC3339` 再加可选的纳秒小数部分，也就是响应里给出的那种形态），都返回 400 `invalid_updated_at`（消息为「缺少或错误的 updated_at」）；不存在「不带版本号就无条件覆盖」的写法。比较是**字符串精确相等**，所以要把服务端给的值原样回传。
- 带了但已过期（该行在这之后被改过）返回 409 `conflict`，应重新读取内容、拿新的 `updated_at` 再重试。
- 成功响应的 `updated_at` 是这次写入产生的新版本号，下一次 `PATCH` 用它，不要沿用请求里那一个。`published_at` 不受影响：它只在第一次转为已发布时写入，之后冻结，所以编辑不会移动内容在信息流里的位置。
- 判定顺序是：先解析 `updated_at`（400）→ 再查行（未知或已软删除为 404 `not_found`）→ 再合并并做字段校验（如「只有文章可以设置标题」→ 400 `invalid_title`）→ 最后做乐观锁比较（409）。因此一次「既写了当前类型不允许的字段、又带了过期时间戳」的请求得到 400 而不是 409。

上传与图片服务：

- 文件类型由字节推断（`http.DetectContentType`），不信任客户端 MIME：扩展名与内容不符返回 400 `invalid_filename`，内容不是 jpg/png/webp/gif 返回 415 `unsupported_media_type`。
- `BOOP_MAX_UPLOAD_MB`（默认 10MB）是**单文件上限**：文件本身超过该上限返回 413 `payload_too_large`，恰好等于上限的文件可以上传。请求体总上限是该上限加上固定的 multipart 封装预算（64KiB，用于 boundary 与分段头），请求体超出同样返回 413 `payload_too_large`。
- 必须恰好一个文件字段，字段名必须是 `file`，否则返回 400 `invalid_body`；其它非文件表单字段会被忽略。缺少文件、多个文件或非 multipart 请求同样返回 400 `invalid_body`。
- 新文件返回 201；相同字节且属于同一站长时返回 200 且 `reused=true`，复用既有 asset 与文件。响应字段：`id`、`url`、`storage_key`、`mime_type`、`size_bytes`、`width`、`height`、`original_name`、`reused`。`url` 是 `storage_key` 的公开地址：本地保存时为 `/uploads/<storage_key>`，对象存储时为 `BOOP_R2_PUBLIC_URL/<prefix>/<storage_key>`，两种情况下它都等于 `GET` 该资源最终会得到的地址。
- 客户端文件名只作为 `original_name` 元数据保存，绝不进入路径；`storage_key` 由服务端生成为 `YYYY/MM/<32 位随机十六进制>.<ext>`。
- `asset_ids` 必须是当前站长上传过的图片资源：不存在、不属于自己或不是图片 MIME 均返回 400 `invalid_asset`；发布 `photo` 至少需要一张这样的图片，否则返回 400 `invalid_assets`。
- `GET /uploads/{storage_key}` 公开只读、无需登录；键形状不符、目录穿越或文件不存在均返回 404。
  - 上传保存在 `BOOP_DATA_DIR` 时，响应是文件本身，带 `Content-Type`、`nosniff` 与 `Cache-Control: public, max-age=31536000, immutable`。
  - 上传保存在对象存储时，响应是 301 永久重定向到该对象在 `BOOP_R2_PUBLIC_URL` 下的地址，同样带 immutable 缓存头。重定向只针对形状合法的键，所以它不会成为发布任意地址的通道；已发布的页面直接引用对象地址，不经过这条路由。

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
- `GET` 返回 `site_name`、`site_description`、`site_avatar_url`、`site_icon_url`、`site_timezone`、`page_size`、`registration_enabled`、`comments_enabled`、`comments_moderation_enabled`、`ai_enabled`、`ai_base_url`、`ai_chat_model`、`ai_embedding_model`、`ai_author_status_ttl_hours`，以及 `github_client_id_set`、`github_client_secret_set`、`ai_api_key_set`、`master_key` 四个布尔标志。**任何密钥明文都不会出现在响应里**，页面只知道某个密钥是否已配置。
- `PATCH` 只接受上述字段加上三个密钥字段（`github_client_id`、`github_client_secret`、`ai_api_key`）和 `clear_secret`；上传目录、单文件大小与允许的 MIME 类型只能由环境变量配置，请求里出现即 400 `invalid_body`。
- `site_avatar_url` 与 `site_icon_url` 允许为空（分别表示不配置站点头像、不配置站点图标），非空时必须是**无凭据的 http/https 绝对 URL**，且不超过 2048 个字符；相对路径、其它协议、带 `user:pass@` 的值一律 400 `invalid_settings`。`site_icon_url` 是独立的浏览器标签页图标，只影响 `<link rel="icon">`，不参与页面里的头像渲染。
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
- `GET /admin` 站长管理入口，303 跳转到 `/admin/settings`；游客重定向到 `/login`，普通读者得到 403 HTML 页。左栏底部对站长渲染一个指向 `/admin` 的齿轮入口，游客与读者的页面里没有这段 DOM；移动端顶栏用同样只对站长可见的图标按钮承担同一入口。
- `GET /admin/comments`、`GET /admin/settings` 站长页面；两者的游客都重定向到 `/login`，普通读者得到 403 HTML 页。
- 已登录时（站长与读者都有）左栏底部与移动端顶栏各渲染一个 `[data-logout]` 按钮，`app.js` 用它发出 `POST /api/v1/auth/logout`，成功后跳回 `/` 让服务端重新渲染游客外壳；游客的页面里这两个按钮都不存在。两处落点是必需的：左栏在 700px 以下 `display:none`，顶栏在 700px 以上 `display:none`，只留一处在某一个宽度区间里就点不到。这一步**没有无脚本回退**：CSRF 令牌只走 `X-CSRF-Token` 请求头（`guardUnsafeMethods` 只读该头），普通表单提交取不到它，服务端会按设计返回 403 `csrf_invalid`；结束会话不能做成 `GET` 链接，否则任意第三方页面用一个图片标签就能把访客登出。
- 未知页面返回带 request_id 的统一 404；API 永远不返回 HTML 错误页。

品牌与头像：

- `site_name`、`site_description`、`site_avatar_url` 注入所有页面的外壳：标题后缀、桌面与移动品牌名、品牌 `aria-label`、搜索页输入框的标签与占位符、默认 `meta description`，以及首页可见文案（`sr-only` 标题与底部说明）。页面外壳不再硬编码任何品牌名。
- 渲染头像的优先级是 `site_avatar_url` → 站长账号 `users.avatar_url` → 内置 SVG，用于快捷发布、信息流卡片、收藏卡片与内容详情。页面 `<link rel="icon">` 用另一条链：`site_icon_url` → `site_avatar_url` → 内置 SVG 图标，因此标签页图标可以独立于头像配置。两条链互不影响。CSP 的 `img-src` 为 `'self' data: http: https:`，否则配置的绝对地址会被浏览器直接拦掉；脚本、样式与连接仍然是同源（`script-src 'self'`、`connect-src 'self'`）。
- 展示路径读取设置失败时回退到默认值并写一条告警（页面仍然可用）；而设置表单和写入路径在设置损坏时返回 500 / 4xx，避免把默认值写回去覆盖真实设置。
- 首页、内容详情、收藏页在已经读过设置时复用同一次读取（只注入外壳），不会为品牌再查一次库。

页面元数据（每个 HTML 页面）：

- 每个页面都带 `<link rel="canonical">`、`og:type`、`og:site_name`、`og:url`、`og:title`、`og:description`、`twitter:card`；详情页另有 `article:published_time` 与 `article:modified_time`。索引页另有 `og:image`（`site_avatar_url` → `site_icon_url`，取到了就让 `twitter:card` 变成 `summary_large_image`），详情页只有在**自己**有封面图时才声明 `og:image`，不借站点品牌图然后在结构化数据里声称它是这篇文章的图。
- 规范地址由 `BOOP_BASE_URL` 加上**百分号编码**的请求路径拼成，**查询串被丢掉**：`/`、`/?type=article`、`/?type=photo`、`/?cursor=...` 全部归到 `/`。它们渲染同一个 `<title>` 与同一批卡片，只是顺序或筛选不同，为一份文档声明多个地址只会分散抓取信号；`cursor` 更是能按翻页生成无限多个 URL。请求 Host 与 `X-Forwarded-*` 从不参与，否则伪造头就能让本进程对外宣告别人的源站。
- `<title>` 与 `og:title` 由同一个 `pageMeta.DocumentTitle` 方法生成，不会各写一套而漂移。详情页两者优先用 `posts.seo_title`，而可见的 `<h1>` 仍用真实标题：为搜索结果挑的措辞不一定是页面该显示的措辞。
- 描述优先 `posts.seo_description`，否则摘要，否则正文里**读者能看到的文本**。文章正文走的是与 RSS 描述同一条 goldmark + bluemonday 管线：把已被净化掉的 `<script>` 先还原成摘要文本再发出来，等于把写入时清掉的内容又放回机器可读副本里；标题回退同样取可见文本，所以 `## 小标题` 不会连着井号一起进 `<h1>`。
- 结构化数据是 `<script type="application/ld+json">` 数据块：首页是 `WebSite`（搜索引擎据此取站点名），文章是 `BlogPosting`，动态与摄影是 `SocialMediaPosting`——Google 的 Article 指引明确要求不是文章的内容不要标记成文章，schema.org 也为“信息流里的帖文”提供了这个子类型。
- 该数据块**不受 `script-src 'self'` 限制**，也不需要 nonce 或 `unsafe-inline`：HTML 规范在 “prepare the script element” 里先按 `type` 判定脚本类型，不是 JavaScript MIME 类型的元素会在此之前直接返回，而 CSP 的内联检查发生在其后的步骤，因此浏览器把它当作纯数据块，根本走不到 CSP 检查。内容由 `encoding/json` 生成，`<`、`>`、`&` 与 U+2028/U+2029 已转义为 `\uXXXX`，所以标题里写 `</script>` 也闭合不了该元素。
- `/search`、`/bookmarks`、`/login`、`/register`、`/admin*` 带 `<meta name="robots" content="noindex, nofollow">`。这些路径同时被 robots.txt 排除：规则让守规矩的爬虫省下一次请求，noindex 覆盖不守规矩的。

