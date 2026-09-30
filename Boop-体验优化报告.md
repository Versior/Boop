# Boop 体验优化报告

> 生成时间：2026-09-29
> 方法：① 通读代码建立事实基线 → ② 竞品资深用户视角简报（联网调研） → ③ 本产品资深用户视角简报（读码） → ④ 交叉核对与裁决
> 代码基线：本地 `main` @ `113ef8f`（已领先 `origin/main` 3 个提交，未推送）
> 补充：第二轮对标 Ech0（Go + SQLite 同类自托管项目）的读码分析见 [`Boop-对标-Ech0.md`](./Boop-对标-Ech0.md)，该文对本文的 P0 清单与备份/静态资源/阅读统计三项方案做了修订。
> **执行状态**：本文与对标文档列出的 P0 清单已执行，逐条结果、实测数据与过程中新发现并修掉的缺陷见 [`Boop-优化执行记录.md`](./Boop-优化执行记录.md)。

---

## 0. 摘要

**一句话结论**：Boop 的实现质量已经足够高，短板**全部在「作为一个写作工具」的功能面**，而不是在代码质量或架构上。它现在是一个**优秀的发布器**，还不是一个可以托付十年的**写作与内容管理器**。

三条判断：

1. **两份独立简报在同一个位置撞车**：竞品视角与本产品视角都把「没有草稿列表、没有任何编辑/删除入口」放在第一位。这不是巧合——`internal/server/handlers_admin_posts.go` 里 `POST/PATCH/DELETE` 三个接口早就齐了，`content.Update` 甚至带 `updated_at` 乐观锁，**只是没有页面**。这是全项目投入产出比最高的一处补齐。

2. **有一个真实的性能缺陷被两个视角同时低估**：`likes` 表主键是 `(user_id, post_id)`，但首页每张卡片都会执行 `SELECT COUNT(*) FROM likes WHERE post_id = p.id`。由于 `store.go` 把连接池钉在 `maxOpenConns = 1`，首页 20 张卡片意味着**在这唯一的连接上串行扫 20 遍 likes 全表**。目前数据量小所以察觉不到，它会随点赞数增长而线性恶化，属于「上线时无事、半年后莫名其妙变慢」的典型。

3. **Boop 真正的护城河不是功能，是「可 `cp` 走的十年」**：内容、评论、点赞、收藏、全文索引全在一个 SQLite 文件里，升级是换二进制，回滚是换回旧二进制，无供应商、无 Node/MySQL 组合、无需要单独部署的伙伴服务。竞品（Ghost）在结构上给不了这一点——它的方向恰好相反。**建议把这个特性当成产品身份来经营，而不是当成一句部署文案。**

**Top 5 行动**（全部不破坏现有工程铁律）：

| # | 做什么 | 成本 | 收益 |
|---|---|---|---|
| 1 | 迁移 004：`CREATE INDEX idx_likes_post ON likes(post_id)` | 分钟级 | 首页计数查询从全表扫描变索引查找 |
| 2 | `/static/*` 补 `ETag` + 长缓存，并加 gzip 中间件 | 小 | 首屏字节降 70%+，二次访问静态资源 0 字节 |
| 3 | 站长内容管理页 `/admin/posts`（草稿列表 + 编辑 + 软删除） | 中 | 从「发布器」变成「可维护的博客」 |
| 4 | `<head>` 补 `og:*` / `canonical` / JSON-LD，加 `/robots.txt` 与 `/sitemap.xml` | 小 | 外部可发现性，是「推荐给别人」的前提 |
| 5 | 评论/发布后不再整页 reload（保留阅读位置） | 小–中 | 每天都会撞到一次的体验毛刺 |

---

## 1. 现状基线

### 1.1 规模与形态（实测）

| 维度 | 数值 |
|---|---|
| Go 代码 | 25,247 行，10 个内部包（`ai` `auth` `config` `content` `media` `search` `secretbox` `server` `settings` `social` `store`） |
| 路由 | 57 条注册（`internal/server/server.go:107-163`） |
| 页面模板 | 9 个 + `base.html`，共 791 行 |
| 前端资源 | `app.css` 801 行 / `app.js` 781 行（各约 29 KB，无构建链） |
| 数据迁移 | 3 个（`001_initial` / `002_search` / `003_asset_content_hash`），不可修改 |
| 测试 | 约 367 个测试函数；`go test ./...`、`go test -race ./...` 全绿 |
| 产物 | 单二进制约 15–23 MB |

