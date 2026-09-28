# Boop

Boop 是一个为个人品牌设计的轻量个人博客：使用 Go 服务端渲染、SQLite 和原生 CSS/JavaScript，在 1 核、500MB–1GB 内存的小服务器上也能运行。

界面参考 X/Twitter 的信息层级与交互密度，但不复制其品牌资产或社交平台机制。站长负责发布内容，读者可以注册、评论、回复、点赞和收藏。

## 当前能力

- 动态、文章、摄影三种内容和首页快捷发布
- 邮箱密码登录、GitHub OAuth、注册开关
- 评论与一级回复、可选审核、点赞、收藏
- 本地图片上传、类型检测、大小限制和内容去重
- 后台站点、认证、评论和 AI 设置
- 访问触发的 AI 作者状态缓存，无定时任务
- 文章摘要、标签和 SEO 写作建议，不自动覆盖草稿
- SQLite FTS5 搜索、X 风格搜索结果页
- RSS 2.0 输出
- 浅色/深色主题和响应式三栏布局

## 技术结构

- Go 1.27
- `net/http`、`html/template`、embedded static assets
- SQLite（`modernc.org/sqlite`，WAL，单进程）
- goldmark + bluemonday
- 原生 CSS/JavaScript

项目不需要 Node 构建、Redis、ORM、消息队列或独立前端服务。

## 快速开始

### 1. 配置环境

复制 `.env.example` 中的变量到运行环境。生产环境至少应设置：

- `BOOP_SESSION_SECRET`：不少于 32 字节的随机值
- `BOOP_MASTER_KEY`：32 字节随机数据的 Base64，用于加密 OAuth/AI 密钥
- `BOOP_BASE_URL`：站点公开地址
- `BOOP_SECURE_COOKIES=true`：HTTPS 生产环境必须启用

PowerShell 生成随机值：

```powershell
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(48)) # session secret
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)) # master key
```

### 2. 初始化站长

```powershell
go run ./cmd/boop init-owner --email owner@example.com --name "站长"
```

程序会在终端中隐藏输入密码，并拒绝创建第二个 owner。

### 3. 启动

```powershell
go run ./cmd/boop serve
```

默认监听 `127.0.0.1:8080`，数据保存在 `./data`。

## 开发验证

```powershell
gofmt -w .
go test ./... -count=1
go vet ./...
node --check web/static/app.js
git diff --check
```

## 文档

- [产品规格](docs/PRODUCT.md)
- [HTTP API](docs/API.md)
- [数据库与事务规则](docs/DATABASE.md)
- [MVP 实施计划](docs/superpowers/plans/2026-09-28-boop-mvp.md)
- [AI 实施约束](docs/superpowers/task8-ai-implementation-brief.md)
- [搜索与 RSS 实施约束](docs/superpowers/task9-search-rss-implementation-brief.md)

## 项目状态

MVP Tasks 1–9 已完成并经过测试与独立审查。下一阶段是计划中的 Task 10：容器部署、Caddy 示例、备份恢复、端到端测试、smoke 脚本和低资源性能门禁。

准备让新的智能体继续开发时，请先阅读仓库根目录的 [`AGENTS.md`](AGENTS.md)。

## 安全提示

- 不要提交 `.env`、API Key、OAuth Secret、Session Secret 或 `BOOP_MASTER_KEY`。
- `BOOP_MASTER_KEY` 丢失后，已保存的 GitHub/AI 密钥无法恢复。
- SQLite 数据库与上传目录应作为同一份持久数据一起备份。

## License

当前仓库尚未声明开源许可证。
