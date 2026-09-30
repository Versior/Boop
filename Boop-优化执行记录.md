# Boop 优化执行记录

> 对应 [`Boop-体验优化报告.md`](./Boop-体验优化报告.md) 与 [`Boop-对标-Ech0.md`](./Boop-对标-Ech0.md) 的 P0 清单。
> 基线 `113ef8f`，本轮共 **10 个提交**（`28e04df` … `e8904e7`），+2250 / −75 行，新增 34 个测试函数。
> 全部改动均已通过完整门禁，且每条结论都附实测输出。

---

## 0. 摘要

P0 七项中完成 **6 项**，唯一未动的是 **LICENSE** —— 它需要人来决定许可类型（MIT 还是 Apache-2.0），不是代码问题。原报告里若干「建议新增中间件 / 定时任务」的方案被改写成「构建期预压缩 + 挂在登录上」，因此实际改动比报告初稿更小、并发模型没有变化。

执行过程中另外发现并修掉 **3 个既有缺陷**，其中两个是本轮自己引入的（见 §3）。

> **§0–§4 只覆盖第一轮（P0 清单）。** 后面还有三轮追加修复：**§5** 按严重度全修四个已确认缺陷
> （中文搜索、退出登录入口、地址编码、`PATCH` 的 `updated_at` 文档）；**§6** 按他的截图意见
> 移除右栏搜索框，并修掉由此暴露的「空轨道占位」旧缺陷；**§7** 后台不再沿用前台导航，
> 设置按分类拆成四个子页。

| # | P0 条目 | 状态 | 提交 |
|---|---|---|---|
| ① | LICENSE | ⏸ 待决策 | —— |
| ② | likes(post_id) 索引 | ✅ | `5969bf2` |
| ③ | 静态资源预压缩 + ETag | ✅ | `b439247` `efae119` |
| ④ | robots.txt / sitemap.xml | ✅ | `605bd8d` |
| ⑤ | 三处小修 + session 清扫 | ✅ | `c153de5` `e726ae8` |
| ⑥ | go mod tidy | ✅ | `28e04df` |
| ⑦ | CI 质量门禁 | ✅ | `e8904e7` |
| 补 | canonical / og:* / JSON-LD | ✅ | `724de19` |
| 补 | 详情页照片比例 + 移动端搜索入口 | ✅ | `8491354` |

---

## 1. 逐条记录

### `28e04df` chore: mark goldmark, bluemonday and x/net as direct dependencies

`go mod tidy` 把 `github.com/yuin/goldmark`、`github.com/microcosm-cc/bluemonday`、`golang.org/x/net` 从 `// indirect` 块移到直接依赖块。**已核对 `go.sum` 零变化**，只调整了 require 分组——也就是说它们一直是被直接使用的，只是标注错了。

### `5969bf2` perf: index likes(post_id) so the reaction count stops scanning

新增 `internal/store/migrations/004_likes_post_index.sql`。`likes` 主键是 `(user_id, post_id)`，`post_id` 上没有前导索引，而每个信息流卡片都要 `COUNT(*) ... WHERE post_id = ?`。

实测 `EXPLAIN QUERY PLAN`：

| 状态 | 计划 |
|---|---|
| 加索引前 | `SCAN l` |
| 加索引后 | `SEARCH l USING COVERING INDEX idx_likes_post (post_id=?)` |
| 反方向（作者查询） | `SEARCH likes USING COVERING INDEX sqlite_autoindex_likes_1` |

用 `NOT INDEXED` 复现旧计划，证明断言不是空转。`docs/DATABASE.md` 同步记录，并说明 **bookmarks 刻意不加索引**（没有按 post 统计收藏的查询）。

### `c153de5` refactor: order search results by the bare published_at column

搜索的游标与 `ORDER BY` 去掉 `COALESCE`，与 `content.Feed`、RSS 一致（已发布行一定有非空 `published_at`，这是写入侧保证的）。

**诚实标注：实测 `EXPLAIN QUERY PLAN` 前后不变**（FTS5 虚拟表扫描驱动计划，两种写法都要临时 B-tree 排序）。因此这是一致性修复，**不宣称提速**。

### `e726ae8` fix: sweep expired sessions instead of leaking a row per sign-in

此前只有 `LookupSession` 删过期行，而且只删当前 cookie 那一条——每次登录都新插一行，过期行永远留着。新增 `auth.SweepExpiredSessions`，**挂在登录成功之后而不是定时器上**：不引入新的并发模型，进程仍然只有「请求」一种写数据库的时机。

用 `sessionSweep` 结构 + 可注入时钟，测试不 sleep。断言：首次清扫删掉 2 行（48 小时前过期 + 边界正好 `now`）、存活行不受影响；间隔内不清扫；越过后再清。

### `b439247` perf: pre-compress the static assets and serve them immutable

启动时用 `sync.OnceValue` 把 `web/static` 编译成 bundle：原始字节 + gzip 副本（仅当确实更小）+ **每种表示各自的**强 ETag。

实测压缩收益（真进程 curl）：

| 资源 | 原始 | gzip | 降幅 |
|---|---|---|---|
| `app.css` | 29,024 | 6,796 | −77% |
| `app.js` | 28,986 | 7,711 | −73% |
| `boop-mark.svg` | 505 | 348 | −31% |

两个关键细节：

- 两种表示的 ETag 必须不同（`"<sha256>"` 与 `"<sha256>-gzip"`），并带 `Vary: Accept-Encoding`。共用一个标签会让共享缓存拿 gzip 字节去满足明文请求——缓存正是拿 ETag 当重验证键的。
- `embed.FS` 没有修改时间，所以不输出 `Last-Modified`，ETag 是唯一校验器；它是**实际发出的字节**的 sha256。

因此每个 `/static` URL 都带内容派生的版本号（12 位十六进制，覆盖所有资源的文件名与摘要），一年 `immutable` 缓存才成立：发版改版本号，URL 跟着变。

### `efae119` fix: serve ranges of a static asset from the identity bytes

修掉 `b439247` 自己引入的两个缺陷：

- **带 `Range` 的请求曾被切成一段 gzip 流**。字节范围作用于「被选中的表示」，客户端会拿到无法解压的字节。现在有 `Range` 就回落到明文表示（`Accept-Encoding` 只是偏好不是要求，这是合法答案）。
- **压缩响应曾退化成 chunked**，因为 `ServeContent` 对有 `Content-Encoding` 的响应故意不填长度。现在显式写出压缩后长度；`ServeContent` 切范围时会覆盖该值，所以这个头只影响压缩分支。

### `605bd8d` feat: publish robots.txt and a sitemap that lists every post

`GET /robots.txt`：`Allow: /` + 一组 `Disallow`（`/admin`、`/api/`、`/auth/`、`/search`、`/bookmarks`、`/login`、`/register`）。`/search` 被排除是因为结果集随查询串无限展开，每次抓取都要跑一次全文检索。

`GET /sitemap.xml`：首页 + **每一条**已发布内容，逐条带自己的 `lastmod`（取 `updated_at`）。查询只投影 `slug, updated_at`，用 `idx_posts_feed` 能直接满足的顺序，并按协议上限 50000 条封顶。首页条目**故意不带** `lastmod`（首页只渲染最新一页，没有单个时间戳能描述它何时变化）。

两个文档的绝对地址都只由 `BOOP_BASE_URL` 生成，**永不**读请求 Host 或 `X-Forwarded-*`——这正是 Ech0 的 `resolveBaseURL` 信任转发头那处问题的反面教材。

### `724de19` feat: give every page a canonical address, a link preview and structured data

`pageMeta` + `base.html` 渲染 `canonical` / `og:*` / `twitter:card` / JSON-LD / `noindex`。

- **规范地址丢掉查询串**：`/`、`/?type=article`、`/?cursor=...` 全部归到 `/`。它们渲染同一个 `<title>` 与同一批卡片，`cursor` 更是能按翻页生成无限 URL。地址是 `BOOP_BASE_URL` + **百分号编码**的路径。
- **JSON-LD 不受 `script-src 'self'` 限制，也不需要 nonce**。这不是猜测：HTML 规范在 “prepare the script element” 里先按 `type` 判定脚本类型，不是 JavaScript MIME 类型的元素在**第 12 步**就 `return`，而 CSP 的内联检查在**第 21 步**，永远走不到。内容由 `encoding/json` 生成，`<`、`>`、`&`、U+2028/U+2029 已转义为 `\uXXXX`，所以标题里写 `</script>` 也闭合不了该元素（有测试用真实 payload 验证往返）。
- **文章是 `BlogPosting`，动态与摄影是 `SocialMediaPosting`**：Google 的 Article 指引明确要求不是文章的内容不要标记成文章。
- `/search`、`/bookmarks`、`/login`、`/register`、`/admin*` 带 `noindex, nofollow`。

### `8491354` fix: stop cropping the photograph on its own page and reach search on a phone

详情页照片不再按 `8/5 + object-fit:cover` 裁切（`.post-detail .photo-art{aspect-ratio:auto}`），信息流卡片保留统一缩略图比例。移动端底部导航补上「搜索」，顺序与左栏图标条一致。

### `e8904e7` ci: run the AGENTS.md gate on every push

`verify` 作业按 AGENTS.md 的顺序跑门禁，另加三条本机清单没有的检查：`go mod tidy -diff`（正是 `28e04df` 修的那类漂移）、`CGO_ENABLED=0 go build`（守住「零 CGO 单二进制」这条产品边界）、把 `git diff --check` 对准本次推送的区间（干净工作树上它什么也比不了）。`race` 单独一个作业，因为它慢好几倍。

---

## 2. 实测证据汇总

```
gofmt -l .                    → 无输出
go vet ./...                  → 通过
go test ./... -count=1        → 12 个包全部 ok，404 个测试通过
go test -race ./... -count=1  → 12 个包全部 ok
node --check web/static/app.js → 通过
git diff --check              → 干净
go mod tidy -diff             → 无差异
CGO_ENABLED=0 go build ./cmd/boop → 成功
```

真进程验证（临时实例，`BOOP_BASE_URL=https://blog.example.com`）：

- `robots.txt` → 196 字节，`Sitemap: https://blog.example.com/sitemap.xml`
- `sitemap.xml` → 合法 XML，`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`
- `/static/app.css` 明文 → `Content-Length: 29024`，`Cache-Control: public, max-age=31536000, immutable`
- `/static/app.css` + `Accept-Encoding: gzip` → `Content-Encoding: gzip`，ETag 带 `-gzip` 后缀
- 带匹配 `If-None-Match` → `304 Not Modified`
- `Accept-Encoding: gzip;q=0` → 回落明文
- 首页 HTML → `/static/app.css?v=a77463086b7a`（三处 URL 都带版本号）

---