### 1.2 能力矩阵

| 已具备 | 已具备（容易被忽略，别重复造） |
|---|---|
| 邮箱+密码 / GitHub OAuth 登录 | 上传按 sha256 去重（`media.go` + 迁移 003） |
| 三种内容形态：动态 / 文章 / 摄影 | `/uploads/*` 已有 `Cache-Control: public, max-age=31536000, immutable`（`handlers_upload.go:171`） |
| 评论二级回复 + 审核队列 + 站长审核页 | FTS5 查询对语法字符免疫（引号包裹 + AND 连接） |
| 点赞、收藏（私有） | RSS 用窄查询且**刻意**不使用 `COALESCE` 以保住 `idx_posts_feed` |
| FTS5 搜索（高亮片段 + 游标分页） | AI 作者状态缓存刷新永不阻塞渲染（后台单飞） |
| RSS 2.0（`/feed.xml`） | CSP 全站禁绝 `unsafe-inline` |
| 站长设置页（站点名称/简介/头像/图标/时区/分页/开关/密钥/AI） | 图标导航保留无障碍文字（`.sr-only` clip + `title`） |
| AI 写作助手（摘要 / 标签 / SEO，只建议不写入） | 空态、错误态、`:focus-visible` 焦点环齐全；无 JS 仍可读 |

### 1.3 已核实的缺口

- **没有草稿列表页，没有任何编辑或删除入口**。`POST/PATCH/DELETE /api/v1/admin/posts` 只有 API；前端唯一的写入面是首页那个一次性的「快捷发布器」（`web/templates/home.html:11`）。
- 没有定时/预约发布。
- `<head>` 没有 `og:*`、`canonical`、JSON-LD；没有 `/robots.txt`、`/sitemap.xml`。
- **`/static/*` 没有任何缓存头**：`handleStatic`（`server.go:269`）只设 `Content-Type` 就直接 `w.Write`，因此每次页面导航都要重下约 58 KB 的 CSS+JS。
- 全链路**没有 gzip 压缩**（`internal/` 内除上传响应外无压缩相关代码）。
- 只有 RSS，没有 Atom、没有 JSON Feed。
- 没有邮件、订阅推送、Webmention、ActivityPub。
- 没有阅读数据统计。
- 详情页没有阅读时长、目录、相关阅读、上下篇。
- 没有多作者、没有主题系统。
- **过期 session 永不清理**：`internal/auth/session.go` 只在登出（:252）与轮换（:113）时按 token 删除，过期行会长期堆积（`idx_sessions_expires` 索引已就绪但没人用）。
- **搜索排序自废索引**且与 RSS 的讲究自相矛盾：`internal/search/search.go:130` 使用 `ORDER BY COALESCE(p.published_at,'') DESC`，而 `handlers_feed.go` 明确因为「包进表达式就用不上索引」而不套 `COALESCE`。
- 发布器**只能提交一张图**：`web/static/app.js:317` 写死 `body.asset_ids = [asset.id]`，而内容模型的 `AssetIDs` 支持多张、去重链路也已就绪。
- 详情页照片被裁切：`post.html:20` 复用 `.photo-art`，该类写死 `aspect-ratio:8/5; object-fit:cover`（`app.css:265`）。
- 移动端（≤700px）**没有搜索入口**：左栏与右栏全部隐藏（`app.css:774`），底部导航（`base.html:180`）五项里没有搜索。
- Task 10 未开始：无 Dockerfile / compose / Caddy 示例 / 备份恢复 runbook / E2E smoke / 性能门禁（空闲 RSS ≤150 MB、缓存首页 p95 ≤150 ms）。
- 工程配套：无 LICENSE、无 CI、`go.mod` 中 `goldmark`/`bluemonday`/`golang.org/x/net` 被误标 `// indirect`、`POST /feed.xml` 实际返回 403 而非注释声称的 405、上传图片不剥离 EXIF。

### 1.4 性能关键路径（读码结论）

| 路径 | 现状 | 判断 |
|---|---|---|
| 首页渲染 | 约 6 条查询 + `settings.Load` 全表读 + `ownerIdentity` 查 users | 可接受，待 p95 实测 |
| 首页卡片计数 | 每卡片一次 `likes` 全表扫描 + 一次带 EXISTS 的评论计数 | **likes 那条是真问题**（见 §5 P0-1） |
| **连接池** | `store.go:34` `maxOpenConns = 1`，DSN 带 `_txlock=immediate` | **这是整个性能判断的地基**：所有语句串行共用唯一连接 → 任何慢写/慢读都会阻塞全站；也因此**绝不能**做「每次浏览写一次库」 |
| 静态资源 | 无缓存头、无压缩，每次导航全量重传 | 明显浪费 |
| `/uploads/*` | 已有 immutable 长缓存 | **已经做对了，不要动** |
| 关闭流程 | `main.go` 仅 `db.Close()`，无 `wal_checkpoint(TRUNCATE)` | WAL 文件可能残留，备份文档必须写对 |

