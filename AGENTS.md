# Boop Agent Guide

本文件供接手仓库的开发智能体使用。

## 开始前

1. 运行 `git status --short`，不得覆盖或回滚已有修改。
2. 阅读 `README.md`、`docs/PRODUCT.md`、`docs/API.md`、`docs/DATABASE.md`。
3. 阅读 `docs/superpowers/plans/2026-09-28-boop-mvp.md`；Tasks 1–9 已完成，下一项是 Task 10。
4. 从 `go.mod`、现有代码和测试确认工具链，不自行替换技术栈。

## 不可改变的产品边界

- 名称：Boop。
- 视觉参考 X/Twitter 的布局、间距、边框、信息密度和克制蓝色强调；不得复制 X 品牌资产。
- 只有 owner 发布；读者只参与评论、回复、点赞与收藏。
- 内容只有动态、文章、摄影；不要添加“项目”模块。
- 登录为邮箱密码或 GitHub；不要求邮箱验证；注册和评论审核可由后台开关。
- AI 作者状态由页面访问触发缓存刷新，不得增加定时任务或独立 worker。
- AI 建议不得自动覆盖编辑器内容。
- 目标机器为 1 核、500MB–1GB；优先单进程、低内存和少依赖。

## 架构边界

- 单 Go 进程提供 SSR、静态资源和 JSON API。
- SQLite 是唯一持久服务；单进程运行是 v0.1 的并发边界。
- 上传可以放在本地目录或兼容 S3 的对象存储（`BOOP_R2_*`，见 `docs/STORAGE.md`），但两者都是同一个进程内的分支：不得为此引入 SDK、队列、预签名服务或外部读取代理。对象存储的签名是标准库手写的 SigV4，只实现 `PutObject` 与 `DeleteObject`。
- 不引入 SPA、Redis、ORM、队列、微服务、代码生成器或只有一个实现的接口。
- 优先复用现有模块和标准库；不要为未来需求预建抽象。
- 数据库迁移一经提交不得修改，只能新增版本。
- 保持现有 API、错误码、权限和数据库语义兼容。

## 安全规则

- 所有写路由必须保留认证、角色授权、CSRF、Origin、body limit 和严格输入校验。
- 不得记录或返回 API Key、密码、OAuth Secret、Session token、提示词或模型原始响应。
- 密钥继续使用 `BOOP_MASTER_KEY` 加密，不得回退为明文配置。
- HTML 必须由 `html/template` 自动转义，或来自现有已消毒的 `body_html` 路径。
- 不把 `.env`、`data/`、数据库、上传内容或构建产物提交到 Git。

## 当前实现状态

- Tasks 1–7：基础服务、SQLite、认证、内容、上传、互动、后台设置与 GitHub OAuth。
- Task 8：AI 作者状态与写作助手。
- Task 9：RSS 2.0（该任务曾附带一套 SQLite FTS5 站内搜索，已按站长要求整体移除：页面、API、`internal/search` 包与 FTS 表都不在代码里）。
- 当前下一项：Task 10 发布交付。

Task 8/9 的具体约束已经保存在：

- `docs/superpowers/task8-ai-implementation-brief.md`
- `docs/superpowers/task9-search-rss-implementation-brief.md`

## Task 10 交付要求

严格按计划文件 Task 10 推进，至少包括：

- 多阶段 Dockerfile、compose 示例和安全环境变量示例
- Caddy HTTPS 反向代理说明
- owner 初始化、升级、持久化、备份与恢复
- 关键业务端到端测试，不依赖真实 GitHub 或 AI
- Windows PowerShell `scripts/smoke.ps1`
- `go test -race ./...`、`go vet ./...`、release build、临时实例 smoke
- 记录二进制大小、空闲 RSS、缓存首页 p95
- RSS 超过 150MB 或缓存首页 p95 超过 150ms 时不得宣称发布通过

## 完成前验证

```powershell
gofmt -w .
go test ./... -count=1
go vet ./...
node --check web/static/app.js
git diff --check
```

涉及部署或性能时，还要执行计划中的 race、smoke 和实际进程测量。所有完成声明必须附带真实命令结果；失败不得伪装为通过。

同一套门禁由 `.github/workflows/ci.yml` 执行，另有三条本机不强制但 CI 会拦的检查：`go mod tidy -diff`（依赖标注与代码不一致时直接失败）、`CGO_ENABLED=0 go build ./cmd/boop`（新增任何需要 C 编译器的依赖都会在这里暴露），以及独立的 `go test -race ./...`。本地命令与 CI 只应因机器不同而产生差异。

## 提交约定

- 改动保持聚焦，一项任务一个可审查提交。
- 不顺手重构无关模块。
- 未经用户明确要求，不 push、force-push、rebase 或改写历史。
- 提交前检查 staged diff 和敏感信息。