## 3. 过程中发现并修掉的既有缺陷

这三条都不在报告的 P0 清单里，是动手时才暴露的。

1. **`posts.seo_title` / `posts.seo_description` 是死字段**（半修）
   建表有列、`content.Post` 有字段、AI 助手能生成、`docs/DATABASE.md` 有记录——但**创建/更新接口不接受它们，渲染侧也从不读取**。现在渲染侧接上了（`<title>`/`og:title`/描述优先用它们，可见 `<h1>` 仍用真实标题）。**写入侧仍未接**：AI 生成的 SEO 建议目前存不下来。
2. **状态页的 `/static/app.css` 没有版本号**（本轮 `b439247` 引入）
   `writeStatusPage` 是手写 HTML，漏了 `?v=`。一个只看过 404 页的浏览器会把那个 URL 钉住一年。
3. **搜索页在移动端导航里没有高亮**（既有，本轮修复时发现）
   桌面左栏标了 `/search` 为当前项，移动端底部导航没有搜索入口，所以 `/search` 是唯一一个在手机上「什么都没高亮」的页面。原测试把这个缺陷写进了断言（`want 1`），已改为与首页一致的 `want 2`。

另外顺手修掉一个**用户可见**的问题：文章没有标题时，`<h1>` 与 `<title>` 的回退取的是 Markdown **源码**首行，所以正文以 `## 我的标题` 开头时，页面上会显示带井号的 `## 我的标题`；摘要回退则会把已被净化器删掉的 `<script>` 标签作为**纯文本**重新发出来。现在正文先过与 RSS 描述同一条 goldmark + bluemonday 管线，只取读者能看到的文本。

---

## 4. 仍未做的

### 需要人决定

- **LICENSE**（P0 第 1 项）。当前仓库无 LICENSE，默认保留全部权利。若要与 Ech0 之类 AGPL 项目做代码级互通，必须先在许可证上做选择；**MIT 与 Apache-2.0 的区别**主要是后者含专利授权与商标条款。这一步不定，`Boop-对标-Ech0.md` §3.1 讨论的 AGPL 传染边界就一直悬着。

### 代码侧待办（按价值排序）

1. **`seo_title` / `seo_description` 的写入侧**：创建/更新接口接受这两个字段 + 长度校验（对齐 AI 助手的 160 / 300 字上限）+ 乐观锁，AI 建议才能真正落库。
2. **`/admin/posts` 内容管理页**（P1）：目前改内容只能走 API，后台只有设置与评论审核两个页面。
3. **`boop export -o` 导出为可读 Markdown 目录树**（`Boop-对标-Ech0.md` 新增建议 #23，影响评 5 分）：把护城河从「一个 `.db` 文件」升级为「一个能用 grep/git/diff 的目录树」。
4. **首页筛选视图的 `<title>`**：`/`、`/?type=article`、`/?type=photo` 目前共用同一个 `<title>`（站点名）。规范地址把它们归到一处是合理的，但若将来想让它们各自可被索引，需要先给出各自的标题。

### 需要用户决定的操作

- 本地领先 `origin/main` **22 个提交**（上一轮 3 个 + 第一轮 10 个 + R2 4 个 + 两轮修复 5 个）。用户此前明确说过「先不推，我要先看」，因此没有 push。
- 三份分析文档（`Boop-项目评审报告.md`、`Boop-体验优化报告.md`、`Boop-对标-Ech0.md`）与本文件都刻意保持未跟踪。`.workbuddy-ai/` 同样未跟踪，而且**没有写进 `.gitignore`**，一次 `git add -A` 就会把它一起提交；建议补一条忽略规则，改之前会先确认。
- 推送之后 CI 才会第一次真正运行（`.github/workflows/ci.yml` 是新增文件），届时若 Ubuntu 上的 `go mod tidy -diff` 与 macOS 结论不一致，需要再对齐。

---

## 5. 第二轮修复：按严重度全修四个已确认缺陷

上一轮把每个功能与按钮都真机验证了一遍，确认了 **4 项产品缺陷 + 1 项文档偏差**，当时一项未修。本轮逐项修完，**一个缺陷一个提交**，每项都跑同一套完整门禁（`gofmt -l .`、`go vet ./...`、`go mod tidy -diff`、`go test ./... -count=1`、`go test -race ./... -count=1`、`CGO_ENABLED=0 go build -trimpath`、`node --check web/static/app.js`、`git diff --check`）并重建二进制、起真实实例做端到端复验。

### 5.1 `7308d85` 中文搜索：索引里没有词边界，就别再向索引要

**根因**：`002_search.sql` 的 `tokenize='unicode61'` 把连续 CJK 当成**一个 token**，索引里根本没有中文词边界，所以查询「评论」永远匹配不到「一段待评论的动态」。改查询表达式救不了——能改的只有「在哪个数据结构上查」。

**方案（零迁移）**：按查询词的字符集分流。含 CJK 走 `posts` 三列的 `LIKE '%词%'` 子串检索（每个词都必须出现，等价于 FTS 的 AND）；不含 CJK 走原 FTS5 路径，语义不变。两条路径共用校验、游标、排序、页大小与片段标记。**没有新增迁移、没有重建表、没有回填**——上轮探针论证过的「迁移 003 + CJK 逐字拆分」需要重建表并让触发器调用一个按连接注册的 Go 函数，而 `modernc.org/sqlite` 的自定义函数只对注册之后新开的连接可用，这条路走不通。

**实测**（两万条 / 2670 万字符 ≈ 80MB UTF-8，真实进程 + HTTP）：稠密命中 **1.0 ms**（与 FTS 路径 0.9 ms 同档）、零命中 **195 ms**；`EXPLAIN QUERY PLAN` 为 `SEARCH p USING INDEX idx_posts_feed (status=?)`，无临时 B 树。`ORDER BY published_at DESC` 让扫描直接吃索引，`LIMIT` 一旦凑够一页就收工——这是稠密命中与零命中差两个数量级的原因。**该换 bigram 的信号**已写进 `docs/DATABASE.md`：零命中中文查询稳定超过约 100 ms，或语料越过约一万条。

**过程中修正的两个认知**：相邻 CJK 是一个词（「今天评论」不会拆成「今天」+「评论」）；片段窗口会切掉命中词**前面**的内容，所以断言要挑活下来的那一半。原先的测试 fixture 全把中文用空格隔开，真机中文不分词写——这正是测试全绿而线上 0 命中的原因，已补上不分词的真实串。

### 5.2 `3444743` 退出登录：端点一直是好的，只是没人调用它

`POST /api/v1/auth/logout` 存在且实测 200，但 9 个模板里没有任何 UI 调用它；已登录用户访问 `/login` 被 303 弹回首页，页面上既无账户信息也无登出按钮——**在界面上退不出去**。

左栏在 700px 以下 `display:none`、顶栏在 700px 以上 `display:none`，所以**必须两处落点**：桌面左栏 `.rail-foot`、手机顶栏 `.tb-actions`。两处由 `app.js` 的 `[data-logout]` 一起接管：先禁用全部入口再发请求（连点三次只发一次），成功后跳回首页让服务端重新渲染游客外壳。`pageView` 为此新增 `SignedIn`——`CSRFToken` 能"顺便"表达同一件事，但凭证不是「谁在请求」的陈述。

**没有无脚本回退是设计而不是疏漏**：CSRF 令牌只走 `X-CSRF-Token` 请求头，普通表单拿不到它；也不能做成 `GET` 链接，否则任意第三方页面用一个图片标签就能把访客登出。已写进 `docs/API.md`。

端到端（三种身份 × 两种视口，每个「点击登出」用例各用一个独立会话）：游客 0 处；读者桌面可见左栏那一处；站长桌面登出 + 齿轮；读者手机左栏隐藏、顶栏可见；站长手机登出 + 两个后台图标；点击后一次 POST 带 CSRF 且 200；登出后停在首页、外壳变回游客、`/api/v1/auth/me` 401。**10 项全过。**

### 5.3 `426c2e5` slug 编码：页面是对的，机器读的文档不是

`<loc>http://…/p/这是一段待评论的动态-…</loc>` —— sitemap 与 feed 输出的是**原文**，而同一时刻 HTML 的信息流卡片 `href` 与详情页 `<link rel="canonical">` **都是编码的**。差异的来源：`html/template` 会自动百分号编码 `href`，`encoding/xml` 只转义五个 XML 实体。于是这堆「合法 XML」里的地址既不符合 sitemap 协议（`<loc>` 要求是转义后的 URI），也不是 `guid isPermaLink="true"` 声称的「阅读器能打开的地址」。**顺带发现搜索 API 的 `url` 字段有同一个洞**，一并修了。

统一走 `postPath`（`url.PathEscape`，单段路径，所以 slug 里的 `/` 不会变成分隔符）与 `postURL`（对 `BOOP_BASE_URL` 取绝对）。**HTML 侧的 view model 故意保持原文**：`href` 的转义属于模板那个上下文，预先编码会让所有链接依赖「模板 normaliser 保留已有 `%XX`」这个实现细节。`slug` 字段也保持原文——它是客户端用来比较的标识符，不是地址。

端到端：sitemap 仍是合法 XML，6 条 loc **没有任何一条含非 ASCII**，每条抓下来都是 200 且该页 canonical 与 loc **完全相等**；feed 每条 link 都 200 且 `guid == link`、`isPermaLink="true"`。

### 5.4 `c4fdac8` 文档：`PATCH` 的 `updated_at` 是必填

原措辞「部分更新，带 `updated_at` 乐观锁」读起来像可选。实测四种结果：缺字段 → 400 `invalid_updated_at`；值不能解析 → 400；合法但过期 → 409 `conflict`；正确 → 200 且响应里是**新的**版本号。判定顺序是**解析(400) → 查行(404) → 字段校验(400) → 乐观锁(409)**，所以「既写了当前类型不允许的字段、又带过期时间戳」得到 400 而不是 409。另外两点容易踩：比较是**字符串精确相等**（要原样回传，重新解析再格式化会 409）；`TimestampFormat` 是 `RFC3339Nano`，`Format` 会去掉尾随零。

---

## 6. 第三轮：按截图意见移除右栏搜索框

### 6.1 `fix: drop the right-rail search box and the track it left behind`

他要的是「这里的搜索框去除」。截图指的是**右栏**那个（紧贴「AI 作者状态」卡片上方），
不是搜索页中栏顶部那个。

**为什么不能只删那个 `<form>`**：右栏里只有搜索表单和作者状态卡片两样东西，而卡片只在首页渲染
（`pageView.AIStatus` 只在 `handleHome` 赋值）。只删表单的话，**除首页以外每个页面的右栏都会变成
空的 `<aside>`** —— 带着 `border-left` 白占一列，拖出一条没有内容的竖线。