---

## 2. 竞品视角：Ghost 的资深用户会为什么留下，又为什么可能搬走

### 2.1 对标选择

| 候选 | 一行对比 | 与 Boop 可比性 |
|---|---|---|
| **Ghost** | Node + MySQL 全功能出版平台，会员/邮件/分析/联邦齐备 | **同为自托管、同为站长自发布、有真实重度写作者群体** |
| Micro.blog | 托管式，核心是发一次同步到 Mastodon/Bluesky | 定位最接近，但**不能自托管** |
| Bear Blog | 无 JS 无追踪的托管极简博客 | 极简取向一致，但无自托管、无评论体系 |
| WriteFreely | Go 写的 ActivityPub 联邦极简博客 | 技术形态最近，但无评论/点赞/收藏 |
| Hugo | 静态生成器 | 产物极快，但评论/搜索/会员全靠外挂 |

**选定 Ghost**：唯一同时具备「自托管 + 真实重度写作者 + 成熟生态」的对手。它的粘性建在**会员 / 邮件 / 分析 / 联邦**四层上，正好是 Boop 要攻的位置；而它的导出格式本身定义了 Boop 的「入场券」。

来源：<https://ghost.org/changelog/6/>、<https://ghost.org/help/exports/>

### 2.2 迁移门槛（按「我搬家的顺序」排列）

#### ① 草稿列表 + 编辑/删除入口
- **对手现状**：Ghost Admin 有完整 Posts 列表、草稿筛选、批量操作。
- **为什么是硬需求**：积累上千篇之后，没有列表和编辑入口等于给我一个**只能一次性写入**的编辑器，旧文无法纠错。
- **Boop 最小可行版本**：新增 owner-only 的 `/admin/posts` 页面（状态/类型筛选 + 游标分页），复用已有的 `POST/PATCH/DELETE /api/v1/admin/posts`；删除沿用软删除，不需要新迁移。
- **我怎么验收**：能在 UI 里翻到三年前的一篇 `article`，改标题、换封面、改状态、软删除，全程不碰 curl。

#### ② 定时/预约发布
- **对手现状**：Schedule for later，时间按站点时区解释（<https://ghost.org/help/publishing-content/>）。
- **Boop 最小可行版本**：新增 `scheduled` 语义 + 公开查询条件改为 `published_at <= now()`。**不需要常驻调度器**——读时判断即可，这恰好符合「单进程、无队列」的铁律。
- **我怎么验收**：预约 3 篇，次日刷新首页时间与顺序正确，无任何后台进程。

#### ③ 迁移入场券：301 + SEO 基建
- **对手现状**：Ghost 自动生成 XML sitemap、canonical、结构化数据、Open Graph（<https://ghost.org/help/seo>），并支持手写 301/302（<https://ghost.org/help/redirects/>）。
- **为什么是硬需求**：换站最大的成本是外链失效与权重归零。承接不了旧 URL 和社交卡片，我会先被搜索引擎罚一次。
- **Boop 最小可行版本**：`base.html` 的 `<head>` 补 `canonical`/`og:*`/JSON-LD（BlogPosting）；新增 `/robots.txt` 与 `/sitemap.xml`（从 `internal/content` 流式生成，不落库）；新增 `redirects` 表并在路由表最前命中 301。
- **我怎么验收**：旧文 5 个 URL 手写 301 后全部 200；`curl -I` 见正确 canonical；富结果测试通过。

#### ④ 导入通道（Ghost JSON → Boop）
- **对手现状**：导出为单个 JSON（Posts/Pages/Tags/Settings），Members 为 CSV（<https://ghost.org/help/exports/>），导入走 Settings → Advanced → Import/Export（<https://ghost.org/help/imports/>）。
- **Boop 最小可行版本**：新增 `boop import` 子命令（**不做 Web 路由**），映射到 `moment/article/photo`，图片复用已有的内容哈希去重，用原 slug 做唯一键保证幂等。
- **我怎么验收**：连跑两次无重复行；抽样 20 篇的正文/时间/标签/图片全部对上。

