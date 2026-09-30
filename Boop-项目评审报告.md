# Boop 项目评审报告

**评审对象**：`https://github.com/Versior/Boop`
**评审时间**：2026-09-29
**评审方式**：GitHub REST API 元数据 + 完整克隆源码静态审查 + **本地工具链实测（Go 1.27.1 / Node 22）**
**评审范围**：架构选型、安全实现、代码质量、测试、工程配套、文档

---

## 一、结论摘要

**Boop 的代码质量显著高于同规模个人项目；它的短板不在实现，而在工程配套。**

实测结论：代码可构建、可启动、测试全绿、竞态检测干净，安全基线扎实。它已经是一个**经过独立验证的功能完备 MVP**，但距离**可上线交付**和**可复用开源项目**还差三件配套工作。

三个决定性缺口：

1. **无 LICENSE** —— README 自述"尚未声明开源许可证，默认保留全部权利"。
2. **交付配套缺失** —— 无 Dockerfile、无 CI、无备份恢复脚本，Task 10 未开始。
3. **发现并修复了 1 个发布门禁级缺陷** —— 登录限流测试在 `-race` 下会因墙钟自然回填而偶发失败，直接阻塞 Task 10 的 `go test -race` 门禁。已在本地修复（测试侧注入时钟，见第五节）。

| 维度 | 评分 | 说明 |
|---|---|---|
| 架构与选型 | 9 / 10 | 边界克制，依赖极简，无过度设计 |
| 安全实现 | 9 / 10 | 基线扎实，细节处理成熟 |
| 代码质量与可读性 | 9 / 10 | 注释解释"为什么"，0 个 TODO/FIXME |
| 测试 | 8.5 / 10 | 367 个测试函数，全绿；1 处时钟依赖已修复 |
| 工程配套 | 4 / 10 | 无 LICENSE / CI / Docker |
| 文档 | 9 / 10 | 决策级文档，非流水账 |
| **综合（作为源码）** | **8.5 / 10** | 值得精读的参考实现 |
| **综合（作为可交付产品）** | **5.5 / 10** | 尚不具备上线条件 |

---

## 二、项目事实（已实测校验）

| 项目 | 值 |
|---|---|
| 描述 | Lightweight X-inspired personal blog built with Go SSR and SQLite |
| 主语言 | Go |
| 提交数 | 20 |
| 贡献者 | 1（Versiorii） |
| Star / Fork | 0 / 0 |
| 受管文件 | 86 |
| Go 代码量 | 25,041 行，23 个测试文件 |
| 测试函数 | 367 个，含 205 个子测试 |
| 前端 | 797 行 CSS + 781 行 JS，无构建链 |
| 直接依赖 | 3 个：`x/crypto`、`x/term`、`modernc.org/sqlite` |
| 许可证 | **无** |
| 仓库大小 | 347 KB |
| **构建产物** | **15 MB**（`-trimpath -ldflags "-s -w"`） |

**技术栈**：Go `net/http` + `html/template` + embedded assets；SQLite（WAL + FTS5，单进程写入）；goldmark + bluemonday（Markdown → 已消毒 HTML）；OpenAI-compatible API（可选、访问触发、结果缓存）。

**产品边界**：只有 owner 发布；读者可评论 / 回复 / 点赞 / 收藏；内容形态仅动态、文章、摄影三种。

---

## 三、突出优点

### 1. 安全实现是最大亮点

这一层的完成度明显超出同类项目，且每一处都写了理由：