顺带查出一件**早就存在**的布局缺陷：`.app` 的 `grid-template-columns` 写死三道轨
`68px 600px 336px`，而 ≤1239px 的断点只把 `.rail-right` 设为 `display:none`。**显式轨道即使空着
也照样占位**，所以：

| 视口 | 改动前 | 改动后 |
| --- | --- | --- |
| 700–1239px | 中栏偏左 **336px**（实测 `left=41 right=376`） | 左右留白相等（`209/208`、`216/216`） |
| ≤1003px | 三道轨合计 1004px > 视口，**横向溢出**（@800 `leftGap=-109`） | 不再溢出（@800 留白 `59/58`） |

**改法**：`base.html` 里 `<aside class="rail-right">` 整块用 `{{if .AIStatus.Text}}` 包住
（连容器一起），`.app` 加条件类 `has-rail`；`app.css` 把基础栅格改成**两道轨**，`.app.has-rail`
才是三道轨，≤1239px 断点里也要撤掉第三道轨。顺手删掉死字段 `pageView.SearchQuery` 与
`handlers_search.go` 里给它的赋值——右栏表单是它唯一的消费者。

**实测**（真机 + 无头 Chrome，新老二进制对照，`layout.mjs`）：改动后 **PASS 45 / FAIL 0**；
右栏 `border-left` 只画在卡片那一段（`offsetHeight=171`），不再有拖到底的空竖线；
作者状态卡片在 **1440 / 1239 / 1100 / 800 / 700 / 375** 六个宽度上都不丢（宽屏在右栏，其余由
`.status-mobile` 接管）；搜索入口在各宽度都可见（宽屏左栏图标，手机底部导航）。

**文档**：`docs/PRODUCT.md` 两处、`docs/API.md` 一处。`docs/superpowers/task9-…brief.md` 里
「The right-rail search form remains」是**当时**的任务书，属历史记录，**没有改**。

---

## 7. 第四轮：后台自成一体的导航 + 设置按分类拆页

### 7.1 `2957a30` feat: give the back end its own navigation and split settings by category

他的原话是「后台内容怎么不分类 前端图标怎么后台也有」，澄清答复是「参考其他竞品 或者相似的项目
要高级 清晰明了 分类明确得体」＋「拆成 4 个子页 + 标签导航」。两句抱怨其实是同一件事：**后台一直
在沿用前台的骨架**。

**根因**：后台页面走的是 `shellView(r, navNeutralFilter)`，只把导航高亮清掉，外壳一模一样——
左栏还是前台的图标条（首页/搜索/文章/摄影/收藏），移动端还是同一个底部导航。于是管理页看起来
就是信息流，还摆着五个在那里**没有任何意义**的入口。设置本身则是**一页到底的长滚动**，四个主题
挤在同一个 `<form>` 里，只有 `<h3>` 小标题分隔，没有任何分类导航。

**改法**：

- `pageView.Admin`（`"settings"` / `"comments"`，前台恒空）+ `adminShellView`；骨架用
  `{{if .Admin}}…{{else}}…{{end}}` **二选一**渲染侧栏与底部导航，不是同时渲染两套。两处侧栏
  都是 `<aside>`，共用 `data-rail` 钩子。
- 后台侧栏**带文字**（设置 / 评论审核 / 回到前台 / 退出登录）：后台要被**读**，不是被**认**。
  后台只有两栏（`230px + 600px`），**根本不渲染**只属于首页的右栏。
- 断点：`230 + 600 = 830` → **900px 以下收起侧栏并强制显示顶栏**，否则 700–899 之间顶栏本来
  不出现，后台会没有任何导航入口；≤700px 由后台底部导航接管。
- 设置拆成 `/admin/settings/{section}`（站点 / 注册与评论 / GitHub 登录 / AI），顶部标签行是
  四个分类唯一的通路。**拆页不需要后端改动**：`PATCH /api/v1/admin/settings` 的每个字段都是
  可选的指针，缺省即保持原值，所以一页只提交自己那几个字段，页面不必回传别页的字段，也不可能
  互删彼此的设置。`/admin` 与 `/admin/settings` 都 303 到第一个分类，未知分类是统一 404。
- 每页标题就是分类名（「站点设置」…），模块归属挪到标题之上的一行小字（`站长后台 · 设置`）——
  窄屏侧栏收起后仍有上下文。表单里原来的 `<h3>` 随之删除，`.settings-title` 成了**没有任何
  模板引用**的死规则，一并删掉。抽出 `ownerOnlyPage` 消掉每个后台页重复的「游客 303 / 读者 403」。

**回归钉子**：新增 `TestTheTwoSurfacesKeepTheirOwnNavigation`，拿**旧模板**跑过（`git stash`）
确认它会失败：后台页曾渲染 `rail-left`、`href="/search"`、`href="/bookmarks"`、`?type=article`、
`?type=photo` 与「移动端导航」底部条，且没有 `rail-admin` / `is-admin`。设置测试改成分类表驱动，
含**跨分类字段泄漏**检查（本页不得出现别页的字段名）与「页面自己报名」检查。

**实测**（真机 + 无头 Chrome，`adminlayout.mjs`，**PASS 53 / FAIL 0**）：

| 场景 | `class` | 轨道 | 侧栏 | 顶栏 | 底部导航 | 前台痕迹 | 留白 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 设置·站点 @1440 | `app is-admin` | `230px 600px` | 可见（4 项带文字） | 隐藏 | 隐藏 | 0 | 305/305 |
| 评论审核 @1100 | `app is-admin` | `230px 600px` | 可见 | 隐藏 | 隐藏 | 0 | 135/135 |
| 设置·站点 @800 | `app is-admin` | `600px` | 收起 | **显示**（接手导航） | 隐藏 | 0 | 100/100 |
| 设置·站点 @375 | `app is-admin` | 单栏 | 收起 | 显示 | 可见（3 项） | 0 | — |

四个宽度都 `overflow=0`；四个分类页的标签行都是 4 个且恰好 1 个 `aria-current`；前台对照页
@1440 仍是 2 个可见 `/search` 链接、没有 `rail-admin`。

**文档**：`docs/PRODUCT.md` 的 §6 从「管理设置」改写成「站长后台」（6.1 导航 / 6.2 设置分类，
含四个分类的表格）；`docs/API.md` 的 HTML 页面两条改到 `/admin/settings/{section}`。

**顺带查明一个既有缺陷（未修）**：全量测试里 `TestBookmarksAPIAndPageArePrivate` 偶发报
`order = [1 2], want newest first`。时间戳存的是 `UTC().Format(time.RFC3339Nano)` 的 TEXT，
而排序与游标都按**字符串**比较，`RFC3339Nano` 会裁掉小数末尾的零，于是 `…20.46878Z`
**大于** `…20.468781Z`（`Z` 比任何数字大）。实测微秒网格上相隔 1µs 的一对：**100 万次里
10 万次顺序相反**；换成本地时区（`+08:00`）是 0 次，**只有 UTC 形态会中招**。影响是「同一秒
创建的两条内容相对顺序可能颠倒」，分页不会漏项（排序与游标同一套比较）。修法应是定宽时间戳，
属独立的全表数据改动，**没有塞进这次提交**。

---

## 8. 第五轮：存储配置搬进后台

### 8.1 `1cb379e` feat: let the owner configure object storage from the settings page

他的原话是「**后台怎么没有cf r2 的配置**」。事实成立：R2 是第三轮接的，但配置只走
`BOOP_R2_*` 环境变量，后台五个分类页里没有它的位置，换桶必须改环境变量再重启。

**方向是他在选项里定的**：三选一里他选了「网页里可改（我不建议）」——我在描述里列了代价
（配置进数据库、要动事务内校验、切换不搬对象），他仍然要网页可改。
随后我问「已有图片的兼容怎么做（不加迁移只警告 vs 加迁移 005 保证不断图）」，
他反问「**什么意思现在不是测试环境没有数据吗**」——他正是对的：`git ls-files` 里没有任何
`uploads/` 或 `data/` 目录，仓库零上传产物。所以**「保护已有数据」的前提不成立**，
我据此**放弃了「迁移 005 + 回填」的方案**，只做最小正确实现，把「切换不搬对象」做成页面上
**常驻**的红字警告。

**一条容易被「顺手补全」破坏的规矩**：`Seed` 必须跳过 `storage.*` 六个键。判据是
「`settings` 表里有没有 `storage.mode` 这一行」，写进去就等于替站点宣布「已保存」——
那些**只配了 `BOOP_R2_*`、没配 `BOOP_MASTER_KEY`** 的部署升级后会被判成「已保存」，
于是新上传悄悄写进本地目录。`TestSeedLeavesTheStorageCategoryAlone` 专门钉这条，
`TestSeedWritesEveryDocumentedKeyOnce` 的 want 表**保持 14 项不变**。

**免重启生效**：`server.storage atomic.Pointer[media.Options]` 快照，构造期读一次、
**每次 `PATCH /api/v1/admin/settings` 成功后**再读一次；`mediaOpts()` 读快照，为空才回退
`environmentMediaOpts()`（这条回退是给直接构造 server 的测试用的）。

**整组校验**：单字段校验查不出「模式是对象存储、但桶名和凭据都没有」。`settings.Apply`
在 `touchesStorage(update)` 时于**同一事务内**调 `checkStorageAfterUpdate`——读当前值、
把本次 `Values` 合并进去、叠加本次 `Secrets`/`Clear`，拿「落下去之后」的状态过
`Storage.Validate`。**只读不写**，被拒时事务照常回滚，错误里点名缺哪些字段。
环境变量路径的分工不变：仍在**启动时**由 `config.validateStorage` 拒。

**凭据**：`storage.access_key_id` / `storage.secret_access_key` 走既有 `BOOP_MASTER_KEY` 进
`secret_settings`；没有 master key 时 import 只 warn 不写，页面照旧说密钥字段存不下来——
但**读桶只需要公开地址**，所以没 key 的部署仍能用桶读图。

**切换不搬对象**是既定行为（`assets` 没有存储位置列，`Options.URL` 一律按当前配置生成地址），
本轮没有改它，只是把它从 `docs/STORAGE.md` 搬进页面：那段警告**不因保存而消失**。

**回归钉子**：新增 `internal/settings/storage_test.go`（11 例）、
`internal/server/handlers_settings_storage_test.go`（5 例）、`cmd/boop/storage_test.go`（2 例）；
分类表测试加第五项（10 个 marker，含两个 `data-secret-clear`）。
**反向确认**：临时把 `refreshStorage` 调用改成 `if false`、并去掉 `Seed` 里的 `storageKey` 跳过，
四个测试如期转红，再恢复并复验全绿。