#### ⑤ 静态资源缓存
- **对手现状**：Ghost 官方生产栈前置 Nginx 承担静态与主题资源缓存。
- **Boop 最小可行版本**：`handleStatic` 补 `ETag`（`go:embed` 的内容可在启动时算 hash）+ `Cache-Control` 长缓存。**注意：`/uploads/*` 已经做对了，缺的只有 `/static/*`。**
- **我怎么验收**：二次访问返回 304；空闲 RSS 与首页 p95 实测达标。

#### ⑥ Feed 多样性与本地阅读统计
- **对手现状**：Ghost 6 的原生分析基于 Tinybird/ClickHouse、无 Cookie（<https://ghost.org/changelog/6/>）；订阅触达靠邮件，而**自托管必须自己接 Mailgun**——"Currently the only bulk mail API we support is Mailgun"（<https://docs.ghost.org/faq/mailgun-newsletters>、<https://docs.ghost.org/newsletters>）。
- **Boop 最小可行版本**：加 Atom 与 JSON Feed 两个路由；阅读统计做**按天聚合**且**绝不逐次写库**（见 §8 反面清单）。
- **我怎么验收**：统计不落原始 IP、不引第三方；两个 Feed 通过 validator。

### 2.3 回不去的那一条

**① 整站就是一个文件，备份是 `cp`。**
Ghost 是 Node + MySQL + Nginx 的组合，官方只支持特定 OS/Node/MySQL 版本组合，并且 6.0 把能力继续拆向外部署的组件（<https://ghost.org/changelog/6/>）。搬到 Boop，内容 + 评论 + 点赞 + 收藏 + 全文索引全在一个 SQLite 里，可以 `cp` 一份到笔记本上**离线搜完十年写的字**。这是 Ghost 结构上给不了的。

**② 升级不再是迁移。**
Boop 升级是换二进制，回滚是换回旧二进制；迁移只可前进这一点反而是承诺——线上路径永远窄且可测。

**③ 读者互动不需要付费栈。**
Ghost 的原生评论服务于登录会员，而会员体系天然连着 Stripe 与 Mailgun。Boop 里读者注册即可评论、点赞、收藏，站长不必为了「有人能在文章下说话」而维护一套付费基础设施。

### 2.4 不建议追的方向（竞品视角）

| 不做 | 理由 | 代价 |
|---|---|---|
| newsletter / 邮件投递 | 做发信等于引入投递信誉工程与队列，直接违反铁律 | 放弃邮件触达，用 Atom/JSON Feed 补 |
| 付费会员 / Stripe | 那是另一条业务线 | 不变现，也不背 webhook 与税务 |
| 完整 ActivityPub | Ghost 6 为此拆了独立部署组件，是队列与出站重试的重活 | 放弃联邦发现流量 |
| 分析平台化 | Tinybird/ClickHouse 是给多租户出版商用的 | 只保留一张按天聚合的小表 |
| 主题 / 插件市场 | 会催生「只有一个实现的接口」与代码生成器，违反铁律 | 外观定制弱，用少量模板变量补 |

---

## 3. 自家用户视角：作为每天用它的站长，缺什么

### 3.1 我的一天与摩擦点

早上开 `/`，先扫右栏 AI 作者状态卡确认站点还活着。写长文在首页快捷发布器：点「文章」→ 填标题 → 正文写进 `textarea` → 逗号分隔标签 → 可选摘要 → 点 AI 助手的摘要/标签/SEO →「采用」→「发布」。照片模式选图后**立即上传**并预览。

摩擦点（按遇到频率排序）：

1. **错字改不了**——PATCH/DELETE 只有 API，详情页没有按钮，也没有列表能找到那篇。
2. **草稿是黑洞**——内容域支持 `draft`，但没有任何页面能看到它。
3. **发布/评论后整页 reload**——评论一篇长文后被弹回顶部，是无差别每天都撞的毛刺。
4. **手机上找不到搜索**。
5. 详情页的照片被裁成 8:5。

### 3.2 效率：速度

