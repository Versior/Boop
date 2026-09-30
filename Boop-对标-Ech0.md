# Boop × Ech0 对标分析

> 生成时间：2026-09-29
> 方法：**读码比对**，不是读 README 比对。Ech0 以 `--depth 1` 克隆到本地逐文件核对，所有结论都带文件行号。
> Boop 基线：本地 `main` @ `113ef8f`
> Ech0 基线：`f53e4eb9304f78774bcbe45166c494b1cee30850`（v5.8.0，2026-09-28，2067 ⭐ / 159 fork / AGPL-3.0）
> 本文是对 [`Boop-体验优化报告.md`](./Boop-体验优化报告.md) 的补充与修订，不替换它。
> **执行状态**：本文 §6 的 P0 清单已执行，逐条结果与实测数据见 [`Boop-优化执行记录.md`](./Boop-优化执行记录.md)。

---

## 0. 摘要

**结论先行**：Ech0 是一个比 Boop 大一个数量级、功能面完整得多的**产品**（109,598 行 Go / 43,092 行前端 / 132 个直接依赖）；Boop 是一个 25,247 行 Go / 1,582 行前端 / 3 个直接依赖的**工具**。把 Ech0 当"追赶目标"会毁掉 Boop；但 Ech0 里有 **4 个机制**值得原样移植、**2 个思路**值得改写后采用、**8 个东西**碰都不能碰。

| 分类 | 条目 |
|---|---|
| ✅ **直接移植**（已验证、与工程铁律不冲突） | ① `VACUUM INTO` 一致性备份 ② 嵌入期预压缩静态资源 ③ Actor 式阅读统计 ④ 全站写保护 `WriteGuard` |
| 🔧 **改写后采用** | ① robots/sitemap（**不要**抄它的 baseURL 推导） ② 可读迁移格式（capsule 的**思想**，不是格式） |
| ⛔ **明确不可抄** | AGPL-3.0 代码 · GORM AutoMigrate · CGO/sqlite-vec · `LIKE` 搜索 · Vue SPA · 132 依赖 · 无上限连接池 · 信任 `X-Forwarded-*` |
| 🏆 **Boop 已经更强**（不许退步） | FTS5 全文检索 · SSR 的逐页 SEO · 纯 Go 零 CGO 交付 · 3 个依赖 · 不可变编号迁移 |

**一句话差异**：Ech0 用**体量**换功能广度；Boop 用**约束**换可托付性。两者都不错，但抄错方向的代价对 Boop 是不可逆的。

---

## 1. 硬数据对照

| 维度 | Boop @ `113ef8f` | Ech0 @ v5.8.0 | 比值 |
|---|---|---|---|
| Go 代码行 | 25,247 | 109,598 | 4.3× |
| 前端代码行 | 1,582（原生 CSS+JS，无构建链） | 43,092（Vue + TS） | 27× |
| 直接依赖 | **3** | **132** | 44× |
| 测试函数 | 约 367 | 1,278 | 3.5× |
| 路由 | 57 | Gin + Huma/OpenAPI 分组注册 | — |
| 数据层 | 纯 Go `modernc.org/sqlite`，手写 SQL | GORM + `mattn/go-sqlite3`（**cgo**）+ `sqlite-vec`（**cgo**） | — |
| 迁移 | 3 个不可变编号 SQL，手写 | GORM `AutoMigrate` 20 个模型 + 12 个自定义 `Migrator` | — |
| 检索 | **FTS5**（高亮片段 + 游标分页） | `content LIKE '%x%'`（`internal/repository/echo/echo.go:79,407,497`） | — |
| 渲染 | SSR（`html/template` + `embed`） | Vue SPA + REST（`internal/handler/web/web.go`） | — |
| 连接池 | `maxOpenConns = 1` | **未设置任何上限**（无 `SetMaxOpenConns`） | — |
| 部署 | 单二进制 + 一个 `.db` | Docker / Compose / Helm / CLI / TUI + S3 | — |
| 许可 | **无 LICENSE 文件** | AGPL-3.0-or-later + 商业授权 | — |

> 依赖数、前端行数、测试函数数均为本地实测（`go.mod` 的 `require` 块条目数 / `find web/src` 汇总 / `^func Test` 计数）。

**这组数字本身就是结论**：Ech0 的 132 个依赖里有 `gin` `gorm` `huma` `gocron` `ristretto` `sqlite-vec` `aws-sdk-go-v2`(4 个模块) `go-oidc` `go-webauthn` `anthropic-sdk-go`。每一个都是一条需要跟进的安全公告、一次破坏性升级、一个 Boop 目前完全不需要面对的问题面。

---

## 2. 关键机制逐项核对

以下每一条都在 Ech0 源码里定位过，并核对 Boop 当前实现。

### 2.1 一致性备份：`VACUUM INTO`（✅ 直接移植）

**Ech0 的做法**（`internal/database/database.go:81-83`）：

```go
func SnapshotTo(dstPath string) error {
	return GetDB().Exec("VACUUM INTO ?", dstPath).Error
}
```