**实测**（真进程 + 40 行假桶 `ThreadingHTTPServer` + 干净数据目录，`storage-check.sh`
**PASS 40 / FAIL 0**）：

| 节 | 断言的东西 |
| --- | --- |
| 1 | 本地页文案：当前生效＝本地目录、`BOOP_R2_*` 只播种一次的说明、常驻警告、下拉与两个凭据字段 |
| 2 | 半配置（只给模式 + 桶名）**400** + 错误含 `incomplete` + 仍按环境跑 + **一行都没写** |
| 3 | 完整配置 200、`storage_environment` 转 False、立刻切到 object、桶/读取地址/凭据都对、密钥明文不出现在响应里 |
| 4 | 上传 201、响应地址是桶地址、**本地目录没有多出文件**、假桶收到 PUT、带 `immutable` 缓存头 |
| 5 | `GET /uploads/<新key>` **301** 到桶；**切换前就存在的本地对象同样 301**（警告说的正是这件事） |
| 6 | 页面改口说「桶 boop-uploads」、给出读取地址、环境变量提示消失、前缀回填 |
| 7 | 切回 local 200、立刻生效、桶名/读取地址/凭据**全部保留** |
| 8 | 本地模式下上传回到磁盘（文件数 +1）、`GET /uploads/<key>` 直接 200 |
| 9 | 库里正好 6 行 `settings` + 2 行 `secret_settings` |

截图：`admin-backend-storage-{1440,800,375}.png`（从未保存过：本地目录 + 环境变量提示 +
红字警告）、`admin-backend-storage-object-{1440,full}.png`（已切到桶：`当前生效：桶 boop-uploads，
浏览器读自 …`、凭据显示「已配置，留空则保持不变」）。

**文档**：`docs/STORAGE.md` 配置章节改成「在后台的『存储』设置页里二选一」并说明「保存后立刻
生效」与「环境变量只播种一次」；`docs/PRODUCT.md` §6.2 从四个子页改成五个、§9 补 `BOOP_MASTER_KEY`
前提；`docs/API.md` 补六个字段、六个 `_set` 与三个只读的 `storage_effective_*`，并写明单字段与
整组两条校验规则；`docs/DATABASE.md` 补约束与「`storage.*` 不在播种之列」。

**门禁**：`gofmt -l .` 无输出、`go vet ./...` 无输出、`go test -race -count=1 ./...` 全绿
（server 195.5s）。

---

## 9. 第六轮：设置页文案去重

### 9.1 `36d3f73` fix: stop the settings pages saying the same thing three times

他贴了一张 GitHub 设置页的截图，说「**这里太乱太杂了 一堆重复的文字说明**」。截图范围正好是
「页头那行小字 → 标题 → 说明 → 标签行」，对着渲染结果数一遍，一处不差：

| 页 | 同一句话出现的地方 |
| --- | --- |
| 所有页 | 「站长后台 · 设置」+ 左栏高亮的「设置」+ 标题「{分类}设置」+ 标签行的当前项 |
| GitHub | 回调地址说了两遍；未配置 master key 说了**三遍**（通用警告 + 分类内提示 + 密钥字段旁） |
| AI | 逐字重复：页头与表单内的「作者状态与写作助手在配置完成后生效」完全一样 |
| 存储 | 迁移警告说了两遍（页头说明 + 红字警告块，**这条是上一轮自己写出来的**） |

**改法**：页头只剩标题与一句说明，删掉 `.page-kicker`（两个后台模板 + CSS 规则一起删）；
表单里只留「读者猜不到、不写就会被拒」的事；缺 master key 的警告**按分类写**
（`adminSettingsSection.Warn`），每页恰好一条并说出这一页自己的后果。
`BOOP_R2_*` 那段留着但缩短——它是有条件的，属于信息不是重复。

**顺带修掉两处真错**：站点页与 `docs/PRODUCT.md`、`docs/API.md` 都还在说「上传目录只能由环境变量
配置」，而上一轮之后存放位置已经可在网页端二选一（改为「二选一，但路径不能任意填写；大小上限与
MIME 白名单仍只由环境变量控制」）；AI 页字段标题写「缓存**天数**」而字段是小时，单位只写在提示里，
标题改为「作者状态缓存（小时）」。

**回归钉子**：`TestSettingsPagesSayEachThingOnce` 读**渲染后的散文**（页头说明 + 所有提示 + 所有警告），
剥标签、按 `。`/`；` 切句、规范化空白，任何 ≥8 字的片段在同一页出现两次即失败；另要求页头说明恰好一条、
且不再有 `page-kicker`。**反向确认**：把那行小字加回去、再把 AI 页头那句复制进表单，测试如期报出
五页的 `still puts a module line above the title` 与 `says "…" 2 times`。
`TestASecretCategoryCarriesOneWarning` 取代原来只测两页的子测试：遍历全部分类，带凭据的恰好一条警告、
不带的零条，`未配置 BOOP_MASTER_KEY` 每页恰好出现一次。

**门禁**：`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check` 干净、
`go test -race -count=1 ./...` 全绿（server 188.0s）。

---

## 10. 第七轮：首页作者头部 + 封面图 + 内容类型行

### 10.1 `8b4795a` feat: open the home page with an author header and a content-type row

他贴了一张「用户主页」截图问 `为什么不参考这个UI页面`。核对之后，那张图里**大部分不能搬**：Boop 没有
用户主页、也没有关注关系（`docs/PRODUCT.md` §7 非目标），照搬关注/粉丝/徽章墙只会得到一排永远不动的
空壳数字。于是用选择题问他做到哪一步，他选 **`连封面图一起做`**——所以只落地能成立的三样：封面图、
作者头部、内容类型行。

**为什么要给类型行加条数**：左栏的图标条已经能走到同样的三个视图（首页/文章/摄影），
一排只有链接的标签就只是重复——这正是他上一轮为设置页报过的那类问题。所以每一项带**已发布内容条数**，
条数才是这一行存在的理由。`content.ContentCounts` 用一条 `GROUP BY type`，可见性规则
（`status='published' AND deleted_at IS NULL`）与 `Feed` 逐字一致，因此条数不可能算进信息流会拒绝列出的内容。

**为什么动态没有自己的标签**：服务端对 `type=moment` 直接 400（`docs/PRODUCT.md` §5.1），
所以 UI 上也不提供。`feedTabs(filter, counts)` 从**已经过校验的 filter** 推导，高亮项不可能与页面不符。

**为什么续页不画作者头部**：`cursor` 非空时 `authorHeader.Show` 为假。续页是同一份信息流的下一段，
在每一段上加一条 200px 横幅是噪音；类型行留着，因为它同时是回到顶部的那条路，且条数描述整个站点
而不是这一页。

**封面图** `site.cover_url` 与头像/图标同一种形状：留空，或无凭据的 http/https 绝对 URL。
它同时顶上**索引页链接预览的第一顺位**（`brandImage`：封面图 → 头像 → 图标）：头像是个方图，
以前却让它承担 `summary_large_image`，而封面图才是唯一按全宽观看绘制的那张。

**三处既有断言是「跟着功能走」而不是被绕开的**：站长首页的配置头像从 2 处变 3 处
（头部、发布器、卡片）；首页 `aria-current="page"` 从 2 处变 3 处（左栏、底部导航、类型行）；
`TestHomeMarksActiveFilter` 另加一条「类型行标记的是这次请求要的那个 tab」——否则把高亮写死在第一项也能过。

**真机实测**（测试站点 8097，换成新二进制之后）：类型行渲染成 `全部 4 / 文章 1 / 摄影 2`，
而库里是 5 条内容——差的那 1 条正是草稿动态，说明计数与信息流用的是同一条可见性规则。
截图 `site-home2-{1440,1440-full,800,375,375-full,article-1440,photo-375,admin-site-1440}.png`
与深色两张 `site-dark-{home-1440,admin-site-1440}.png`。

**门禁**：`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check` 干净、
`go test -race -count=1 ./...` 全绿（server 196.9s）。

---

## 11. 第八轮：左栏改带文字的侧栏 + 首页头部补两条日期

### 11.0 `8871baa` feat: give the left rail its site's name and the header its two dates

他看完上一轮的截图后说：`不好看 左侧图标太多啦 而且logo旁边没有品牌的名字 不够高级
主页没有参考我发给你的图片吗 人家不是很多信息吗 除了关注 粉丝 这些不要 其他的呢`。
两条独立要求：左栏重做、首页头部补信息（但**不要**关注/粉丝）。

### 11.1 左栏：68px 图标条 → 240px 带文字侧栏

> **本节已被 §12 修订。** 240px 的带文字侧栏当天就被他否掉（`左侧导航栏说了不要文字`）——
> 我把他说的「左侧图标太多啦」读成了「把图标换成文字」，读错了。左栏的实际终态是 180px 图标栏，
> 见 §12。本节其余部分（作者头部、`PublishedSpan`、断点与 375px 溢出这两条）仍然有效。

**问题不是「图标画得不好」，是「有三项根本不是页面」**：原来 7 个一模一样的圆图标里，
「外观」是个开关、「我的」直接链到 `/login`（对已登录的人只是空跳转），而品牌名与每一项的文字
被 `.sr-only` 那组选择器藏掉了——所以「logo 旁边没有品牌的名字」和「图标太多」是同一个毛病的两面。

新的分工只有三块：`.rail-head`（品牌行 = 标记 + 站点名 + 外观开关）、`.nav`（**恰好 5 项**：
首页/搜索/文章/摄影/收藏，与底部导航同一组目的地、同一顺序、同一组地址）、
`.rail-foot`（账号区：未登录=登录；已登录=退出登录；站长再加站长后台）。`.rail-foot` 从
「只有登录才渲染」改成**恒渲染**，底部导航的「我的」一并改名「登录」，两处对 `/login` 的叫法
不再分叉。

**断点必须一起动**：侧栏 240 + 中栏 600 = 840，所以前台的收起点从 ≤700 提到 **≤899**，
与后台（230 + 600 = 830）共用同一道断点。收起后前台靠顶栏 + 底部导航，后台靠顶栏 + 后台底部导航。

**只有截图能发现的坑**：把老代码里 `@media (max-width:700px)` 的 `.app{display:block}` 换成
`grid-template-columns:var(--main-w)` 之后，375px 上横向溢出——600px 的轨道在 375px 视口里
两侧都出屏，正文被切掉半行。修法是两层：≤899 只让侧栏消失、中栏仍是居中的 600px 轨道；
**≤640**（中栏自己就 600px，放不下就该回流式）再 `display:block`。`go test` 对这件事
一个字都不会说。

### 11.2 作者头部：两条日期 + 订阅入口

补的是 `.author-facts` 一行：`始于 2026年9月29日` · `最近更新于 20:49` · `RSS 订阅`。
**没有**关注/粉丝/合集（§7 非目标）。