| 现象 | 根因（指到代码） | 改法 | 成本 | 可验证收益 |
|---|---|---|---|---|
| 首页随点赞数变慢 | `content/posts.go` 的 `postColumns` 每卡片一条 `SELECT COUNT(*) FROM likes WHERE post_id = p.id`；`likes` 主键为 `(user_id, post_id)`（迁移 001），**`post_id` 无前导索引** | 迁移 004 加 `CREATE INDEX idx_likes_post ON likes(post_id)` | 小 | 计数查询 O(log n)。**因 `maxOpenConns=1` 所有语句串行，收益被放大** |
| 每次导航重下 ~58 KB | `handleStatic`（`server.go:269`）只设 `Content-Type`，无 `ETag`/`Cache-Control` | 启动时按嵌入内容算 hash 作 ETag，`If-None-Match` 回 304 | 小 | 二次访问静态 0 字节 |
| 文本资源明文传输 | 全链无 gzip（`internal/server/middleware.go`） | 在安全响应头同层加 `compress/gzip`（跳过 `/uploads`） | 小 | 首屏字节降 70–80% |
| 搜索结果排序多一次临时排序 | `search.go:130` 用 `COALESCE(p.published_at,'')` 排序，与 `handlers_feed.go` 刻意不套 `COALESCE` 的理由矛盾 | 去掉 `COALESCE`（已发布行该列非空） | 极小 | 省一次 temp B-tree |
| 备份不可用 | WAL 模式下裸拷 `boop.db` 会丢 `-wal`；`main.go` 退出时无 `wal_checkpoint(TRUNCATE)` | Task 10 文档统一改为 `sqlite3 .backup`；退出路径补一次 checkpoint | 小 | 恢复真的可用 |
| session 表无限增长 | `auth/session.go` 只在登出/轮换时删行 | 仿限流的「请求驱动节流清扫」（1 分钟一次，`idx_sessions_expires` 已就绪） | 小 | 500 MB 机器上表不再无限长 |

### 3.3 效率：工作质量

- **站长内容管理页（最高性价比）**：接口齐备（`handlers_admin_posts.go` 的 create/patch/delete）、`content.Update` 还带 `updated_at` 乐观锁，只差页面。列表（草稿/已发布/归档 + 筛选 + 游标分页）+ 编辑表单 + 软删除按钮，即可把「发布器」升级为「博客」。
- **标签只写不读**：`tags`/`post_tags` 落了库、详情页也渲染，但没有 `/tag/{slug}` 路由，`tags.slug` 从未被消费；FTS 也只索引 title/body/excerpt，**搜标签词搜不到**。补一个标签页 + 把标签名并入索引，站内互链立刻成立。
- **编辑器没有预览**：Markdown 只在服务端渲染（`content/markdown.go`），长文排版只能发布后才知道对不对。复用同一套渲染规则做只读预览即可，不必新增依赖（`goldmark` 已在）。
- **发布器只能一张图**：`app.js:317` 写死 `[asset.id]`，而模型允许 20 张、去重链路已就绪。摄影形态因此被压成单图。
- **AI 助手的设计已经很克制**（只建议、必须点「采用」才写入、SEO 只提供复制），不要动。

### 3.4 UI 与交互

| 现状 | 问题 | 改法 | 成本 |
|---|---|---|---|
| `≤700px` 时左栏右栏全隐藏，底部导航五项无搜索（`app.css:774`、`base.html:180`） | 手机上无法进入搜索 | 底部导航补一项，或顶栏加图标按钮 | 小 |
| 详情页复用 `.photo-art`（`.photo-art{aspect-ratio:8/5;object-fit:cover}`，`app.css:265`） | 首页当封面裁切合理，详情页应显示原比例 | 详情页用 `height:auto` 的 variant | 小 |
| `post.html` 无目录、无阅读时长、无上下篇、无相关阅读 | 长文阅读体验薄，老内容无二次消费入口 | 由 `body_html` 的 h2/h3 生成目录 + 字数估算；上下篇用已发布的相邻行 | 中 |
| 写操作后整页 reload（`app.js` 的 `reloadSoon()` 与发布路径） | 阅读位置丢失、重复下载静态资源 | 保留服务端渲染：`fetch` 同一 URL，用 `DOMParser` 只替换子树；或至少恢复 `scrollY` | 小–中 |

**已经做得好、不要动的**：`sr-only` 让图标导航保留无障碍文字；`:focus-visible` 全局焦点环；空态与错误态齐全；`post.html` 的评论是 SSR 的，无 JS 仍可读。

### 3.5 粘性