调用点 `internal/migrator/exporter/fs/exporter.go:24`：
`snapshot.Create(snapshot.WithConsistentDB(database.SnapshotTo))` —— 先 `VACUUM INTO` 出一份一致的库，再用 overlay FS 把这份库替换进要打包的数据目录，最后整目录压成 zip。快照文件排除 `-wal`/`-shm` sidecar（`writer_test.go:91` 有对应测试）。

**为什么这条重要**：`Boop-体验优化报告.md` §5 第 12 项写的是 `sqlite3 .backup` —— 那要求部署机器上装 `sqlite3` CLI。**`VACUUM INTO` 是纯 SQL，一行 `Exec`，不需要任何外部二进制**，而且同样保证事务一致视图（不像 `cp boop.db` 会拿到撕裂的快照）。

**对 Boop 的落地**：

```
boop backup -o /path/to/snapshots
```

- 内部就是 `db.ExecContext(ctx, "VACUUM INTO ?", dst)`（`?` 绑定，不做字符串拼接）。
- 目标文件已存在时 `VACUUM INTO` 会报错 —— 这也是它比 `cp` 安全的地方之一：不会静默覆盖。
- **必须写进文档的一条副作用**：Boop 的 `store.go:32-35` 是 `maxOpenConns = 1`，这条 `Exec` 会独占那唯一连接。个人站数据量下是毫秒级，但备份期间所有请求排队 —— 要在 runbook 里明说，别让人以为它零成本。

### 2.2 静态资源：嵌入期预压缩（✅ 直接移植，**替换**原报告的运行时 gzip 中间件）

**Ech0 的做法**（`internal/handler/web/web.go:79-92`）：

```go
encoding := ctx.GetHeader("Accept-Encoding")
if strings.Contains(encoding, "gzip") {
	gzPath := fullPath + ".gz"
	gzFile, err := fileServer.Open(gzPath)
	if err == nil {
		defer func() { _ = gzFile.Close() }()
		stat, _ := gzFile.Stat()
		ctx.Header("Content-Encoding", "gzip")
		ctx.Header("Content-Type", getMimeType(fullPath))
		setCacheControlHeader(ctx, requestPath)
		http.ServeContent(ctx.Writer, ctx.Request, gzPath, stat.ModTime(), gzFile)
		return
	}
}
```

要点是：**`.gz` 文件是构建产物，不是运行时产物**。请求路径上没有任何压缩计算，只是"打开另一个文件"。`.gz` 不存在就静默回退到未压缩版本 —— 没有协商失败的可能。

配套的 `internal/handler/humares/docs.go:77-84` 同样是 `//go:embed` 的**预压缩常量** `scalarBundleGz`，`gzip.NewReader` 只用于测试反解。

**为什么这条对 Boop 特别重要**：`Boop-体验优化报告.md` §7.2 提议新增一层 gzip 中间件，并自己加了一句警告 —— "压缩应在内存中完成后再写，不要包成流式管道拖住连接"。这个警告本身就是信号：**运行时压缩需要包装 `ResponseWriter`、需要自己管 `Content-Length`、需要自己发 `Vary: Accept-Encoding`，还要在 `WriteTimeout` 60s 的预算里和 `maxOpenConns = 1` 抢时间。而 Boop 要压的东西只有两个文件：`app.css` 801 行 / `app.js` 781 行，各约 29 KB。**

对手是 58 KB 的静态文本，不值得为此引入一层中间件和三类 header 正确性问题。

**对 Boop 的落地（修订原报告 P0-2 / P0-3）**：

1. 构建时（或一次性提交）产出 `web/static/app.css.gz`、`web/static/app.js.gz`，随 `go:embed` 一起进二进制。
2. `handleStatic` 改成：
   - 启动时遍历 `web.FS` 算一次 sha256 → `map[name]string`，作为 **ETag**（`embed.FS` 里 `fs.Stat` 的 ModTime 是零值，不能用来做 `Last-Modified`，所以必须用内容哈希）。
   - 请求时先比 `If-None-Match` → 命中回 304。
   - 其次比 `Accept-Encoding` 含 gzip → 有 `.gz` 就发 `.gz` + `Content-Encoding: gzip`，否则发原文。
   - 都走 `http.ServeContent`（拿 `Range` / `If-Modified-Since` / `Accept-Ranges` 的免费实现），而不是手写 `w.Write`。
   - 补 `Cache-Control: public, max-age=31536000, immutable`（文件名不带版本，所以 ETag 是唯一的重验手段）。

**净效果**：首屏 58 KB → 约 15 KB；二次访问 0 字节（304）；**每请求 CPU 增量为零**；不新增中间件、不新增 `Vary` 漏发的风险；比原方案少约 50 行代码。

### 2.3 robots / sitemap / SEO（🔧 抄意图，**不要**抄实现）

Ech0 有 `/robots.txt` 与 `/sitemap.xml`（`internal/router/resource.go` 注册，实现在 `internal/handler/common/common.go:147-191`）。