**为什么是这两条**：它们说的是这个站真实存在的事实，而且**不重复**类型行的三个条数——
如果头部再放一遍「共 4 篇」，那就是他反复报过的「同一件事说两遍」。
`content.PublishedSpan` 一条语句两个标量子查询，排序**与 `Feed` 逐字一致**
（`published_at DESC, id DESC` / `ASC, id ASC`），可见性规则也同一个，所以头部的「最近更新于」
不可能是信息流顶上那张卡之外的东西；草稿与软删除都不算端（有专门的测试对账）。
「始于」另写一个 `siteDate`（带年），因为 `displayTime` 对今年的时间戳只给「1月2日」，
「始于 1月2日」就不成一个事实了。

**只在第一页查**：`PublishedSpan` 包在 `if view.Header.Show` 里，续页不多一条查询、不多一行字。

### 11.3 测试

- `TestPublishedSpanMatchesTheFeedEnds` / `TestPublishedSpanOnAnEmptySite` /
  `TestPublishedSpanIssuesOneQuery`（内容层，含「恰好一条语句」）。
- `TestTheFrontRailNamesTheBrandAndKeepsFiveDestinations`（第八轮叫
  `TestTheFrontRailSpeaksTheSameLanguageAsTheAdminSidebar`）：5 个目的地、导航里没有
  `外观/我的/登录/退出登录`、游客的账号区是 `/login`、站长的有 `/admin` + `data-logout` 且没有
  `/login`。这一轮它**读 `app.css`** 断言 `.rail-left .brand-name` 与 `.rail-left .nav-item .label`
  都不在 `.sr-only` 那组选择器里——第九轮把前者的断言反了过来（见 §12.3）。
- `TestHomePageAuthorHeaderAndTypeRow`：头部先建一条**草稿**（它是表里最早的一行，跨度查询若漏掉
  可见性规则就会选到它、`published_at` 为空、整行消失），再断言两条日期的 `datetime` 就是库里
  那两端、可读值不为空、RSS 入口存在；`TestHomePageCursorLink` 断言续页不重复 `.author-facts`。

**门禁**：`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check` 干净、
`go test -race -count=1 ./...` 全绿。**真机**：测试站点（8097）换二进制后，
游客的侧栏渲染 5 项 + 登录、站长的渲染站长后台 + 退出登录；首页实测
`始于 <time datetime="2026-09-29T12:49:05.960049Z">2026年9月29日</time>` 与
`最近更新于 20:49`（站点时区 Asia/Shanghai）。截图 `site-rail*`（1440 / 1240 / 1024 / 899 /
375 整页 / 搜索 / 筛选 / 后台 / 深色）。

---

## 12. 第九轮：左栏改回图标栏 + 品牌名换衬线

### 12.0 `3f602a3` feat: take the left rail back to an icon rail and set the site's name in a serif

他把我上一轮的左栏截图递回来说：`少爷的博客改成这个项目的名字 字体高级艺术点 左侧导航栏
说了不要文字`。三件事：站名要显示的是**这个项目的名字**、字体要**高级艺术**一点、左栏
**不要文字**——最后这句是对 §11.1 的直接纠正。另外我问了一句「品牌位那行字写什么」，
他的答复是 `默认Boop 但是后台可以设置修改`。

### 12.1 左栏：240px 带文字侧栏 → 180px 图标栏

**读错在哪**：他说「左侧图标太多啦」，我给的是「减少图标数量 + 把剩下的配文字」。他要的分明是
「这栏就不该有那一片导航文字」——**图标太多**说的是**这一栏的存在方式**，不是计数。
`logo 旁边没有品牌的名字` 和它也不是一回事：品牌名要出现，**导航文字不要叫同一个名字**。

改成三块，三者都居中：

| 位置 | 内容 |
| --- | --- |
| `.rail-top > .brand` | 标记 + 站点名（`site_name`，出厂默认 `Boop`） |
| `.rail-top > .nav` | **恰好 5 项**：首页 / 搜索 / 文章 / 摄影 / 收藏 |
| `.rail-foot` | 外观开关、站长后台（仅站长）、退出登录 / 登录 |

- **导航文字回到无障碍树**：`.rail-left .nav-item .label` 加进 `.sr-only` 选择器组，
  每一项另带 `title`。读屏与鼠标都知道那是什么，版面上不出文字。
- **品牌名不在此列**：它是这一栏唯一的可见文字，理由是「这是哪个站」不属于图标能表达的事。
  这两条是两条独立的规矩，写进同一个测试的两个断言里，谁挪动都会红。
- `.rail-head` / `.rail-toggle` 删掉。外观开关回到 `.rail-foot`，与账号项**同形**
  （整行 46px 圆角块）——它是开关不是页面，所以不占导航的一格。
- `--rail-w` 240 → **180**。图标栏不需要 240。180 + 600 + 336 = 1116 ≤ 1240，
  180 + 600 = 780 ≤ 900，所以 ≤899 与 ≤640 两道断点**不动**（后者解决的是 600px 中栏
  在 375px 视口里的横向溢出，与左栏宽度无关）。
- `.nav-item` 改成图标居中、当前项用 `--accent-soft` 底色表达——**没有文字可以加粗了**。

### 12.2 品牌名：「高级艺术」= 系统衬线栈，不下载字体

栈是 `"Didot","Bodoni 72","Iowan Old Style","Palatino Linotype",Georgia,"Songti SC",
"STSong","Source Han Serif SC","Noto Serif CJK SC","SimSun",serif`，19px / weight 600 /
`letter-spacing:.01em`。衬线 + 一点点字距，比无衬线加粗更像一本刊物的刊头。

**为什么不自托管字体**：站点名是站长自己填的任意文本。拉丁字体可以按字符子集裁剪，CJK 不行——
思源宋体一类只能整包（5MB 起），而这是台 1 核 500MB、且**不向任何外部域发请求**的机器。
所以走系统已有的衬线（macOS 的 Songti SC / Didot、Windows 的 SimSun、Android 的
Noto Serif CJK）。这是**取舍，不是遗漏**：如果哪天他愿意付体积的代价，可以只嵌拉丁子集，
对中文站名仍然无效。

**站名的来源没变**：左栏继续读 `site_name`。查了一遍 `internal/settings/settings.go`，
`Defaults()` 里的出厂值本来就是 `SiteName: "Boop"`（第 150 行），站长在后台「站点」里能改——
所以「默认 Boop + 后台可设置」是**零代码改动**，我只是把测试站点被改过的 `site_name`
PATCH 回 `Boop`，让他看见默认值。

### 12.3 测试

`TestTheFrontRailNamesTheBrandAndKeepsFiveDestinations`（即 §11 那个测试改名）改动两处：

- CSS 断言**反向**：切出 `.sr-only` 到第一个 `{` 之间的选择器组，断言**含**
  `.rail-left .nav-item .label`（导航不出文字）、**不含** `.rail-left .brand-name`
  （品牌名必须可见）。
- 断言 `.rail-left` 里有 `<span class="brand-name">`（品牌名在 DOM 里，不是被藏了）。

**写这个测试踩的一脚**：`.rail-left .nav-item .label{` 不能直接跟在 `.sr-only` 后面写成
`.sr-only .rail-left .nav-item .label{`（那是后代选择器，永远不匹配）；而如果写成
`.sr-only,` 换行 `.rail-left .nav-item .label{}`，那个多出来的 `{}` 会变成一条空规则、
把紧随其后的 `:where(a,button,input):focus-visible` 挤到别处去。正确写法是**同一组、逗号分隔、
只留一个 `{`**。
`TestSignedInPagesOfferSignOut` 的注释里 `700px` → `899px`（上一轮漏改的注释）。

### 12.4 门禁与真机

`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check web/static/app.js` 干净、
`go test -race -count=1 ./...` 全绿（`internal/server` 196.4s）。模板走 `web/embed.go` 嵌入，
所以改完必须**重建二进制**再换进测试站点。

真机（8097）：左栏目视 `[b] Boop`（衬线）+ 5 个纯图标 + 底部 3 个控件（月亮 / 齿轮 / 退出），
375px 顶栏的品牌名同样是衬线。截图 `site-v2-home-1440.png`、`site-v2-home-375-full.png`、
`site-v2-article-1440.png`、`site-v2-darkdark.png`（在 `.workbuddy-ai/screenshots/`）。

### 12.5 `baedf66` fix: set the brand name larger and give it the mark's blue dot

他看了 12.1 那版的截图说：`字体不好看 和logo不搭`。

**病根不是「衬线不对」，是字号太小**。高对比衬线的粗细对比极大，Didot 在 19px 上最细处只有约
**0.7 个像素**，屏幕把它四舍五入抹掉之后整行字是**发虚、发灰**的——「高级」在字面上就是
**笔画的确定性**，发虚立刻显廉价。这是排版上的常识（display 字体不能小尺寸用），我上一版
正是踩在这上面：选对了字体、用错了字号。

改三处：

- `font-size: 19px → 23px`、`weight: 600 → 700`、`letter-spacing: .01em → -.005em`。
  23px 让细笔画回到 1px 以上；**粗笔画约 2.5px，正好等于那块 34px 标记里单线的实际粗细**
  （描边 1.8/24 × 34 ≈ 2.55px）。字与标记因此落在同一个笔画重量上——这才是「搭」，
  不是「都用衬线」就叫搭。
- `.brand-name::after`：一颗 5px 的蓝点（`var(--accent)`），取的就是标记里那颗蓝点。
  **做成伪元素而不是写进文字**，所以站长把站名改成任何别的内容这点都还在，模板不用动。
  他说的「不搭」有一半是从这儿来的：标记有蓝点，文字什么都没有。
- `.tb-brand .brand-name` 18px → 21px（移动端顶栏同一个道理）。

**选型过程没有留在脑子里**：`.workbuddy-ai/screenshots/brand-specimen.png` 与
`brand-specimen2.png` 是两张比选图——十一套方案、每张卡上半是 180px 真实左栏、下半是放大三倍
的笔画，第二张还带一行深色对照与一句病根说明。以后要换字体不用再从零试。

**关于「手绘单线 wordmark」**（比选图里的方案 I）：把 Boop 四个字母用与 logo 完全同一支笔画出来
（`b` 直接用 logo 的字形，`o`/`o`/`p` 是同一个 bowl 半径、同一条基线），是 100% 「搭」的做法，
也在图上摆给他看了。**没有采用**，因为绑死默认名：站长改了站名就只剩字体回退，等于引入第二条
代码路径换一个只在默认名成立的效果。留作备选。

### 12.6 门禁与真机（`baedf66`）