- **习惯性粘性**：把发布器放在首页首屏是整个产品最强的钩子；AI 作者状态卡给了每天一个「看看站点」的理由。**真正缺的是闭环反馈**——读者评论后没有任何「有人回复你」的信号，作者发布后也没有「刚有 X 条新评论」，动线到发布就断了。
- **资产性粘性**：内容全在这台 SQLite 里、RSS 已通、图片本地化，这是真实的「搬不走」。但**新读者入口很薄**：无 `og:*`/`canonical`/JSON-LD、无 sitemap/robots——分享到社交平台没有卡片、搜索引擎收录慢。
- 标签页与归档/年度页落地后，粘性会从「今天发了吗」变成「这里有东西可翻」。

---

## 4. 交叉验证：重合、冲突与修正

### 4.1 两视角重合项（最高置信，优先做）

| 建议 | 竞品视角 | 本产品视角 |
|---|---|---|
| 草稿列表 + 编辑/删除入口 | 第 1 位（迁移门槛 ①） | 第 1 位（质量项，影响 5） |
| 静态资源缓存 | 迁移门槛 ⑤ | 优先级 2 |
| `og:*`/canonical/sitemap/robots | 迁移门槛 ③ | 优先级 5 |
| Feed 多样性 | 迁移门槛 ⑥ | 优先级 6 附近 |

两个互不通气的视角独立指向同一批结论，说明这不是分析偏好，而是产品事实。

### 4.2 冲突项与裁决

| 冲突 | 裁决与依据 |
|---|---|
| 竞品视角建议「新增 `post_views` 表做阅读统计」；本产品视角明确反对 | **采纳后者**。依据：`store.go` 的 `maxOpenConns = 1` + `_txlock=immediate`，每次写都取写锁并占用唯一连接。一次浏览一次 UPSERT 会与发布、评论、限流争用同一把锁。若要统计，只能从访问日志派生、或采样后批量写。 |
| 竞品视角建议「Webmention 收发」；本产品视角反对完整 ActivityPub | **折中**：两者都先不做。Webmention 需要出站请求与重试，本质是队列。等 P2 之后再评估。 |
| 竞品视角建议 301 重定向表 + Ghost 导入；本产品视角未提 | **保留但降级**。它是「别人敢不敢搬过来」的开关，对单人自用场景优先级取决于是否真要迁移。进 P1 尾段。 |
| 竞品视角建议「定时发布」放第 2 位 | **降级到 P2 头部**。它确实需要，但不像编辑入口那样每天撞一次；且实现（读时 `published_at <= now()`）不需要架构改动。 |

### 4.3 我核对发现的偏差（已修正）

1. **竞品简报说「`/uploads/*` 无 Cache-Control」——错。** `handlers_upload.go:171` 已经设置 `public, max-age=31536000, immutable`，并有测试断言（`handlers_upload_test.go:583`）。真正的缺口只有 `/static/*`。若照原简报去做，会白白改一个已经正确的路径。
2. **本产品简报的 `likes` 全表扫描结论——对，而且更严重。** `likes` 主键确为 `(user_id, post_id)`，`post_id` 无前导索引；而 `maxOpenConns = 1` 意味着这些扫描不是并行分摊，而是**串行占住唯一连接**。该条从「小优化」提升为 P0 第 1 项。
3. **本产品简报提到的搜索 `COALESCE` ——对，但收益有限。** `search.go:130` 确实如此，但结果集已被 FTS `MATCH` 收窄，临时排序规模很小。保留为 P0 的附带小修，不作为独立项。

---

## 5. 优先级矩阵（总表）