**但它有两个明显问题，都不能抄：**

**问题一 —— sitemap 是硬编码的，不含任何内容页**（`common.go:168-172`）：

```go
URLs: []sitemapURL{
	{Loc: baseURL + "/", ChangeFreq: "daily", Priority: "1.0"},
	{Loc: baseURL + "/hub", ChangeFreq: "daily", Priority: "0.8"},
	{Loc: baseURL + "/rss", ChangeFreq: "hourly", Priority: "0.6"},
},
```

三条，写死。真正带内容的 sitemap 只存在于 **capsule 的静态站构建**里（`internal/capsule/build/build.go` 会 `writeFile(dir, "sitemap.xml", ...)`）。根因很好理解：Ech0 是 Vue SPA，服务端手上根本没有"文章列表"这个概念可以吐出来 —— 内容页 URL 是前端路由的产物。**它为此额外造了一整条 SSG 流水线才拿到逐页 sitemap。**

**问题二 —— base URL 直接信任请求头**（`common.go:129-145`）：

```go
if forwardedProto := strings.TrimSpace(ctx.GetHeader("X-Forwarded-Proto")); forwardedProto != "" {
	scheme = strings.TrimSpace(strings.Split(forwardedProto, ",")[0])
}
...
if forwardedHost := strings.TrimSpace(ctx.GetHeader("X-Forwarded-Host")); forwardedHost != "" {
	host = strings.TrimSpace(strings.Split(forwardedHost, ",")[0])
}
```

无条件采信。如果部署在不会剥离这两个头的反代后面（或直接裸奔），任何人都能让你的 `robots.txt` 里出现 `Sitemap: https://attacker.example/sitemap.xml`。这不是理论风险，是标准配置错误。

**对 Boop 的落地**：`Boop-体验优化报告.md` §7.3 已经写对了 —— "用 `BOOP_BASE_URL` + 当前路径，**不要读请求 Host**"。这条与 Ech0 恰好相反，保持原判。

**这里有一个 Boop 白拿的优势值得明说**：Boop 是 SSR，`GET /p/{slug}` 在服务端就同时握有"这篇文章"和"这篇文章的绝对 URL"。所以：

- `/sitemap.xml` 可以直接遍历 `published` 且未删除的文章流式写出，**逐篇 `<url>` + `<lastmod>`**，一次查询 + 一次编码，不需要落库、不需要分页、不需要任何额外架构。
- 同理 `<head>` 里的 `canonical` / `og:*` / `BlogPosting` JSON-LD 在 SSR 下是**零成本**的；Ech0 要为每一页做这些，得在 SPA 里注入或者走 SSG。

**结论：SEO 这项上，Boop 的架构比 Ech0 更适合做这件事，而且实现更便宜。** 不要因为 Ech0 有而 Boop 没有就以为这是"追赶"，这是"白送"。

### 2.4 阅读统计：Actor + 非阻塞投递 + 小时级落库（✅ 直接移植的**蓝图**）

这是本轮核对里最完整、最值得原样借鉴的一段代码。

**形态**（`internal/visitor/visitor.go`）：一个 `Tracker` 结构体，内部起**唯一一个 goroutine**（`go s.run()`），所有状态变更通过 channel 串行进这个 goroutine —— **没有 mutex，没有锁竞争，没有第二个写者**。

**写入路径非阻塞**（`visitor.go:44-56`）：

```go
func (s *Tracker) Record(r *http.Request, ip string) {
	if r == nil || r.Method != http.MethodGet {
		return
	}
	event := recordEvent{ipHash: hashIP(ip), at: time.Now().UTC()}
	select {
	case s.recordCh <- event:
	default:   // ← 缓冲区满就丢弃，请求路径永不被拖住
	}
}
```

只有 GET 计数；IP 经 FNV-1a 哈希后使用（不存原始 IP）；PV 同 IP 5 分钟内去重（`pvWindow = 5 * time.Minute`，`canCountPV`）；UV = 当天不同 IP 哈希数；只保留 7 天（`keepDays = 7`，`gc` 负责裁剪）。**这是一套为"虚荣指标"精心定的精度上限 —— 记录是有损的，这是正确的取舍。**

**落库路径是小时级，不是每请求**（`internal/task/scheduled/visitor.go:36-47`）：

```go
_, err := s.NewJob(
	gocron.DurationJob(60*time.Minute),
	gocron.NewTask(func() {
		v.flush(context.Background())
		cutoff := cutoffDate(time.Now().UTC())
		if err := v.repo.DeleteOlderThan(context.Background(), cutoff); err != nil { ... }
	}),
	gocron.WithTags(visitorSnapshotTag),
)
```

外加 `OnStop` 时补一次 `flush`（`visitor.go:55-57`），启动时 `restore` 最近 7 天（`visitor.go:70-85`）。