`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check` 干净、`go test -race -count=1 ./...`
全绿（`internal/server` 197.2s）。重建二进制换进测试站点（8097），4 倍放大截取左栏品牌区
（`site-v3-railzoom.png`）：`[b] Boop•` —— Didot 23px/700 的衬线锋利不发虚，蓝点落在 x-height
中线靠上，与标记那颗点呼应；375px 顶栏、深色（`site-v3-darkdark.png`）同样正常。

---

## 13. 第十轮：站名的字体换成他给的那枚字标（`e3a7e74`）

### 13.1 他说了什么

> `@/Users/versior/Desktop/boop.svg boop的字体全部用这个logo`

也就是：他给的那份矢量稿（一个几何风、带切角的「Boop」字样）不只是 logo，要当成**站名的唯一字形**用。
上一轮 §12 里我把「手绘单线 wordmark」列为方案 I 留作备选，理由是「绑死默认名」——这一轮他直接把
现成的字标给了出来，那条顾虑从「值不值」变成了「必须处理的问题」，所以这轮的全部分量都在**退路**上。

### 13.2 字标能替的只有它自己拼的那个词

这是整轮唯一的设计约束，也是唯一会让人做错的地方：**字标是「Boop」这四个字母的图形**，
它不是一个字体。站长把站名改成「少爷的博客」，页面顶上仍然画着一枚写着 Boop 的字标，
那就是在对一个不属于自己的名字招人。

所以两副面孔，由 `pageView.SiteWordmark` 挑：

```go
SiteWordmark: values.SiteName == settings.Defaults().SiteName,
```

- **出厂名** → 画 `#i-wordmark`（内联 sprite symbol），站名照留一份在无障碍树里（`.sr-only`）：
  那枚 svg 是 `aria-hidden` 的，不留真文字，外面那个 `<a>` 就没有名字了。
- **改过名** → 退回 `品牌方块标记 + 站名的排版`（就是 §12 那一套衬线），`brand-mark` 片段
  才输出那块 34px 方块。

判定写成 `== settings.Defaults().SiteName` 而不是 `== "Boop"`：出厂名将来若改，判定与文案
不会各自漂移。测试夹具的站点名正是 `Boop`（`newContentFixtureWithConfig` 不写 settings，
`Load` 从 `Defaults()` 起手），所以默认路径就是字标那条。

**字标不配方块标记**。在 `wm-lockup.png` 那张比选图上把三种做法摆在一起（只放字标 / 标记 + 字标 /
标记在上字标在下），选定「只放字标」：字标自己就是一整套字形，前面再顶一个带「b」的方块，
等于把同一句话起两次头。

### 13.3 两个模板片段必须写在 base.html 最外层

`brand-mark` 与 `brand-type` 写在 `base.html` 的**第一行之前**（`base` 那个 define 之外）。
原因不是整理癖：**Go 模板不允许 define 套 define**。第一版把两段插在文件中间（也就是
`base` 这个 define 体内），模板一解析就报：

```
template: base.html:133: unexpected <define> in command
```

而 `base.html` 除这两段之外整份都在 `base` 一个 define 里，所以这两段只能待在文件最外层——
写在最前面比写在最后面好读：先看两段共用的碎片，再看页面骨架。这条已经写进代码注释与
`docs/API.md`，因为它是那种「下一个人顺手挪进去就炸」的结构。

### 13.4 踩到一个只有真正渲染才看得见的坑（这一轮的重点）

代码改完、DOM 断言全绿之后上真机，**左栏品牌区一片空白**。查下来是：

- 元素确实在、尺寸也对（`getBoundingClientRect()` 给了 120 × 45.3、位置居中）；
- `fill` 解析成 `rgb(15,20,25)`、symbol 里 6 条 path 一条不少；
- 单独把这枚 svg 内联进一个测试页（`wm-lockup.html`）**渲染得好好的**。

根因是 `<use>` 的视口规则：`<use>` 的 `width/height` 默认 100%，百分比落在**外层用户坐标系的
(0,0)** 上；symbol 的内容再按 symbol 自己的 viewBox 缩放进那块视口。原来 symbol 的
viewBox 是 `245 346.9 900.5 340.1`（为了贴紧字形，把原文件那一大圈空白 margin 去掉量出来的），
而外层 `use` 的视口在 (0,0)~(900.5,340.1)——**可见区域在 y 346.9 以下，字全被画在了视野上方**。

修法是让原点归零，和 sprite 里其他 18 个 symbol 保持一致：

```html
<symbol id="i-wordmark" viewBox="0 0 900.5 340.1">
  <g transform="translate(-245,-346.9) translate(0,1040) scale(0.1,-0.1)">
```

`translate(-245,-346.9)` 就是把原来的原点减掉，**站长给的路径数据一个字没动**。

教训值得单独记：这类缺陷 DOM 断言抓不到（元素在、类名对、尺寸对），只有真截图才看得见；
而「内联一份到测试页看效果」会**骗过自己**——内联的 svg 没有 `<use>`，也就没有这条规则。
所以这轮同时加了一条能抓到的断言：

```go
if !strings.Contains(page(nil), `<symbol id="i-wordmark" viewBox="0 0 `) {
    t.Error("the wordmark symbol has a non-zero viewBox origin, which hides it when drawn through <use>")
}
```

### 13.5 尺寸与颜色

| 落点 | 宽度 | 依据 |
| --- | --- | --- |
| 前台左栏 | 120px | 硬上限是 180 − 两侧各 14（`.rail-left` 内边距）− `.brand` 两侧各 8 = **136px** |
| 移动端顶栏 | 92px | 与 54px 的顶栏成比例 |
| 后台侧栏 | 108px | 与「站长后台」那一行叠成一列 |

只定宽、`height:auto`（2.65:1 推出来），`fill:var(--ink)`——深浅两档主题各自取自己那份，
**不需要另写一条深色规则**。`.admin-brand-text` 补 `gap:2px`：字标「p」有一条下伸的腿，
紧贴下一行汉字会糊在一起。

### 13.6 测试

`TestTheFrontRailNamesTheBrandAndKeepsFiveDestinations` 现在**按顺序**断言两副面孔：

1. 出厂名下：左栏有 `<svg class="wordmark"` 与 `<span class="sr-only">Boop</span>`，
   且**没有** `class="brand-name"`、**没有** `class="brandmark"`；
2. `PATCH site_name=少爷的博客` 之后：`<span class="brand-name">少爷的博客</span>` 回来、
   `class="wordmark"` 消失、`class="brandmark"` 回来。

第 2 条是这轮真正要钉的：默认名那条路每张截图、每次跑测试都会经过，退路却可能悄悄烂掉。
夹具里原本就有一条改名断言（`handlers_settings_test.go` 的 `<span class="brand-name">少爷的博客</span>`），
它正好落在退路上，也一并继续守着。

### 13.7 门禁与真机（`e3a7e74`）

`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check` 干净、`go test -race -count=1 ./...`
全绿（`internal/server` 198.6s）。重建二进制换进测试站点（8097），截图：

- `wm3-rail.png`（4 倍放大）字标笔画清晰、在栏内居中；`wm3-home-1440.png` 整页看像一枚刊头；
  `wm3-home-375.png` 顶栏 92px 与图标齐肩；`wm3-dark-1440.png` 深色下自动转浅色；
  `wm4-admin.png` 后台侧栏字标 + 「站长后台」。
- **两面都走了一遍真路**：`PATCH site_name=少爷的博客` → `wm4-renamed.png` 显示方块标记 +
  衬线站名（五个汉字起以省略号收尾），`PATCH site_name=Boop` → 字标回来。站名已还原。

已知代价（写进 `docs/PRODUCT.md`）：180px 的左栏扣掉边距只剩 135px，「34px 标记 + 长站名」
放不下，超出的部分以省略号收尾。这是图标栏换来的，**不是字号问题**——把字号调小只会回到
§12 那个「细笔画发虚」的坑里。

---

## 14. 第十一轮：删掉整个站内搜索（`b6d4c39`）

他的原话是两句：先「搜索图标不要在左侧栏」，再（看到三个选项后没有选，而是把范围放大）
「搜索按钮清除 前端搜索功能不要并且清除相关代码」。

### 14.1 为什么不是一个按钮

左栏那枚放大镜只是露在外面的一个角。删掉它之后，桌面上再没有任何一条能走到 `/search`
的路——而站内搜索本身还在：一个留着页面、API 和全文索引、却没有任何入口的检索功能。
这种「看不见但活着」的功能比看得见的死按钮更糟：它继续在每次写入时触发触发器，继续
被 robots.txt 排除，继续在文档里承诺一件做不到的事。所以删的是**一整个功能**。

### 14.2 一次改动要牵到哪几层

| 层 | 删掉的东西 |
| --- | --- |
| 前台模板 | 左栏导航 5 项 → 4 项；移动端底部导航去掉那项；sprite 里的 `#i-search`；`web/templates/search.html` 整个文件 |
| 前台样式 | `app.css` 第 7.5.1 节（`.search-*` 与 `.result*`，47 行）整段 |
| 后端路由 | `GET /search`、`GET /api/v1/search`、`/api/v1/search/` 三条；`parsePages` 名字表里的 `"search"` |
| 后端代码 | `internal/search` 整包（`search.go` 496 行 + `search_test.go` 635 行）、`handlers_search.go`（213 行）与其测试（479 行） |
| 发现文档 | `robots.txt` 的 `Disallow: /search`、`isPrivatePath` 里的 `/search` |
| 数据库 | 迁移 005：`DROP TABLE post_search` + 三个 `DROP TRIGGER` |

净结果是 **+179 −2135**。

### 14.3 迁移 005 为什么不改 002

已发布的迁移一字不动，这是这个仓库从第一轮起就有的规矩：任何时刻的库状态都要能从零
重放出来。所以 002 里那段 `CREATE VIRTUAL TABLE post_search ...` 原样留着，删的是新增的
005，用 `DROP ... IF EXISTS` 写，对「全新安装」和「已有库升级」都成立。

删得掉的理由写进了 005 的注释：external-content 的 FTS5 表是**纯派生数据**，任何时刻都
能由 `posts` 重建；而它的三个触发器不是——它们会在每次 `posts` 的 insert/update/delete
时触发。一张没人读的表可以慢慢烂在原地，三个没人需要却每次都跑的触发器不行。

### 14.4 测试改的是「断言什么」

- `store_test.go` 不是从表名列表里划掉 `post_search` 就完事，而是**断言它和三个触发器
  不存在**（`schemaObjectCount(t, db, "table", "post_search") == 0`）。删掉一张表，和真的
  不再依赖它，是两件事，测试钉的是后一件。
- `TestBottomNavOffersSearch` 换成 `TestBothBarsMarkTheCurrentDestination`：四个前台目的地
  逐个查两条栏各标记一次当前项。**计数写在每条栏内部**（`navBlock(..., "nav")` /
  `navBlock(..., "bottom-nav")`），不是整篇文档——信息流的类型行也会标一次自己，那不是
  任何一条栏。