- **会话**：32 字节随机 token，数据库只存 SHA-256；每次登录执行轮换（防会话固定）；比较用 `subtle.ConstantTimeCompare`。
- **CSRF**：由 token hash 经 HMAC 派生（`boop-csrf-v1` 标签版本化），不存明文；叠加 Origin / Referer 同源校验；无 Origin 证据且未登录时直接拒绝写请求。
- **密码**：bcrypt（DefaultCost）；未知邮箱也执行 dummy hash 校验，抹平登录耗时侧信道。
- **密钥存储**：AES-256-GCM，将 setting key 作为 AAD 绑定，密文无法跨行搬运；`BOOP_MASTER_KEY` 缺失时功能报"未配置"而非退化为明文。
- **HTTP 头**：CSP 明确禁止 `unsafe-inline` / nonce 逃逸口，`nosniff`、`X-Frame-Options: DENY`、`frame-ancestors 'none'`、Permissions-Policy 全开。**已实测确认响应头齐全**。
- **文件上传**：按字节嗅探真实 MIME，并要求客户端扩展名与内容一致（拒绝伪装）；存储键强校验 `<YYYY>/<MM>/<32 hex>.<ext>` 使目录穿越不可达；16 字节随机键不可猜；临时文件 + `rename` 原子落盘；SHA-256 去重且有事务内二次检查防竞态。
- **限流**：token bucket，覆盖登录 / 注册 / 评论 / 评论-IP / OAuth / AI 六类；email 键先哈希再入 map，防止长邮箱名膨胀内存；map 惰性清扫（`sweepEvery=1m`、`idleTTL=10m`）。
- **出站请求**：GitHub 与 AI 调用均有显式超时 + 响应体硬上限（1 MiB）+ 错误只暴露稳定 code，绝不带 API Key、prompt、模型原文或上游响应体。AI 另有进程级并发信号量（上限 2）。
- **XSS**：`template.HTML` 的每一处注入点都标了 `nolint:gosec` 并注明"写入时已消毒"，没有裸露的未消毒 HTML 通路。

### 2. 数据层设计规范

WAL + FTS5；迁移文件声明为**不可变**（变更只能新增编号）；索引大量使用部分索引（如 `WHERE deleted_at IS NULL`、`WHERE status='pending'`），说明作者理解实际查询形态而非机械建索引。分页统一使用 `<published_at,id>` 游标，保证不跳行、不重复，且搜索结果复用同一游标——这是一个容易被忽略的一致性细节。

### 3. 可测试性与工程纪律

- 时钟、`http.Client`、OAuth base URL 全部可注入；GitHub 流程用 `httptest` 打桩，测试不依赖真实外部服务。
- `AGENTS.md` 明确禁止引入 SPA / Redis / ORM / 队列 / 微服务 / 代码生成器，以及"只有一个实现的接口"——这是一条克制且正确的架构约束。
- 全仓库 **0 个 TODO / FIXME / HACK**。
- 提交约定要求"所有完成声明必须附带真实命令结果；失败不得伪装为通过"。

### 4. 文档是决策级的

`docs/` 共 824 行，包含 PRODUCT / API / DATABASE / MVP 实施计划 / Task 8、9 实施约束。文档写成约束与理由，而不是功能罗列——数据库文档是 schema 的唯一真源，代码注释会回指具体章节（如 `docs/PRODUCT.md §5.2`）。

---

## 四、实测记录（Go 1.27.1 / darwin-arm64）

仓库自述"Tasks 1–9 已完成并通过测试"——本次已独立复现，**结论成立**。

| 检查项 | 命令 | 结果 |
|---|---|---|
| 格式 | `gofmt -l .` | 无输出（干净） |
| 静态检查 | `go vet ./...` | 通过 |
| 单元测试 | `go test ./... -count=1` | 12 个包全部 `ok` |
| 竞态测试 | `go test -race ./... -count=1` | 12 个包全部 `ok`（修复后） |
| 构建 | `go build -trimpath -ldflags "-s -w" ./cmd/boop` | 成功，15 MB |
| 前端语法 | `node --check web/static/app.js` | 通过 |
| 空白检查 | `git diff --check` | 通过 |
| 启动冒烟 | 临时数据目录启动 `boop serve` | 正常，日志为结构化 JSON |
| `/healthz` `/readyz` `/` `/feed.xml` `/api/v1/posts` | curl | 200 / 200 / 200 / 200（RSS 2.0）/ 200 |
| 安全响应头 | curl -D | CSP、nosniff、X-Frame-Options、Referrer-Policy、Permissions-Policy 全部存在 |
| 404 | `GET /nope` | 404，JSON 错误信封 |
| 数据目录 | 启动后 | 正确生成 `boop.db` + WAL/SHM |