**净写入量：约 24 次 UPSERT / 天**（外加正常关闭一次）。相比之下，"每次浏览写一次库"在 Boop 上是灾难：那会占住 `maxOpenConns = 1` 的唯一连接，和发布、评论、限流抢同一把写锁。

**对 Boop 的落地与优先级修订**：

`Boop-体验优化报告.md` §5 第 21 项把"阅读统计"评为**「需注意」**，理由正是"不能逐次写库"。现在这条担忧有了已验证的形状，可以**从「需注意」降为「受约束」**：

- 一个 goroutine + 四个 channel（`record` / `query` / `today` / `load`），零 mutex。
- 计数只进内存 map，**不进数据库**。
- 落库由一个 60 分钟 ticker 驱动，一条 UPSERT。
- 关机时 flush 一次。
- 对外只有一个只读的 `Last7Days()` / `TodayStat()`。

**代价诚实说明**：进程被 `kill -9` 会丢掉至多 1 小时的计数。对一个个人博客的 PV 曲线来说，这个损失可以忽略 —— 但要在文档里写清楚，不要假装它是精确的。另外这套东西要真的有用，得先有内容量；**建议排在 P2，不要因为蓝图清晰就提前做。**

### 2.5 可读迁移格式：capsule 的思想（🔧 改写后采用 —— **本轮最重要的新增项**）

Ech0 的 capsule 是一个"内容胶囊"（`internal/capsule/`）：

- **格式**（`internal/capsule/schema.go:11-16`）：
  ```go
  const (
  	ManifestPath = "ech0.yaml"
  	CommentsPath = "comments.yaml"
  	EchoesDir    = "echoes"
  	FilesDir     = "files"
  )
  ```
- **每篇内容是一个 Markdown 文件**（`internal/capsule/layout.go:38-45`）：
  ```go
  func EchoPath(id string, createdAt time.Time) string {
  	utc := createdAt.UTC()
  	short := strings.ReplaceAll(id, "-", "")
  	if len(short) > 8 { short = short[len(short)-8:] }
  	return fmt.Sprintf("%s/%04d/%s-%s.md", EchoesDir, utc.Year(), utc.Format("2006-01-02"), short)
  }
  ```
  即 `echoes/2026/2026-09-29-a1b2c3d4.md`。
- **正文不在 YAML 里**（`EchoDoc.Content` 是 `yaml:"-"`），YAML 只放元数据（id / created_at / tags / layout / files / extension），正文就是那个 `.md` 文件本身。
- **它还能被编译成静态站**（`internal/capsule/build/`）：`build.go` 把 dataset 渲染成 HTML，`feed.go` 生成 feed 与 sitemap，`assets.go` 收资源 —— capsule 不只是备份，它是一个**可执行的出口**。作者自己的话是"可导入其他实例或**编译成静态站点**"。

**为什么这是本轮最重要的发现**：`Boop-体验优化报告.md` §0 把 Boop 的护城河定义为「**可 `cp` 走的十年**」。这句话是对的，但目前它指的是一份 `.db` 文件 —— **一个 SQLite 二进制**。而 Ech0 展示了护城河的更高形态：**一个人类可读、与工具无关的目录树**。

差别不是修辞：

| | 只有 `.db` | `.db` + 可读导出 |
|---|---|---|
| 十年后能打开吗 | 需要 SQLite 或兼容工具 | 任何文本编辑器 / `grep` / `git` |
| 能进版本控制看 diff 吗 | 不能 | 能 |
| 能全文搜索吗 | 要起工具 | `grep -r` |
| Boop 这个项目消失了呢 | 要自己写解析器 | 内容还在，格式自己解释自己 |
| 能迁到别的系统吗 | 要写转换脚本 | 几乎任何博客系统都吃 Markdown 目录 |

**对 Boop 的落地（新增，建议 P1）**：

```
boop export -o ./boop-export
```

产出：

```
boop-export/
  boop.yaml                     # 站点元数据 + 导出时间 + schema 版本 + 清单
  content/2026/2026-09-29-hello-world.md      # 每篇一个文件，front-matter 放元数据
  content/2026/2026-09-29-hello-world.assets/ # 该篇引用的图片（从 data/uploads 复制）
  comments.yaml                 # 评论按 post slug 归组
  files/                        # 未被任何文章引用的上传文件（可选）
```

设计要点（**每一条都是为了让格式"自我解释"**）：

1. **每篇一个 `.md` 文件**，路径含年月日 + slug，排序即时间序。文件名就是 URL 的自然对应物。
2. **YAML front-matter 放元数据**（`id` / `slug` / `type` / `published_at` / `tags` / `status` / `assets`），正文纯 Markdown —— 这样 `hugo`、`jekyll`、`obsidian`、`astro` 直接能吃。
3. **图片就近放**，用相对路径引用，让导出目录可以整体搬走而不丢图。
4. **评论单独一个 YAML**，不混进正文文件 —— 评论是"别人的话"，把它塞进你自己的 Markdown 会污染正文。
5. **`schema_version` 从一开始就写进 `boop.yaml`**。Ech0 的 `SchemaVersion = 1` 就是这个作用：格式会演进，没有版本号就没有迁移的可能。
6. **导出必须是只读操作** —— 只读数据库 + `os.MkdirAll` + 写文件，绝不碰线上数据。这一点让它可以放心地在任何时刻运行。