- `TestTheFrontRailNamesTheBrandAndKeepsFiveDestinations` → `...KeepsFourDestinations`，条目
  数断言 5 → 4。
- 删 `handlers_search_test.go` 时暴露了它顺手定义的 `insertPost` / `softDeletePost`：
  `handlers_feed_test.go`、`handlers_seo_test.go`、`metadata_test.go` 三个文件都在用。
  这两个助手搬到 `handlers_content_test.go`，只挪位置、不改内容。

### 14.5 顺手修掉的两处旧注释错误

这两处都不是这次改坏的，是这次改动让它们露出来的：

- 断点注释写「前台 240 + 600 = 840」，而左栏早就不是 240 而是 180（`--rail-w`），实际是
  180 + 600 = 780。断点值 899 不动（更保守，仍然正确），只把数字改成事实。
- 左栏小节注释写「五项而不是七项」，改成「四项」。

### 14.6 门禁与真机（`b6d4c39`）

`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check` 干净、`go test -race -count=1 ./...`
全绿（`internal/server` 194.4s）。这里有一次真实的返工：第一版 `TestBothBarsMarkTheCurrentDestination`
断言整篇文档只有 2 个 `aria-current="page"`，首页与筛选页实际是 3（两条栏各一 + 类型行一），
测试如实报错，于是把计数收进每条栏内部。

真机（8097，换二进制重启，**用已有的库**）：

| 检查 | 结果 |
| --- | --- |
| 迁移 | `schema_migrations` 由 `1,2,3,4` → `1,2,3,4,5`；`post_search` 与三个触发器消失 |
| 数据 | `posts` 5、`users` 2、`comments` 2、`assets` 6、`settings` 15——升级前后逐表相同 |
| 健康 | `/healthz` 200、`/readyz` `ready` |
| 路由 | `/search` 与 `/search?q=x` → **404 HTML**；`/api/v1/search` 与 `/api/v1/search/x` → **JSON 404** `not_found` |
| 未受影响 | `/`、`/?type=article`、`/?type=photo`、`/login`、`/register`、`/feed.xml`、`/robots.txt`、`/sitemap.xml` 全 200；`/bookmarks` 对游客 303 |
| robots.txt | Disallow 只剩 `/admin`、`/api/`、`/auth/`、`/bookmarks`、`/login`、`/register` |
| 左栏 DOM | `<nav class="nav" aria-label="主导航">` 内**恰好 4 个** `nav-item`（控件组那两个不在其中）；`href="/search"` 0 处；`id="i-search"` 0 处 |
| 截图 | `search-rail-full.png` 四个图标（首页高亮）；`search-bottomnav2.png` 移动端底部五项（首页/文章/摄影/收藏/登录） |

---

## 15. 第十二轮：首页作者头部照他给的截图重排（`a8a51a2`）

### 15.1 他说了什么

`主页信息排版按这个图片`，附一张 X 个人主页截图。他对范围只有一句补充：`关注那一行不要 改成自我简介`。

### 15.2 截图与旧版差在哪

旧版作者头部（§10 那一轮做的）是一条横排：封面图一条横带，头像与文字**并排**（`align-items:flex-end`），头像被卡在封面下沿上，名称只能挤在它右侧那半行里，站名越长越先被挤断。

截图里头像是一块**独立分区**：单独占一行、压住封面图下沿，下面的名称、简介、事实都比它窄、与它左边缘对齐。所以横排改竖排，容器 `display:flex;flex-direction:column;align-items:flex-start`。

尺寸照截图比例定，没有一处在截图里能直接量出来，是把截图里各块占中栏宽度的比例折到中栏 600px 上：

| 项 | 旧 | 新 | 依据 |
| --- | --- | --- | --- |
| 头像 | 84px | **104px** | 截图里头像约占栏宽四分之一 |
| 压封面 | -42px | **-52px** | 约为自身一半 |
| 头像描边 | 4px | **5px** | 与头像同比例放大 |
| 名称 | 20px | **22px** | 截图里名称比站名标识明显高一级 |
| 简介行高 | 1.5 | **1.55** | 同名 |
| 事实行 | 纯文字 | **带图标** | 见 15.3 |

竖排之后有一个 `align-items:flex-start` 带来的新问题：`.author-meta` 不给宽度就按内容宽度走，长简介会横着溢出栏宽。修法是 `align-self:stretch`（**不是 `flex:1`**——这里是竖向 flex，`flex:1` 管的是高度）。

### 15.3 事实行加图标

三件事（RSS 订阅 / 始于 / 最近更新于）原先是一串纯文字，读起来像一句被空格切碎的句子。各配一枚图标之后它们变成三个并列的条目，也正是截图里那一行的形态。新增两枚 sprite symbol，图形语言沿用这套精灵的 24 网格 / 只描边 / 1.8 描边：

- `#i-rss`
- `#i-calendar`（「始于」与「最近更新于」共用）

两个只有真正渲染才看得见的细节：

1. **订阅那颗点得自己写死填充。** `.ic` 的基类是 `fill:none`，只给 `circle` 描边会得到一个小圆环，看着像「有一个附件」而不是「一个订阅源」。所以它显式写 `fill:currentColor;stroke:none`。
2. **图标颜色要跟着链接色走。** 订阅项是 `.author-rss`，橙色字；若图标留 `--muted-2` 就成了「橙字 + 灰图标」。`.author-rss .ic-fact{color:currentColor}`。

没有造 `#i-at`、`#i-pin` 这类图标——站上既没有「所在地」也没有「个人网站」这两个字段，造了就是空壳。

### 15.4 他明确不要的那一行

截图里「关注中 / 关注者 / 获赞」那一行整块不存在，那行的位置改为简介。这与 `docs/PRODUCT.md` §7 一致：Boop 只有一个作者、没有关注关系。所以这一轮**没有新增任何计数**——测试里原有的 `关注` / `粉丝` / `合集` 三个反向断言原样留着，就是钉这一点的。

### 15.5 CSS 一条顺序上的坑

```css
.author-id .avatar{width:104px;height:104px;border:5px solid var(--bg);background:var(--bg)}
.author-head.has-cover .author-id .avatar{margin-top:-52px}
```

这两条**同特异性**（都是 0,3,0），后写的胜。顺序颠倒时 `margin-top:-52px` 会被上面那条的 `width/height/border` 覆盖掉——注意不是被覆盖，是两条各自生效、但 `margin-top` 那条写在前面时它的声明会被后面的规则整体重排覆盖。总之结果是头像不再压封面，而**所有 DOM 断言全绿**。所以第二条必须写在第一条之后，这一笔在文件里留了注释。

### 15.6 测试

`TestHomePageAuthorHeaderAndTypeRow` 三处收紧：

1. 事实行断言改成带图标的形式（三条各自的完整标签）。
2. 补 `#i-rss` / `#i-calendar` 两枚 symbol 的 `viewBox="0 0 24 24"` 断言——`<use>` 指到 viewBox 原点非 0 的 symbol 会整幅画到视野外，而其余断言全绿。这个坑 §13.4 已经付过一次代价。
3. 从 `/static/app.css` 读三条布局规矩：