---

## 五、发现的问题与风险（按优先级）

### P0 —— 阻塞性问题

**1. 无 LICENSE**
README 明确"默认保留全部权利"。法律上他人既不能合规使用，也不能提交贡献。若打算公开，这是第一优先级。个人自用可忽略。

**2. `go test -race` 门禁失败：登录限流测试依赖墙钟（已修复）**

**现象**：`go test -race ./...` 中 `internal/server` 包 FAIL：

```
--- FAIL: TestLoginRateLimitRefusesWithRetryAfter (9.64s)
    --- FAIL: .../the_same_address_stays_limited (0.53s)
        handlers_social_test.go:882: status = 401, want the address limit to hold
```

**根因**：登录限流器以真实时间运行（`newServer` 中 `newLimiters(time.Now)`）。`loginBurst=10`、`loginRefill=6s`。测试先耗尽地址桶，随后在 `the same address stays limited` 子测试中断言同一地址仍被限流。地址桶每 6 秒自然回填一个 token，而该用例自身包含 11 次带 bcrypt 的登录尝试（在 `-race` 或高负载机器上显著变慢，实测用例耗时 9.64s），墙钟跨过 6 秒后地址重新可用，于是返回 401 而非 429。

**性质**：**测试缺陷，不是产品缺陷**——生产环境 6 秒回填是设计意图。但它是**时间敏感的偶发失败**（单独跑常绿、全包跑才红），恰好卡在 Task 10 的发布门禁上，属于必须处理项。

**修复**：`limiter` 本身已支持注入时钟（`newLimiters(now)`），只是 HTTP 层夹具未使用。在测试开头把登录限流器切换到手动时钟：

```go
clock := newFakeClock()
f.srv.limiters.login = newLimiters(clock.Now).login
```

**验证**：`-race` 下定点点重跑 3 次全绿；整个 `internal/server` 包 `-race` 全绿；`go test -race ./...` 12 个包全绿。

### P1 —— 上线前必须补齐

**3. Task 10 未开始，不具备交付形态**
缺 Dockerfile、compose 示例、Caddy HTTPS 示例、owner 初始化/升级/备份恢复流程、端到端 smoke、低资源性能门禁、发布门禁（计划中的门限：RSS ≤150 MB、缓存首页 p95 ≤150 ms）。

**4. 无 CI**
所有质量承诺依赖人工执行，没有自动化回归门禁。仓库无 `.github/`。上面第 2 条正是"无 CI 会漏掉什么"的实例：这个偶发失败在本地手工跑一次通常不会暴露。

### P2 —— 建议关注

**5. 限流的单进程假设**
限流器是进程内存态：多副本部署时额度按副本数翻倍，重启后清零。文档已声明"单进程是 v0.1 并发边界"，不算隐藏缺陷；但代码只读 `RemoteAddr`、不解析 `X-Forwarded-For`。部署在反代后若未由 Caddy/nginx 覆写来源地址，限流会按代理 IP 聚合，导致误伤或失效。文档有提及，代码不处理，需在部署配置中保证。

**6. `go.mod` 直接依赖标注不正确**
`goldmark`、`bluemonday`、`golang.org/x/net` 是被直接 import 的（Markdown 渲染与消毒），但在 `go.mod` 中被标为 `// indirect`，说明提交前未跑 `go mod tidy`。不影响构建，建议补一次；本次为保持改动聚焦未纳入提交。

**7. `POST /feed.xml` 返回 403 而非预期的 405**
路由层的 `handleFeedMethodFallback` 注释声明"405 总是带 Allow 头"，但 `guardUnsafeMethods` 先于它执行：无 Origin 证据且未登录的写请求会被判 `origin_required` 403，因此该 405 分支在未认证场景下不可达。安全性无问题（拒绝更早），但与注释描述的契约不一致。建议二选一：放宽该路径的 Origin 前置校验，或修正注释与文档。