**顺带的附加值**：这份导出天然就是**备份**（比 `VACUUM INTO` 更耐时间），也是**迁移到任何其他博客系统的出口**。而且它反过来证明了一个卖点：*"Boop 的内容不是被锁在 Boop 里的"* —— 这句话比任何一个新功能都更能解释"为什么敢用十年"。

**不建议的**：不要现在做 Ech0 那样的 `capsule build`（把导出编译成静态站）。那是一条完整的 SSG 流水线，是另外一个产品。导出到 Markdown 目录为止。

### 2.6 全站写保护：`WriteGuard`（✅ 直接移植，作为可选小件）

**Ech0 的做法**（`internal/middleware/maintenance.go:21-42`）：一个进程内的原子布尔 `writeLocked`；置位时，任何非 `GET/HEAD/OPTIONS` 请求直接 `503` + `Retry-After: 30`：

```go
var readOnlySafeMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
}
```

读请求照常放行，写请求统一挡掉。

**对 Boop 的落地**：Boop 的写面很小（发布、评论、点赞、收藏、改设置、上传），这个中间件大概 20 行，与 `guardUnsafeMethods` 同层。用途：

- **换二进制时的安全窗口**：先置只读 → 确认没有写进 → 停服务 → 换 binary → 起服务 → 解除。
- **备份前**（虽然 `VACUUM INTO` 本身已经一致，但让人心里踏实）。
- 站长设置页加一个"维护模式"开关。

**诚实评估**：Boop 是单人站，这个功能的实际价值**低于**上面四项。它的真正价值在于"升级流程有一个可验证的中间态"，而不是"维护公告页"。**放 P2，不要挤进 P0。**

### 2.7 写保护之外：审计过但**不需要**抄的东西

| Ech0 的做法 | 核对结果 |
|---|---|
| `internal/middleware/staticfile.go:20-34`：可内联类型（image/audio/video）给 `immutable` 缓存，其余给 `Content-Disposition: attachment` 强制下载 | **Boop 不需要。** Boop 的 `media.Store` 只接受 jpg/png/webp/gif（`handlers_upload.go` 的 `ErrUnsupportedMedia` 分支明写），不存在上传 HTML/SVG 的路径，也就没有同源存储型 XSS 的入口。另外 Boop 已在 `middleware.go:61` 全局设 `X-Content-Type-Options: nosniff`。**这条已经做对了，别动。** |
| `/uploads/*` 的缓存头 | Boop 的 `handlers_upload.go:171` 早有 `public, max-age=31536000, immutable`，且有测试断言（`handlers_upload_test.go:583`）。**同上，别动。** |
| `internal/middleware/origin.go`：Origin/Referer 校验 | Boop 已有等价实现（CSRF 基线 + `guardUnsafeMethods`）。跳过。 |
| `internal/middleware/nocache.go` | Boop 对动态页面的默认行为是标准 `net/http`，无 `Cache-Control` 泄漏问题。跳过。 |
| `internal/middleware/poweredby.go` | 纯装饰。跳过。 |

---

## 3. ⛔ 不可抄清单（附理由）

按危险程度排序。

### 3.1 **AGPL-3.0 —— 这是法律问题，不是技术问题**

Ech0 的 license 是 **AGPL-3.0-or-later**（`LICENSE`，34 KB；另有 `NOTICE` 与 `COMMERCIAL.md` 双授权）。AGPL 是**强传染性**的，并且第 13 条专门覆盖**网络服务**场景：通过网络提供修改版服务的，必须向使用者提供对应源码。

**具体到 Boop**：

- **读它的代码学设计思路** → 安全。思路、算法、架构模式不受版权保护。
- **把它的代码片段（哪怕几十行）复制进 Boop** → 高危。Boop 目前**没有 LICENSE 文件**（`ls` 确认，根目录只有 `.env.example` / `.gitignore` / `AGENTS.md` / `README.md`），一旦掺入 AGPL 代码而后对外提供服务，就可能被要求以 AGPL 开源全部源码，或者收到合规通知。

**因此本报告的所有"移植"建议，都必须是"按描述的机制独立实现"，不是"抄那段代码"**。已经逐条检查过：报告里引用的所有代码段都短到只有机制描述作用，实现细节（命名、结构、边界处理）都要 Boop 自己重写。

**顺带**：`Boop-体验优化报告.md` §5 第 22 项已经提出"LICENSE + CI（并行，P0）"。这条现在**从"补个文件"升级为"必须在采纳任何 Ech0 思路之前定下来"**。建议 Boop 选一个明确许可（MIT 或 Apache-2.0 均可，取决于是否希望被商业使用），把边界先画清楚。

### 3.2 GORM `AutoMigrate` + 一堆补丁式 `Migrator`