| # | 建议 | 类型 | 影响 | 成本 | 铁律 | 阶段 |
|---|---|---|---|---|---|---|
| 1 | 迁移 004：`idx_likes_post ON likes(post_id)` | 速度 | 5 | 1 | 受约束 | P0 |
| 2 | `/static/*` ETag + 长缓存 | 速度 | 4 | 1 | 受约束 | P0 |
| 3 | gzip 中间件 | 速度 | 4 | 1 | 受约束 | P0 |
| 4 | 详情页照片按原比例 | UI | 3 | 1 | 受约束 | P0 |
| 5 | 移动端补搜索入口 | UI | 3 | 1 | 受约束 | P0 |
| 6 | 过期 session 清扫 | 速度 | 2 | 1 | 受约束 | P0 |
| 7 | 搜索 ORDER BY 去 `COALESCE` | 速度 | 2 | 1 | 受约束 | P0 |
| 8 | `og:*`/canonical/JSON-LD + `robots.txt` + `sitemap.xml` | 粘性 | 4 | 2 | 受约束 | P0 |
| 9 | 站长内容管理页 `/admin/posts`（列表+编辑+软删除） | 质量 | 5 | 3 | 受约束 | P1 |
| 10 | 标签 `/tag/{slug}` + 标签并入 FTS | 质量 | 4 | 2 | 受约束 | P1 |
| 11 | 发布/评论后片段替换、保留滚动 | UI | 4 | 3 | 受约束 | P1 |
| 12 | 备份 runbook（`sqlite3 .backup`）+ 退出 `wal_checkpoint` | 速度 | 4 | 3 | 受约束 | P1 |
| 13 | 性能门禁实测并入 Task 10（RSS ≤150 MB、p95 ≤150 ms） | 速度 | 4 | 3 | 受约束 | P1 |
| 14 | 发布器多图 `asset_ids[]` | 质量 | 3 | 2 | 受约束 | P1 |
| 15 | 详情页目录 + 阅读时长 + 上下篇 | UI | 3 | 2 | 受约束 | P1 |
| 16 | Markdown 编辑预览 | 质量 | 3 | 3 | 受约束 | P1 |
| 17 | Atom + JSON Feed | 粘性 | 3 | 2 | 受约束 | P1 |
| 18 | 301 重定向表 + `boop import`（Ghost JSON） | 粘性 | 3 | 3 | 受约束 | P1 尾 |
| 19 | 定时发布（读时 `published_at <= now()`，无调度器） | 质量 | 3 | 3 | 受约束 | P2 |
| 20 | 站内未读评论提示（读取时计算，不做邮件/worker） | 粘性 | 4 | 4 | 需注意 | P2 |
| 21 | 阅读统计（只做按天聚合 + 采样，不逐次写库） | 粘性 | 3 | 4 | 需注意 | P2 |
| 22 | LICENSE + CI（GitHub Actions 跑同一套门禁） | 交付 | 4 | 2 | 受约束 | P0 并行 |

> 「受约束」= 不触碰任何工程铁律。需要特别注意的两项（20、21）已在 §8 说明为什么**不做成**后台任务。

---

## 6. 分阶段路线图

### P0 — 立即（全部小改，可在一到两个工作批次内完成并各自独立提交）

> **执行状态：已完成**（10 个提交，`28e04df` … `e8904e7`）。逐条结果、实测数据与过程中新发现的缺陷见 [`Boop-优化执行记录.md`](./Boop-优化执行记录.md)。

顺序按「影响÷成本」：

1. 迁移 004：`likes(post_id)` 索引
2. `/static/*` 缓存头 + gzip 中间件（同一次提交内做，都属于传输层）
3. `<head>` 元数据 + `robots.txt` + `sitemap.xml`
4. 三处小修：详情页照片不裁切、移动端搜索入口、搜索排序去 `COALESCE`；过期 session 清扫
5. 并行：LICENSE + CI

> P0 全部不新增依赖、不改数据模型（除一条索引迁移）、不改路由语义，符合「一项任务一个可审查提交」。

### P1 — 下一个里程碑（与 Task 10 合并推进）

9 → 10 → 11 → 12 → 13 为主线（先把「可维护」和「可交付」补齐），14–18 为侧翼。

其中 **12 与 13 建议与 Task 10 合并成一个「可交付」批次**：备份 runbook、退出时 WAL checkpoint、性能门禁实测是同一件事的三个面——「这台机器上它可靠吗、快吗、我能救回来吗」。

### P2 — 需要额外判断

19–21。每一项都要先回答一个问题：**它会不会偷偷引入第二个并发模型？** 定时发布不会（读时判断），未读提示要设计成读取时计算，阅读统计必须采样 + 批量写。

---

## 7. 关键项落地要点

### 7.1 P0-1：`likes(post_id)` 索引

```sql
-- internal/store/migrations/004_likes_post_index.sql
CREATE INDEX idx_likes_post ON likes(post_id);
```

验收：`EXPLAIN QUERY PLAN` 对 `SELECT COUNT(*) FROM likes WHERE post_id = ?` 不再出现 `SCAN likes`；已有测试全绿（迁移是新增文件，不改既有迁移）。

### 7.2 P0-2 / P0-3：`/static/*` 缓存与 gzip

- `handleStatic`：启动时遍历 `go:embed` 的 FS 算 sha256（只算一次，存进 map），响应带 `ETag` + `Cache-Control: public, max-age=31536000, immutable`（内容随构建变，文件名不变，所以必须靠 ETag 而不是版本 query）。
- 新增一层 `gzip` 中间件，与 `securityHeaders` 同层；`Content-Type` 为文本类才压缩，跳过 `/uploads` 与已压缩格式。
- 注意：`WriteTimeout` 为 60s、`maxOpenConns = 1`，压缩应在内存中完成后再写，不要包成流式管道拖住连接。