**8. 上传图片不重编码**
按原始字节存储。合规范畴内的 JPEG/PNG 可能携带 EXIF（含 GPS）等元数据；GIF/WebP 解码器历史上有过漏洞面。CSP 已使 SVG 这类主动内容无法利用（SVG 直接被拒），因此**不是 XSS 风险**，但存在隐私泄露与资源面收缩空间。可考虑转码或剥离 EXIF。

**9. 上传文件公开且永久缓存**
`Cache-Control: public, max-age=31536000, immutable`，键随机不可猜，但 URL 一旦泄露无法撤回（无删除接口、无签名 URL）。个人站可接受。

**10. 无邮箱验证**
这是明确的产品决策（注册靠后台开关 + 评论审核），意味着任何人可占用任意未注册邮箱地址作为账号标识。个人站风险低。

**11. 反垃圾仅限速率**
有限流，无内容层反垃圾，AI 也未用于评论审核。开放注册 + 开放评论时需要人工介入。

**12. `BOOP_MASTER_KEY` 不可恢复**
密钥丢失则已存储的 GitHub/AI 密钥永久不可解。README 已提示。若要降低风险，可考虑导出/轮换机制。

**13. SQLite 驱动选型需实测**
使用 `modernc.org/sqlite`（纯 Go，免 CGO、交叉编译友好），代价是性能与内存占用通常弱于 `mattn/go-sqlite3`。在 1 核 / 500 MB 目标上是否达标，正是 Task 10 性能门禁要回答的问题，目前**没有数据**（本次沙箱环境无法读取进程 RSS）。

---

## 六、加固建议（按投入产出排序）

| 优先级 | 动作 | 收益 |
|---|---|---|
| 1 | 选定并加入 LICENSE | 决定项目能否被他人使用和贡献 |
| 2 | 加最小 CI（fmt / vet / test / `go test -race` / `node --check`） | 让偶发失败在合并前暴露，而非依赖人工 |
| 3 | 完成 Task 10（Dockerfile + Caddy + 备份恢复 + smoke + 性能门禁） | 从源码变成可部署产物 |
| 4 | 部署文档中明确反代必须覆写来源地址 | 避免限流按代理 IP 聚合失效 |
| 5 | 补 `go mod tidy`，修正直接依赖标注 | 保持模块元数据准确 |
| 6 | 校正 `POST /feed.xml` 的 403/405 契约 | 消除注释与行为的偏差 |
| 7 | 上传链路加 EXIF 剥离或转码（可选） | 消除元数据泄露面 |

---

## 七、总体评价

Boop 是一个**按设计文档写代码**的项目，而不是堆功能的项目。作者对安全细节、并发竞态、错误脱敏、游标分页一致性等容易被跳过的部分都有处理，并且把"为什么不这么做"写进了注释和 `AGENTS.md`。限量依赖（3 个直接依赖）、无构建链、无 ORM、无队列的克制选型，与"1 核 500 MB"的目标是自洽的。

经本地实测，它**能构建、能启动、测试全绿、竞态检测干净**——仓库的自我声明是可信的。本次唯一发现的缺陷是一个时间敏感的测试写法，属于"无 CI 掩盖偶发失败"这一过程问题，而非实现问题。

补齐三个工程配套缺口（许可证、CI、部署交付）的成本是可预估且机械的，不需要重构。**完成 Task 10 之后，这个项目的可交付性评分会从 5.5 直接跳到 8 以上。**

---

## 附：评审局限性

1. **未做动态安全测试**：未做渗透测试，未跑 `govulncheck` 依赖漏洞扫描。
2. **未测资源占用**：沙箱禁止读取进程 `ps`。二进制体积（15 MB）已实测，但空闲 RSS 与首页 p95 未测——这两项是 Task 10 的门禁指标，仍需在目标机器上补齐。
3. **前端未逐行审查**：`app.js`（781 行）与 `app.css`（797 行）仅做语法与结构确认，未做逻辑级审查。
4. **未评估协作面**：未检查是否存在已关闭的 PR、Issue 讨论或分支差异。
5. **仅评审单次快照**：`main` 分支 20 次提交的时点状态，未跟踪后续变更。