Ech0 的建表方式是 `internal/database/database.go:136-163` —— 把 20 个模型丢给 `GetDB().AutoMigrate(models...)`，然后接一串手写迁移器（`database.go:116-133`）：

```go
dbMigration.NewLegacyTimeNormalizerMigrator(...),
dbMigration.NewStorageTimeSanitizeMigrator(),
dbMigration.NewStorageTimeValidateMigrator(),
dbMigration.NewStorageTimeUnixMigrator(),
dbMigration.NewStorageTimeSchemaRebuildMigrator(),   // ← 整表重建
dbMigration.NewOAuthBindingsDropMigrator(),
dbMigration.NewLegacyInboxesDropMigrator(),
dbMigration.NewAgentProtocolCollapseMigrator(),
dbMigration.NewAgentSettingProtocolRenameMigrator(),
dbMigration.NewUserLocalAuthBackfillMigrator(),
dbMigration.NewUsersPasswordDropMigrator(),          // ← 删列
dbMigration.NewEchoExtensionOrphansMigrator(),
```

**这一串的名字本身就是警告**。"TimeSchemaRebuild"（时间字段类型无法原地改，只能重建表）、"UsersPasswordDrop"、"OAuthBindingsDrop"、"AgentProtocolCollapse" —— 这些都是 schema 随功能试错而漂移的化石。`AutoMigrate` 让"加个字段"变得免费，代价是 schema 失去单一事实来源，最终必须靠一串手写补丁来收拾。

Boop 的做法（3 个不可变编号 SQL：`001_initial` / `002_search` / `003_asset_content_hash`）**严格更优**：每一次 schema 变更都是一个可 review、可 diff、可回放的文本文件，任何时刻的库状态都能从零重放出来。

**结论：不要引入 ORM，不要引入 AutoMigrate。** Boop 的手写 SQL + 编号迁移是这个项目最好的工程决策之一。

### 3.3 CGO

`internal/database/database.go:13,29,97-98`：

```go
sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
...
"gorm.io/driver/sqlite"     // ← mattn/go-sqlite3，cgo
...
if dbType == "sqlite" {
	sqlite_vec.Auto()
```

`mattn/go-sqlite3` 和 `sqlite-vec-go-bindings/cgo` 都要求 CGO。Ech0 为此必须构建 Docker 镜像、维护 `docker/build.Dockerfile`、用 Helm 分发 —— 换来的是向量检索能力。

Boop 用 `modernc.org/sqlite`（纯 Go 移植），因此可以 `CGO_ENABLED=0` 静态编译，交叉编译到 ARM/树莓派是 `GOOS=linux GOARCH=arm64 go build` 一条命令，CI 里不需要 C 工具链、不需要基础镜像。

**对于"为 1 核 500MB 小服务器而生"的定位，零 CGO 是核心资产，不是可以交易的筹码。**

### 3.4 `LIKE` 搜索

`internal/repository/echo/echo.go:79,407,497` 三处都是：

```go
query = query.Where("content LIKE ?", "%"+search+"%")
```

`%...%` 前导通配 —— **索引完全用不上，必然全表扫描**。而且它没有相关度排序、没有高亮片段。

Boop 用的是 FTS5，带高亮片段、带 `MATCH` 语法、带游标分页（`internal/search/search.go`），并且刻意用引号包裹 + AND 连接来免疫 FTS 语法字符。**这是 Boop 明显更强的地方，也是唯一一处"内容检索"这个核心能力上 Boop 领先。**（Ech0 的向量检索服务于 AI Copilot 的 RAG，是另一件事，不构成对全文搜索的替代。）

### 3.5 Vue SPA + 43,092 行前端

Boop 的铁律是"无 Node 构建链"（`web/` 用 `go:embed` 直接嵌入 801 行 CSS + 781 行 JS）。Ech0 的前端是 Vue 3 + TypeScript + UnoCSS + pnpm 多模块（`web/` / `site/` / `hub/`），仅 `web/src` 就 43,092 行。

引入构建链对 Boop 意味着一连串连锁反应：`node_modules`、锁文件、CI 里的 Node 步骤、构建产物与二进制的版本对齐问题、`pnpm-lock.yaml` 的安全公告。**与"单二进制、`cp` 就能跑"的定位直接冲突。**

### 3.6 132 个直接依赖

`go.mod` 实测 132 条 `require`。包括 `gin` `gorm` `huma/v2`（OpenAPI）`gocron/v2`（调度）`ristretto/v2`（缓存）`aws-sdk-go-v2`（4 个模块，S3）`coreos/go-oidc` `go-webauthn` `anthropic-sdk-go` `charm.land/huh` `charm.land/lipgloss`（TUI）……

Boop 有 **3 个**（`golang.org/x/crypto` / `golang.org/x/term` / `modernc.org/sqlite`）。

> 附带发现：Boop 的 `go.mod` 把 `goldmark`、`bluemonday`、`douceur` 错误地标成了 `// indirect`，但它们是 Markdown 渲染与 HTML 净化的**直接**依赖。这是一行 `go mod tidy` 的事，顺手修掉。