```go
`.author-id{display:flex;flex-direction:column`,
`.author-id .avatar{width:104px;height:104px;`,
`.author-head.has-cover .author-id .avatar{margin-top:-52px}`,
```

markup 分不出「并排」与「上下」，只能读样式表。这是这一轮新增的测试思路：**当断言的是一句布局意图时，被测对象在 CSS 里而不是 HTML 里。**

### 15.7 门禁与真机（`a8a51a2`）

`gofmt -l .` 无输出、`go vet ./...` 无输出、`node --check web/static/app.js` 干净、`go test -race -count=1 ./...` 全绿（`internal/server` 190.7s）。这一轮没有返工。

真机（8097，换二进制重启、**用已有的库**）：`author-head has-cover`、头像 104×104、`og:image` 指向当前封面、`twitter:card` 仍是 `summary_large_image`；截图 `authorhead-1440.png` / `authorhead-375.png` 两端无裁切。另用 `/tmp/boop-verify/setcover.sh` 临时清空 `site_cover_url` 复验 `is-plain` 分支（16px 内边距、头像不上移），截图 `authorhead-plain-1440.png`，**验完已还原封面**。

---

## 16. 第十三轮：封面图与字标归档 + 与两笔并行提交的合并（`06169ec` `8cefc77`）

### 16.1 他说了什么

`所有的修改推送仓库更新` → 推完之后他追问 `全部的修改上传了？背景呢 logo呢` → 给出封面处置的选择：`归档并设为出厂默认（推荐）`。

这两问都问在了点上。**背景（封面图）当时确实没进仓库**：它只存在于测试站点的数据目录 `/tmp/boop-verify/site-data/uploads/…` 里，而那是临时目录——系统清理一次就没了，仓库里一条记录都没有。字标那枚 `boop-wordmark.svg` 同理，只在 `a8a51a2` 那一轮被内联进 `base.html`，源文件从来没入过库。

### 16.2 第一次推送被拒：远端有两笔并行提交

`git push` 返回 `Updates were rejected because the remote contains work that you do not have locally`。`git fetch` 之后看到 **Versiorii 推了两个提交**：

| 提交 | 内容 |
| --- | --- |
| `aab0581` | 前台左栏改成只剩标记（`--rail-w` 180px → 68px） |
| `6e519d1` | 加了一张**自己画的** `web/static/brand/boop-cover.svg`，由模板 `{{if}}else` 兜底当默认封面 |

处置顺序：先 `git branch backup/pre-parallel-merge` 保住我在推的那个提交，再 `git reset --hard origin/main` 换成新基线，然后**逐项重新评估**我的改动能不能落上去——不是机械 rebase，因为两边的活儿有重叠。

`aab0581` 只把**前台左栏**改成只剩标记；`brand-type`（字标）在**移动端顶栏与后台侧栏仍在用**，所以 `#i-wordmark` 不是孤儿，字标源文件该归档、注释该留。

### 16.3 同一个目标的两套做法：封面之争

两笔提交都想做「新站点自带一张封面」，路子完全不同：

| | `6e519d1`（远端） | 这一轮 |
| --- | --- | --- |
| 图 | 自己画的深色信号场 SVG（678 字节） | **他给的那张 PNG**（1200×400，73,465 字节） |
| 落点 | 模板里的 `{{if .Header.CoverURL}}…{{else}}…{{end}}` | 设置层的默认值 `settings.DefaultSiteCover` |
| 「不要封面」 | **无法表达**——清空这一栏仍然会回落到那张 SVG | 清空即 `.is-plain`，横带整条不渲染 |

他选的 `归档并设为出厂默认` 指的是**他给的那张图**，所以取 PNG 这一版。这里有一个不能让步的点：**`is-plain` 必须留着**。封面的默认值一旦落在设置层，新站点的初始状态就是「有封面」，此时如果把模板兜底也留着或把 `is-plain` 删掉，站长就没有任何办法表达「我不想要这张横幅」——一个设置项永远无法被关掉。两套做法各自的注释与文档也互斥（`6e519d1` 写的是「站长未配置封面时使用内置品牌封面」），一并改掉。

那份自画的 SVG 因此成了**重复的第二个「默认封面」**，`8cefc77` 把它删掉：仓库里同时躺着两个都自称默认的东西，下一个人一定会用错。它还在 `6e519d1` 里，想要回来一条 revert 就够。

### 16.4 出厂默认是三处配合，缺一处都不成立

**① 默认值必须写成站点自己的路径，不能写绝对地址。**

播种发生在还不知道部署域名的时刻（`settings.Seed` 只拿到 `db`，拿不到 `cfg.BaseURL`），写死绝对地址会把**播种那一刻的域名焊进每一个新装**。所以 `DefaultSiteCover = "/static/brand/boop-cover.png"` 是一条裸路径。

**② `/static/` 下的资源带一年 `immutable` 缓存，裸路径必须补版本号。**

```go
func staticURL(image string) string {
    if !strings.HasPrefix(image, "/"+staticDirPrefix) {
        return image
    }
    return image + "?v=" + staticAssets().version
}
```

平时这个版本号由模板拼成 `?v={{.StaticVersion}}`；出厂封面是一条**裸路径**，渲染时若不补，将来换图会被老访客的浏览器钉住一年——而页面看起来一切正常，是最容易漏的一处。只给 `/static/` 前缀补，其余原样穿过：绝对地址属于别人的缓存策略，`/uploads/…` 由它自己的处理器按文件名寻址，版本查询对它没有意义。

**③ 后台那三个地址框从 `type="url"` 改成 `type="text"`。**

`type="url"` 会在浏览器端拒绝提交相对地址，而页面打开的默认值正好就是一条路径——也就是标准流程「打开设置页、什么都不改、点保存」会被**静默拦下**。真正的校验在服务端，这个属性只制造了这一种失败，没有换来任何保护。

### 16.5 校验的线画在哪

`validateAssetURL` 从此接受两种形态：**无凭据的 http/https 绝对地址**，或**以单个 `/` 开头的本站路径**。后者是必需的——上传接口返回的就是 `/uploads/2026/09/<hash>.png`，只收绝对地址等于要求站长把自己站点的域名拼在接口刚给他的那串路径前面。

仍然拒绝的三种，每一种都对应一个具体的绕过方式：

| 形态 | 为什么必须拒 |
| --- | --- |
| `//host/…` 协议相对地址 | 看着像本站路径，浏览器按跨域解析 |
| 反斜杠 `/\host/…` | 浏览器把反斜杠当斜杠用，从上一条的缝隙里穿过去 |
| `data:` / `javascript:` / `blob:` | 这三个值会进 `src`/`href`，在 scheme 那一步出局 |

顺带修掉一处旧注释的残缺：原注释第三行直接接上了新段落的第一行，读起来是一句半截话。这一轮把整段文档注释重写了。

### 16.6 测试

- `internal/settings` 的校验表：`relative` 三条由**拒绝**翻成**接受**，补 `//host`、反斜杠、含空格的路径、`data url` 四条仍须拒绝的用例；默认值与播种断言改用 `DefaultSiteCover`；再补一条「出厂默认这个值本身合法」。
- 后台接口的 `reject` 表：`relative` 三条换成三条协议相对地址 + 一条 `data url`。
- 新增 `TestPatchSettingsAcceptsAPathOnThisSite`：钉住**两种渲染后果的区别**——`/static/` 下的路径补 `?v=`，`/uploads/` 下的路径原样渲染不加，而 `og:image` 两种都要补成绝对地址。
- 新增 `TestANewSiteShowsTheShippedCover`：不只看 DOM，还**真的 GET 了那张图**核对 200 / `image/png` / 非空。地址写错只会渲染成一张破图，而「`src` 里有地址、类名是 `has-cover`、元素存在」这三条断言**全绿**。
- `TestHomePageAuthorHeaderAndTypeRow` 里清空封面之后的那个分支改成断言 `is-plain`（横带不渲染、旧地址不出现），并从 `/static/app.css` 多读一条 `.author-head.is-plain .author-id{padding-top:16px}`——没有横带时头部得自己撑出上边距，否则头像贴着栏顶。
- `TestIndexPreviewPrefersTheCoverImage` 改成**先验出厂封面**（带 `?v=` 且是绝对地址）再清空走头像这条链；`TestIndexHeadCarriesCanonicalAndSiteDocument` 的 `twitter:card` 由 `summary` 改成 `summary_large_image` 并补 `og:image` 断言——新站点的初始状态已经是「有封面」了。
- `boop-wordmark.svg` 加进了 `static_test` 的取样列表，否则它是一枚没人引用的孤儿文件，哪天从 embed 里掉出去也没人知道。

### 16.7 门禁与真机

两笔各自跑一次完整门禁，都全绿：`gofmt -l .` 无输出、`go vet ./...` 无输出、`go test -race -count=1 ./...` 全绿（`internal/server` 196.0s / 198.7s）。

真机（换二进制重启，两台实例）：

| 场景 | 结果 |
| --- | --- |
| 8097 已有库，重启前后逐行比对 | `settings` **15 行逐字节相同**、迁移 `1-5` 不变；已有的绝对地址头像与图标原样留着 |
| 8097 首页 | `author-head has-cover`、`src="/static/brand/boop-cover.png?v=945fe9949f87"`、`og:image` 是绝对地址且带版本号、`twitter:card` = `summary_large_image` |
| 8097 `GET` 出厂封面 | `200 image/png` **73,465 字节**，sha256 与仓库里那份**一致** |
| 8098 全新库 | `site.cover_url = "/static/brand/boop-cover.png"`，其余 14 个键都是文档默认值 |
| 后台清空封面 | `200` → 首页 `is-plain`、横带消失、`og:image` 回落到头像 |
| 后台写入 `/uploads/2026/09/<hash>.jpg` | `200` → `has-cover`，`src` **原样**渲染不带 `?v=`，`og:image` 补成绝对地址 |
| `//evil.example.com/c.png` | `400` `must not be a protocol-relative address` |
| `/\evil.example.com/c.png` | `400` `must not contain a backslash or a space` |
| `data:image/png;base64,AAAA` | `400` `must use http or https, or start with / for a path on this site` |
| `/a b.png` | `400` `must not contain a backslash or a space` |
| 后台设置页 | 三个地址框都是 `type="text"`，封面框预填 `/static/brand/boop-cover.png`，提示文案两行内不溢出 |

截图：`cover-shipped-1440.png`、`cover-shipped-375.png`、`cover-plain-1440.png`、`cover-admin-1440.png`。

### 16.8 提交数

`a8a51a2`（§15）+ `896e17f`（四份报告入库 + 忽略 `.workbuddy-ai/`）+ 本轮的 `06169ec`、`8cefc77` = 本地领先一度到 **4 个提交**，全部已推送。`origin/main` 现在是 `8cefc77`。

---

## 附：如何复核

```bash
git log --oneline 113ef8f..HEAD          # 第一轮（P0 清单）9 个提交
git log --oneline 08b01cb..HEAD          # 第二轮（本文 §5）4 个提交
git log --oneline c4fdac8..HEAD          # 第三轮（本文 §6）1 个提交
git log --oneline 2957a30 -1             # 第四轮（本文 §7）1 个提交
git log --oneline 1cb379e -1             # 第五轮（本文 §8）1 个提交
git log --oneline 36d3f73 -1             # 第六轮（本文 §9）1 个提交
git log --oneline 8b4795a -1             # 第七轮（本文 §10）1 个提交
git log --oneline 8871baa -1             # 第八轮（本文 §11）1 个提交
git log --oneline 3f602a3 -1             # 第九轮（本文 §12）1 个提交
git log --oneline 3f602a3~1..baedf66    # 第九轮的两个提交：先改回图标栏，再把品牌名放大加蓝点
git log --oneline baedf66..e3a7e74      # 第十轮（本文 §13）1 个提交：站名换字标 + 改名退路
git log --oneline e3a7e74..b6d4c39      # 第十一轮（本文 §14）1 个提交：删掉整个站内搜索
git log --oneline b6d4c39 -1             # 第十二轮（本文 §15）1 个提交：首页作者头部照截图重排
git log --oneline a8a51a2..8cefc77       # 第十三轮（本文 §16）3 个提交：报告入库、站内路径、出厂封面
git show --stat <commit>                  # 单个提交的改动面
git show <commit>                         # 含提交信息里的实测数据
```

每个提交的信息里都写了「为什么这么做」和实测输出；`docs/API.md`、`docs/DATABASE.md`、`AGENTS.md` 已同步更新契约变化（发现文档、页面元数据、静态资源缓存、索引、CI 门禁，以及 §5 的搜索两条路径、退出登录入口、地址编码、`PATCH` 的 `updated_at`，§6 的右栏构成与 `docs/PRODUCT.md` 的两处措辞，§7 的后台导航与设置分类，§8 的存储分类、环境变量只播种一次与整组校验，§9 的后台页头剥掉模块行与「上传目录」这处已过时的说法，§10 的首页作者头部与 `site.cover_url`、首页类型行的计数规则与 `docs/API.md` 里的索引预览优先级，§11 的左栏形态与断点、作者头部的两条日期与 `content.PublishedSpan`，§12 的左栏终态（180px 图标栏、
导航文字回无障碍树）、衬线品牌名的取舍与字号依据（`3f602a3` + `baedf66`，§13 的站名字标两副面孔与 `pageView.SiteWordmark`、`use` 引用 symbol 时 viewBox 原点必须归零这条坑，§14 的站内搜索整段移除——`docs/API.md` 的搜索 API 与页面契约改成「站内搜索已移除」并说明 `post_search` 的去向，`docs/DATABASE.md` 的 schema 快照删掉 FTS 表、补上 005 的说明，`docs/PRODUCT.md` §5.5 由「搜索与 RSS」改成「RSS」并把入口数由五改回四，`README.md` 的 SQLite 徽章与 FTS5 字样、`AGENTS.md` 的 Task 9 描述，§15 的作者头部新层级与「没有统计数字那一行」的理由——`docs/PRODUCT.md` §5.1 与 `docs/API.md` 的 HTML 页面章都改成了「头像单占一行并压住封面下沿 + 带图标的一行事实 + 明确没有计数行」）。