### 7.3 P0-8：`<head>` 与可发现性

- `base.html`：`canonical`（用 `BOOP_BASE_URL` + 当前路径，**不要读请求 Host**）、`og:title/description/type/url/image`、`article:published_time`；详情页加 `BlogPosting` JSON-LD。
- 新增 `GET /robots.txt`（允许全部，声明 sitemap）与 `GET /sitemap.xml`（只含 `published` 且未删除，按 `published_at DESC` 流式写出，不落库、不分页）。
- 验收：社交平台调试器能取到卡片；`curl -I` 的 canonical 与 `BOOP_BASE_URL` 一致。

### 7.4 P1-9：站长内容管理页

- 路由：`GET /admin/posts`（页面）、可选 `GET /api/v1/admin/posts` 列表接口（当前只有 POST/PATCH/DELETE）。
- 页面：状态筛选（草稿/已发布/归档）+ 类型筛选 + 游标分页（复用公共信息流的 `<published_at,id>` 游标）+ 编辑表单 + 软删除按钮 + 发布/撤稿。
- **必须复用** `content.Update` 的 `updated_at` 乐观锁，冲突时给确定性错误而不是静默覆盖。
- 权限、CSRF、Origin 校验、body limit 全部沿用现有中间件，不新增旁路。

### 7.5 P1-11：不再整页 reload

保留服务端渲染不变：写操作成功后 `fetch` 同一 URL → `DOMParser` 解析 → 只替换 `.comments` 子树（或 `.post`），并保持 `scrollY`。这条路径不引入任何前端框架、不复制模板，符合铁律。

---

## 8. 不建议做的事（反面清单）

| 不做 | 理由 | 代价 |
|---|---|---|
| **逐次写库的阅读量** | `maxOpenConns = 1` + `_txlock=immediate`：一次浏览一次 UPSERT 会和发布/评论/限流抢同一把写锁 | 只保留按天聚合或日志派生 |
| **邮件 / newsletter** | 需要投递信誉工程与队列，竞品的这个功能本身就是靠 Mailgun 撑的 | 放弃邮件触达 |
| **付费会员 / Stripe** | 另一条业务线，带来 webhook、税务、退款 | 不变现 |
| **完整 ActivityPub** | 需要出站请求 + 重试，本质是队列；竞品为此拆了独立部署组件 | 放弃联邦流量 |
| **SPA 化 / Redis / 队列 / ORM** | 铁律禁用，且当前 SSR + 原生 JS 已能覆盖；reload 换成片段替换就够 | 无 |
| **主题 / 插件系统** | 会催生「只有一个实现的接口」和代码生成器 | 外观定制弱 |
| **多作者 / 「项目」模块** | `docs/PRODUCT.md` 明确非目标，会牵动整个权限与信息流模型 | 无 |
| **分析平台化** | 单站长不需要多租户数据仓库 | 只保留一张小表 |

---

## 9. 风险与未决

1. **性能数字仍未经实测**。本报告的所有性能判断都来自读码（索引缺失、无缓存头、连接池上限），**没有真实 p95 与 RSS 数据**。Task 10 的门禁必须补上，否则无法证明 P0-1/P0-2 的收益量级。
2. **`maxOpenConns = 1` 是一把双刃剑**。它消除了锁竞争、简化了推理，但也让任何慢语句成为全局阻塞。P0-1 因此不是「优化」而是「消除一个已知的全局阻塞源」。长期应保留这份简单性，不要为了提高并发去放宽它。
3. **`POST /feed.xml` 返回 403 而非注释声称的 405**——文档与实现不一致，建议与 P0 一起收掉。
4. **`go.mod` 的直接依赖被标为 `// indirect`**——需要单独跑 `go mod tidy` 并单独提交，不要混进功能提交。
5. **无 LICENSE 会让「别人敢不敢用」成为一句空话**——与「提高粘性」直接相关，建议 P0 并行处理。
6. **上传不剥离 EXIF**：摄影形态下这可能泄露拍摄地点（家庭住址级别的隐私风险）。建议评估，若处理则需要一个纯 Go 的 JPEG 段重写，成本中等。
7. **报告的竞品事实来自公开来源**，已标注 URL；但竞品迭代快，涉及具体功能名与限制的结论建议在决策时二次核对。