每一个依赖都是一条长期责任：安全公告要跟、破坏性升级要处理、传递依赖冲突要解。130 个 vs 3 个，是两种完全不同的维护生活方式。

### 3.7 无上限连接池

Boop 的 `internal/store/store.go:32-35`：`maxOpenConns = 1` + DSN `_txlock=immediate`。
Ech0 的 `internal/database/database.go:68`：`_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_txlock=immediate` —— **同样的 `_txlock=immediate`**，但全文搜索确认 **Ech0 从未调用 `SetMaxOpenConns` / `SetMaxIdleConns` / `SetConnMaxLifetime`**，即 GORM 默认的无限连接池。

两者对同一个问题给出了**相反**的答案，而且各自内部自洽：

| | Boop：单连接串行 | Ech0：无限池 + busy_timeout |
|---|---|---|
| 写法冲突 | 不可能（根本没有并发写） | 可能，靠 `busy_timeout=5000` 退避重试 |
| 最坏表现 | **一条慢语句阻塞全站** | 写突发时 `database is locked` + 最长 5 秒停顿 |
| 需要的前提 | **索引必须干净** | 连接数必须可控 |

**这个对照把一条结论钉死了**：`Boop-体验优化报告.md` §0 把"`likes` 缺 `post_id` 索引"评为 P0 第 1 项 —— 在 `maxOpenConns = 1` 的架构下，那次全表扫描不是"分摊到某个连接上"，而是**独占唯一连接串行扫描**。**索引卫生对 Boop 是正确性级别的要求，不是性能优化。**

**所以：绝对不要因为 Ech0 没设上限就把 Boop 的上限放开。** 要放开，就必须同时接受锁竞争、引入 `busy_timeout`、并重新论证所有写路径的幂等性 —— 那是一次架构变更，不是一次调优。

### 3.8 信任 `X-Forwarded-Proto` / `X-Forwarded-Host`

见 §2.3 的"问题二"。Ech0 把它用来拼 sitemap 里的绝对 URL。Boop 已经在报告里定了"用 `BOOP_BASE_URL`，不读请求 Host"，**保持原判**。

---

## 4. 🏆 Boop 已经更强的地方（不许退步）

这份对照容易被误读成"Boop 落后"，所以要显式列出 Boop 领先的项：

1. **全文检索**：FTS5 + 高亮片段 + 游标分页，对比 Ech0 的 `LIKE '%x%'` 全表扫描。这是内容型产品的核心能力。
2. **SSR 的逐页 SEO**：Boop 在服务端同时握有内容和 URL，`canonical` / `og:*` / JSON-LD / 逐篇 sitemap 都是"填模板"级别的成本。Ech0 因为 SPA，要为同一件事造 SSG 流水线。
3. **零 CGO 交付**：`CGO_ENABLED=0` 单静态二进制，交叉编译一行命令，CI 不需要 C 工具链。
4. **3 个直接依赖**：攻击面、升级面、供应链面都小两个数量级。
5. **不可变编号迁移**：schema 的单一事实来源，可 audit、可重放。Ech0 的 12 个补丁式 `Migrator` 是反面教材。
6. **性能地基明确**：`maxOpenConns = 1` + `_txlock=immediate` 是一个**能被推理**的模型 —— 任何写都是全序的，任何读都有一致的隔离预期。这个性质让所有并发问题都退化成"索引和查询复杂度"问题。

**最后一项是这个项目最被低估的资产。** Ech0 用无限连接池换取了吞吐上的弹性，代价是它必须永远带着 `busy_timeout` 和重试语义；Boop 用单连接换掉了整整一类 bug。在 1 核 500MB 的目标机器上，Boop 的取舍更划算。

---

## 5. 对既有优化报告的修订

以下修订直接落在 `Boop-体验优化报告.md` 上。

### 5.1 改写

| 原条目 | 修订 |
|---|---|
| §5 #2 `/static/*` ETag + 长缓存 | **保留**，但补充：`embed.FS` 的 ModTime 是零值，`Last-Modified` 不可用，必须用**启动时算一次的内容哈希 ETag**。 |
| §5 #3 gzip 中间件 | **改为**「构建期预压缩 `.gz` + 请求期按 `Accept-Encoding` 择优发送」。取消新增中间件。见本文 §2.2。 |
| §5 #12 备份 runbook（`sqlite3 .backup`） | **改为**「服务内 `VACUUM INTO` + `boop backup -o` 子命令」。不再要求部署机器装 `sqlite3`。见本文 §2.1。 |
| §5 #21 阅读统计（原评「需注意」） | **降为「受约束」**：有了已验证的 actor + 小时级 flush 蓝图，它不再需要第二个并发模型。但仍建议留在 P2。见本文 §2.4。 |
| §7.2 的"注意：压缩应在内存中完成后再写"警告 | 该警告随 gzip 中间件一并作废 —— 预压缩方案里根本不存在流式压缩这条路径。 |

