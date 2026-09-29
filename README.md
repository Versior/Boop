<p align="center">
  <img src="web/static/brand/boop-mark.svg" width="112" height="112" alt="Boop Logo">
</p>

<h1 align="center">Boop</h1>

<p align="center">
  <strong>把文章、动态与摄影，放进一个真正属于你的主页。</strong>
</p>

<p align="center">
  <a href="https://github.com/Versior/Boop/stargazers"><img alt="GitHub Stars" src="https://img.shields.io/github/stars/Versior/Boop?style=flat-square&color=0f1419"></a>
  <a href="https://github.com/Versior/Boop/commits/main"><img alt="Last Commit" src="https://img.shields.io/github/last-commit/Versior/Boop?style=flat-square&color=1d9bf0"></a>
  <img alt="Go" src="https://img.shields.io/badge/Go-1.27-00ADD8?style=flat-square&logo=go&logoColor=white">
  <img alt="SQLite" src="https://img.shields.io/badge/SQLite-FTS5-003B57?style=flat-square&logo=sqlite&logoColor=white">
  <img alt="No Node.js" src="https://img.shields.io/badge/Node.js-not_required-339933?style=flat-square&logo=nodedotjs&logoColor=white">
</p>

<p align="center">
  <a href="#-为什么选择-boop">亮点</a> ·
  <a href="#-功能">功能</a> ·
  <a href="#-快速开始">快速开始</a> ·
  <a href="#-技术架构">架构</a> ·
  <a href="#-项目文档">文档</a>
</p>

---

## ✨ 为什么选择 Boop

| 轻量运行 | 个人品牌 | AI 辅助 |
| :---: | :---: | :---: |
| Go 单体 + SQLite，无 Node 构建、Redis、ORM 和消息队列 | 文章、动态、摄影统一成一条有辨识度的个人时间线 | 访问触发作者状态总结，并提供摘要、标签和 SEO 建议 |

Boop 只有站长发布内容，信息层级清晰、紧凑；读者可以注册、评论、回复、点赞与收藏。

## 🧩 功能

- **内容创作** — 动态、文章、摄影三种内容，首页快捷发布
- **读者互动** — 评论与一级回复、点赞、收藏、可选评论审核
- **身份认证** — 邮箱密码、GitHub OAuth、后台注册开关
- **AI 能力** — 作者状态总结、文章摘要、标签与 SEO 建议；建议不会自动覆盖草稿
- **媒体管理** — 图片上传、真实类型检测、大小限制与内容去重；可按需存到本地目录或兼容 S3 的对象存储（如 Cloudflare R2）
- **内容发现** — SQLite FTS5 全文搜索、结果页高亮与游标分页、RSS 2.0
- **站点体验** — 浅色/深色主题、响应式三栏布局、后台集中配置
- **安全基础** — CSRF、防开放重定向、加密保存第三方密钥、安全 Cookie 配置

## ⚡ 轻量到什么程度

Boop 面向 `1 vCPU / 500MB–1GB RAM` 的个人服务器：一个 Go 进程、一个 SQLite 数据库和一个上传目录即可运行。图片可以改存到兼容 S3 的对象存储（见 [`docs/STORAGE.md`](docs/STORAGE.md)），此时浏览器直接从对象存储取图，不占用本机带宽。AI 请求按需发生且带缓存，不需要常驻任务队列或定时任务。

```text
Browser ──> Go SSR + JSON API ──> SQLite (WAL + FTS5)
                  │
                  ├──> Local uploads
                  ├──> S3-compatible bucket (optional)
                  └──> OpenAI-compatible API (optional)
```

## 🚀 快速开始

### 环境要求

- Go `1.27+`
- 支持 SQLite FTS5 的目标平台

### 1. 获取代码

```bash
git clone https://github.com/Versior/Boop.git
cd Boop
```

### 2. 配置环境

参考 [`.env.example`](.env.example) 设置运行环境。生产环境至少配置：

| 变量 | 作用 |
| --- | --- |
| `BOOP_SESSION_SECRET` | 不少于 32 字节的随机会话密钥 |
| `BOOP_MASTER_KEY` | 32 字节随机数据的 Base64，用于加密 OAuth/AI 密钥 |
| `BOOP_BASE_URL` | 站点公开地址 |
| `BOOP_SECURE_COOKIES=true` | HTTPS 生产环境必须启用 |

PowerShell 生成随机值：

```powershell
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(48)) # session secret
[Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32)) # master key
```

### 3. 初始化站长并启动

```bash
go run ./cmd/boop init-owner --email owner@example.com --name "站长"
go run ./cmd/boop serve
```

打开 `http://127.0.0.1:8080`。默认数据目录为 `./data`；初始化命令会隐藏密码输入，并拒绝创建第二个 owner。

## 🏗️ 技术架构

| 层 | 方案 |
| --- | --- |
| 服务端 | Go `net/http`、`html/template`、embedded assets |
| 数据 | SQLite、WAL、FTS5、单进程写入模型 |
| 内容 | goldmark + bluemonday |
| 前端 | 原生 CSS / JavaScript，无独立前端服务 |
| AI | OpenAI-compatible API，可选启用、访问触发、结果缓存 |

```text
cmd/boop/          程序入口与 owner 初始化
internal/server/   HTTP、模板数据与静态资源服务
internal/store/    SQLite、迁移与事务边界
internal/settings/ 后台设置与密钥加密
web/templates/     服务端渲染模板
web/static/        CSS、JavaScript 与品牌资源
docs/              产品、API、数据库与实施说明
```

## 🧪 开发验证

```bash
gofmt -w .
go test ./... -count=1
go vet ./...
node --check web/static/app.js
git diff --check
```

## 📚 项目文档

- [产品规格](docs/PRODUCT.md)
- [HTTP API](docs/API.md)
- [数据库与事务规则](docs/DATABASE.md)
- [MVP 实施计划](docs/superpowers/plans/2026-09-28-boop-mvp.md)
- [AI 实施约束](docs/superpowers/task8-ai-implementation-brief.md)
- [搜索与 RSS 实施约束](docs/superpowers/task9-search-rss-implementation-brief.md)
- [智能体接手指南](AGENTS.md)

## 🗺️ 项目状态

MVP Tasks 1–9 已完成并通过测试与独立审查。下一阶段 Task 10 将补齐容器部署、Caddy 示例、备份恢复、端到端 smoke、低资源性能门禁和发布流程。

## 🔐 安全提示

- 不要提交 `.env`、API Key、OAuth Secret、Session Secret 或 `BOOP_MASTER_KEY`
- `BOOP_MASTER_KEY` 丢失后，已保存的 GitHub/AI 密钥无法恢复
- SQLite 数据库与上传目录必须作为同一份持久数据一起备份

## License

当前仓库尚未声明开源许可证；在许可证确定前，默认保留全部权利。

<p align="center">
  <sub>Designed for personal publishing. Built to stay small.</sub>
</p>