### 5.2 新增

| # | 建议 | 类型 | 影响 | 成本 | 铁律 | 阶段 |
|---|---|---|---|---|---|---|
| 23 | `boop export -o` 导出为可读 Markdown 目录树（`content/YYYY/` + `boop.yaml` + `comments.yaml` + 就近资源） | 粘性 | **5** | 3 | 受约束（纯只读） | **P1** |
| 24 | `GET /robots.txt` + `GET /sitemap.xml`，sitemap **逐篇**含 `<loc>` + `<lastmod>`（SSR 白送的能力，不做可惜） | 粘性 | 4 | 1 | 受约束 | P0 |
| 25 | `go mod tidy` 修正 `goldmark` / `bluemonday` / `douceur` 的 `// indirect` 误标 | 交付 | 2 | 1 | 受约束 | P0 |
| 26 | 维护模式（只读）：进程内原子布尔 + 非 GET 一律 503 + `Retry-After`，站长设置页开关 | 质量 | 2 | 1 | 受约束 | P2 |
| 27 | **LICENSE** —— 在采纳任何第三方项目思路之前先定下来（见本文 §3.1） | 交付 | **5** | 1 | 受约束 | **P0**（插队到最前） |

> #23 的影响评 5 分不是因为它"功能强"，而是因为它是**唯一一条同时增强可迁移性、备份可靠性、对外说服力的改动**。它把"可 `cp` 走的十年"从一句部署文案变成一份人能读的目录。

### 5.3 明确不做

- ❌ 把导出目录编译成静态站（Ech0 的 `capsule build`）—— 那是另一个产品。
- ❌ 向量检索 / RAG / MCP server —— Ech0 这两块的先决条件（cgo、多依赖、RAG 流水线）全部与 Boop 的铁律冲突。
- ❌ WebAuthn / OIDC / S3 / 多账户权限 / TUI / Helm —— 每一个都在把 Boop 往"另一个 Ech0"的方向推。
- ❌ 放开 `maxOpenConns`。

---

## 6. 修订后的 P0 清单（5 项 → 7 项）

按"影响 ÷ 成本"排序，全部不触碰工程铁律：

| 顺序 | 做什么 | 依据 |
|---|---|---|
| 1 | **LICENSE**（先画法律边界，再动手吸收外部思路） | 本文 §3.1 |
| 2 | 迁移 004：`CREATE INDEX idx_likes_post ON likes(post_id)` | 原报告 §7.1 + 本文 §3.7（单连接放大） |
| 3 | `/static/*`：预压缩 `.gz` + 内容哈希 ETag + `immutable` | 本文 §2.2 |
| 4 | `/robots.txt` + 逐篇 `/sitemap.xml` + `base.html` 的 `canonical` / `og:*` / JSON-LD | 本文 §2.3 |
| 5 | 三处小修：详情页照片不裁切、移动端搜索入口、搜索排序去 `COALESCE`；过期 session 清扫 | 原报告 §6 |
| 6 | `go mod tidy` 修正依赖标注 | 本文 §3.6 |
| 7 | CI：把 `gofmt -l` / `go vet` / `go test -race` / `node --check` 接上 GitHub Actions | 原报告 §5 #22 |

P1 主线不变（内容管理页 `/admin/posts` 是第一优先），**新增 #23 `boop export`** 作为并列第一优先 —— 理由见 §5.2 的注。

---

## 7. 下一步

1. **P0 已全部执行完毕**（10 个提交，`28e04df` … `e8904e7`），逐条结果、实测数据与过程中新发现的缺陷见 [`Boop-优化执行记录.md`](./Boop-优化执行记录.md)。唯一未动的是 §3.1 的 **LICENSE**，它需要人决定 MIT 还是 Apache-2.0。
2. **本文是否需要改进既有报告的正文？** 本文只做增量修订，没有回改 `Boop-体验优化报告.md` 的正文。若要让两份文档合并成一份单一事实来源，说一声就合并。
3. **本地仍有 13 个未推送提交**（上一轮 3 个 + 本轮 10 个），等审阅后推送。CI 也要等首次推送才会真正跑起来。
4. **P1 是否开工？** 两位并列第一优先：`/admin/posts` 内容管理页（原报告 §5 #9，影响 5）与 `boop export -o` 可读目录树（本文 #23，影响 5）。

---

### 附：核对方法与可复现性

- Ech0 源码：`git clone --depth 1 https://github.com/lin-snow/Ech0.git`，基线 `f53e4eb`（v5.8.0）。
- 本文引用的所有 Ech0 行号均在本地克隆上逐条打开确认；引用 Boop 的行号同样在本地 `113ef8f` 工作树上确认。
- 数值指标（代码行数、依赖数、测试函数数）为本地实测，非引用自 README。
- **未实测**：Ech0 的实际内存占用、并发写入下的锁竞争表现、capsule 在真实迁移中的往返成功率。本文对这三项的表述均基于代码阅读推断，已相应标注。
